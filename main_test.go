package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var relayBinary string

func TestMain(m *testing.M) {
	if os.Getenv("MCP_LAZY_FAKE_SERVER") == "1" {
		fakeServer()
		return
	}
	dir, err := os.MkdirTemp("", "mcp-lazy-bin-")
	if err != nil {
		panic(err)
	}
	relayBinary = filepath.Join(dir, "mcp-lazy")
	if out, err := exec.Command("go", "build", "-o", relayBinary, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeServer is a small stdio MCP server. It appends what happens to FAKE_LOG.
func fakeServer() {
	note := func(text string) {
		f, err := os.OpenFile(os.Getenv("FAKE_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintln(f, text)
			f.Close()
		}
	}
	note("start")
	tools := os.Getenv("FAKE_TOOLS")
	if tools == "" {
		tools = "echo"
	}
	out := bufio.NewWriter(os.Stdout)
	send := func(text string) { out.WriteString(text + "\n"); out.Flush() }
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 1 {
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			json.Unmarshal(line, &m)
			switch m.Method {
			case "initialize":
				var p struct {
					ClientInfo struct {
						Name string `json:"name"`
					} `json:"clientInfo"`
				}
				json.Unmarshal(m.Params, &p)
				note("initialize client=" + p.ClientInfo.Name)
				send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"fake","version":"1"},"instructions":"fake instructions"}}`)
			case "server/discover":
				note("discover")
				send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"error":{"code":-32601,"message":"Method not found"}}`)
			case "notifications/initialized":
				note("initialized")
			case "tools/list":
				var list []string
				for _, name := range strings.Split(tools, ",") {
					list = append(list, `{"name":"`+name+`", "description":"a <b> & c", "inputSchema":{"type":"object"}}`)
				}
				send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"tools":[` + strings.Join(list, ",") + `]}}`)
			case "tools/call":
				var p struct {
					Name string          `json:"name"`
					Meta json.RawMessage `json:"_meta"`
				}
				json.Unmarshal(m.Params, &p)
				note("call " + p.Name + " meta=" + string(p.Meta))
				if p.Name == "crash" {
					os.Exit(3)
				}
				cwd, _ := os.Getwd()
				text, _ := json.Marshal("env=" + os.Getenv("FAKE_SEAT") + " cwd=" + cwd)
				send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"content":[{"type":"text","text":` + string(text) + `}]}}`)
			}
		}
		if err != nil {
			note("eof")
			return
		}
	}
}

type client struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     io.WriteCloser
	lines  chan string
	log    string
	cache  string
	closed bool
}

func start(t *testing.T, dir string, env []string, flags ...string) *client {
	t.Helper()
	c := &client{t: t, log: filepath.Join(dir, "server.log"), cache: filepath.Join(dir, "cache"), lines: make(chan string, 64)}
	args := append([]string{"--cache-dir", c.cache}, flags...)
	args = append(args, "--", os.Args[0], "-test.run=^$")
	c.cmd = exec.Command(relayBinary, args...)
	c.cmd.Dir = dir
	c.cmd.Env = append(os.Environ(), "MCP_LAZY_FAKE_SERVER=1", "FAKE_LOG="+c.log)
	c.cmd.Env = append(c.cmd.Env, env...)
	c.cmd.Stderr = os.Stderr
	var err error
	if c.in, err = c.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1<<20), 1<<24)
		for scanner.Scan() {
			c.lines <- scanner.Text()
		}
		close(c.lines)
	}()
	t.Cleanup(c.close)
	return c
}

func (c *client) close() {
	if c.closed {
		return
	}
	c.closed = true
	c.in.Close()
	done := make(chan struct{})
	go func() { c.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		c.cmd.Process.Kill()
		c.t.Error("mcp-lazy did not exit after its input closed")
	}
}

func (c *client) send(text string) { fmt.Fprintln(c.in, text) }

func (c *client) read() map[string]json.RawMessage {
	c.t.Helper()
	select {
	case line, ok := <-c.lines:
		if !ok {
			c.t.Fatal("mcp-lazy closed its output")
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			c.t.Fatalf("not JSON: %s", line)
		}
		return m
	case <-time.After(10 * time.Second):
		c.t.Fatal("no answer from mcp-lazy")
		return nil
	}
}

