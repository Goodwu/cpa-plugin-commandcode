package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeChunk(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var chunk map[string]any
	if err := json.Unmarshal(raw, &chunk); err != nil {
		t.Fatalf("chunk is not valid JSON: %v (%s)", err, raw)
	}
	return chunk
}

func firstDelta(t *testing.T, chunk map[string]any) map[string]any {
	t.Helper()
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("chunk has no choices: %v", chunk)
	}
	return choices[0].(map[string]any)["delta"].(map[string]any)
}

func firstCall(t *testing.T, chunks [][]byte) map[string]any {
	t.Helper()
	if len(chunks) == 0 {
		t.Fatal("no chunks emitted")
	}
	return firstDelta(t, decodeChunk(t, chunks[0]))["tool_calls"].([]any)[0].(map[string]any)
}

func mustEmit(t *testing.T, enc *streamEncoder, typeName, payload string) [][]byte {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		t.Fatal(err)
	}
	chunks, err := enc.emit(ccEvent{Type: typeName, Data: data})
	if err != nil {
		t.Fatalf("emit %s: %v", typeName, err)
	}
	return chunks
}

func ccEventFrom(typeName, payload string) ccEvent {
	var data map[string]any
	_ = json.Unmarshal([]byte(payload), &data)
	return ccEvent{Type: typeName, Data: data}
}

func TestStreamEncoderTextAndReasoning(t *testing.T) {
	enc := newStreamEncoder("m")
	start := mustEmit(t, enc, "start", `{}`)
	if len(start) != 1 {
		t.Fatalf("start chunks = %d", len(start))
	}
	if firstDelta(t, decodeChunk(t, start[0]))["role"] != "assistant" {
		t.Fatal("start delta must set role")
	}
	text := mustEmit(t, enc, "text-delta", `{"text":"hello"}`)
	if firstDelta(t, decodeChunk(t, text[0]))["content"] != "hello" {
		t.Fatal("text-delta must map to content")
	}
	reasoning := mustEmit(t, enc, "reasoning-delta", `{"text":"thinking"}`)
	if firstDelta(t, decodeChunk(t, reasoning[0]))["reasoning_content"] != "thinking" {
		t.Fatal("reasoning-delta must map to reasoning_content")
	}
}

func TestStreamEncoderParallelToolCalls(t *testing.T) {
	enc := newStreamEncoder("m")
	mustEmit(t, enc, "start", `{}`)

	// Two parallel calls streaming without an upstream index must not collide.
	a1 := firstCall(t, mustEmit(t, enc, "tool-call-delta", `{"toolCallId":"A","name":"read","arguments":"{\"p\":"}`))
	b1 := firstCall(t, mustEmit(t, enc, "tool-call-delta", `{"toolCallId":"B","name":"write","arguments":"x"}`))
	a2 := firstCall(t, mustEmit(t, enc, "tool-call-delta", `{"toolCallId":"A","arguments":"\"f\"}"}`))

	if a1["index"] != float64(0) || b1["index"] != float64(1) {
		t.Fatalf("indices must be allocated per tool call id: A=%v B=%v", a1["index"], b1["index"])
	}
	if a1["id"] != "A" || a1["type"] != "function" {
		t.Fatalf("first delta carries the id once: %v", a1)
	}
	if a2["index"] != float64(0) || a2["id"] != nil {
		t.Fatalf("later deltas reuse the index and omit the id: %v", a2)
	}

	// Final canonical tool-call matching the accumulated arguments emits an
	// empty suffix and reuses the delta's index.
	fin := firstCall(t, mustEmit(t, enc, "tool-call", `{"toolCallId":"A","toolName":"read","input":{"p":"f"}}`))
	fn := fin["function"].(map[string]any)
	if fn["arguments"] != "" {
		t.Fatalf("canonical equality should emit empty suffix, got %q", fn["arguments"])
	}
	if fin["index"] != float64(0) {
		t.Fatalf("canonical final call must reuse index 0: %v", fin)
	}
}

func TestStreamEncoderFinishUsageAndError(t *testing.T) {
	enc := newStreamEncoder("m")
	finish := mustEmit(t, enc, "finish", `{"finishReason":"tool-call","totalUsage":{"inputTokens":10,"outputTokens":4}}`)
	if len(finish) != 2 {
		t.Fatalf("finish must add a usage chunk: %d", len(finish))
	}
	first := decodeChunk(t, finish[0])
	choices := first["choices"].([]any)
	if choices[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason mapping: %v", choices[0])
	}
	usageChunk := decodeChunk(t, finish[1])
	usage := usageChunk["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 14 {
		t.Fatalf("usage = %v", usage)
	}
	if !enc.finished() {
		t.Fatal("finished flag")
	}

	errChunks := enc.errorChunks("boom")
	if len(errChunks) != 2 {
		t.Fatalf("error pair expected")
	}
	content := firstDelta(t, decodeChunk(t, errChunks[0]))["content"].(string)
	if !strings.Contains(content, "[upstream error] boom") {
		t.Fatalf("error content = %q", content)
	}
}

func TestToolArgumentSuffix(t *testing.T) {
	if s, err := toolArgumentSuffix(`{"a":`, `{"a":1}`); err != nil || s != "1}" {
		t.Fatalf("prefix case: %q %v", s, err)
	}
	if s, err := toolArgumentSuffix(`{ "a" : 1 }`, `{"a":1}`); err != nil || s != "" {
		t.Fatalf("whitespace-insensitive equivalence: %q %v", s, err)
	}
	if _, err := toolArgumentSuffix(`{"a":2}`, `{"a":1}`); err == nil {
		t.Fatal("conflicting arguments must error")
	}
}

func TestAggregateNonStreaming(t *testing.T) {
	agg := &aggregateResult{callIndex: map[string]int{}}
	applyAggregateEvent(agg, ccEventFrom("reasoning-delta", `{"text":"think "}`))
	applyAggregateEvent(agg, ccEventFrom("text-delta", `{"text":"answer"}`))
	applyAggregateEvent(agg, ccEventFrom("tool-call-delta", `{"toolCallId":"X","name":"f","arguments":"{\"a"}`))
	applyAggregateEvent(agg, ccEventFrom("tool-call-delta", `{"toolCallId":"X","arguments":":1}"}`))
	applyAggregateEvent(agg, ccEventFrom("tool-call", `{"toolCallId":"X","toolName":"f","input":{"a":1}}`))
	applyAggregateEvent(agg, ccEventFrom("finish", `{"finishReason":"tool-call","totalUsage":{"inputTokens":3,"outputTokens":2}}`))

	response := buildNonStreamingResponse("m", agg)
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	if parsed["object"] != "chat.completion" {
		t.Fatalf("object = %v", parsed["object"])
	}
	message := parsed["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["reasoning_content"] != "think " {
		t.Fatalf("reasoning = %v", message["reasoning_content"])
	}
	calls := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("one merged call expected: %v", calls)
	}
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if fn["arguments"] != `{"a":1}` {
		t.Fatalf("canonical call must replace the delta accumulation, got %q", fn["arguments"])
	}
	if parsed["choices"].([]any)[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Fatal("finish reason mapping")
	}
	usage := parsed["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 5 {
		t.Fatalf("usage = %v", usage)
	}
}

func TestAggregateError(t *testing.T) {
	agg := &aggregateResult{callIndex: map[string]int{}}
	applyAggregateEvent(agg, ccEventFrom("error", `{"message":"quota exceeded"}`))
	if agg.UpstreamErr != "quota exceeded" {
		t.Fatalf("UpstreamErr = %q", agg.UpstreamErr)
	}
}
