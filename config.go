package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type envFlags map[string]string

func (e envFlags) String() string { return "relay environment overrides" }
func (e envFlags) Set(value string) error {
	key, text, ok := strings.Cut(value, "=")
	if !ok || !strings.HasPrefix(key, "MCP_LAZY_") || strings.ContainsAny(key, " .\n\t") {
		return fmt.Errorf("env must be MCP_LAZY_NAME=value")
	}
	e[key] = text
	return nil
}

type registration struct {
	Schema     int                `json:"schema"`
	Client     string             `json:"client"`
	Config     string             `json:"config"`
	Server     string             `json:"server"`
	Relay      string             `json:"relay"`
	PriorRelay string             `json:"priorRelay,omitempty"`
	Command    string             `json:"command"`
	Args       []string           `json:"args"`
	HadArgs    bool               `json:"hadArgs"`
	HadEnv     *bool              `json:"hadEnv,omitempty"`
	Managed    envFlags           `json:"managedEnv"`
	BaseEnv    map[string]*string `json:"baseEnv"`
}

func configCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mcp-lazy config wrap|reapply|unwrap|status [flags]")
		return 2
	}
	action := args[0]
	if action != "wrap" && action != "reapply" && action != "unwrap" && action != "status" {
		fmt.Fprintln(os.Stderr, "mcp-lazy: unknown config action")
		return 2
	}
	fs := flag.NewFlagSet("config "+action, flag.ContinueOnError)
	client := fs.String("client", "", "claude/cursor for JSON, codex/grok for TOML")
	config := fs.String("file", "", "explicit client configuration file")
	server := fs.String("server", "", "one existing stdio server registration")
	stateDir := fs.String("state-dir", "", "persistent registration metadata and backups directory")
	relayPath := fs.String("relay", "", "absolute relay binary (default: this executable)")
	dryRun := fs.Bool("dry-run", false, "report changed field names without writing files or revealing values")
	managed := envFlags{}
	fs.Var(managed, "env", "MCP_LAZY_NAME=value; repeat to set relay options")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *config == "" || *server == "" || (*client != "claude" && *client != "cursor" && *client != "codex" && *client != "grok") {
		fmt.Fprintln(os.Stderr, "mcp-lazy: --client, --file and --server are required")
		return 2
	}
	path, err := filepath.Abs(*config)
	if err == nil {
		path, err = filepath.EvalSymlinks(path)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcp-lazy: configuration file is not accessible")
		return 1
	}
	if *stateDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcp-lazy: specify --state-dir")
			return 2
		}
		*stateDir = filepath.Join(base, "mcp-lazy", "registrations")
	}
	*stateDir, err = filepath.Abs(*stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcp-lazy: invalid state directory")
		return 2
	}
	if *relayPath == "" {
		*relayPath, err = os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcp-lazy: cannot locate relay executable")
			return 1
		}
	}
	*relayPath, err = filepath.Abs(*relayPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcp-lazy: invalid relay path")
		return 2
	}
	if action == "wrap" || action == "reapply" {
		if info, err := os.Stat(*relayPath); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			fmt.Fprintln(os.Stderr, "mcp-lazy: relay binary must be an existing executable file")
			return 2
		}
	}
	reg := registration{Schema: 1, Client: *client, Config: path, Server: *server, Relay: *relayPath, Managed: managed, BaseEnv: map[string]*string{}}
	if err := manageRegistration(action, reg, *stateDir, *dryRun); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-lazy:", err)
		return 1
	}
	return 0
}

func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:16])
}

func decodeConfig(data []byte, client string) (map[string]any, error) {
	var model map[string]any
	var err error
	if client == "claude" || client == "cursor" {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		err = decoder.Decode(&model)
		if err == nil && !json.Valid(data) {
			err = fmt.Errorf("invalid JSON")
		}
	} else {
		err = toml.Unmarshal(data, &model)
	}
	if err != nil || model == nil {
		return nil, fmt.Errorf("invalid %s configuration", client)
	}
	return model, nil
}

