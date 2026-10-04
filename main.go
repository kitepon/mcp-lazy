// mcp-lazy starts a stdio MCP server only when a client actually needs it.
//
// It sits between an MCP client and a stdio MCP server:
//
//	mcp-lazy [flags] -- <command> [args...]
//
// The handshake (initialize) and the listings (tools/list and friends) are
// answered from a cache recorded on an earlier run. The real server is started
// on the first request that needs it, in this process's own environment and
// working directory, and is given the client's original initialize request.
// From then on mcp-lazy is a plain pipe in both directions.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"time"
)

const version = "0.1.0"

// cacheSchema changes whenever the cache file layout changes.
const cacheSchema = 1

// The listings a client asks for right after the handshake.
var listMethods = map[string]bool{
	"tools/list":               true,
	"prompts/list":             true,
	"resources/list":           true,
	"resources/templates/list": true,
}

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

func (m *message) hasID() bool { return len(m.ID) > 0 && string(m.ID) != "null" }

// entry is what one server answered for one protocol version.
type entry struct {
	Initialize json.RawMessage            `json:"initialize"`
	Lists      map[string]json.RawMessage `json:"lists"`
}

type cacheFile struct {
	Schema  int               `json:"schema"`
	Command []string          `json:"command"`
	Entries map[string]*entry `json:"entries"`
	// Before maps a request a client makes ahead of initialize (server/discover)
	// to the response the server gave, as {"result":...} or {"error":...}.
	Before map[string]json.RawMessage `json:"before,omitempty"`
}

type relay struct {
	command   []string
	cachePath string
	idleStop  time.Duration
	log       func(string, ...any)

	out   *bufio.Writer
	outMu sync.Mutex

	cache *cacheFile

	// Handshake as the client sent it, replayed to the server when it starts.
	initParams  json.RawMessage
	proto       string
	initialized bool

	// Server process.
	cmd      *exec.Cmd
	childIn  io.WriteCloser
	running  bool
	starting bool
	queue    [][]byte // client lines waiting for the server's handshake
	spawns   int
	initID   string
	checkIDs map[string]string // internal request id -> listing method being verified

	inflight       map[string]string // client request id -> method, awaiting the server
	early          map[string]bool   // client request ids sent before initialize
	earlyRequests  map[string]message
	uncacheable    map[string]bool // client request ids whose listing answer must not be cached
	serverInflight map[string]bool // server request ids awaiting the client

	childLines chan []byte
	childDone  chan error
}

func main() {
	fs := flag.NewFlagSet("mcp-lazy", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: mcp-lazy [flags] -- <command> [args...]\n\nflags:\n")
		fs.PrintDefaults()
	}
	cacheDir := fs.String("cache-dir", "", "directory for the recorded handshake (default: the user cache directory)")
	idleStop := fs.Duration("idle-stop", 0, "stop the server after this long without traffic; 0 keeps it running once started")
	showVersion := fs.Bool("version", false, "print the version and exit")
	verbose := fs.Bool("verbose", false, "log what mcp-lazy does to stderr")
	logFile := fs.String("log-file", "", "append what mcp-lazy does to this file")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *showVersion {
		fmt.Println("mcp-lazy", version)
		return
	}
	command := fs.Args()
	if len(command) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	dir := *cacheDir
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		dir = filepath.Join(base, "mcp-lazy")
	}
	r := &relay{
		command:        command,
		cachePath:      filepath.Join(dir, cacheKey(command)+".json"),
		idleStop:       *idleStop,
		out:            bufio.NewWriter(os.Stdout),
		inflight:       map[string]string{},
		early:          map[string]bool{},
		earlyRequests:  map[string]message{},
		uncacheable:    map[string]bool{},
		serverInflight: map[string]bool{},
		checkIDs:       map[string]string{},
		childLines:     make(chan []byte, 64),
		childDone:      make(chan error, 1),
		log:            func(string, ...any) {},
	}
	if *verbose || *logFile != "" {
		r.log = func(format string, args ...any) {
			text := fmt.Sprintf("mcp-lazy[%d] %s "+format+"\n", append([]any{os.Getpid(), time.Now().UTC().Format("15:04:05.000")}, args...)...)
			if *verbose {
				os.Stderr.WriteString(text)
			}
			if *logFile != "" {
				if f, err := os.OpenFile(*logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
					f.WriteString(text)
					f.Close()
				}
			}
		}
	}
	r.cache = loadCache(r.cachePath, command)
	os.Exit(r.run(os.Stdin))
}

