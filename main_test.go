package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var relayBinary string

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "wake-probe" {
		fakeWakeProbe()
		return
	}
	if os.Getenv("MCP_LAZY_FAKE_GRANDCHILD") == "1" {
		signal.Ignore(syscall.SIGTERM)
		os.WriteFile(os.Getenv("FAKE_GRANDCHILD_PID"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		for {
			time.Sleep(time.Hour)
		}
	}
	if os.Getenv("MCP_LAZY_FAKE_SERVER") == "1" {
		fakeServer()
		return
	}
	dir, err := os.MkdirTemp("", "mcp-lazy-bin-")
	if err != nil {
		panic(err)
	}
	relayBinary = filepath.Join(dir, "mcp-lazy")
	buildArgs := []string{"build", "-o", relayBinary}
	if os.Getenv("MCP_LAZY_TEST_RACE") == "1" {
		buildArgs = append(buildArgs, "-race")
	}
	buildArgs = append(buildArgs, ".")
	if out, err := exec.Command("go", buildArgs...).CombinedOutput(); err != nil {
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
	if os.Getenv("FAKE_IGNORE_STOP") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	if path := os.Getenv("FAKE_GRANDCHILD_PID"); path != "" {
		child := exec.Command(os.Args[0], "-test.run=^$")
		child.Env = append(os.Environ(), "MCP_LAZY_FAKE_SERVER=0", "MCP_LAZY_FAKE_GRANDCHILD=1")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if child.Start() != nil {
			os.Exit(4)
		}
		go child.Wait()
	}
	if os.Getenv("FAKE_NEVER_READ") == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}
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
				if marker := os.Getenv("FAKE_HANG_MARKER"); marker != "" {
					if _, err := os.Stat(marker); err == nil {
						note("hang initialize")
						continue
					}
				}
				var p struct {
					ClientInfo struct {
						Name string `json:"name"`
					} `json:"clientInfo"`
				}
				json.Unmarshal(m.Params, &p)
				note("initialize client=" + p.ClientInfo.Name)
				capabilities := `{"tools":{"listChanged":true}}`
				if os.Getenv("FAKE_ALL_LISTS") == "1" {
					capabilities = `{"tools":{},"prompts":{},"resources":{}}`
				}
				send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"protocolVersion":"2025-06-18","capabilities":` + capabilities + `,"serverInfo":{"name":"fake","version":"1"},"instructions":"fake instructions"}}`)
			case "server/discover":
				if marker := os.Getenv("FAKE_HANG_DISCOVER_MARKER"); marker != "" {
					if _, err := os.Stat(marker); err == nil {
						note("hang discover")
						continue
					}
				}
				note("discover")
				if os.Getenv("FAKE_DISCOVER_SUCCESS") == "1" {
					send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"probe":` + string(m.Params) + `}}`)
					continue
				}
				if os.Getenv("FAKE_DISCOVER_ERROR") == "1" {
					send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"error":{"code":-32000,"message":"unavailable"}}`)
					continue
				}
				send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"error":{"code":-32601,"message":"Method not found"}}`)
			case "notifications/initialized":
				note("initialized")
			case "tools/list":
				if marker := os.Getenv("FAKE_HANG_LIST_MARKER"); marker != "" {
					if _, err := os.Stat(marker); err == nil {
						note("hang list")
						continue
					}
				}
				if os.Getenv("FAKE_HANG_LISTS") == "1" {
					note("hang list")
					continue
				}
				if os.Getenv("FAKE_LIST_ERROR") == "1" {
					send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"error":{"code":-32000,"message":"list unavailable"}}`)
					continue
				}
				var list []string
				for _, name := range strings.Split(tools, ",") {
					list = append(list, `{"name":"`+name+`", "description":"a <b> & c", "inputSchema":{"type":"object"}}`)
				}
				cursor := ""
				if os.Getenv("FAKE_PAGINATED") == "1" && !strings.Contains(string(m.Params), "cursor") {
					cursor = `,"nextCursor":"page2"`
				}
				send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"tools":[` + strings.Join(list, ",") + `]` + cursor + `}}`)
			case "prompts/list":
				send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"prompts":[{"name":"hello"}]}}`)
			case "resources/list":
				send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"resources":[{"uri":"test://one","name":"one"}]}}`)
			case "resources/templates/list":
				send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"resourceTemplates":[{"uriTemplate":"test://{id}","name":"template"}]}}`)
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
				if p.Name == "slow" {
					time.Sleep(400 * time.Millisecond)
				}
				cwd, _ := os.Getwd()
				text, _ := json.Marshal("env=" + os.Getenv("FAKE_SEAT") + " cwd=" + cwd)
				send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"content":[{"type":"text","text":` + string(text) + `}],"_meta":{"threadId":"reply-thread","nested":[1,true]}},"extension":"retained"}`)
			}
		}
		if err != nil {
			note("eof")
			if os.Getenv("FAKE_IGNORE_STOP") == "1" {
				for {
					time.Sleep(time.Hour)
				}
			}
			return
		}
	}
}

