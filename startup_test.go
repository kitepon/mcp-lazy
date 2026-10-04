package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoveryTimeoutIsNotCachedAndRetryWorks(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "hang")
	os.WriteFile(marker, []byte("hang"), 0o600)
	c := start(t, dir, []string{"FAKE_HANG_DISCOVER_MARKER=" + marker}, "--start-timeout", "100ms")
	c.send(`{"jsonrpc":"2.0","id":0,"method":"server/discover","params":{"protocolVersion":"2026-07-28"}}`)
	if m, _ := c.answer("0"); !strings.Contains(string(m["error"]), "timed out") {
		t.Fatalf("expected timeout: %v", m)
	}
	os.Remove(marker)
	c.send(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"protocolVersion":"2026-07-28"}}`)
	if m, _ := c.answer("1"); !strings.Contains(string(m["error"]), "Method not found") {
		t.Fatalf("retry did not reach real server: %v", m)
	}
}

func TestInternalListRefreshTimeoutAndRetry(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, nil)
	marker := filepath.Join(dir, "hang")
	os.WriteFile(marker, []byte("hang"), 0o600)
	c := start(t, dir, []string{"FAKE_HANG_LIST_MARKER=" + marker}, "--start-timeout", "100ms")
	c.handshake("seat")
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"slow"}}`)
	if m, _ := c.answer("3"); !strings.Contains(string(m["error"]), "timed out") {
		t.Fatalf("expected refresh timeout: %v", m)
	}
	os.Remove(marker)
	c.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo"}}`)
	if m, _ := c.answer("4"); m["result"] == nil {
		t.Fatalf("retry failed: %v", m)
	}
}

func TestCheckRecordsAllAdvertisedListTypes(t *testing.T) {
	dir := t.TempDir()
	out, stderr, err := checkCommand(t, dir, []string{"FAKE_ALL_LISTS=1"})
	if err != nil {
		t.Fatalf("check failed: %v %s", err, stderr)
	}
	var report struct {
		Lists map[string]struct {
			Count  int
			Cached bool
		} `json:"lists"`
	}
	json.Unmarshal(out, &report)
	if len(report.Lists) != 4 {
		t.Fatalf("expected four list types: %s", out)
	}
	for method, list := range report.Lists {
		if list.Count != 1 || !list.Cached {
			t.Fatalf("%s not recorded: %s", method, out)
		}
	}
}

func TestCheckRejectsInvalidInitializeBeforeStartingServer(t *testing.T) {
	dir := t.TempDir()
	out, stderr, err := checkCommand(t, dir, []string{`MCP_LAZY_CHECK_INITIALIZE={"protocolVersion":"2025-06-18"}`})
	if err == nil || len(out) != 0 || !strings.Contains(string(stderr), "CHECK_INITIALIZE") {
		t.Fatalf("invalid init accepted: %s %s %v", out, stderr, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "server.log")); !os.IsNotExist(err) {
		t.Fatal("invalid check started the server")
	}
}

func TestCheckCustomIdentityAndCacheWriteFailure(t *testing.T) {
	dir := t.TempDir()
	env := []string{`MCP_LAZY_CHECK_INITIALIZE={"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"Claude Code","version":"1"}}`}
	out, stderr, err := checkCommand(t, dir, env)
	if err != nil {
		t.Fatalf("check failed: %v %s", err, stderr)
	}
	if !strings.Contains(string(out), `"ok":true`) {
		t.Fatalf("bad report: %s", out)
	}
	if log, _ := os.ReadFile(filepath.Join(dir, "server.log")); !strings.Contains(string(log), "initialize client=Claude Code") {
		t.Fatal("check did not use configured client identity")
	}
	blocked := filepath.Join(dir, "regular-file")
	os.WriteFile(blocked, []byte("file"), 0o600)
	out, stderr, err = checkCommand(t, dir, env, "--cache-dir", blocked)
	if err == nil || len(out) != 0 || !strings.Contains(string(stderr), "saving cache") {
		t.Fatalf("cache write failure reported success: %s %s %v", out, stderr, err)
	}
}
