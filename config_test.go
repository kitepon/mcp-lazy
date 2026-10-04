package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func configRun(t *testing.T, action, client, path, state string, extra ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := []string{"config", action, "--client", client, "--file", path, "--server", "sample", "--state-dir", state}
	args = append(args, extra...)
	cmd := exec.CommandContext(ctx, relayBinary, args...)
	return cmd.CombinedOutput()
}
func configRead(t *testing.T, path, client string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeConfig(data, client)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func configMust(t *testing.T, action, client, path, state string, extra ...string) []byte {
	t.Helper()
	out, err := configRun(t, action, client, path, state, extra...)
	if err != nil {
		t.Fatalf("%s: %v %s", action, err, out)
	}
	return out
}

func TestConfigWrapReapplyAfterSetupAndUnwrapPreserveOtherSettings(t *testing.T) {
	for _, client := range []string{"claude", "cursor", "codex", "grok"} {
		t.Run(client, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config")
			state := filepath.Join(dir, "state")
			initial := `{"large":9007199254740993,"mcpServers":{"sample":{"command":"node","args":["server.mjs","--original"],"env":{"TOKEN":"private-fixture","MCP_LAZY_IDLE_STOP":"30s"},"cwd":"/project","enabled":true,"env_vars":["CODEX_HOME"]},"other":{"command":"other","args":[]}}}`
			if client == "codex" || client == "grok" {
				initial = "# root comment\nlarge = 9007199254740993\n[mcp_servers.sample]\ncommand = 'node' # command note\nargs = [\n 'server.mjs',\n '--original',\n]\nenv_vars = ['CODEX_HOME']\nenabled = true\ncwd = '/project'\n[mcp_servers.sample.env]\nTOKEN = 'private-fixture'\nMCP_LAZY_IDLE_STOP = '30s'\n# other comment\n[mcp_servers.other]\ncommand = 'other'\nargs = []\n"
			}
			os.WriteFile(path, []byte(initial), 0640)
			original := configRead(t, path, client)
			out := configMust(t, "wrap", client, path, state, "--env", "MCP_LAZY_IDLE_STOP=0", "--env", "MCP_LAZY_CACHE_DIR=/cache", "--dry-run")
			if bytes.Contains(out, []byte("private-fixture")) {
				t.Fatal("dry-run leaked value")
			}
			if data, _ := os.ReadFile(path); string(data) != initial {
				t.Fatal("dry-run modified configuration")
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatal("dry-run wrote state")
			}
			configMust(t, "wrap", client, path, state, "--env", "MCP_LAZY_IDLE_STOP=0", "--env", "MCP_LAZY_CACHE_DIR=/cache")
			m := configRead(t, path, client)
			entry, _ := serverEntry(m, client, "sample")
			if entry["command"] != relayBinary || !reflect.DeepEqual(entry["args"], []any{"node", "server.mjs", "--original"}) {
				t.Fatalf("bad wrapper: %v", entry)
			}
			baseline, _ := serverEntry(original, client, "sample")
			for _, key := range []string{"env_vars", "cwd", "enabled"} {
				if !reflect.DeepEqual(entry[key], baseline[key]) {
					t.Fatalf("lost %s", key)
				}
			}
			before, _ := os.ReadFile(path)
			configMust(t, "reapply", client, path, state)
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("reapply nested wrapper or rewrote unchanged file")
			}
			if client == "codex" || client == "grok" {
				for _, comment := range []string{"# root comment", "# command note", "# other comment"} {
					if !bytes.Contains(after, []byte(comment)) {
						t.Fatalf("lost %s", comment)
					}
				}
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0640 {
				t.Fatal("config permissions changed")
			}
			// Simulate setup replacing this registration with a newer direct command.
			updated := strings.ReplaceAll(initial, "node", "node-new")
			updated = strings.ReplaceAll(updated, "--original", "--new")
			updated = strings.ReplaceAll(updated, "30s", "45s")
			os.WriteFile(path, []byte(updated), 0640)
			configMust(t, "reapply", client, path, state)
			entry, _ = serverEntry(configRead(t, path, client), client, "sample")
			if !reflect.DeepEqual(entry["args"], []any{"node-new", "server.mjs", "--new"}) {
				t.Fatalf("did not adopt latest setup: %v", entry)
			}
			configMust(t, "unwrap", client, path, state)
			expected, err := decodeConfig([]byte(updated), client)
			if err != nil {
				t.Fatal(err)
			}
			got := configRead(t, path, client)
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("unwrap differs from latest direct settings:\ngot %v\nwant %v", got, expected)
			}
			backups, _ := os.ReadDir(filepath.Join(state, "backups"))
			if len(backups) != 3 {
				t.Fatalf("backup count %d", len(backups))
			}
			for _, f := range backups {
				info, _ := os.Stat(filepath.Join(state, "backups", f.Name()))
				if info.Mode().Perm() != 0600 {
					t.Fatal("backup not private")
				}
			}
		})
	}
}