func TestStartupTimeoutWhenServerDoesNotReadLargeInitialize(t *testing.T) {
	dir := t.TempDir()
	c := start(t, dir, []string{"FAKE_NEVER_READ=1"}, "--start-timeout", "100ms")
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "seat", "version": "1"}, "large": strings.Repeat("x", 1<<20)}})
	c.send(string(payload))
	if m, _ := c.answer("1"); !strings.Contains(string(m["error"]), "timed out") {
		t.Fatalf("expected timeout: %v", m)
	}
	c.close()
}

func TestGrandchildCleanupOnSignalIdleAndStartupTimeout(t *testing.T) {
	for _, action := range []string{"signal", "idle", "timeout", "kill-escalation"} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "grandchild.pid")
			env := []string{"FAKE_GRANDCHILD_PID=" + path}
			flags := []string{"--start-timeout", "100ms"}
			if action == "idle" {
				flags = append(flags, "--idle-stop", "100ms")
			}
			if action == "timeout" {
				env = append(env, "FAKE_NEVER_READ=1")
			}
			if action == "kill-escalation" {
				env = append(env, "FAKE_IGNORE_STOP=1")
			}
			c := start(t, dir, env, flags...)
			if action == "timeout" {
				c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
			} else {
				c.handshake("seat")
			}
			pid := waitGrandchild(t, path)
			switch action {
			case "signal":
				c.cmd.Process.Signal(syscall.SIGTERM)
				c.close()
			case "idle":
				c.waitLog("eof")
			case "timeout":
				if m, _ := c.answer("1"); !strings.Contains(string(m["error"]), "timed out") {
					t.Fatalf("expected timeout: %v", m)
				}
			case "kill-escalation":
				c.close()
			}
			assertGrandchildStopped(t, pid)
		})
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
	return startArgs(t, dir, env, args)
}

func startArgs(t *testing.T, dir string, env, args []string) *client {
	t.Helper()
	c := &client{t: t, log: filepath.Join(dir, "server.log"), cache: filepath.Join(dir, "cache"), lines: make(chan string, 64)}
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

func checkCommand(t *testing.T, dir string, env []string, flags ...string) ([]byte, []byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := append([]string{"--check", "--cache-dir", filepath.Join(dir, "cache"), "--start-timeout", "1s"}, flags...)
	args = append(args, "--", os.Args[0], "-test.run=^$")
	cmd := exec.CommandContext(ctx, relayBinary, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "MCP_LAZY_FAKE_SERVER=1", "FAKE_LOG="+filepath.Join(dir, "server.log"))
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("check did not finish within 5s")
	}
	return []byte(stdout.String()), []byte(stderr.String()), err
}

