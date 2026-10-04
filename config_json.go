package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type jsonMember struct {
	key                    string
	start, valueStart, end int
}

// Locate object members without serializing the rest of the document. Duplicate
// keys are ambiguous to different clients, so refuse them before editing.
func jsonMembers(data []byte) ([]jsonMember, int, error) {
	pos := 0
	space := func() {
		for pos < len(data) && strings.ContainsRune(" \r\n\t", rune(data[pos])) {
			pos++
		}
	}
	space()
	if pos == len(data) || data[pos] != '{' {
		return nil, 0, fmt.Errorf("edited JSON path must be an object")
	}
	pos++
	var members []jsonMember
	seen := map[string]bool{}
	for {
		space()
		if pos == len(data) {
			return nil, 0, fmt.Errorf("invalid JSON object")
		}
		if data[pos] == '}' {
			return members, pos, nil
		}
		member := jsonMember{start: pos}
		dec := json.NewDecoder(bytes.NewReader(data[pos:]))
		if err := dec.Decode(&member.key); err != nil {
			return nil, 0, err
		}
		if seen[member.key] {
			return nil, 0, fmt.Errorf("duplicate key in edited JSON object")
		}
		seen[member.key] = true
		pos += int(dec.InputOffset())
		space()
		if pos == len(data) || data[pos] != ':' {
			return nil, 0, fmt.Errorf("invalid JSON member")
		}
		pos++
		space()
		member.valueStart = pos
		var raw json.RawMessage
		dec = json.NewDecoder(bytes.NewReader(data[pos:]))
		if err := dec.Decode(&raw); err != nil {
			return nil, 0, err
		}
		pos += int(dec.InputOffset())
		member.end = pos
		members = append(members, member)
		space()
		if pos < len(data) && data[pos] == ',' {
			pos++
			continue
		}
		if pos == len(data) || data[pos] != '}' {
			return nil, 0, fmt.Errorf("invalid JSON object delimiter")
		}
	}
}

func patchJSONField(data []byte, path []string, value any) ([]byte, error) {
	members, close, err := jsonMembers(data)
	if err != nil {
		return nil, err
	}
	for i, member := range members {
		if member.key != path[0] {
			continue
		}
		start, end := member.valueStart, member.end
		var replacement []byte
		if len(path) > 1 {
			replacement, err = patchJSONField(data[start:end], path[1:], value)
		} else if value != nil {
			replacement, err = json.Marshal(value)
		} else {
			start = member.start
			if i+1 < len(members) {
				end = members[i+1].start
			} else if i > 0 {
				start = members[i-1].end
			}
		}
		if err != nil {
			return nil, err
		}
		return bytes.Join([][]byte{data[:start], replacement, data[end:]}, nil), nil
	}
	if value == nil {
		return append([]byte{}, data...), nil
	}
	if len(path) > 1 {
		value = nestedJSONValue(path[1:], value)
	}
	key, _ := json.Marshal(path[0])
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	insertion := append(append(key, ':', ' '), encoded...)
	position := close
	if len(members) > 0 {
		position = members[len(members)-1].end
		insertion = append([]byte(", "), insertion...)
	}
	return bytes.Join([][]byte{data[:position], insertion, data[position:]}, nil), nil
}

func nestedJSONValue(path []string, value any) any {
	for i := len(path) - 1; i >= 0; i-- {
		value = map[string]any{path[i]: value}
	}
	return value
}

func patchJSON(data []byte, server string, changes map[string]any) ([]byte, error) {
	fields := make([]string, 0, len(changes))
	for field := range changes {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	next := append([]byte{}, data...)
	for _, field := range fields {
		path := []string{"mcpServers", server}
		if strings.HasPrefix(field, "env.") {
			path = append(path, "env", strings.TrimPrefix(field, "env."))
		} else {
			path = append(path, field)
		}
		var err error
		next, err = patchJSONField(next, path, changes[field])
		if err != nil {
			return nil, err
		}
	}
	return next, nil
}