func TestConfigTOMLFormsAndMissingArgs(t *testing.T) {
	forms := []string{
		"[mcp_servers.sample]\ncommand = 'node'\n",
		"[mcp_servers.sample]\ncommand = 'node'\nenv = {TOKEN = 'private-fixture'}\n",
		"[mcp_servers]\nsample.command = 'node'\nsample.env.TOKEN = 'private-fixture'\n",
		"mcp_servers.sample.command = 'node'\nmcp_servers.sample.env.TOKEN = 'private-fixture'\n[ui]\nmode = 'keep'\n",
		"[mcp_servers.'sample']\r\ncommand = 'node'\r\n[mcp_servers.'sample'.env]\r\nTOKEN = 'private-fixture'\r\n",
	}
	for _, initial := range forms {
		t.Run(initial, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config")
			state := filepath.Join(dir, "state")
			os.WriteFile(path, []byte(initial), 0600)
			configMust(t, "wrap", "codex", path, state, "--env", "MCP_LAZY_CACHE_DIR=/cache")
			entry, _ := serverEntry(configRead(t, path, "codex"), "codex", "sample")
			if !reflect.DeepEqual(entry["args"], []any{"node"}) {
				t.Fatalf("args: %v", entry)
			}
			configMust(t, "unwrap", "codex", path, state)
			entry, _ = serverEntry(configRead(t, path, "codex"), "codex", "sample")
			if entry["args"] != nil || entry["command"] != "node" {
				t.Fatalf("unwrap shape: %v", entry)
			}
			env, _ := entryEnv(entry)
			if env["MCP_LAZY_CACHE_DIR"] != nil {
				t.Fatal("managed env retained")
			}
		})
	}
}

func TestConfigUnwrapPreservesLaterUserEnvChangesAndBinaryUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	state := filepath.Join(dir, "state")
	os.WriteFile(path, []byte(`{"mcpServers":{"sample":{"command":"node","args":[],"env":{"MCP_LAZY_IDLE_STOP":"30s"}}}}`), 0600)
	configMust(t, "wrap", "claude", path, state, "--env", "MCP_LAZY_IDLE_STOP=0")
	newer := filepath.Join(dir, "new-relay")
	data, _ := os.ReadFile(relayBinary)
	os.WriteFile(newer, data, 0700)
	configMust(t, "reapply", "claude", path, state, "--relay", newer)
	m := configRead(t, path, "claude")
	entry, _ := serverEntry(m, "claude", "sample")
	if entry["command"] != newer {
		t.Fatal("upgrade not applied")
	}
	entry["env"].(map[string]any)["MCP_LAZY_IDLE_STOP"] = "user-edited"
	encoded, _ := json.Marshal(m)
	os.WriteFile(path, encoded, 0600)
	configMust(t, "unwrap", "claude", path, state)
	entry, _ = serverEntry(configRead(t, path, "claude"), "claude", "sample")
	if entry["env"].(map[string]any)["MCP_LAZY_IDLE_STOP"] != "user-edited" {
		t.Fatal("unwrap overwrote user edit")
	}
	if entry["command"] != "node" {
		t.Fatal("unwrap after binary upgrade failed")
	}
}

