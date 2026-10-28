package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// streamEncoder converts CC stream events into OpenAI chat.completion.chunk
// JSON payloads, ported from the reference proxy's OpenAIStreamEncoder.
type streamEncoder struct {
	id              string
	created         int64
	model           string
	toolCallIndex   int
	sawFinish       bool
	toolCallIDIndex map[string]int
	toolArguments   map[int]string
	toolMetadata    map[int]toolIdentity
}

type toolIdentity struct {
	id   string
	name string
}

func newStreamEncoder(model string) *streamEncoder {
	return &streamEncoder{
		id:              "chatcmpl-" + newUUID(),
		created:         time.Now().Unix(),
		model:           model,
		toolCallIDIndex: map[string]int{},
		toolArguments:   map[int]string{},
		toolMetadata:    map[int]toolIdentity{},
	}
}

var openAIFinishMap = map[string]string{
	"stop":             "stop",
	"length":           "length",
	"content_filtered": "content_filter",
	"tool-call":        "tool_calls",
	"tool-calls":       "tool_calls",
	"tool_call":        "tool_calls",
	"error":            "stop",
}

func (e *streamEncoder) finished() bool { return e.sawFinish }

func (e *streamEncoder) baseChunk() map[string]any {
	return map[string]any{
		"id":      e.id,
		"object":  "chat.completion.chunk",
		"created": e.created,
		"model":   e.model,
	}
}

// resolveToolCallIndex allocates a stable OpenAI streaming index per tool call
// id. Without it, parallel tool-call deltas that carry no upstream index would
// all be numbered 0 and clients would merge them into one call.
func (e *streamEncoder) resolveToolCallIndex(toolCallID string, upstreamIndex *int) int {
	if toolCallID != "" {
		if known, ok := e.toolCallIDIndex[toolCallID]; ok {
			return known
		}
		index := e.toolCallIndex
		if upstreamIndex != nil {
			index = *upstreamIndex
		}
		e.toolCallIDIndex[toolCallID] = index
		if upstreamIndex != nil && index >= e.toolCallIndex {
			e.toolCallIndex = index + 1
		} else {
			e.toolCallIndex++
		}
		return index
	}
	if upstreamIndex != nil {
		return *upstreamIndex
	}
	index := e.toolCallIndex
	e.toolCallIndex++
	return index
}

// toolMetadataDelta emits id/name the first time they are seen for an index
// and rejects conflicting metadata.
func (e *streamEncoder) toolMetadataDelta(index int, id, name string) (toolIdentity, error) {
	seen := e.toolMetadata[index]
	var delta toolIdentity
	for _, pair := range []struct {
		field string
		value string
	}{{"id", id}, {"name", name}} {
		if pair.value == "" {
			continue
		}
		current := seen.id
		if pair.field == "name" {
			current = seen.name
		}
		if current != "" && current != pair.value {
			return toolIdentity{}, fmt.Errorf("inconsistent upstream tool metadata")
		}
		if current == "" {
			if pair.field == "id" {
				seen.id = pair.value
				delta.id = pair.value
			} else {
				seen.name = pair.value
				delta.name = pair.value
			}
		}
	}
	e.toolMetadata[index] = seen
	return delta, nil
}

