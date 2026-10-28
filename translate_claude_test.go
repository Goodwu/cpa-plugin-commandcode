package main

import (
	"encoding/json"
	"strings"
	"testing"
)

type sseFrame struct {
	Event string
	Data  map[string]any
}

func parseFrames(t *testing.T, chunks [][]byte) []sseFrame {
	t.Helper()
	var frames []sseFrame
	for _, chunk := range chunks {
		text := string(chunk)
		if !strings.HasSuffix(text, "\n\n") {
			t.Fatalf("frame not SSE-terminated: %q", text)
		}
		var event, data string
		for _, line := range strings.Split(strings.TrimSuffix(text, "\n\n"), "\n") {
			if rest, ok := strings.CutPrefix(line, "event: "); ok {
				event = rest
			} else if rest, ok := strings.CutPrefix(line, "data: "); ok {
				data = rest
			} else {
				t.Fatalf("unexpected SSE line %q in frame %q", line, text)
			}
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			t.Fatalf("frame data not JSON: %v (%s)", err, data)
		}
		frames = append(frames, sseFrame{Event: event, Data: parsed})
	}
	return frames
}

func claudeEmit(t *testing.T, enc *claudeEncoder, typeName, payload string) []sseFrame {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		t.Fatal(err)
	}
	chunks, err := enc.emit(ccEvent{Type: typeName, Data: data})
	if err != nil {
		t.Fatalf("emit %s: %v", typeName, err)
	}
	return parseFrames(t, chunks)
}

func frameTypes(frames []sseFrame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Event)
	}
	return out
}

func TestClaudeStreamTextSequence(t *testing.T) {
	enc := newClaudeEncoder("m")
	all := claudeEmit(t, enc, "start", `{}`)
	all = append(all, claudeEmit(t, enc, "text-delta", `{"text":"Hel"}`)...)
	all = append(all, claudeEmit(t, enc, "text-delta", `{"text":"lo"}`)...)
	all = append(all, claudeEmit(t, enc, "finish", `{"finishReason":"stop","totalUsage":{"inputTokens":7,"outputTokens":3}}`)...)

	got := strings.Join(frameTypes(all), ",")
	want := "message_start,content_block_start,content_block_delta,content_block_delta,content_block_stop,message_delta,message_stop"
	if got != want {
		t.Fatalf("event sequence = %s", got)
	}
	first := all[0].Data["message"].(map[string]any)
	if first["role"] != "assistant" || first["type"] != "message" {
		t.Fatalf("message_start shape: %v", first)
	}
	// deltas assemble the text
	text := ""
	for _, f := range all {
		if f.Event == "content_block_delta" {
			text += f.Data["delta"].(map[string]any)["text"].(string)
		}
	}
	if text != "Hello" {
		t.Fatalf("assembled = %q", text)
	}
	for _, f := range all {
		if f.Event == "message_delta" {
			if f.Data["delta"].(map[string]any)["stop_reason"] != "end_turn" {
				t.Fatalf("stop_reason: %v", f.Data)
			}
			usage := f.Data["usage"].(map[string]any)
			if usage["input_tokens"].(float64) != 7 || usage["output_tokens"].(float64) != 3 {
				t.Fatalf("usage split: %v", usage)
			}
		}
	}
	if !enc.finished() {
		t.Fatal("finished flag")
	}
}

func TestClaudeStreamThinkingThenText(t *testing.T) {
	enc := newClaudeEncoder("m")
	var all []sseFrame
	all = append(all, claudeEmit(t, enc, "reasoning-delta", `{"text":"think"}`)...)
	all = append(all, claudeEmit(t, enc, "text-delta", `{"text":"answer"}`)...)
	all = append(all, claudeEmit(t, enc, "finish", `{"finishReason":"stop"}`)...)

	kinds := []string{}
	for _, f := range all {
		if f.Event == "content_block_start" {
			kinds = append(kinds, f.Data["content_block"].(map[string]any)["type"].(string))
		}
	}
	if strings.Join(kinds, ",") != "thinking,text" {
		t.Fatalf("block kinds = %v", kinds)
	}
	// thinking delta carries the "thinking" field, text block index must differ
	var thinkingIndex, textIndex float64 = -1, -1
	for _, f := range all {
		if f.Event == "content_block_delta" {
			delta := f.Data["delta"].(map[string]any)
			if delta["type"] == "thinking_delta" {
				thinkingIndex = f.Data["index"].(float64)
				if delta["thinking"] != "think" {
					t.Fatalf("thinking field: %v", delta)
				}
			}
			if delta["type"] == "text_delta" {
				textIndex = f.Data["index"].(float64)
			}
		}
	}
	if thinkingIndex != 0 || textIndex != 1 {
		t.Fatalf("indices: thinking=%v text=%v", thinkingIndex, textIndex)
	}
}