func TestConfigRefusesUnsafeOrUnsupportedRegistrationsWithoutEditing(t *testing.T) {
	cases := []struct{ client, data string }{
		{"claude", `{"mcpServers":{"sample":{"url":"https://example.invalid","command":"node"}}}`},
		{"claude", `{"mcpServers":{"sample":{"command":"mcp-lazy","args":["node"]}}}`},
		{"claude", `{"mcpServers":{"sample":{"command":"node","args":[42]}}}`},
		{"codex", `mcp_servers = { sample = {command = "node"} }`},
		{"claude", `{"mcpServers":{"sample":{"command":"node"}}} trailing`},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		path := filepath.Join(dir, "config")
		state := filepath.Join(dir, "state")
		os.WriteFile(path, []byte(tc.data), 0600)
		if _, err := configRun(t, "wrap", tc.client, path, state); err == nil {
			t.Fatalf("accepted %s", tc.data)
		}
		if data, _ := os.ReadFile(path); string(data) != tc.data {
			t.Fatal("failed edit changed config")
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	state := filepath.Join(dir, "state")
	os.WriteFile(path, []byte(`{"mcpServers":{"sample":{"command":"node"}}}`), 0600)
	if _, err := configRun(t, "reapply", "claude", path, state); err == nil {
		t.Fatal("reapply without saved state accepted")
	}
	configMust(t, "wrap", "claude", path, state)
	out := configMust(t, "status", "claude", path, state)
	if !bytes.Contains(out, []byte(`"wrapped":true`)) {
		t.Fatalf("status: %s", out)
	}
}

func TestConfigRecoversInterruptedBinaryUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	state := filepath.Join(dir, "state")
	os.WriteFile(path, []byte(`{"mcpServers":{"sample":{"command":"node","args":["server.mjs"]}}}`), 0600)
	configMust(t, "wrap", "claude", path, state)
	nextRelay := filepath.Join(dir, "new-relay")
	binary, _ := os.ReadFile(relayBinary)
	os.WriteFile(nextRelay, binary, 0700)
	files, _ := os.ReadDir(state)
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		statePath := filepath.Join(state, file.Name())
		data, _ := os.ReadFile(statePath)
		var reg registration
		if err := json.Unmarshal(data, &reg); err != nil {
			t.Fatal(err)
		}
		// Simulate interruption after metadata replacement but before config replacement.
		reg.PriorRelay, reg.Relay = reg.Relay, nextRelay
		data, _ = json.Marshal(reg)
		os.WriteFile(statePath, data, 0600)
	}
	configMust(t, "reapply", "claude", path, state, "--relay", nextRelay)
	entry, _ := serverEntry(configRead(t, path, "claude"), "claude", "sample")
	if entry["command"] != nextRelay || !reflect.DeepEqual(entry["args"], []any{"node", "server.mjs"}) {
		t.Fatalf("interrupted upgrade nested or failed: %v", entry)
	}
}

func TestConfigConcurrentWrappersKeepBothRegistrations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	state := filepath.Join(dir, "state")
	os.WriteFile(path, []byte(`{"mcpServers":{"sample":{"command":"node","args":["one.mjs"]},"other":{"command":"node","args":["two.mjs"]}}}`), 0600)
	type outcome struct {
		out []byte
		err error
	}
	results := make(chan outcome, 2)
	for _, server := range []string{"sample", "other"} {
		go func(server string) {
			out, err := configRun(t, "wrap", "claude", path, state, "--server", server)
			results <- outcome{out, err}
		}(server)
	}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent wrap: %v %s", result.err, result.out)
		}
	}
	m := configRead(t, path, "claude")
	for _, server := range []string{"sample", "other"} {
		entry, err := serverEntry(m, "claude", server)
		if err != nil || entry["command"] != relayBinary {
			t.Fatalf("lost %s: %v %v", server, entry, err)
		}
	}
}