// emit turns one CC event into zero or more chunk payloads.
func (e *streamEncoder) emit(event ccEvent) ([][]byte, error) {
	switch event.Type {
	case "start":
		e.toolCallIndex = 0
		return e.encodeChunk(map[string]any{"role": "assistant"}, nil), nil
	case "text-delta":
		text := eventString(event.Data, "text")
		if text == "" {
			return nil, nil
		}
		return e.encodeChunk(map[string]any{"content": text}, nil), nil
	case "reasoning-delta":
		text := eventString(event.Data, "text")
		if text == "" {
			return nil, nil
		}
		return e.encodeChunk(map[string]any{"reasoning_content": text}, nil), nil
	case "tool-call-delta":
		toolCallID := eventString(event.Data, "toolCallId")
		index := e.resolveToolCallIndex(toolCallID, eventIndex(event.Data))
		arguments := ""
		if value, ok := event.Data["arguments"].(string); ok {
			arguments = value
		}
		name := eventString(event.Data, "name")
		metadata, err := e.toolMetadataDelta(index, toolCallID, name)
		if err != nil {
			return nil, err
		}
		e.toolArguments[index] += arguments
		toolCall := map[string]any{
			"index":    index,
			"function": map[string]any{"arguments": arguments},
		}
		if metadata.id != "" {
			toolCall["id"] = metadata.id
			toolCall["type"] = "function"
		}
		if metadata.name != "" {
			toolCall["function"].(map[string]any)["name"] = metadata.name
		}
		return e.encodeChunk(map[string]any{"tool_calls": []any{toolCall}}, nil), nil
	case "tool-call":
		toolCallID := eventString(event.Data, "toolCallId")
		toolName := eventString(event.Data, "toolName", "name")
		args := stringifyToolInput(event.Data["input"], event.Data["arguments"])
		index := e.resolveToolCallIndex(toolCallID, eventIndex(event.Data))
		emitted := e.toolArguments[index]
		suffix, errSuffix := toolArgumentSuffix(emitted, args)
		if errSuffix != nil {
			return nil, errSuffix
		}
		e.toolArguments[index] = emitted + suffix
		metadata, err := e.toolMetadataDelta(index, toolCallID, toolName)
		if err != nil {
			return nil, err
		}
		toolCall := map[string]any{
			"index":    index,
			"function": map[string]any{"arguments": suffix},
		}
		if metadata.id != "" {
			toolCall["id"] = metadata.id
			toolCall["type"] = "function"
		}
		if metadata.name != "" {
			toolCall["function"].(map[string]any)["name"] = metadata.name
		}
		return e.encodeChunk(map[string]any{"tool_calls": []any{toolCall}}, nil), nil
	case "finish":
		e.sawFinish = true
		usage := extractUsage(event.Data)
		finishReason := "stop"
		if raw, ok := event.Data["finishReason"].(string); ok && raw != "" {
			finishReason = raw
		}
		chunks := e.encodeChunk(map[string]any{}, ptrString(openAIFinishReason(finishReason)))
		if usage != nil {
			chunk := e.baseChunk()
			chunk["choices"] = []any{}
			chunk["usage"] = openAIUsage(usage)
			chunks = append(chunks, marshalJSON(chunk))
		}
		return chunks, nil
	case "error":
		e.sawFinish = true
		message := eventString(event.Data, "message")
		if message == "" {
			if nested, ok := event.Data["error"].(map[string]any); ok {
				message = eventString(nested, "message")
			}
		}
		if message == "" {
			raw, _ := json.Marshal(event.Data)
			message = string(raw)
		}
		return e.errorChunks(message), nil
	}
	return nil, nil
}

func (e *streamEncoder) encodeChunk(delta map[string]any, finishReason *string) [][]byte {
	chunk := e.baseChunk()
	reason := any(nil)
	if finishReason != nil {
		reason = *finishReason
	}
	chunk["choices"] = []any{map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": reason,
	}}
	return [][]byte{marshalJSON(chunk)}
}

func openAIFinishReason(raw string) string {
	if mapped, ok := openAIFinishMap[raw]; ok {
		return mapped
	}
	return "stop"
}

// errorChunks surfaces upstream failures as a valid content+finish chunk pair:
// clients always see a uniform stream shape instead of an ad-hoc error frame.
func (e *streamEncoder) errorChunks(message string) [][]byte {
	hostLog("warn", "commandcode-go: upstream error in stream: "+truncate(message, 200))
	content := e.encodeChunk(map[string]any{"content": "[upstream error] " + message}, nil)
	finish := e.encodeChunk(map[string]any{}, ptrString("stop"))
	return append(content, finish...)
}

// finishChunks synthesizes the terminal delta for streams that ended without
// a finish event (upstream dropped mid-response).
func (e *streamEncoder) finishChunks(reason string) [][]byte {
	if reason == "" {
		reason = "stop"
	}
	return e.encodeChunk(map[string]any{}, ptrString(reason))
}

// forceFinish satisfies the shared terminator interface for the OpenAI encoder.
func (e *streamEncoder) forceFinish() [][]byte {
	if e.finished() {
		return nil
	}
	return e.finishChunks("stop")
}

// failStream surfaces a mid-stream failure as the uniform content+finish
// chunk pair so the client always sees a legal stream shape.
func (e *streamEncoder) failStream(message string) [][]byte {
	if e.finished() {
		return nil
	}
	return e.errorChunks(message)
}

func openAIUsage(usage *usageData) map[string]any {
	result := map[string]any{}
	prompt := 0
	completion := 0
	if usage.HasPrompt {
		prompt = usage.PromptTokens
	}
	if usage.HasCompletion {
		completion = usage.CompletionTokens
	}
	total := prompt + completion
	if usage.HasTotal {
		total = usage.TotalTokens
	}
	result["prompt_tokens"] = prompt
	result["completion_tokens"] = completion
	result["total_tokens"] = total
	if usage.HasCached {
		result["prompt_tokens_details"] = map[string]any{"cached_tokens": usage.CachedTokens}
	}
	if usage.HasReasoning {
		result["completion_tokens_details"] = map[string]any{"reasoning_tokens": usage.ReasoningTokens}
	}
	return result
}

// aggregateResult is the non-streaming aggregation of one CC event stream.
type aggregateResult struct {
	Content     strings.Builder
	Reasoning   strings.Builder
	ToolCalls   []map[string]any
	callIndex   map[string]int
	Finish      string
	Usage       *usageData
	UpstreamErr string
}