func TestColdInitializeTimeoutAndRetry(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "hang")
	os.WriteFile(marker, []byte("hang"), 0o600)
	c := start(t, dir, []string{"FAKE_HANG_MARKER=" + marker}, "--start-timeout", "100ms")
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"cold","version":"1"}}}`)
	if m, _ := c.answer("1"); !strings.Contains(string(m["error"]), "timed out") {
		t.Fatalf("expected timeout: %v", m)
	}
	os.Remove(marker)
	c.handshake("retry")
	if got := strings.Count(c.serverLog(), "start"); got != 2 {
		t.Fatalf("got %d starts, want 2", got)
	}
}

func TestCachedInitializeTimeoutFailsQueuedRequestsAndRetries(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, nil)
	marker := filepath.Join(dir, "hang")
	os.WriteFile(marker, []byte("hang"), 0o600)
	c := start(t, dir, []string{"FAKE_HANG_MARKER=" + marker}, "--start-timeout", "100ms")
	c.handshake("warm")
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo"}}`)
	c.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo"}}`)
	seen := map[string]bool{}
	for len(seen) < 2 {
		m := c.read()
		if !strings.Contains(string(m["error"]), "timed out") {
			t.Fatalf("expected timeout: %v", m)
		}
		seen[string(m["id"])] = true
	}
	if !seen["3"] || !seen["4"] {
		t.Fatalf("wrong ids: %v", seen)
	}
	os.Remove(marker)
	c.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo"}}`)
	if m, _ := c.answer("5"); m["result"] == nil {
		t.Fatalf("retry failed: %v", m)
	}
	if strings.Contains(c.serverLog(), "call echo") && strings.Count(c.serverLog(), "call echo") != 1 {
		t.Fatal("timed-out queued calls were replayed")
	}
}

