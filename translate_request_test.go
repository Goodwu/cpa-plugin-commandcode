package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func chatMsg(raw string) chatMessage {
	var msg chatMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		panic(err)
	}
	return msg
}

func TestToCCRequestBasicShape(t *testing.T) {
	req := &chatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []chatMessage{chatMsg(`{"role":"system","content":"be nice"}`), chatMsg(`{"role":"user","content":"hi"}`)},
	}
	body := toCCRequest(req, "thread-1")
	if body.Params.Model != "deepseek/deepseek-v4-pro" {
		t.Fatalf("model = %q", body.Params.Model)
	}
	if !body.Params.Stream {
		t.Fatal("stream must be forced true")
	}
	if body.ThreadID != "thread-1" {
		t.Fatalf("threadId = %q", body.ThreadID)
	}
	if body.PermissionMode != "standard" {
		t.Fatalf("permissionMode = %q", body.PermissionMode)
	}
	// system is lifted out of messages into params.system.
	for _, msg := range body.Params.Messages {
		if msg.Role == "system" {
			t.Fatal("system messages must not stay in params.messages")
		}
	}
	if body.Params.System == "" || !strings.Contains(body.Params.System, "be nice") {
		t.Fatalf("system = %q", body.Params.System)
	}
	// no tools => safeguard appended
	if !strings.Contains(body.Params.System, "chat-only environment") {
		t.Fatal("no-tools safeguard missing from system")
	}
	last := body.Params.Messages[len(body.Params.Messages)-1]
	text, _ := last.Content.(string)
	if !strings.Contains(text, "Tool execution is disabled") {
		t.Fatalf("safeguard missing from user message: %q", text)
	}
}

func TestToCCRequestToolsRoundTrip(t *testing.T) {
	req := &chatRequest{
		Model: "kimi-k3",
		Messages: []chatMessage{
			chatMsg(`{"role":"user","content":"list files"}`),
			chatMsg(`{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"ls","arguments":"{\"path\":\".\"}"}}]}`),
			chatMsg(`{"role":"tool","tool_call_id":"call-1","content":"a.go b.go"}`),
		},
		Tools: []chatTool{{Type: "function", Function: struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		}{Name: "ls", Parameters: map[string]any{"type": "object"}}}},
	}
	body := toCCRequest(req, "t2")
	if len(body.Params.Tools) != 1 || body.Params.Tools[0].Name != "ls" {
		t.Fatalf("tools = %+v", body.Params.Tools)
	}
	var assistant, toolResult bool
	for _, msg := range body.Params.Messages {
		parts, ok := msg.Content.([]ccContentPart)
		if !ok {
			continue
		}
		for _, part := range parts {
			switch part.Type {
			case "tool-call":
				assistant = true
				if part.ToolName != "ls" || part.ToolCallID != "call-1" {
					t.Fatalf("tool-call part = %+v", part)
				}
				input, _ := part.Input.(map[string]any)
				if input["path"] != "." {
					t.Fatalf("tool-call input not parsed: %+v", part.Input)
				}
			case "tool-result":
				toolResult = true
				if part.ToolName != "ls" || part.Output == nil || part.Output.Value != "a.go b.go" {
					t.Fatalf("tool-result part = %+v", part)
				}
			}
		}
	}
	if !assistant || !toolResult {
		t.Fatalf("assistant=%v toolResult=%v messages=%+v", assistant, toolResult, body.Params.Messages)
	}
	// With tools present the safeguard must not fire.
	if strings.Contains(body.Params.System, "chat-only environment") {
		t.Fatal("safeguard applied despite tools")
	}
}

func TestPruneDanglingTools(t *testing.T) {
	messages := []ccMessage{
		{Role: "assistant", Content: []ccContentPart{{Type: "tool-call", ToolCallID: "a"}, {Type: "text", Text: "hi"}}},
		{Role: "assistant", Content: []ccContentPart{{Type: "tool-call", ToolCallID: "b"}}},
		{Role: "tool", Content: []ccContentPart{{Type: "tool-result", ToolCallID: "a"}}},
	}
	pruned := pruneDanglingTools(messages)
	if len(pruned) != 2 {
		t.Fatalf("expected 2 messages, got %d: %+v", len(pruned), pruned)
	}
	first, _ := pruned[0].Content.([]ccContentPart)
	if len(first) != 2 {
		t.Fatalf("paired call plus text should remain: %+v", first)
	}
}

func TestImagePartsBecomeCCImage(t *testing.T) {
	req := &chatRequest{
		Model:    "deepseek-v4-flash-vision",
		Messages: []chatMessage{chatMsg(`{"role":"user","content":[{"type":"text","text":"what?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}`)},
	}
	body := toCCRequest(req, "t3")
	parts, ok := body.Params.Messages[0].Content.([]ccContentPart)
	if !ok {
		t.Fatal("expected parts")
	}
	if parts[1].Type != "image" || parts[1].Image != "data:image/png;base64,AAA" {
		t.Fatalf("image part = %+v", parts[1])
	}
}

func TestResolveToolChoice(t *testing.T) {
	cases := []struct {
		raw      string
		wantType string
		wantName string
		wantNil  bool
	}{
		{`"auto"`, "", "", true},
		{`"none"`, "", "", true},
		{`"required"`, "any", "", false},
		{`{"type":"function","function":{"name":"x"}}`, "tool", "x", false},
		{`{"type":"any"}`, "any", "", false},
		{`{"type":"tool","name":"y"}`, "tool", "y", false},
	}
	for _, tc := range cases {
		got := resolveToolChoice(json.RawMessage(tc.raw))
		if tc.wantNil {
			if got != nil {
				t.Fatalf("%s -> %+v, want nil", tc.raw, got)
			}
			continue
		}
		if got == nil || got.Type != tc.wantType || got.Name != tc.wantName {
			t.Fatalf("%s -> %+v", tc.raw, got)
		}
	}
}

func TestNormalizeStop(t *testing.T) {
	if normalizeStop(json.RawMessage(`""`)) != nil {
		t.Fatal("empty string stop dropped")
	}
	if normalizeStop(json.RawMessage(`[]`)) != nil {
		t.Fatal("empty list stop dropped")
	}
	if normalizeStop(json.RawMessage(`{"bad":1}`)) != nil {
		t.Fatal("object stop dropped")
	}
	if normalizeStop(json.RawMessage(`"END"`)) == nil {
		t.Fatal("string stop kept")
	}
	if normalizeStop(json.RawMessage(`["A","B"]`)) == nil {
		t.Fatal("list stop kept")
	}
}

func TestExtractUsageShapes(t *testing.T) {
	total := map[string]any{"totalUsage": map[string]any{
		"inputTokens": 10.0, "outputTokens": 5.0,
		"inputTokenDetails":  map[string]any{"cacheReadTokens": 4.0},
		"outputTokenDetails": map[string]any{"reasoningTokens": 2.0},
	}}
	usage := extractUsage(total)
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 ||
		usage.CachedTokens != 4 || usage.ReasoningTokens != 2 {
		t.Fatalf("camel shape: %+v", usage)
	}
	snake := map[string]any{"usage": map[string]any{"prompt_tokens": 8.0, "completion_tokens": 3.0}}
	usage = extractUsage(snake)
	if usage == nil || !usage.HasTotal || usage.TotalTokens != 11 {
		t.Fatalf("snake shape: %+v", usage)
	}
	if extractUsage(map[string]any{}) != nil {
		t.Fatal("no usage should be nil")
	}
}