// answer reads until the response with this id arrives; other lines are returned too.
func (c *client) answer(id string) (map[string]json.RawMessage, []map[string]json.RawMessage) {
	c.t.Helper()
	var others []map[string]json.RawMessage
	for {
		m := c.read()
		if string(m["id"]) == id && m["method"] == nil {
			return m, others
		}
		others = append(others, m)
	}
}

func (c *client) handshake(name string) {
	c.t.Helper()
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"` + name + `","version":"9"}}}`)
	m, _ := c.answer("1")
	if !strings.Contains(string(m["result"]), "fake instructions") {
		c.t.Fatalf("unexpected initialize result: %s", m["result"])
	}
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	m, _ = c.answer("2")
	if !strings.Contains(string(m["result"]), `"echo"`) {
		c.t.Fatalf("unexpected tools/list result: %s", m["result"])
	}
}

func (c *client) serverLog() string {
	data, _ := os.ReadFile(c.log)
	return string(data)
}

func (c *client) waitLog(want string) {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(c.serverLog(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("server log never contained %q:\n%s", want, c.serverLog())
}

// record runs once without a cache so the handshake is recorded.
func record(t *testing.T, dir string, env []string) {
	t.Helper()
	c := start(t, dir, env)
	c.handshake("recorder")
	c.close()
	if !strings.Contains(c.serverLog(), "start") {
		t.Fatal("the first run must start the server to record the handshake")
	}
	os.Remove(c.log)
}

func TestFirstRunPassesThroughAndRecords(t *testing.T) {
	dir := t.TempDir()
	c := start(t, dir, nil)
	c.handshake("first")
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	if m, _ := c.answer("3"); !strings.Contains(string(m["result"]), "cwd=") {
		t.Fatalf("unexpected call result: %s", m["result"])
	}
	if got := strings.Count(c.serverLog(), "start"); got != 1 {
		t.Fatalf("server started %d times, want 1", got)
	}
	files, _ := os.ReadDir(c.cache)
	if len(files) != 1 {
		t.Fatalf("cache files: %d, want 1", len(files))
	}
}

func TestHandshakeAndListingDoNotStartTheServer(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, nil)
	c := start(t, dir, nil)
	c.handshake("idle-seat")
	c.send(`{"jsonrpc":"2.0","id":5,"method":"ping"}`)
	c.answer("5")
	time.Sleep(300 * time.Millisecond)
	if log := c.serverLog(); log != "" {
		t.Fatalf("the server ran although nothing needed it:\n%s", log)
	}
}

func TestFirstCallStartsTheServerAsThisClient(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, []string{"FAKE_SEAT=recorder"})
	c := start(t, dir, []string{"FAKE_SEAT=seat-b"})
	c.handshake("seat-b-cli")
	c.send(`{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"echo","arguments":{},"_meta":{"threadId":"t-42"}}}`)
	m, _ := c.answer(`"call-1"`)
	real, _ := filepath.EvalSymlinks(dir)
	if text := string(m["result"]); !strings.Contains(text, "env=seat-b") || !strings.Contains(text, "cwd="+real) {
		t.Fatalf("the server did not run in this client's environment and directory: %s (want cwd=%s)", text, real)
	}
	log := c.serverLog()
	for _, want := range []string{"initialize client=seat-b-cli", "initialized", `call echo meta={"threadId":"t-42"}`} {
		if !strings.Contains(log, want) {
			t.Fatalf("server log lacks %q:\n%s", want, log)
		}
	}
	c.send(`{"jsonrpc":"2.0","id":"call-2","method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	c.answer(`"call-2"`)
	if got := strings.Count(c.serverLog(), "start"); got != 1 {
		t.Fatalf("server started %d times, want 1", got)
	}
}

