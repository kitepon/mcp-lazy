package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2/unstable"
)

type configEdit struct {
	start, end int
	text       string
}

// Patch complete TOML expressions, using the library's parser to locate them.
// Comments and settings outside the edited expressions remain byte-for-byte.
// Enclosing inline tables are rejected rather than rewritten destructively.
func patchToml(data []byte, server string, changes map[string]any) ([]byte, error) {
	base := []string{"mcp_servers", server}
	targets := map[string][]string{}
	for field := range changes {
		path := append([]string{}, base...)
		if strings.HasPrefix(field, "env.") {
			path = append(path, "env", strings.TrimPrefix(field, "env."))
		} else {
			path = append(path, field)
		}
		targets[field] = path
	}
	var parser unstable.Parser
	parser.KeepComments = true
	parser.Reset(data)
	var section []string
	// An insertion belongs immediately after its table header. Root insertions
	// use dotted keys at the beginning of the document.
	insertions := map[string]int{"": 0}
	sections := map[string][]string{"": nil}
	var edits []configEdit
	done := map[string]bool{}
	pathKey := func(path []string) string { encoded, _ := json.Marshal(path); return string(encoded) }
	for parser.NextExpression() {
		node := parser.Expression()
		if node.Kind != unstable.Table && node.Kind != unstable.ArrayTable && node.Kind != unstable.KeyValue {
			continue
		}
		var key []string
		keyEnd := 0
		it := node.Key()
		for it.Next() {
			key = append(key, string(it.Node().Data))
			keyEnd = int(it.Node().Raw.Offset + it.Node().Raw.Length)
		}
		if node.Kind == unstable.Table || node.Kind == unstable.ArrayTable {
			section = key
			if value, removing := changes["env"]; removing && value == nil && reflect.DeepEqual(section, append(append([]string{}, base...), "env")) {
				// Table nodes have no Raw span; the key nodes locate the header.
				start := bytes.LastIndexByte(data[:int(node.Child().Raw.Offset)+1], '[')
				end := keyEnd + bytes.IndexByte(data[keyEnd:], ']') + 1
				if start < 0 || end <= keyEnd {
					return nil, fmt.Errorf("unsupported TOML env table header")
				}
				edits = append(edits, configEdit{start, end, ""})
				done["env"] = true
				continue
			}
			if node.Kind == unstable.Table {
				first := node.Child()
				start := int(first.Raw.Offset)
				end := len(data)
				if offset := bytes.IndexByte(data[start:], '\n'); offset >= 0 {
					end = start + offset + 1
				}
				insertions[pathKey(section)] = end
				sections[pathKey(section)] = append([]string{}, section...)
			}
			continue
		}
		path := append(append([]string{}, section...), key...)
		if value, removing := changes["env"]; removing && value == nil {
			envPath := append(append([]string{}, base...), "env")
			if len(path) >= len(envPath) && reflect.DeepEqual(path[:len(envPath)], envPath) {
				edits = append(edits, configEdit{int(node.Raw.Offset), int(node.Raw.Offset + node.Raw.Length), ""})
				done["env"] = true
				continue
			}
		}
		// env may be an inline table. Merge it through the decoded map, while
		// keeping the rest of the document unchanged.
		if reflect.DeepEqual(path, append(append([]string{}, base...), "env")) {
			var env map[string]any
			// Decode this expression under a temporary key with the normal parser.
			valueStart := int(node.Value().Raw.Offset)
			if valueStart == 0 {
				return nil, fmt.Errorf("unsupported TOML env expression")
			}
			decoded, err := decodeConfig(append([]byte("env = "), data[valueStart:int(node.Raw.Offset+node.Raw.Length)]...), "codex")
			if err != nil {
				return nil, err
			}
			env, _ = decoded["env"].(map[string]any)
			if env == nil {
				return nil, fmt.Errorf("env must be a TOML table")
			}
			modified := false
			for field, value := range changes {
				if strings.HasPrefix(field, "env.") {
					name := strings.TrimPrefix(field, "env.")
					if value == nil {
						delete(env, name)
					} else {
						env[name] = value
					}
					done[field], modified = true, true
				}
			}
			if modified {
				value, err := tomlInline(env)
				if err != nil {
					return nil, err
				}
				edits = append(edits, configEdit{int(node.Raw.Offset), int(node.Raw.Offset + node.Raw.Length), tomlPath(key) + " = " + value})
			}
			continue
		}
		for field, target := range targets {
			if reflect.DeepEqual(path, target) {
				text := ""
				if value := changes[field]; value != nil {
					encoded, err := tomlInline(value)
					if err != nil {
						return nil, err
					}
					text = tomlPath(key) + " = " + encoded
				}
				edits = append(edits, configEdit{int(node.Raw.Offset), int(node.Raw.Offset + node.Raw.Length), text})
				done[field] = true
			} else if len(path) < len(target) && reflect.DeepEqual(path, target[:len(path)]) {
				return nil, fmt.Errorf("enclosing inline TOML registration is unsupported; use [mcp_servers.<name>] tables")
			}
		}
	}
	if parser.Error() != nil {
		return nil, fmt.Errorf("invalid TOML configuration")
	}
	fields := make([]string, 0, len(targets))
	for field := range targets {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	insertText := map[int]string{}
	for _, field := range fields {
		if done[field] || changes[field] == nil {
			continue
		}
		target := targets[field]
		position, parent := 0, 0
		for key, prefix := range sections {
			if len(prefix) > parent && len(prefix) < len(target) && reflect.DeepEqual(prefix, target[:len(prefix)]) {
				parent, position = len(prefix), insertions[key]
			}
		}
		encoded, err := tomlInline(changes[field])
		if err != nil {
			return nil, err
		}
		insertText[position] += tomlPath(target[parent:]) + " = " + encoded + "\n"
	}
	for position, text := range insertText {
		if position > 0 && data[position-1] != '\n' {
			text = "\n" + text
		}
		edits = append(edits, configEdit{position, position, text})
	}
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	next := append([]byte{}, data...)
	for _, edit := range edits {
		next = append(append(append([]byte{}, next[:edit.start]...), []byte(edit.text)...), next[edit.end:]...)
	}
	return next, nil
}

func tomlPath(path []string) string {
	parts := make([]string, len(path))
	for i, part := range path {
		encoded, _ := json.Marshal(part)
		parts[i] = string(encoded)
	}
	return strings.Join(parts, ".")
}

func tomlInline(value any) (string, error) {
	switch v := value.(type) {
	case string:
		encoded, _ := json.Marshal(v)
		return string(encoded), nil
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			var err error
			parts[i], err = tomlInline(item)
			if err != nil {
				return "", err
			}
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			encoded, err := tomlInline(v[key])
			if err != nil {
				return "", err
			}
			parts = append(parts, tomlPath([]string{key})+" = "+encoded)
		}
		return "{ " + strings.Join(parts, ", ") + " }", nil
	default:
		return "", fmt.Errorf("unsupported value in edited TOML field")
	}
}
