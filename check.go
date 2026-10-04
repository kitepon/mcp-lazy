package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// runCheck uses a fresh live conversation and never invokes a tool. A successful
// check replaces this protocol's entry only after every advertised list completes.
func (r *relay) runCheck() error {
	params := json.RawMessage(os.Getenv("MCP_LAZY_CHECK_INITIALIZE"))
	if len(params) == 0 {
		params = json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"mcp-lazy-check","version":"` + version + `"}}`)
	}
	var init struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	if json.Unmarshal(params, &init) != nil || init.ProtocolVersion == "" || init.Capabilities == nil || init.ClientInfo.Name == "" || init.ClientInfo.Version == "" {
		return fmt.Errorf("MCP_LAZY_CHECK_INITIALIZE must contain protocolVersion, capabilities and clientInfo name/version")
	}
	deadline := time.NewTimer(r.startTimeout)
	defer deadline.Stop()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	if err := r.startChild(false); err != nil {
		return err
	}
	defer r.stopChild()
	request := 0
	query := func(method string, arguments json.RawMessage) (json.RawMessage, error) {
		request++
		id := fmt.Sprintf("mcp-lazy-inspect-%d", request)
		payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": arguments})
		if err != nil {
			return nil, err
		}
		r.writeChild(payload)
		for {
			select {
			case line := <-r.childLines:
				var m message
				if json.Unmarshal(line, &m) != nil {
					return nil, fmt.Errorf("%s: server stdout is not JSON", method)
				}
				if m.Method != "" {
					if m.hasID() {
						return nil, fmt.Errorf("%s: server requested client interaction (%s); check is noninteractive", method, m.Method)
					}
					continue
				}
				var responseID string
				if json.Unmarshal(m.ID, &responseID) != nil || responseID != id {
					continue
				}
				if len(m.Error) > 0 {
					return nil, fmt.Errorf("%s: server returned an error: %s", method, m.Error)
				}
				var result map[string]json.RawMessage
				if json.Unmarshal(m.Result, &result) != nil || result == nil {
					return nil, fmt.Errorf("%s: expected an object result", method)
				}
				return m.Result, nil
			case err := <-r.childDone:
				r.childExited(err)
				return nil, fmt.Errorf("%s: server exited before answering: %v", method, err)
			case err := <-r.writeErrors:
				return nil, fmt.Errorf("%s: server input failed: %w", method, err)
			case <-deadline.C:
				return nil, fmt.Errorf("startup timed out after %s while checking %s", r.startTimeout, method)
			case <-signals:
				return nil, fmt.Errorf("check interrupted")
			}
		}
	}
	result, err := query("initialize", params)
	if err != nil {
		return err
	}
	var initialized struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if json.Unmarshal(result, &initialized) != nil || initialized.ProtocolVersion == "" || initialized.Capabilities == nil || initialized.ServerInfo.Name == "" || initialized.ServerInfo.Version == "" {
		return fmt.Errorf("initialize: missing protocolVersion, capabilities or serverInfo")
	}
	if initialized.ProtocolVersion != init.ProtocolVersion {
		return fmt.Errorf("initialize: negotiated %s, requested %s; set MCP_LAZY_CHECK_INITIALIZE for the negotiated version", initialized.ProtocolVersion, init.ProtocolVersion)
	}
	r.writeChild([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	entry := &entry{Initialize: result, Lists: map[string]json.RawMessage{}}
	type listReport struct {
		Count  int  `json:"count"`
		Pages  int  `json:"pages"`
		Cached bool `json:"cached"`
	}
	lists := map[string]listReport{}
	for _, descriptor := range []struct{ method, capability, field string }{
		{"tools/list", "tools", "tools"}, {"prompts/list", "prompts", "prompts"},
		{"resources/list", "resources", "resources"}, {"resources/templates/list", "resources", "resourceTemplates"},
	} {
		if _, ok := initialized.Capabilities[descriptor.capability]; !ok {
			continue
		}
		var report listReport
		arguments := json.RawMessage(`{}`)
		seen := map[string]bool{}
		for {
			page, err := query(descriptor.method, arguments)
			if err != nil {
				return err
			}
			var fields map[string]json.RawMessage
			json.Unmarshal(page, &fields)
			var items []json.RawMessage
			if raw, ok := fields[descriptor.field]; !ok || json.Unmarshal(raw, &items) != nil || string(raw) == "null" {
				return fmt.Errorf("%s: missing array %s", descriptor.method, descriptor.field)
			}
			report.Count += len(items)
			report.Pages++
			var cursor string
			if raw := fields["nextCursor"]; len(raw) > 0 && string(raw) != "null" {
				if json.Unmarshal(raw, &cursor) != nil {
					return fmt.Errorf("%s: invalid nextCursor", descriptor.method)
				}
			}
			if cursor == "" {
				if report.Pages == 1 {
					entry.Lists[descriptor.method] = page
					report.Cached = true
				}
				break
			}
			if seen[cursor] {
				return fmt.Errorf("%s: repeated pagination cursor", descriptor.method)
			}
			seen[cursor] = true
			arguments, _ = json.Marshal(map[string]string{"cursor": cursor})
		}
		lists[descriptor.method] = report
	}
	// Reap before reporting success. The deferred call is then a no-op.
	r.stopChild()
	r.cache.Entries[init.ProtocolVersion] = entry
	if err := r.saveCache(); err != nil {
		return fmt.Errorf("saving cache: %w", err)
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "version": version, "protocolVersion": init.ProtocolVersion, "cachePath": r.cachePath, "lists": lists})
}