func serverEntry(model map[string]any, client, server string) (map[string]any, error) {
	section := "mcpServers"
	if client == "codex" || client == "grok" {
		section = "mcp_servers"
	}
	servers, _ := model[section].(map[string]any)
	entry, _ := servers[server].(map[string]any)
	if entry == nil {
		return nil, fmt.Errorf("server registration was not found")
	}
	if entry["url"] != nil {
		return nil, fmt.Errorf("only stdio registrations can be wrapped")
	}
	command, ok := entry["command"].(string)
	if !ok || command == "" {
		return nil, fmt.Errorf("stdio command must be a nonempty string")
	}
	return entry, nil
}

func entryArgs(entry map[string]any) ([]string, bool, error) {
	raw, exists := entry["args"]
	if !exists {
		return []string{}, false, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, false, fmt.Errorf("args must be an array of strings")
	}
	result := []string{}
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, false, fmt.Errorf("args must be an array of strings")
		}
		result = append(result, text)
	}
	return result, true, nil
}

func entryEnv(entry map[string]any) (map[string]any, error) {
	if entry["env"] == nil {
		return map[string]any{}, nil
	}
	values, ok := entry["env"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("env must be a table/object")
	}
	for _, value := range values {
		if _, ok := value.(string); !ok {
			return nil, fmt.Errorf("env values must be strings")
		}
	}
	return values, nil
}

func manageRegistration(action string, requested registration, stateDir string, dryRun bool) error {
	return manageRegistrationWithWrite(action, requested, stateDir, dryRun, atomicWriteChecked)
}