// applyAggregateEvent folds one CC event into the non-streaming accumulator.
func applyAggregateEvent(sink *aggregateResult, event ccEvent) {
	switch event.Type {
	case "error":
		message := eventString(event.Data, "message")
		if message == "" {
			if nested, ok := event.Data["error"].(map[string]any); ok {
				message = eventString(nested, "message")
			}
		}
		sink.UpstreamErr = message
	case "text-delta":
		sink.Content.WriteString(eventString(event.Data, "text"))
	case "reasoning-delta":
		sink.Reasoning.WriteString(eventString(event.Data, "text"))
	case "tool-call-delta":
		id := eventString(event.Data, "toolCallId")
		arguments := ""
		if value, ok := event.Data["arguments"].(string); ok {
			arguments = value
		}
		index, ok := sink.callIndex[id]
		if !ok {
			index = len(sink.ToolCalls)
			sink.callIndex[id] = index
			sink.ToolCalls = append(sink.ToolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      eventString(event.Data, "name"),
					"arguments": arguments,
				},
			})
			return
		}
		entry := sink.ToolCalls[index]
		fn := entry["function"].(map[string]any)
		fn["arguments"] = fn["arguments"].(string) + arguments
	case "tool-call":
		// Bridges emit tool-call-delta*N then a final tool-call with the same
		// id; the canonical final payload replaces the accumulation.
		id := eventString(event.Data, "toolCallId")
		entry := map[string]any{
			"id":   id,
			"type": "function",
			"function": map[string]any{
				"name":      eventString(event.Data, "toolName", "name"),
				"arguments": stringifyToolInput(event.Data["input"], event.Data["arguments"]),
			},
		}
		if index, ok := sink.callIndex[id]; ok {
			sink.ToolCalls[index] = entry
		} else {
			sink.callIndex[id] = len(sink.ToolCalls)
			sink.ToolCalls = append(sink.ToolCalls, entry)
		}
	case "finish":
		sink.Usage = extractUsage(event.Data)
		if raw, ok := event.Data["finishReason"].(string); ok && raw != "" {
			sink.Finish = raw
		}
	}
}

func buildNonStreamingResponse(model string, result *aggregateResult) map[string]any {
	message := map[string]any{"role": "assistant"}
	if result.Reasoning.Len() > 0 {
		message["reasoning_content"] = result.Reasoning.String()
	}
	if len(result.ToolCalls) > 0 {
		message["tool_calls"] = result.ToolCalls
	}
	if result.Content.Len() > 0 {
		message["content"] = result.Content.String()
	} else if len(result.ToolCalls) == 0 {
		message["content"] = ""
	}
	finishReason := "stop"
	if result.Finish != "" {
		finishReason = result.Finish
	}
	response := map[string]any{
		"id":      "chatcmpl-" + newUUID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": openAIFinishReason(finishReason),
		}},
	}
	if result.Usage != nil {
		response["usage"] = openAIUsage(result.Usage)
	}
	return response
}

func stringifyToolInput(candidates ...any) string {
	for _, value := range candidates {
		switch typed := value.(type) {
		case string:
			return typed
		case nil:
			continue
		default:
			raw, err := json.Marshal(value)
			if err == nil {
				return string(raw)
			}
		}
	}
	return ""
}

func eventIndex(data map[string]any) *int {
	if value, ok := eventNumber(data, "index"); ok {
		index := int(value)
		return &index
	}
	return nil
}

func ptrString(value string) *string { return &value }

func marshalJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		raw = []byte(`{"error":{"message":"plugin marshal failure","type":"upstream_error"}}`)
	}
	return raw
}

// toolArgumentSuffix reconciles a canonical tool-call argument string with the
// bytes already streamed to the client, mirroring the reference helper: a
// prefix match returns the tail; JSON-equivalent payloads emit nothing; a
// whitespace-insensitive diff is validated by re-parsing the merge.
func toolArgumentSuffix(emitted, canonical string) (string, error) {
	if strings.HasPrefix(canonical, emitted) {
		return canonical[len(emitted):], nil
	}
	if jsonEquivalent(emitted, canonical) {
		return "", nil
	}
	offset := 0
	inString := false
	escaped := false
	for _, char := range emitted {
		if !inString && (char == ' ' || char == '\t' || char == '\r' || char == '\n') {
			continue
		}
		for offset < len(canonical) && isJSONWhitespace(canonical[offset]) {
			offset++
		}
		encoded := string(char)
		if offset+len(encoded) > len(canonical) || canonical[offset:offset+len(encoded)] != encoded {
			return "", fmt.Errorf("inconsistent upstream tool arguments")
		}
		offset += len(encoded)
		switch {
		case escaped:
			escaped = false
		case inString && char == '\\':
			escaped = true
		case char == '"':
			inString = !inString
		}
	}
	suffix := canonical[offset:]
	if jsonEquivalent(emitted+suffix, canonical) {
		return suffix, nil
	}
	return "", fmt.Errorf("inconsistent upstream tool arguments")
}

func isJSONWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

func jsonEquivalent(a, b string) bool {
	var left, right any
	if err := json.Unmarshal([]byte(a), &left); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &right); err != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}