func TestUnchangedToolsAreNotAnnounced(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, nil)
	c := start(t, dir, nil)
	c.handshake("seat")
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	if _, others := c.answer("3"); len(others) != 0 {
		t.Fatalf("unexpected lines before the answer: %v", others)
	}
	c.send(`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	if _, others := c.answer("4"); len(others) != 0 {
		t.Fatalf("the client was told the tools changed although they did not: %v", others)
	}
}

func TestChangedToolsAreAnnouncedAfterTheServerStarts(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, nil)
	c := start(t, dir, []string{"FAKE_TOOLS=echo,added"})
	c.handshake("seat")
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	_, before := c.answer("3")
	seen := before
	for len(seen) == 0 {
		seen = append(seen, c.read())
	}
	if method := string(seen[0]["method"]); method != `"notifications/tools/list_changed"` {
		t.Fatalf("expected a list_changed notification, got %v", seen[0])
	}
	c.send(`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	if m, _ := c.answer("4"); !strings.Contains(string(m["result"]), `"added"`) {
		t.Fatalf("tools/list did not come from the running server: %s", m["result"])
	}
}

func TestServerCrashAnswersTheCallAndTheNextCallRestarts(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, nil)
	c := start(t, dir, nil)
	c.handshake("seat")
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"crash","arguments":{}}}`)
	if m, _ := c.answer("3"); !strings.Contains(string(m["error"]), "server exited") {
		t.Fatalf("expected an error for the crashed call, got %v", m)
	}
	c.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	if m, _ := c.answer("4"); m["result"] == nil {
		t.Fatalf("the call after a crash failed: %v", m)
	}
	if got := strings.Count(c.serverLog(), "start"); got != 2 {
		t.Fatalf("server started %d times, want 2", got)
	}
}

func TestIdleStopEndsTheServerAndTheNextCallRestarts(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, nil)
	c := start(t, dir, nil, "--idle-stop", "300ms")
	c.handshake("seat")
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	c.answer("3")
	c.waitLog("eof")
	c.send(`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	c.answer("4")
	c.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	if m, _ := c.answer("5"); m["result"] == nil {
		t.Fatalf("the call after an idle stop failed: %v", m)
	}
	if got := strings.Count(c.serverLog(), "start"); got != 2 {
		t.Fatalf("server started %d times, want 2", got)
	}
}

func TestClosingInputStopsTheServer(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, nil)
	c := start(t, dir, nil)
	c.handshake("seat")
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	c.answer("3")
	c.close()
	c.waitLog("eof")
}

// Claude Code and Grok ask server/discover before initialize and fall back when it fails.
func TestRequestAheadOfInitializeIsRecordedAndThenAnsweredWithoutTheServer(t *testing.T) {
	dir := t.TempDir()
	discover := `{"jsonrpc":"2.0","id":0,"method":"server/discover","params":{"protocolVersion":"2026-07-28"}}`
	c := start(t, dir, nil)
	c.send(discover)
	if m, _ := c.answer("0"); !strings.Contains(string(m["error"]), "Method not found") {
		t.Fatalf("the first run must pass the server's own answer on: %v", m)
	}
	c.handshake("recorder")
	c.close()
	if log := c.serverLog(); !strings.Contains(log, "discover") || strings.Count(log, "start") != 1 {
		t.Fatalf("unexpected first run:\n%s", log)
	}
	os.Remove(c.log)

	c = start(t, dir, nil)
	c.send(discover)
	if m, _ := c.answer("0"); !strings.Contains(string(m["error"]), "Method not found") {
		t.Fatalf("the recorded answer was not replayed: %v", m)
	}
	c.handshake("idle-seat")
	time.Sleep(300 * time.Millisecond)
	if log := c.serverLog(); log != "" {
		t.Fatalf("the server ran although nothing needed it:\n%s", log)
	}
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	if m, _ := c.answer("3"); m["result"] == nil {
		t.Fatalf("the call failed: %v", m)
	}
	if log := c.serverLog(); !strings.Contains(log, "initialize client=idle-seat") {
		t.Fatalf("the server did not get this client's handshake:\n%s", log)
	}
}

func TestPagedListingIsLeftToTheServer(t *testing.T) {
	if plainListing(json.RawMessage(`{"cursor":"abc"}`)) {
		t.Fatal("a listing with a cursor must not be answered from the cache")
	}
	for _, params := range []string{``, `null`, `{}`, `{"cursor":null}`, `{"_meta":{"progressToken":1}}`} {
		if !plainListing(json.RawMessage(params)) {
			t.Fatalf("%q is a plain first-page listing", params)
		}
	}
}