func manageRegistrationWithWrite(action string, requested registration, stateDir string, dryRun bool, write func(string, []byte, os.FileMode, func() error) error) error {
	key := digest(requested.Client + "\n" + requested.Config + "\n" + requested.Server)
	statePath := filepath.Join(stateDir, key+".json")
	if !dryRun && action != "status" {
		if err := os.MkdirAll(filepath.Join(stateDir, "locks"), 0o700); err != nil {
			return fmt.Errorf("creating state directory failed")
		}
		unlock, err := lockConfig(filepath.Join(stateDir, "locks", digest(requested.Config)+".lock"))
		if err != nil {
			return err
		}
		defer unlock()
	}
	data, err := os.ReadFile(requested.Config)
	if err != nil {
		return fmt.Errorf("reading configuration failed")
	}
	info, err := os.Stat(requested.Config)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("configuration must be a regular file")
	}
	model, err := decodeConfig(data, requested.Client)
	if err != nil {
		return err
	}
	entry, err := serverEntry(model, requested.Client, requested.Server)
	if err != nil {
		return err
	}
	args, hadArgs, err := entryArgs(entry)
	if err != nil {
		return err
	}
	env, err := entryEnv(entry)
	if err != nil {
		return err
	}
	stored := registration{}
	state, readErr := os.ReadFile(statePath)
	exists := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("reading registration state failed")
	}
	if exists && (json.Unmarshal(state, &stored) != nil || stored.Schema != 1 || stored.Client != requested.Client || stored.Config != requested.Config || stored.Server != requested.Server || stored.Command == "" || stored.Managed == nil || stored.BaseEnv == nil) {
		return fmt.Errorf("registration state is invalid")
	}
	command := entry["command"].(string)
	wrapped := exists && (command == stored.Relay || (stored.PriorRelay != "" && command == stored.PriorRelay))
	if action == "status" {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"managed": exists, "wrapped": wrapped, "client": requested.Client, "file": requested.Config, "server": requested.Server})
	}
	if action != "wrap" && !exists {
		return fmt.Errorf("no saved registration; use config wrap first")
	}
	if wrapped && len(args) == 0 {
		return fmt.Errorf("wrapped registration has no original command")
	}
	if !wrapped && (command == requested.Relay || filepath.Base(command) == "mcp-lazy") {
		return fmt.Errorf("registration points at an unmanaged relay; refusing to nest wrappers")
	}
	reg := requested
	previousManaged := envFlags{}
	if exists {
		reg = stored
		for name, value := range stored.Managed {
			previousManaged[name] = value
		}
		if reg.Relay != requested.Relay {
			reg.PriorRelay = reg.Relay
		}
		reg.Relay = requested.Relay
		for name, value := range requested.Managed {
			reg.Managed[name] = value
		}
	}
	changes := map[string]any{}
	if action == "unwrap" {
		if wrapped {
			changes["command"] = args[0]
			if reg.HadArgs || len(args) > 1 {
				changes["args"] = stringsToAny(args[1:])
			} else {
				changes["args"] = nil
			}
		}
		for name, managed := range reg.Managed {
			// Preserve a user's later edit to an environment value owned by this wrapper.
			if current, ok := env[name]; ok && current == managed {
				if old := reg.BaseEnv[name]; old != nil {
					changes["env."+name] = *old
				} else {
					changes["env."+name] = nil
				}
			}
		}
	} else {
		if wrapped {
			reg.Command = args[0]
			reg.Args = append([]string{}, args[1:]...)
		} else {
			reg.Command, reg.Args, reg.HadArgs = command, args, hadArgs
			if !exists {
				hadEnv := entry["env"] != nil
				reg.HadEnv = &hadEnv
			} else {
				// Adopt an unambiguous env shape from a newer direct registration.
				// An object containing only our retained overrides is ambiguous.
				hadEnv := entry["env"] != nil
				known := !hadEnv || len(env) == 0
				for name, value := range env {
					if managed, ok := previousManaged[name]; !ok || value != managed {
						known = true
					}
				}
				if known {
					reg.HadEnv = &hadEnv
				}
			}
			for name := range reg.Managed {
				if old, ok := env[name]; ok && exists && old == previousManaged[name] {
					// setup may retain our overrides when restoring a direct command.
					continue
				}
				if old, ok := env[name]; ok {
					value := old.(string)
					reg.BaseEnv[name] = &value
				} else {
					reg.BaseEnv[name] = nil
				}
			}
		}
		for name := range requested.Managed {
			if _, ok := reg.BaseEnv[name]; !ok {
				if old, ok := env[name]; ok {
					value := old.(string)
					reg.BaseEnv[name] = &value
				} else {
					reg.BaseEnv[name] = nil
				}
			}
		}
		changes["command"] = reg.Relay
		changes["args"] = stringsToAny(append([]string{reg.Command}, reg.Args...))
		for name, value := range reg.Managed {
			changes["env."+name] = value
		}
	}
	if action == "unwrap" && reg.HadEnv != nil && !*reg.HadEnv {
		remaining := len(env)
		for field, value := range changes {
			if strings.HasPrefix(field, "env.") && value == nil {
				if _, ok := env[strings.TrimPrefix(field, "env.")]; ok {
					remaining--
				}
			}
		}
		if remaining == 0 && entry["env"] != nil {
			for field := range changes {
				if strings.HasPrefix(field, "env.") {
					delete(changes, field)
				}
			}
			changes["env"] = nil
		}
	}
	changed := []string{}
	for field, value := range changes {
		var old any
		if strings.HasPrefix(field, "env.") {
			old = env[strings.TrimPrefix(field, "env.")]
		} else {
			old = entry[field]
		}
		if !reflect.DeepEqual(old, value) {
			changed = append(changed, field)
		}
	}
	sort.Strings(changed)
	next, err := patchConfiguration(data, model, requested.Client, requested.Server, changes)
	if err != nil {
		return err
	}
	if dryRun {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"action": action, "dryRun": true, "file": requested.Config, "server": requested.Server, "changedFields": changed})
	}
	// setup/register is an external writer: reject changes noticed since reading.
	if len(changed) > 0 {
		current, err := os.ReadFile(requested.Config)
		if err != nil || !bytes.Equal(current, data) {
			return fmt.Errorf("configuration changed during editing; rerun after setup/register finishes")
		}
	}
	if len(changed) > 0 {
		// Backups include the original settings; keep them private and never print them.
		backupDir := filepath.Join(stateDir, "backups")
		if err := os.MkdirAll(backupDir, 0o700); err != nil {
			return fmt.Errorf("creating backup directory failed")
		}
		backup := filepath.Join(backupDir, key+"-"+time.Now().UTC().Format("20060102T150405.000000000")+".bak")
		if err := atomicWrite(backup, data, 0o600); err != nil {
			return fmt.Errorf("writing backup failed")
		}
	}
	if action != "unwrap" {
		encoded, err := json.MarshalIndent(reg, "", "  ")
		if err != nil {
			return err
		}
		if err := atomicWrite(statePath, encoded, 0o600); err != nil {
			return fmt.Errorf("saving registration state failed")
		}
	}
	rollbackState := func() error {
		if action == "unwrap" {
			return nil
		}
		if exists {
			return atomicWrite(statePath, state, 0o600)
		}
		return os.Remove(statePath)
	}
	if len(changed) > 0 {
		current, err := os.ReadFile(requested.Config)
		if err != nil || !bytes.Equal(current, data) {
			if err := rollbackState(); err != nil {
				return fmt.Errorf("configuration changed and restoring registration state failed; backup is retained")
			}
			return fmt.Errorf("configuration changed during editing; no configuration was overwritten; rerun after setup/register finishes")
		}
		if err := write(requested.Config, next, info.Mode().Perm(), func() error {
			latest, err := os.Stat(requested.Config)
			if err != nil || !os.SameFile(info, latest) || latest.Mode() != info.Mode() {
				return fmt.Errorf("configuration changed during editing")
			}
			current, err := os.ReadFile(requested.Config)
			if err != nil || !bytes.Equal(current, data) {
				return fmt.Errorf("configuration changed during editing")
			}
			return nil
		}); err != nil {
			if err := rollbackState(); err != nil {
				return fmt.Errorf("writing configuration and restoring registration state failed; backup is retained")
			}
			return fmt.Errorf("writing configuration failed: %w; backup is retained", err)
		}
	}
	if action == "unwrap" {
		if err := os.Remove(statePath); err != nil {
			return fmt.Errorf("removing saved registration failed")
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"action": action, "file": requested.Config, "server": requested.Server, "changedFields": changed})
}

