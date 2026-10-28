package main

import (
	"encoding/json"
	"strings"
)

// Native Anthropic output path. The host passes plugin chunks through
// unmodified when the requested client format equals a declared executor
// output format (adapters_executors.go translateExecutorStreamChunks, and
// code_handlers.go writes chunk bytes raw), so for Claude clients the plugin
// must emit fully framed SSE events:
//
//	event: <type>\ndata: <json>\n\n
//
// Chunk contract mirrored from sdk/api/handlers/handlers_plugin_executor_usage_test.go:
// usage is split across message_start (input/cache) and message_delta (output).

type claudeToolBlock struct {
	index   int
	id      string
	name    string
	emitted string // partial_json bytes already streamed (opened only)
	opened  bool
	pending strings.Builder // arguments buffered before the block could open (id/name unknown)
}

type claudeEncoder struct {
	messageID string
	model     string
	nextIndex int
	// current open block
	openKind  string // "", "thinking", "text", "tool_use"
	openIndex int
	openTool  string // tool call id when openKind == tool_use
	startSent bool
	tools     map[string]*claudeToolBlock // keyed by resolved tool call id
	toolOrder []*claudeToolBlock          // creation order for deterministic flush
	finish    string
	usage     *usageData
	sawFinish bool
}

func newClaudeEncoder(model string) *claudeEncoder {
	return &claudeEncoder{
		messageID: "msg_" + compactUUID(),
		model:     model,
		tools:     map[string]*claudeToolBlock{},
	}
}

func (e *claudeEncoder) finished() bool { return e.sawFinish }

func claudeFrame(eventType string, data map[string]any) []byte {
	raw, err := json.Marshal(data)
	if err != nil {
		raw = []byte(`{"type":"error","error":{"type":"api_error","message":"plugin marshal failure"}}`)
	}
	return []byte("event: " + eventType + "\ndata: " + string(raw) + "\n\n")
}

func (e *claudeEncoder) messageStart() [][]byte {
	e.startSent = true
	return [][]byte{claudeFrame("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": e.messageID, "type": "message", "role": "assistant",
			"model": e.model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})}
}

// ensureStart lazily opens the stream; every emit path starts here.
func (e *claudeEncoder) ensureStart(frames [][]byte) [][]byte {
	if e.startSent {
		return frames
	}
	return append(frames, e.messageStart()...)
}

func (e *claudeEncoder) closeBlock() [][]byte {
	if e.openKind == "" {
		return nil
	}
	frames := [][]byte{claudeFrame("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": e.openIndex,
	})}
	e.openKind = ""
	e.openTool = ""
	return frames
}

func (e *claudeEncoder) openBlock(kind string, block map[string]any) [][]byte {
	e.openIndex = e.nextIndex
	e.nextIndex++
	e.openKind = kind
	return [][]byte{claudeFrame("content_block_start", map[string]any{
		"type": "content_block_start", "index": e.openIndex, "content_block": block,
	})}
}

// ensureBlock closes any other open block and opens (or keeps) a sequential
// thinking/text block. CC streams reasoning first then text; a switch back
// after tools just starts a fresh block (valid Anthropic content ordering).
func (e *claudeEncoder) ensureBlock(kind string) [][]byte {
	if e.openKind == kind {
		return nil
	}
	frames := e.closeBlock()
	var block map[string]any
	switch kind {
	case "thinking":
		block = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
	case "text":
		block = map[string]any{"type": "text", "text": ""}
	}
	e.openTool = ""
	return append(frames, e.openBlock(kind, block)...)
}

func (e *claudeEncoder) textDelta(kind, deltaType, field, text string) [][]byte {
	if text == "" {
		return nil
	}
	frames := e.ensureBlock(kind)
	return append(frames, claudeFrame("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": e.openIndex,
		"delta": map[string]any{"type": deltaType, field: text},
	}))
}

// resolveToolBlock finds or creates the block for a tool call id. Deltas
// without an id reuse the most recent not-yet-opened block (CC always sends
// ids, this is defensive).
func (e *claudeEncoder) resolveToolBlock(id string) *claudeToolBlock {
	if id != "" {
		if block, ok := e.tools[id]; ok {
			return block
		}
		block := &claudeToolBlock{index: -1, id: id}
		e.tools[id] = block
		e.toolOrder = append(e.toolOrder, block)
		return block
	}
	for i := len(e.toolOrder) - 1; i >= 0; i-- {
		if block := e.toolOrder[i]; !block.opened && block.id == "" {
			return block
		}
	}
	block := &claudeToolBlock{index: -1}
	e.toolOrder = append(e.toolOrder, block)
	return block
}