func TestClaudeStreamToolCalls(t *testing.T) {
	enc := newClaudeEncoder("m")
	var all []sseFrame
	all = append(all, claudeEmit(t, enc, "tool-call-delta", `{"toolCallId":"A","name":"get_weather","arguments":""}`)...)
	all = append(all, claudeEmit(t, enc, "tool-call-delta", `{"toolCallId":"A","arguments":"{\"city\":"}`)...)
	all = append(all, claudeEmit(t, enc, "tool-call-delta", `{"toolCallId":"A","arguments":"\"Tokyo\"}"}`)...)
	// canonical final call equal to accumulation: must not duplicate bytes
	all = append(all, claudeEmit(t, enc, "tool-call", `{"toolCallId":"A","toolName":"get_weather","input":{"city":"Tokyo"}}`)...)
	all = append(all, claudeEmit(t, enc, "finish", `{"finishReason":"tool-call"}`)...)

	var start map[string]any
	partial := ""
	for _, f := range all {
		if f.Event == "content_block_start" {
			start = f.Data["content_block"].(map[string]any)
		}
		if f.Event == "content_block_delta" {
			delta := f.Data["delta"].(map[string]any)
			if delta["type"] == "input_json_delta" {
				partial += delta["partial_json"].(string)
			}
		}
	}
	if start == nil || start["type"] != "tool_use" || start["id"] != "A" || start["name"] != "get_weather" {
		t.Fatalf("tool_use block start: %v", start)
	}
	if partial != `{"city":"Tokyo"}` {
		t.Fatalf("assembled partial_json = %q", partial)
	}
	for _, f := range all {
		if f.Event == "message_delta" {
			if f.Data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
				t.Fatalf("stop_reason = %v", f.Data["delta"])
			}
		}
	}
}

func TestClaudeToolBufferedBeforeIdKnown(t *testing.T) {
	enc := newClaudeEncoder("m")
	var all []sseFrame
	// Deltas without any id/name are buffered, not dropped.
	all = append(all, claudeEmit(t, enc, "tool-call-delta", `{"arguments":"{\"q\":"}`)...)
	all = append(all, claudeEmit(t, enc, "tool-call-delta", `{"arguments":"1}"}`)...)
	all = append(all, claudeEmit(t, enc, "finish", `{"finishReason":"tool-call"}`)...)

	partial := ""
	var sawTool bool
	for _, f := range all {
		if f.Event == "content_block_start" && f.Data["content_block"].(map[string]any)["type"] == "tool_use" {
			sawTool = true
		}
		if f.Event == "content_block_delta" {
			if delta, ok := f.Data["delta"].(map[string]any); ok && delta["type"] == "input_json_delta" {
				partial += delta["partial_json"].(string)
			}
		}
	}
	if !sawTool {
		t.Fatal("buffered tool must be flushed at finalize")
	}
	if partial != `{"q":1}` {
		t.Fatalf("flushed partial = %q", partial)
	}
}

func TestClaudeErrorEventProducesLegalStream(t *testing.T) {
	enc := newClaudeEncoder("m")
	all := claudeEmit(t, enc, "error", `{"message":"quota"}`)
	types := strings.Join(frameTypes(all), ",")
	if !strings.HasPrefix(types, "message_start") || !strings.HasSuffix(types, "message_stop") {
		t.Fatalf("error stream shape = %s", types)
	}
	text := ""
	for _, f := range all {
		if f.Event == "content_block_delta" {
			text += f.Data["delta"].(map[string]any)["text"].(string)
		}
	}
	if !strings.Contains(text, "[upstream error] quota") {
		t.Fatalf("error text = %q", text)
	}
}

func TestClaudeForceFinishNoopWithoutStart(t *testing.T) {
	enc := newClaudeEncoder("m")
	if frames := enc.forceFinish(); frames != nil {
		t.Fatalf("no frames expected before start, got %v", frames)
	}
	enc.emit(ccEvent{Type: "start", Data: map[string]any{}})
	tail := enc.forceFinish()
	if len(tail) == 0 {
		t.Fatal("force finish must terminate a started stream")
	}
	last := parseFrames(t, tail)[len(parseFrames(t, tail))-1]
	if last.Event != "message_stop" {
		t.Fatalf("last event = %s", last.Event)
	}
}

func TestBuildClaudeNonStreaming(t *testing.T) {
	agg := &aggregateResult{callIndex: map[string]int{}}
	applyAggregateEvent(agg, ccEventFrom("reasoning-delta", `{"text":"think"}`))
	applyAggregateEvent(agg, ccEventFrom("text-delta", `{"text":"hi"}`))
	applyAggregateEvent(agg, ccEventFrom("tool-call", `{"toolCallId":"T","toolName":"f","input":{"a":1}}`))
	applyAggregateEvent(agg, ccEventFrom("finish", `{"finishReason":"tool-call","totalUsage":{"inputTokens":5,"outputTokens":2}}`))

	message := buildClaudeNonStreaming("m", agg)
	raw, _ := json.Marshal(message)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	if parsed["type"] != "message" {
		t.Fatalf("type = %v", parsed["type"])
	}
	blocks := parsed["content"].([]any)
	kinds := []string{}
	for _, b := range blocks {
		kinds = append(kinds, b.(map[string]any)["type"].(string))
	}
	if strings.Join(kinds, ",") != "thinking,text,tool_use" {
		t.Fatalf("content block kinds = %v", kinds)
	}
	tool := blocks[2].(map[string]any)
	input := tool["input"].(map[string]any)
	if input["a"].(float64) != 1 || tool["id"] != "T" || tool["name"] != "f" {
		t.Fatalf("tool_use block = %v", tool)
	}
	if parsed["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", parsed["stop_reason"])
	}
	usage := parsed["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 5 || usage["output_tokens"].(float64) != 2 {
		t.Fatalf("usage = %v", usage)
	}
}

func TestIsClaudeFormat(t *testing.T) {
	for _, name := range []string{"anthropic", "claude", " Claude "} {
		if !isClaudeFormat(name) {
			t.Fatalf("%q should be claude format", name)
		}
	}
	for _, name := range []string{"chat-completions", "responses", ""} {
		if isClaudeFormat(name) {
			t.Fatalf("%q should not be claude format", name)
		}
	}
}
