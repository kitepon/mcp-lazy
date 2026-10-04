package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const claudeDiscoveryParams = `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"claude-code","title":"Claude Code","version":"2.1.289","description":"Anthropic's agentic coding tool","websiteUrl":"https://claude.com/claude-code"},"io.modelcontextprotocol/clientCapabilities":{"roots":{"listChanged":true},"elicitation":{"form":{},"url":{}}}}}`
const claudeCheckInit = `{"protocolVersion":"2025-06-18","capabilities":{"roots":{"listChanged":true},"elicitation":{"form":{},"url":{}}},"clientInfo":{"name":"claude-code","title":"Claude Code","version":"2.1.289","description":"Anthropic's agentic coding tool","websiteUrl":"https://claude.com/claude-code"}}`

func TestCheckRecordsRealClaudeDiscoveryAndVersionUpdateStaysLazy(t *testing.T) {
	dir := t.TempDir()
	env := []string{"MCP_LAZY_CHECK_INITIALIZE=" + claudeCheckInit}
	out, stderr, err := checkCommand(t, dir, env)
	if err != nil {
		t.Fatalf("check: %v %s", err, stderr)
	}
	if !strings.Contains(string(out), `"beforeRequests":1`) {
		t.Fatalf("report: %s", out)
	}
	sum := sha256.Sum256([]byte(claudeDiscoveryParams))
	if hex.EncodeToString(sum[:8]) != "4019dc22a0d88b45" {
		t.Fatal("fixture differs from real CLI capture")
	}
	files, _ := os.ReadDir(filepath.Join(dir, "cache"))
	cache, _ := os.ReadFile(filepath.Join(dir, "cache", files[0].Name()))
	if !strings.Contains(string(cache), "server/discover:4019dc22a0d88b45") {
		t.Fatalf("real key missing: %s", cache)
	}
	os.Remove(filepath.Join(dir, "server.log"))
	for _, params := range []string{claudeDiscoveryParams, strings.Replace(claudeDiscoveryParams, "2.1.289", "2.2.0", 1)} {
		c := start(t, dir, nil)
		c.send(`{"jsonrpc":"2.0","id":"probe","method":"server/discover","params":` + params + `}`)
		m, _ := c.answer(`"probe"`)
		if !isMethodNotFound(json.RawMessage(`{"error":` + string(m["error"]) + `}`)) {
			t.Fatalf("cached error missing: %v", m)
		}
		c.handshake("seat")
		if log := c.serverLog(); log != "" {
			t.Fatalf("first checked seat woke server: %s", log)
		}
		c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo"}}`)
		if m, _ := c.answer("3"); m["result"] == nil {
			t.Fatalf("call: %v", m)
		}
		c.close()
		os.Remove(c.log)
	}
}

func TestDiscoveryFallbackKeepsIdentityProtocolAndCapabilitiesScoped(t *testing.T) {
	for _, params := range []string{
		strings.Replace(claudeDiscoveryParams, "claude-code", "other", 1),
		strings.Replace(claudeDiscoveryParams, "2026-07-28", "2026-08-01", 1),
		strings.Replace(claudeDiscoveryParams, `"listChanged":true`, `"listChanged":false`, 1),
	} {
		dir := t.TempDir()
		if _, stderr, err := checkCommand(t, dir, []string{"MCP_LAZY_CHECK_INITIALIZE=" + claudeCheckInit}); err != nil {
			t.Fatalf("check: %v %s", err, stderr)
		}
		os.Remove(filepath.Join(dir, "server.log"))
		c := start(t, dir, nil)
		c.send(`{"jsonrpc":"2.0","id":0,"method":"server/discover","params":` + params + `}`)
		c.answer("0")
		if !strings.Contains(c.serverLog(), "discover") {
			t.Fatal("different client/protocol/capabilities used a cached unsupported response")
		}
	}
}

func TestSuccessfulDiscoveryIsExactAndClearsUnsupportedFallback(t *testing.T) {
	dir := t.TempDir()
	env := []string{"MCP_LAZY_CHECK_INITIALIZE=" + claudeCheckInit}
	if _, stderr, err := checkCommand(t, dir, env); err != nil {
		t.Fatalf("check: %v %s", err, stderr)
	}
	if _, stderr, err := checkCommand(t, dir, append(env, "FAKE_DISCOVER_SUCCESS=1")); err != nil {
		t.Fatalf("check: %v %s", err, stderr)
	}
	os.Remove(filepath.Join(dir, "server.log"))
	c := start(t, dir, []string{"FAKE_DISCOVER_SUCCESS=1"})
	c.send(`{"jsonrpc":"2.0","id":0,"method":"server/discover","params":` + claudeDiscoveryParams + `}`)
	if m, _ := c.answer("0"); m["result"] == nil {
		t.Fatalf("success not cached: %v", m)
	}
	if c.serverLog() != "" {
		t.Fatal("exact success woke server")
	}
	c.send(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":` + strings.Replace(claudeDiscoveryParams, "2.1.289", "2.2.0", 1) + `}`)
	if m, _ := c.answer("1"); m["result"] == nil {
		t.Fatalf("obsolete fallback served: %v", m)
	}
	if !strings.Contains(c.serverLog(), "discover") {
		t.Fatal("success reused across versions")
	}
}

func TestCheckBeforeOverrideDisableAndFailurePreservation(t *testing.T) {
	for _, value := range []string{`[]`, `[{"method":"server/discover","params":` + claudeDiscoveryParams + `}]`} {
		dir := t.TempDir()
		if _, stderr, err := checkCommand(t, dir, []string{"MCP_LAZY_CHECK_BEFORE=" + value}); err != nil {
			t.Fatalf("check: %v %s", err, stderr)
		}
		log, _ := os.ReadFile(filepath.Join(dir, "server.log"))
		if strings.Contains(string(log), "discover") != (value != "[]") {
			t.Fatalf("override: %s", log)
		}
	}
	for _, value := range []string{`null`, `invalid`, `[{"method":"tools/call","params":{}}]`, `[{"method":"server/discover","params":null}]`} {
		dir := t.TempDir()
		if _, _, err := checkCommand(t, dir, []string{"MCP_LAZY_CHECK_BEFORE=" + value}); err == nil {
			t.Fatal("invalid/side-effectful check accepted")
		}
		if _, err := os.Stat(filepath.Join(dir, "server.log")); !os.IsNotExist(err) {
			t.Fatal("invalid check started server")
		}
	}
	dir := t.TempDir()
	if _, stderr, err := checkCommand(t, dir, nil); err != nil {
		t.Fatalf("check: %v %s", err, stderr)
	}
	files, _ := os.ReadDir(filepath.Join(dir, "cache"))
	path := filepath.Join(dir, "cache", files[0].Name())
	before, _ := os.ReadFile(path)
	if _, _, err := checkCommand(t, dir, []string{"FAKE_DISCOVER_ERROR=1"}); err == nil {
		t.Fatal("discovery failure accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("failed probe overwrote cache")
	}
}