func stringsToAny(items []string) []any {
	result := make([]any, len(items))
	for i, text := range items {
		result[i] = text
	}
	return result
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	return atomicWriteChecked(path, data, mode, nil)
}

// The check runs after the temporary file is synced, immediately before rename.
// Uncooperative writers still require an offline editing window.
func atomicWriteChecked(path string, data []byte, mode os.FileMode, check func() error) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".mcp-lazy-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	return os.Rename(file.Name(), path)
}

func patchConfiguration(data []byte, model map[string]any, client, server string, changes map[string]any) ([]byte, error) {
	entry, err := serverEntry(model, client, server)
	if err != nil {
		return nil, err
	}
	hadEnv := entry["env"] != nil
	env, err := entryEnv(entry)
	if err != nil {
		return nil, err
	}
	effective := map[string]any{}
	for field, value := range changes {
		old := entry[field]
		if strings.HasPrefix(field, "env.") {
			old = env[strings.TrimPrefix(field, "env.")]
		}
		if !reflect.DeepEqual(old, value) {
			effective[field] = value
		}
	}
	changes = effective
	for field, value := range changes {
		if strings.HasPrefix(field, "env.") {
			key := strings.TrimPrefix(field, "env.")
			if value == nil {
				delete(env, key)
			} else {
				env[key] = value
			}
			entry["env"] = env
		} else if value == nil {
			delete(entry, field)
		} else {
			entry[field] = value
		}
	}
	var next []byte
	if client == "claude" || client == "cursor" {
		next, err = patchJSON(data, server, changes)
	} else {
		patchChanges := changes
		if !hadEnv && len(env) > 0 {
			// A new inline table also survives removal of its last managed key.
			patchChanges = map[string]any{"env": env}
			for field, value := range changes {
				if !strings.HasPrefix(field, "env.") {
					patchChanges[field] = value
				}
			}
		}
		next, err = patchToml(data, server, patchChanges)
	}
	if err != nil {
		return nil, err
	}
	verified, err := decodeConfig(next, client)
	if err != nil || !reflect.DeepEqual(verified, model) {
		return nil, fmt.Errorf("edited configuration did not preserve the expected settings")
	}
	return next, nil
}