// toolFrame handles both tool-call-delta and canonical tool-call events,
// emitting partial_json increments once the block is open.
func (e *claudeEncoder) toolFrame(data map[string]any, canonical bool) ([][]byte, error) {
	id := eventString(data, "toolCallId")
	name := eventString(data, "toolName", "name")
	block := e.resolveToolBlock(id)
	if id != "" && block.id == "" {
		block.id = id
		e.tools[id] = block
	}
	if name != "" && block.name == "" {
		block.name = name
	}

	// Incoming bytes for this event, reconciled against what was streamed.
	var inc string
	if canonical {
		incoming := stringifyToolInput(data["input"], data["arguments"])
		soFar := block.emitted + block.pending.String()
		if soFar == "" {
			inc = incoming
		} else {
			var err error
			inc, err = toolArgumentSuffix(soFar, incoming)
			if err != nil {
				return nil, err
			}
		}
	} else if value, ok := data["arguments"].(string); ok {
		inc = value
	}

	var frames [][]byte
	if !block.opened {
		if block.id == "" || block.name == "" {
			// Cannot open a tool_use block without id/name yet: buffer.
			if inc != "" {
				block.pending.WriteString(inc)
			}
			return nil, nil
		}
		if inc == "" && block.pending.Len() == 0 {
			return nil, nil
		}
		frames = append(frames, e.closeBlock()...)
		block.index = e.nextIndex
		e.nextIndex++
		e.openIndex = block.index
		e.openKind = "tool_use"
		e.openTool = block.id
		block.opened = true
		frames = append(frames, claudeFrame("content_block_start", map[string]any{
			"type": "content_block_start", "index": block.index,
			"content_block": map[string]any{"type": "tool_use", "id": block.id, "name": block.name, "input": map[string]any{}},
		}))
		if buffered := block.pending.String(); buffered != "" {
			block.emitted = buffered
			block.pending.Reset()
			frames = append(frames, claudeFrame("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": block.index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": buffered},
			}))
		}
	} else if e.openKind != "tool_use" || e.openTool != block.id {
		// Interleaved tool streams: close the current block and re-anchor
		// without re-emitting content_block_start.
		frames = append(frames, e.closeBlock()...)
		e.openKind = "tool_use"
		e.openIndex = block.index
		e.openTool = block.id
	}
	if inc == "" {
		return frames, nil
	}
	block.emitted += inc
	frames = append(frames, claudeFrame("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": block.index,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": inc},
	}))
	return frames, nil
}

// flushUnopenedTools guarantees buffered args are never lost: on finalize,
// any buffered tool block gets a real id/name (synthesized when the upstream
// never provided one) and is emitted before the message terminates.
func (e *claudeEncoder) flushUnopenedTools() [][]byte {
	var frames [][]byte
	for _, block := range e.toolOrder {
		if block.opened || block.pending.Len() == 0 {
			continue
		}
		if block.id == "" {
			block.id = "toolu_" + compactUUID()[:20]
		}
		if block.name == "" {
			block.name = "unknown_tool"
		}
		frames = append(frames, e.closeBlock()...)
		block.index = e.nextIndex
		e.nextIndex++
		e.openIndex = block.index
		e.openKind = "tool_use"
		e.openTool = block.id
		block.opened = true
		frames = append(frames, claudeFrame("content_block_start", map[string]any{
			"type": "content_block_start", "index": block.index,
			"content_block": map[string]any{"type": "tool_use", "id": block.id, "name": block.name, "input": map[string]any{}},
		}))
		buffered := block.pending.String()
		block.pending.Reset()
		block.emitted = buffered
		frames = append(frames, claudeFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": block.index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": buffered},
		}))
		frames = append(frames, e.closeBlock()...)
	}
	return frames
}

// finalize closes open blocks and emits message_delta + message_stop.
func (e *claudeEncoder) finalize() [][]byte {
	frames := e.flushUnopenedTools()
	frames = append(frames, e.closeBlock()...)
	stopReason := "end_turn"
	switch e.finish {
	case "length":
		stopReason = "max_tokens"
	case "tool_calls", "tool-call", "tool-calls", "tool_call":
		stopReason = "tool_use"
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if e.usage != nil {
		if e.usage.HasPrompt {
			usage["input_tokens"] = e.usage.PromptTokens
		}
		if e.usage.HasCompletion {
			usage["output_tokens"] = e.usage.CompletionTokens
		}
		if e.usage.HasCached {
			usage["cache_read_input_tokens"] = e.usage.CachedTokens
		}
		if e.usage.HasReasoning {
			usage["output_tokens_details"] = map[string]any{"thinking_tokens": e.usage.ReasoningTokens}
		}
	}
	frames = append(frames, claudeFrame("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": usage,
	}))
	frames = append(frames, claudeFrame("message_stop", map[string]any{"type": "message_stop"}))
	e.sawFinish = true
	return frames
}

