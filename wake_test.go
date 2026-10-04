package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fakeWakeProbe() {
	if path := os.Getenv("FAKE_WAKE_PID"); path != "" {
		os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0600)
	}
	if os.Getenv("FAKE_WAKE_HANG") == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if os.Getenv("FAKE_WAKE_ERROR") == "1" {
		os.Exit(2)
	}
	if _, err := os.Stat(os.Getenv("FAKE_WAKE_MARKER")); err == nil {
		os.Exit(0)
	}
	os.Exit(1)
}

func wakeArgs(t *testing.T) string {
	t.Helper()
	data, _ := json.Marshal([]string{os.Args[0], "wake-probe"})
	return string(data)
}

func TestWakePredicateWaitsForInitializeAndRechecksUntilConditionMatches(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, nil)
	marker := filepath.Join(dir, "ready")
	pidFile := filepath.Join(dir, "probe.pid")
	c := start(t, dir, []string{"FAKE_WAKE_MARKER=" + marker, "FAKE_WAKE_PID=" + pidFile, "FAKE_SEAT=wake-seat"}, "--wake-command", wakeArgs(t), "--wake-interval", "30ms")
	c.send(`{"jsonrpc":"2.0","id":0,"method":"ping"}`)
	c.answer("0")
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatal("predicate ran before client initialized")
	}
	c.handshake("wake-client")
	waitGrandchild(t, pidFile)
	if c.serverLog() != "" {
		t.Fatal("false predicate woke server")
	}
	os.WriteFile(marker, []byte("ready"), 0600)
	c.waitLog("initialized")
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo"}}`)
	m, _ := c.answer("3")
	if !strings.Contains(string(m["result"]), "env=wake-seat") {
		t.Fatalf("wake lost env: %v", m)
	}
	if strings.Count(c.serverLog(), "start") != 1 || !strings.Contains(c.serverLog(), "initialize client=wake-client") {
		t.Fatal("wake did not replay client handshake once")
	}
}

func TestHangingPredicateDoesNotDelayOrdinaryCallAndIsKilledOnClose(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, nil)
	pidFile := filepath.Join(dir, "probe.pid")
	c := start(t, dir, []string{"FAKE_WAKE_HANG=1", "FAKE_WAKE_PID=" + pidFile}, "--wake-command", wakeArgs(t), "--wake-timeout", "5s")
	c.handshake("seat")
	pid := waitGrandchild(t, pidFile)
	began := time.Now()
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo"}}`)
	if m, _ := c.answer("3"); m["result"] == nil {
		t.Fatalf("call failed: %v", m)
	}
	if time.Since(began) > time.Second {
		t.Fatal("ordinary request waited for predicate")
	}
	c.close()
	assertGrandchildStopped(t, pid)
}

func TestWakeErrorAndTimeoutLeaveServerAsleep(t *testing.T) {
	for _, behavior := range []string{"FAKE_WAKE_ERROR=1", "FAKE_WAKE_HANG=1"} {
		t.Run(behavior, func(t *testing.T) {
			dir := t.TempDir()
			record(t, dir, nil)
			pidFile := filepath.Join(dir, "probe.pid")
			c := start(t, dir, []string{behavior, "FAKE_WAKE_PID=" + pidFile}, "--wake-command", wakeArgs(t), "--wake-timeout", "100ms", "--wake-interval", "5s")
			c.handshake("seat")
			pid := waitGrandchild(t, pidFile)
			time.Sleep(250 * time.Millisecond)
			assertGrandchildStopped(t, pid)
			if c.serverLog() != "" {
				t.Fatal("error/timeout woke server")
			}
			c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo"}}`)
			if m, _ := c.answer("3"); m["result"] == nil {
				t.Fatalf("call after failed predicate: %v", m)
			}
		})
	}
}

func TestSIGHUPKillsNonReadingChildAndDescendant(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	c := start(t, dir, []string{"FAKE_NEVER_READ=1", "FAKE_GRANDCHILD_PID=" + pidFile}, "--start-timeout", "30s")
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	pid := waitGrandchild(t, pidFile)
	if err := c.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	c.close()
	assertGrandchildStopped(t, pid)
}

func TestSIGHUPKillsPendingWakeProbe(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, nil)
	pidFile := filepath.Join(dir, "probe.pid")
	c := start(t, dir, []string{"FAKE_WAKE_HANG=1", "FAKE_WAKE_PID=" + pidFile}, "--wake-command", wakeArgs(t), "--wake-timeout", "30s")
	c.handshake("seat")
	pid := waitGrandchild(t, pidFile)
	if err := c.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	c.close()
	assertGrandchildStopped(t, pid)
}

func TestWakeEnvironmentOptionsAndCLIOverride(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, nil)
	marker := filepath.Join(dir, "ready")
	os.WriteFile(marker, []byte("ready"), 0600)
	c := start(t, dir, []string{"FAKE_WAKE_MARKER=" + marker, "MCP_LAZY_WAKE_COMMAND=" + wakeArgs(t), "MCP_LAZY_WAKE_INTERVAL=25ms", "MCP_LAZY_WAKE_TIMEOUT=1s"})
	c.handshake("seat")
	c.waitLog("initialized")
	c.close()
	os.Remove(filepath.Join(dir, "server.log"))
	c = start(t, dir, []string{"MCP_LAZY_WAKE_COMMAND=invalid", "MCP_LAZY_WAKE_INTERVAL=invalid", "MCP_LAZY_WAKE_TIMEOUT=invalid"}, "--wake-command", "", "--wake-interval", "1s", "--wake-timeout", "1s")
	c.handshake("seat")
	if c.serverLog() != "" {
		t.Fatal("CLI did not override invalid env wake options")
	}
}

func TestInvalidWakeOptionsRejectedBeforeServerStarts(t *testing.T) {
	for _, arguments := range [][]string{
		{"--wake-command", "invalid"}, {"--wake-command", "[]"},
		{"--wake-command", `["", "arg"]`}, {"--wake-command", `["test", null]`},
		{"--wake-command", `["test", 1]`}, {"--wake-interval", "0"},
		{"--wake-timeout", "-1s"},
	} {
		cmd := exec.Command(relayBinary, append(arguments, os.Args[0], "-test.run=^$")...)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("invalid options accepted: %s", out)
		} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
			t.Fatalf("invalid exit: %v", err)
		}
	}
}

func TestCheckSIGHUPKillsNonReadingServerAndGrandchild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	cmd := exec.Command(relayBinary, "--cache-dir", filepath.Join(dir, "cache"), "--check", "--start-timeout", "30s", os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "MCP_LAZY_FAKE_SERVER=1", "FAKE_LOG="+filepath.Join(dir, "server.log"), "FAKE_NEVER_READ=1", "FAKE_GRANDCHILD_PID="+pidFile)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	pid := waitGrandchild(t, pidFile)
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			t.Fatalf("check interrupt: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("check did not handle SIGHUP")
	}
	assertGrandchildStopped(t, pid)
	if files, _ := os.ReadDir(filepath.Join(dir, "cache")); len(files) > 0 {
		t.Fatal("interrupted check saved cache")
	}
}