// cacheKey names the cache after the command and the files it runs, so that an
// upgraded server records a fresh handshake instead of serving a stale one.
func cacheKey(command []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "schema=%d\n", cacheSchema)
	for i, arg := range command {
		fmt.Fprintf(h, "arg=%s\n", arg)
		path := arg
		if i == 0 {
			if found, err := exec.LookPath(arg); err == nil {
				path = found
			}
		}
		if abs, err := filepath.Abs(path); err == nil {
			if info, err := os.Stat(abs); err == nil && info.Mode().IsRegular() {
				fmt.Fprintf(h, "file=%s size=%d mtime=%d\n", abs, info.Size(), info.ModTime().UnixNano())
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func loadCache(path string, command []string) *cacheFile {
	fresh := &cacheFile{Schema: cacheSchema, Command: command, Entries: map[string]*entry{}, Before: map[string]json.RawMessage{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return fresh
	}
	var c cacheFile
	if json.Unmarshal(data, &c) != nil || c.Schema != cacheSchema || c.Entries == nil {
		return fresh
	}
	c.Command = command
	if c.Before == nil {
		c.Before = map[string]json.RawMessage{}
	}
	return &c
}

// sameJSON compares two JSON texts by value. The cache file is re-encoded when it
// is written, so the bytes differ from what the server sent even when nothing changed.
func sameJSON(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(x, y)
}

func (r *relay) saveCache() {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(r.cache) != nil {
		return
	}
	data := buffer.Bytes()
	dir := filepath.Dir(r.cachePath)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), r.cachePath) != nil {
		os.Remove(tmp.Name())
	}
}

func (r *relay) entry() *entry {
	if r.proto == "" {
		return nil
	}
	return r.cache.Entries[r.proto]
}

func (r *relay) ensureEntry() *entry {
	e := r.cache.Entries[r.proto]
	if e == nil {
		e = &entry{Lists: map[string]json.RawMessage{}}
		r.cache.Entries[r.proto] = e
	}
	if e.Lists == nil {
		e.Lists = map[string]json.RawMessage{}
	}
	return e
}

func (r *relay) run(stdin io.Reader) int {
	clientLines := make(chan []byte, 64)
	clientDone := make(chan struct{})
	go func() {
		readLines(stdin, clientLines)
		close(clientLines)
		close(clientDone)
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	var idle <-chan time.Time
	var idleTimer *time.Timer
	touch := func() {
		if r.idleStop <= 0 {
			return
		}
		if idleTimer == nil {
			idleTimer = time.NewTimer(r.idleStop)
			idle = idleTimer.C
			return
		}
		idleTimer.Reset(r.idleStop)
	}

	for {
		select {
		case line, ok := <-clientLines:
			if !ok {
				clientLines = nil
				continue
			}
			touch()
			r.fromClient(line)
		case line := <-r.childLines:
			touch()
			r.fromChild(line)
		case err := <-r.childDone:
			r.drainChild()
			r.childExited(err)
		case <-idle:
			if r.running && !r.starting && len(r.inflight) == 0 && len(r.serverInflight) == 0 && r.entry() != nil {
				r.log("idle for %s, stopping the server", r.idleStop)
				r.stopChild()
			} else {
				touch()
			}
		case <-clientDone:
			// Lines already read are handled before leaving.
			for clientLines != nil {
				select {
				case line, ok := <-clientLines:
					if !ok {
						clientLines = nil
						break
					}
					r.fromClient(line)
				default:
					clientLines = nil
				}
			}
			r.stopChild()
			return 0
		case <-signals:
			r.stopChild()
			return 0
		}
	}
}

// readLines sends every non-empty line of src. The caller closes dst if it wants to.
func readLines(src io.Reader, dst chan<- []byte) {
	reader := bufio.NewReaderSize(src, 1<<16)
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			dst <- trimmed
		}
		if err != nil {
			return
		}
	}
}

func (r *relay) toClient(line []byte) {
	r.outMu.Lock()
	defer r.outMu.Unlock()
	r.out.Write(line)
	r.out.WriteByte('\n')
	r.out.Flush()
}

func (r *relay) reply(id json.RawMessage, result json.RawMessage) {
	r.toClient([]byte(`{"jsonrpc":"2.0","id":` + string(id) + `,"result":` + string(result) + `}`))
}

func (r *relay) replyError(id json.RawMessage, text string) {
	quoted, _ := json.Marshal(text)
	r.toClient([]byte(`{"jsonrpc":"2.0","id":` + string(id) + `,"error":{"code":-32000,"message":` + string(quoted) + `}}`))
}

// plainListing reports whether a listing request asks for the first, unpaged page.
func plainListing(params json.RawMessage) bool {
	if len(params) == 0 || string(params) == "null" {
		return true
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(params, &fields) != nil {
		return false
	}
	for key, value := range fields {
		if key == "_meta" {
			continue
		}
		if key == "cursor" && string(value) == "null" {
			continue
		}
		return false
	}
	return true
}

func (r *relay) fromClient(line []byte) {
	var m message
	if line[0] != '{' || json.Unmarshal(line, &m) != nil {
		// A batch or something we cannot read: the server decides.
		r.forward(line, nil)
		return
	}
	if m.Method == "" {
		// A response to a request the server made.
		if m.hasID() {
			delete(r.serverInflight, string(m.ID))
		}
		if r.running {
			r.writeChild(line)
		}
		return
	}
	switch {
	case m.Method == "initialize" && m.hasID():
		r.initParams = m.Params
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(m.Params, &params)
		r.proto = params.ProtocolVersion
		if e := r.entry(); e != nil && len(e.Initialize) > 0 && !r.running {
			r.log("handshake answered from the cache (protocol %s)", r.proto)
			r.reply(m.ID, e.Initialize)
			return
		}
		if r.running {
			r.log("handshake passed to the running server (protocol %s)", r.proto)
		} else {
			r.log("no recorded handshake for protocol %q, starting the server", r.proto)
		}
		r.forward(line, &m)
	case m.Method == "notifications/initialized":
		r.initialized = true
		if r.running && !r.starting {
			r.writeChild(line)
		}
	case !r.running && m.Method == "ping" && m.hasID():
		r.reply(m.ID, json.RawMessage(`{}`))
	case !r.running && listMethods[m.Method] && m.hasID() && plainListing(m.Params) && r.cachedList(m.Method) != nil:
		r.reply(m.ID, r.cachedList(m.Method))
	case !r.running && !m.hasID():
		// A notification with no server to hear it (cancelled, roots changed, ...).
	case r.proto == "" && m.hasID():
		// Asked ahead of initialize: newer clients try server/discover first.
		if answer := r.cache.Before[earlyKey(&m)]; len(answer) > 0 && !r.running {
			r.log("%s answered from the cache", m.Method)
			r.toClient([]byte(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,` + string(answer[1:])))
			return
		}
		r.early[string(m.ID)] = true
		r.earlyRequests[string(m.ID)] = m
		r.forward(line, &m)
	default:
		r.forward(line, &m)
	}
}

// earlyKey names a request made ahead of initialize by its method and parameters.
func earlyKey(m *message) string {
	sum := sha256.Sum256(m.Params)
	return m.Method + ":" + hex.EncodeToString(sum[:8])
}

func (r *relay) cachedList(method string) json.RawMessage {
	if e := r.entry(); e != nil {
		return e.Lists[method]
	}
	return nil
}

// forward hands a client line to the server, starting it first when needed.
func (r *relay) forward(line []byte, m *message) {
	if m != nil && m.hasID() && m.Method != "" {
		r.inflight[string(m.ID)] = m.Method
		if listMethods[m.Method] && !plainListing(m.Params) {
			r.uncacheable[string(m.ID)] = true
		}
	}
	if r.running && !r.starting {
		r.writeChild(line)
		return
	}
	if !r.running {
		// A server started lazily gets the client's handshake first. Without one
		// (the first run, or a request ahead of initialize) the lines pass as they are.
		replay := len(r.initParams) > 0 && !(m != nil && m.Method == "initialize")
		if err := r.startChild(replay); err != nil {
			r.log("cannot start the server: %v", err)
			r.failInflight("mcp-lazy: cannot start the server: " + err.Error())
			return
		}
		if !replay {
			r.writeChild(line)
			return
		}
	}
	r.queue = append(r.queue, line)
}

func (r *relay) startChild(replayHandshake bool) error {
	cmd := exec.Command(r.command[0], r.command[1:]...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	r.spawns++
	r.cmd, r.childIn, r.running = cmd, stdin, true
	lines, done := r.childLines, r.childDone
	go func() {
		readLines(stdout, lines)
		done <- cmd.Wait()
	}()
	r.log("server started (pid %d)", cmd.Process.Pid)
	if replayHandshake {
		r.starting = true
		r.initID = fmt.Sprintf("mcp-lazy-init-%d", r.spawns)
		r.writeChild([]byte(`{"jsonrpc":"2.0","id":"` + r.initID + `","method":"initialize","params":` + string(r.initParams) + `}`))
	}
	return nil
}

func (r *relay) writeChild(line []byte) {
	if r.childIn == nil {
		return
	}
	r.childIn.Write(append(append([]byte{}, line...), '\n'))
}

func (r *relay) fromChild(line []byte) {
	var m message
	if line[0] != '{' || json.Unmarshal(line, &m) != nil {
		r.toClient(line)
		return
	}
	if m.Method != "" {
		if m.hasID() {
			r.serverInflight[string(m.ID)] = true
		}
		r.toClient(line)
		return
	}
	if !m.hasID() {
		r.toClient(line)
		return
	}
	var id string
	if json.Unmarshal(m.ID, &id) == nil {
		if r.starting && id == r.initID {
			r.handshakeDone(&m)
			return
		}
		if method, ok := r.checkIDs[id]; ok {
			delete(r.checkIDs, id)
			r.verifyList(method, &m)
			return
		}
	}
	key := string(m.ID)
	method, known := r.inflight[key]
	delete(r.inflight, key)
	skip := r.uncacheable[key]
	delete(r.uncacheable, key)
	if r.early[key] {
		delete(r.early, key)
		r.learnEarly(line, &m)
	} else if known && len(m.Result) > 0 {
		r.learn(method, m.Result, skip)
	}
	r.toClient(line)
}

// learnEarly records the answer to a request made ahead of initialize.
func (r *relay) learnEarly(line []byte, m *message) {
	var request message
	for id, pending := range r.earlyRequests {
		if id == string(m.ID) {
			request = pending
			delete(r.earlyRequests, id)
		}
	}
	if request.Method == "" {
		return
	}
	var answer []byte
	switch {
	case len(m.Result) > 0:
		answer = []byte(`{"result":` + string(m.Result) + `}`)
	case len(m.Error) > 0:
		answer = []byte(`{"error":` + string(m.Error) + `}`)
	default:
		return
	}
	key := earlyKey(&request)
	if !sameJSON(r.cache.Before[key], answer) {
		r.cache.Before[key] = answer
		r.saveCache()
	}
}

// learn records what the server answered, for the next run.
func (r *relay) learn(method string, result json.RawMessage, skip bool) {
	if r.proto == "" {
		return
	}
	switch {
	case method == "initialize":
		e := r.ensureEntry()
		if !sameJSON(e.Initialize, result) {
			e.Initialize = append(json.RawMessage{}, result...)
			e.Lists = map[string]json.RawMessage{}
			r.saveCache()
		}
	case listMethods[method] && !skip && r.entry() != nil:
		var page struct {
			NextCursor *string `json:"nextCursor"`
		}
		if json.Unmarshal(result, &page) != nil || (page.NextCursor != nil && *page.NextCursor != "") {
			return
		}
		e := r.ensureEntry()
		if !sameJSON(e.Lists[method], result) {
			e.Lists[method] = append(json.RawMessage{}, result...)
			r.saveCache()
		}
	}
}

// handshakeDone runs when a lazily started server has answered the replayed initialize.
func (r *relay) handshakeDone(m *message) {
	r.starting = false
	if len(m.Result) == 0 {
		r.log("the server refused the replayed handshake")
		r.failInflight("mcp-lazy: the server refused the handshake")
		r.queue = nil
		r.stopChild()
		return
	}
	e := r.ensureEntry()
	changed := !sameJSON(e.Initialize, m.Result)
	if changed {
		r.log("the server's handshake differs from the recorded one, recording the new one")
		e.Initialize = append(json.RawMessage{}, m.Result...)
		r.saveCache()
	}
	r.writeChild([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	// Check the listings the client was served from the cache against the real ones.
	for method := range e.Lists {
		id := fmt.Sprintf("mcp-lazy-check-%d-%s", r.spawns, method)
		r.checkIDs[id] = method
		r.writeChild([]byte(`{"jsonrpc":"2.0","id":"` + id + `","method":"` + method + `"}`))
	}
	for _, line := range r.queue {
		r.writeChild(line)
	}
	r.queue = nil
}

var listChanged = map[string]string{
	"tools/list":               "notifications/tools/list_changed",
	"prompts/list":             "notifications/prompts/list_changed",
	"resources/list":           "notifications/resources/list_changed",
	"resources/templates/list": "notifications/resources/list_changed",
}

func (r *relay) verifyList(method string, m *message) {
	e := r.entry()
	if e == nil || len(m.Result) == 0 || sameJSON(e.Lists[method], m.Result) {
		return
	}
	r.log("%s differs from the recorded one, telling the client", method)
	var page struct {
		NextCursor *string `json:"nextCursor"`
	}
	if json.Unmarshal(m.Result, &page) == nil && (page.NextCursor == nil || *page.NextCursor == "") {
		e.Lists[method] = append(json.RawMessage{}, m.Result...)
	} else {
		delete(e.Lists, method)
	}
	r.saveCache()
	r.toClient([]byte(`{"jsonrpc":"2.0","method":"` + listChanged[method] + `"}`))
}

func (r *relay) drainChild() {
	for {
		select {
		case line := <-r.childLines:
			r.fromChild(line)
		default:
			return
		}
	}
}

func (r *relay) childExited(err error) {
	r.log("server exited (%v)", err)
	r.running, r.starting, r.cmd, r.childIn = false, false, nil, nil
	r.queue = nil
	r.checkIDs = map[string]string{}
	r.serverInflight = map[string]bool{}
	r.failInflight("mcp-lazy: the server exited before answering")
}

func (r *relay) failInflight(text string) {
	for id := range r.inflight {
		r.replyError(json.RawMessage(id), text)
	}
	r.inflight = map[string]string{}
	r.uncacheable = map[string]bool{}
	r.early = map[string]bool{}
	r.earlyRequests = map[string]message{}
}

// stopChild closes the server's input, waits briefly, then ends it.
func (r *relay) stopChild() {
	if !r.running || r.cmd == nil {
		return
	}
	r.childIn.Close()
	if r.waitChild(3 * time.Second) {
		return
	}
	r.cmd.Process.Signal(syscall.SIGTERM)
	if r.waitChild(2 * time.Second) {
		return
	}
	r.cmd.Process.Kill()
	r.waitChild(time.Hour)
}

// waitChild keeps passing the server's remaining output on while it waits for the exit.
func (r *relay) waitChild(limit time.Duration) bool {
	timeout := time.After(limit)
	for {
		select {
		case line := <-r.childLines:
			r.fromChild(line)
		case err := <-r.childDone:
			r.drainChild()
			r.childExited(err)
			return true
		case <-timeout:
			return false
		}
	}
}