// emit converts one CC event into zero or more raw Claude SSE frames.
func (e *claudeEncoder) emit(event ccEvent) ([][]byte, error) {
	switch event.Type {
	case "start":
		return e.ensureStart(nil), nil
	case "text-delta":
		return e.ensureStart(e.textDelta("text", "text_delta", "text", eventString(event.Data, "text"))), nil
	case "reasoning-delta":
		return e.ensureStart(e.textDelta("thinking", "thinking_delta", "thinking", eventString(event.Data, "text"))), nil
	case "tool-call-delta":
		frames, err := e.toolFrame(event.Data, false)
		if err != nil {
			return nil, err
		}
		return e.ensureStart(frames), nil
	case "tool-call":
		frames, err := e.toolFrame(event.Data, true)
		if err != nil {
			return nil, err
		}
		return e.ensureStart(frames), nil
	case "finish":
		frames := e.ensureStart(nil)
		e.usage = extractUsage(event.Data)
		if raw, ok := event.Data["finishReason"].(string); ok && raw != "" {
			e.finish = raw
		}
		return append(frames, e.finalize()...), nil
	case "error":
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
		hostLog("warn", "commandcode-go: claude upstream error: "+truncate(message, 200))
		frames := e.ensureStart(nil)
		frames = append(frames, e.textDelta("text", "text_delta", "text", "[upstream error] "+message)...)
		return append(frames, e.finalize()...), nil
	}
	return nil, nil
}

// forceFinish synthesizes the terminator when the upstream stream ended
// without a finish event (same role as finishChunks in the OpenAI encoder).
func (e *claudeEncoder) forceFinish() [][]byte {
	if e.sawFinish {
		return nil
	}
	if !e.startSent {
		return nil
	}
	return e.finalize()
}

// failStream closes any open block and ends the stream with the error
// surfaced as text, keeping the Anthropic event grammar intact.
func (e *claudeEncoder) failStream(message string) [][]byte {
	if e.sawFinish {
		return nil
	}
	frames := e.ensureStart(nil)
	frames = append(frames, e.textDelta("text", "text_delta", "text", "[upstream error] "+message)...)
	return append(frames, e.finalize()...)
}

// buildClaudeNonStreaming converts the aggregated CC result into a full
// Anthropic message JSON (used when the executor serves the claude format).
func buildClaudeNonStreaming(model string, agg *aggregateResult) map[string]any {
	content := []map[string]any{}
	if agg.Reasoning.Len() > 0 {
		content = append(content, map[string]any{
			"type": "thinking", "thinking": agg.Reasoning.String(), "signature": "",
		})
	}
	if agg.Content.Len() > 0 {
		content = append(content, map[string]any{"type": "text", "text": agg.Content.String()})
	}
	for _, call := range agg.ToolCalls {
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		var input any = map[string]any{}
		if strings.TrimSpace(args) != "" {
			var parsed any
			if err := json.Unmarshal([]byte(args), &parsed); err == nil {
				input = parsed
			}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": call["id"], "name": name, "input": input,
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	stopReason := "end_turn"
	switch agg.Finish {
	case "length":
		stopReason = "max_tokens"
	case "tool_calls", "tool-call", "tool-calls", "tool_call":
		stopReason = "tool_use"
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if agg.Usage != nil {
		usage["input_tokens"] = agg.Usage.PromptTokens
		usage["output_tokens"] = agg.Usage.CompletionTokens
		if agg.Usage.HasCached {
			usage["cache_read_input_tokens"] = agg.Usage.CachedTokens
		}
		if agg.Usage.HasReasoning {
			usage["output_tokens_details"] = map[string]any{"thinking_tokens": agg.Usage.ReasoningTokens}
		}
	}
	return map[string]any{
		"id": "msg_" + compactUUID(), "type": "message", "role": "assistant",
		"model": model, "content": content,
		"stop_reason": stopReason, "stop_sequence": nil,
		"usage": usage,
	}
}

func compactUUID() string {
	return strings.ReplaceAll(newUUID(), "-", "")
}

// isClaudeFormat reports whether the host asked this executor to produce
// Anthropic output. The host sets ExecutorRequest.Format to the chosen output
// format name (adapters_executors.go prepareExecutorCall); FormatClaude
// stringifies to "claude" while the capability name is "anthropic".
func isClaudeFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "anthropic", "claude":
		return true
	}
	return false
}

// chunkEmitter unifies the OpenAI and Claude stream encoders behind the
// method set the executor pump needs.
type chunkEmitter interface {
	emit(event ccEvent) ([][]byte, error)
	finished() bool
}

// terminator unifies the "stream ended without a finish event / with an
// error" cleanup paths of both encoders.
type terminator interface {
	forceFinish() [][]byte
	failStream(message string) [][]byte
}

var (
	_ chunkEmitter = (*streamEncoder)(nil)
	_ chunkEmitter = (*claudeEncoder)(nil)
	_ terminator   = (*streamEncoder)(nil)
	_ terminator   = (*claudeEncoder)(nil)
)
