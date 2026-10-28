package main

import (
	"encoding/json"
	"strings"
)

// ccEvent is one parsed event from the /alpha/generate NDJSON stream.
// Data holds the event payload with the "type" (and "id") fields removed.
type ccEvent struct {
	Type string
	Data map[string]any
}

// parseCCLine parses a single NDJSON line from the CC stream. The upstream
// mixes "data: {...}" prefixed lines, bare JSON lines, [DONE], keepalive
// blank lines, and two payload shapes:
//
//	flat:   {"type":"text-delta","id":"txt-0","text":"4"}
//	nested: {"type":"text-delta","data":{"text":"Hello"}}
//
// For the flat shape every non-type field is merged into Data (minus id).
// Unparseable or typeless lines are dropped (return ok=false).
func parseCCLine(line string) (ccEvent, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ccEvent{}, false
	}
	dataStr := trimmed
	if rest, found := strings.CutPrefix(trimmed, "data: "); found {
		dataStr = rest
	}
	if dataStr == "[DONE]" {
		return ccEvent{}, false
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(dataStr), &parsed); err != nil {
		return ccEvent{}, false
	}
	eventType, _ := parsed["type"].(string)
	if eventType == "" {
		return ccEvent{}, false
	}
	data := parsed
	if nested, ok := parsed["data"].(map[string]any); ok {
		data = nested
	} else {
		data = make(map[string]any, len(parsed))
		for key, value := range parsed {
			if key == "type" || key == "id" {
				continue
			}
			data[key] = value
		}
	}
	return ccEvent{Type: eventType, Data: data}, true
}

// eventString reads a string field with a fallback key.
func eventString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := data[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func eventNumber(data map[string]any, key string) (float64, bool) {
	switch value := data[key].(type) {
	case float64:
		return value, true
	case int:
		return float64(value), true
	case json.Number:
		if parsed, err := value.Float64(); err == nil {
			return parsed, true
		}
	}
	return 0, false
}