func TestStartupDeadlineDoesNotLimitOrdinaryToolCalls(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, nil)
	c := start(t, dir, nil, "--start-timeout", "100ms")
	c.handshake("seat")
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"slow"}}`)
	if m, _ := c.answer("3"); m["result"] == nil {
		t.Fatalf("ordinary call was timed out: %v", m)
	}
}

func TestEnvironmentConfigurationWithoutOptionsAndCLIOverride(t *testing.T) {
	dir := t.TempDir()
	c := startArgs(t, dir, []string{"MCP_LAZY_CACHE_DIR=" + filepath.Join(dir, "cache"), "MCP_LAZY_START_TIMEOUT=2s", "MCP_LAZY_IDLE_STOP=0", "MCP_LAZY_LOG_FILE=" + filepath.Join(dir, "relay.log"), "MCP_LAZY_VERBOSE=false"}, []string{os.Args[0], "-test.run=^$"})
	c.handshake("env")
	c.close()
	if files, _ := os.ReadDir(c.cache); len(files) != 1 {
		t.Fatalf("env cache not created: %v", files)
	}
	if info, err := os.Stat(filepath.Join(dir, "relay.log")); err != nil || info.Size() == 0 {
		t.Fatal("env log not created")
	}
	c = start(t, dir, []string{"MCP_LAZY_START_TIMEOUT=invalid"}, "--start-timeout", "1s")
	c.handshake("override")
}

func TestInvalidEnvironmentOrDurationRejected(t *testing.T) {
	for _, value := range []string{"invalid", "0", "-1s"} {
		t.Run(value, func(t *testing.T) {
			cmd := exec.Command(relayBinary, os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), "MCP_LAZY_START_TIMEOUT="+value)
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("invalid duration accepted: %s", out)
			} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
				t.Fatalf("wrong exit: %v", err)
			}
		})
	}
}

func TestCheckRecordsAndNextSessionStaysLazy(t *testing.T) {
	dir := t.TempDir()
	out, stderr, err := checkCommand(t, dir, nil)
	if err != nil {
		t.Fatalf("check failed: %v %s", err, stderr)
	}
	var report struct {
		OK    bool `json:"ok"`
		Lists map[string]struct {
			Count  int
			Cached bool
		} `json:"lists"`
	}
	if json.Unmarshal(out, &report) != nil || !report.OK || report.Lists["tools/list"].Count != 1 || !report.Lists["tools/list"].Cached {
		t.Fatalf("bad report: %s", out)
	}
	if log, _ := os.ReadFile(filepath.Join(dir, "server.log")); !strings.Contains(string(log), "eof") {
		t.Fatal("check did not clean up server")
	}
	os.Remove(filepath.Join(dir, "server.log"))
	c := start(t, dir, nil)
	c.handshake("seat")
	if log := c.serverLog(); log != "" {
		t.Fatalf("check cache did not make session lazy: %s", log)
	}
}

func TestCheckTimeoutAndListFailureLeaveOldCacheIntact(t *testing.T) {
	for _, behavior := range []string{"FAKE_HANG_LISTS=1", "FAKE_LIST_ERROR=1"} {
		t.Run(behavior, func(t *testing.T) {
			dir := t.TempDir()
			record(t, dir, nil)
			files, _ := os.ReadDir(filepath.Join(dir, "cache"))
			path := filepath.Join(dir, "cache", files[0].Name())
			before, _ := os.ReadFile(path)
			out, stderr, err := checkCommand(t, dir, []string{behavior}, "--start-timeout", "100ms")
			if err == nil || len(out) != 0 || len(stderr) == 0 {
				t.Fatalf("check should fail without report: %s %s %v", out, stderr, err)
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("failed check modified cache")
			}
		})
	}
}

func TestCheckPaginationIsCountedButNotCachedAsOnePage(t *testing.T) {
	dir := t.TempDir()
	out, stderr, err := checkCommand(t, dir, []string{"FAKE_PAGINATED=1"})
	if err != nil {
		t.Fatalf("check failed: %v %s", err, stderr)
	}
	var report struct {
		Lists map[string]struct {
			Count  int
			Pages  int
			Cached bool
		} `json:"lists"`
	}
	json.Unmarshal(out, &report)
	list := report.Lists["tools/list"]
	if list.Count != 2 || list.Pages != 2 || list.Cached {
		t.Fatalf("wrong paginated report: %s", out)
	}
}

func TestResultsAndMetaAreForwarded(t *testing.T) {
	dir := t.TempDir()
	c := start(t, dir, nil)
	c.handshake("seat")
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","_meta":{"threadId":"caller"}}}`)
	m, _ := c.answer("3")
	if string(m["extension"]) != `"retained"` || !strings.Contains(string(m["result"]), `"threadId":"reply-thread"`) || !strings.Contains(c.serverLog(), `meta={"threadId":"caller"}`) {
		t.Fatalf("meta or envelope lost: %v", m)
	}
}

func waitGrandchild(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			pid, err := strconv.Atoi(string(data))
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("grandchild never started")
	return 0
}

func assertGrandchildStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		// Linux may leave a zombie for the container's init to reap. It is no longer executing.
		if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			if fields := strings.Fields(string(data)); len(fields) > 2 && fields[2] == "Z" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("grandchild %d still executing", pid)
}

func TestGrandchildCleanupOnEOFAndUnexpectedParentExit(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprint(crash), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "grandchild.pid")
			c := start(t, dir, []string{"FAKE_GRANDCHILD_PID=" + path})
			c.handshake("seat")
			pid := waitGrandchild(t, path)
			if crash {
				c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"crash"}}`)
				if m, _ := c.answer("3"); m["error"] == nil {
					t.Fatalf("missing crash error: %v", m)
				}
			} else {
				c.close()
			}
			assertGrandchildStopped(t, pid)
		})
	}
}

func (c *client) close() {
	if c.closed {
		return
	}
	c.closed = true
	c.in.Close()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			c.t.Errorf("mcp-lazy exited with an error: %v", err)
		}
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
