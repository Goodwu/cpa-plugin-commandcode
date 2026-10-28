package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ──────────────────────────────────────────
// Chat Completions request types (downstream)
// ──────────────────────────────────────────

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	Stream              bool            `json:"stream"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	Tools               []chatTool      `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type chatContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// ──────────────────────────────────────────
// Command Code request types (upstream)
// ──────────────────────────────────────────

type ccContentPart struct {
	Type       string    `json:"type"`
	Text       string    `json:"text,omitempty"`
	Image      string    `json:"image,omitempty"`
	ToolCallID string    `json:"toolCallId,omitempty"`
	ToolName   string    `json:"toolName,omitempty"`
	Name       string    `json:"name,omitempty"`
	Input      any       `json:"input,omitempty"`
	Output     *ccOutput `json:"output,omitempty"`
	IsError    bool      `json:"isError,omitempty"`
}

type ccOutput struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type ccMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string | []ccContentPart
}

type ccTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// ccToolChoice mirrors the CC bridge's discriminated union. The bridge
// validates tool_choice as an object and rejects bare strings: accepted types
// are "auto", "any", and {"type":"tool","name":...}. There is NO "required"
// (use "any") and NO "none" (omit and let the model decide).
type ccToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type ccParams struct {
	Model           string          `json:"model"`
	Messages        []ccMessage     `json:"messages"`
	System          string          `json:"system,omitempty"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stop            json.RawMessage `json:"stop,omitempty"`
	Stream          bool            `json:"stream"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Tools           []ccTool        `json:"tools,omitempty"`
	ToolChoice      *ccToolChoice   `json:"tool_choice,omitempty"`
}

type ccRequestBody struct {
	Config         map[string]any `json:"config"`
	Memory         string         `json:"memory"`
	Taste          string         `json:"taste"`
	Skills         string         `json:"skills"`
	PermissionMode string         `json:"permissionMode"`
	Params         ccParams       `json:"params"`
	ThreadID       string         `json:"threadId"`
}

// ──────────────────────────────────────────
// Chat Completions -> CC request
// ──────────────────────────────────────────

func toCCRequest(req *chatRequest, threadID string) ccRequestBody {
	ccMessages, systemPrompt := toCCMessages(req.Messages)
	resolved := resolveModel(req.Model)

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = req.MaxCompletionTokens
	}

	body := ccRequestBody{
		Config:         buildCCConfig(),
		Memory:         "",
		Taste:          "",
		Skills:         "",
		PermissionMode: "standard",
		Params: ccParams{
			Model:           resolved,
			Messages:        ccMessages,
			System:          systemPrompt,
			MaxTokens:       maxTokens,
			Temperature:     req.Temperature,
			TopP:            req.TopP,
			Stop:            normalizeStop(req.Stop),
			Stream:          true, // CC upstream always streams; non-stream clients get an aggregated response
			ReasoningEffort: resolveEffortForModel(resolved, req.ReasoningEffort),
			Tools:           toCCTools(req.Tools),
			ToolChoice:      resolveToolChoice(req.ToolChoice),
		},
		ThreadID: threadID,
	}
	applyNoToolsSafeguard(&body, len(req.Tools) > 0)
	return body
}

// normalizeStop keeps string or []string stop sequences; anything else is dropped.
func normalizeStop(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		if value == "" {
			return nil
		}
		return raw
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil && len(list) > 0 {
		return raw
	}
	return nil
}

func toCCMessages(messages []chatMessage) ([]ccMessage, string) {
	var systemParts []string
	var ccMessages []ccMessage

	// CC tool-result parts must carry toolName: map tool_call_id -> tool name.
	toolNameByID := map[string]string{}
	for _, msg := range messages {
		for _, call := range msg.ToolCalls {
			if call.ID != "" {
				toolNameByID[call.ID] = call.Function.Name
			}
		}
	}

	for _, msg := range messages {
		switch msg.Role {
		case "system", "developer":
			if text, _, isString := contentText(msg.Content); isString {
				systemParts = append(systemParts, text)
				continue
			}
			if parts, ok := contentParts(msg.Content); ok {
				var texts []string
				for _, part := range parts {
					if part.Type == "text" {
						texts = append(texts, part.Text)
					}
				}
				systemParts = append(systemParts, strings.Join(texts, "\n"))
			}
		case "tool":
			resultText := string(msg.Content)
			if text, _, isString := contentText(msg.Content); isString {
				resultText = text
			}
			ccMessages = append(ccMessages, ccMessage{Role: "tool", Content: []ccContentPart{{
				Type:       "tool-result",
				ToolCallID: msg.ToolCallID,
				ToolName:   toolNameByID[msg.ToolCallID],
				Output:     &ccOutput{Type: "text", Value: resultText},
			}}})
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				parts := []ccContentPart{}
				if text, _, isString := contentText(msg.Content); isString && text != "" {
					parts = append(parts, ccContentPart{Type: "text", Text: text})
				}
				for _, call := range msg.ToolCalls {
					parts = append(parts, ccContentPart{
						Type:       "tool-call",
						ToolCallID: call.ID,
						ToolName:   call.Function.Name,
						Input:      parseToolInput(call.Function.Arguments),
					})
				}
				ccMessages = append(ccMessages, ccMessage{Role: "assistant", Content: parts})
				continue
			}
			if text, _, isString := contentText(msg.Content); isString {
				ccMessages = append(ccMessages, ccMessage{Role: "assistant", Content: text})
			}
		case "user":
			if text, _, isString := contentText(msg.Content); isString {
				ccMessages = append(ccMessages, ccMessage{Role: "user", Content: text})
				continue
			}
			if parts, ok := contentParts(msg.Content); ok {
				converted := make([]ccContentPart, 0, len(parts))
				for _, part := range parts {
					if part.Type == "image_url" && part.ImageURL != nil && part.ImageURL.URL != "" {
						converted = append(converted, ccContentPart{Type: "image", Image: part.ImageURL.URL})
						continue
					}
					converted = append(converted, ccContentPart{Type: "text", Text: part.Text})
				}
				ccMessages = append(ccMessages, ccMessage{Role: "user", Content: converted})
			}
		default:
			if text, _, isString := contentText(msg.Content); isString && text != "" {
				ccMessages = append(ccMessages, ccMessage{Role: "user", Content: text})
			}
		}
	}

	system := ""
	if len(systemParts) > 0 {
		system = strings.Join(systemParts, "\n\n")
	}
	return pruneDanglingTools(ccMessages), system
}

// parseToolInput keeps the JSON object when arguments parse, and falls back to
// the raw string when the model emitted non-JSON arguments.
func parseToolInput(arguments string) any {
	if arguments == "" {
		return map[string]any{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(arguments), &parsed); err != nil {
		return arguments
	}
	return parsed
}

// contentText decodes content as a JSON string. JSON null decodes into a
// string without error, so the literal is checked explicitly: null is NOT a
// string for message-shape purposes.
func contentText(raw json.RawMessage) (text string, parts []chatContentPart, isString bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil, trimmed == "null" || trimmed == ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return value, nil, true
	}
	return "", nil, false
}

func contentParts(raw json.RawMessage) ([]chatContentPart, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var parts []chatContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, false
	}
	return parts, true
}

func toCCTools(tools []chatTool) []ccTool {
	if len(tools) == 0 {
		return nil
	}
	result := make([]ccTool, 0, len(tools))
	for _, tool := range tools {
		schema := tool.Function.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		result = append(result, ccTool{Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: schema})
	}
	return result
}

// resolveToolChoice maps OpenAI tool_choice to the CC object union:
// required -> any, function -> tool{name}, auto/none/absent -> omit.
func resolveToolChoice(raw json.RawMessage) *ccToolChoice {
	if len(raw) == 0 {
		return nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		if mode == "required" {
			return &ccToolChoice{Type: "any"}
		}
		return nil
	}
	var object struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil
	}
	switch object.Type {
	case "any":
		return &ccToolChoice{Type: "any"}
	case "tool":
		return &ccToolChoice{Type: "tool", Name: object.Name}
	case "function":
		return &ccToolChoice{Type: "tool", Name: object.Function.Name}
	default:
		return nil
	}
}

// buildCCConfig mirrors the reference proxy's CLI-shaped config block.
func buildCCConfig() map[string]any {
	arch := runtime.GOARCH
	switch arch {
	case "aarch64":
		arch = "arm64"
	case "x86_64", "amd64":
		arch = "amd64"
	}
	workingDir, errGetwd := os.Getwd()
	if errGetwd != nil || workingDir == "" {
		workingDir = "."
	}
	return map[string]any{
		"workingDir":    workingDir,
		"date":          time.Now().UTC().Format("2006-01-02"),
		"environment":   fmt.Sprintf("%s-%s, Node.js %s", runtime.GOOS, arch, cliNodeVersion),
		"structure":     []any{},
		"isGitRepo":     false,
		"currentBranch": "",
		"mainBranch":    "",
		"gitStatus":     "",
		"recentCommits": []any{},
	}
}

const noToolsSystemInstruction = "CRITICAL: You are running in a chat-only environment. Tool execution is disabled. Do not generate or call any tools (e.g. Build, ReadFile, grep, Search, etc.). Respond only with plain text."

const noToolsUserSuffix = "\n\n[System Note: Tool execution is disabled in this environment. Do not output any tool calls (such as Build, Search, ReadFile, grep, etc.). You must answer directly in plain text.]"

// applyNoToolsSafeguard reproduces the reference proxy's guard: when the
// client sent no tools, models trained to emit tool calls are steered to
// plain text via the system prompt and the last user message.
func applyNoToolsSafeguard(body *ccRequestBody, hasTools bool) {
	if hasTools {
		return
	}
	if body.Params.System != "" {
		body.Params.System += "\n\n" + noToolsSystemInstruction
	} else {
		body.Params.System = noToolsSystemInstruction
	}
	messages := body.Params.Messages
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		switch content := messages[i].Content.(type) {
		case string:
			messages[i].Content = content + noToolsUserSuffix
		case []ccContentPart:
			for j := len(content) - 1; j >= 0; j-- {
				if content[j].Type == "text" {
					content[j].Text += noToolsUserSuffix
					messages[i].Content = content
					return
				}
			}
			messages[i].Content = append(content, ccContentPart{Type: "text", Text: noToolsUserSuffix})
		}
		return
	}
}

// pruneDanglingTools drops unpaired tool-call/tool-result parts: CC rejects
// requests where a tool-call has no matching tool-result or vice versa.
func pruneDanglingTools(messages []ccMessage) []ccMessage {
	callIDs := map[string]struct{}{}
	resultIDs := map[string]struct{}{}
	for _, msg := range messages {
		parts, ok := msg.Content.([]ccContentPart)
		if !ok {
			continue
		}
		for _, part := range parts {
			if part.ToolCallID == "" {
				continue
			}
			switch part.Type {
			case "tool-call":
				callIDs[part.ToolCallID] = struct{}{}
			case "tool-result":
				resultIDs[part.ToolCallID] = struct{}{}
			}
		}
	}
	allPaired := len(callIDs) == len(resultIDs)
	if allPaired {
		for id := range callIDs {
			if _, ok := resultIDs[id]; !ok {
				allPaired = false
				break
			}
		}
	}
	if allPaired {
		return messages
	}
	valid := map[string]struct{}{}
	for id := range callIDs {
		if _, ok := resultIDs[id]; ok {
			valid[id] = struct{}{}
		}
	}
	result := make([]ccMessage, 0, len(messages))
	for _, msg := range messages {
		parts, ok := msg.Content.([]ccContentPart)
		if !ok {
			result = append(result, msg)
			continue
		}
		filtered := make([]ccContentPart, 0, len(parts))
		for _, part := range parts {
			if part.Type != "tool-call" && part.Type != "tool-result" {
				filtered = append(filtered, part)
				continue
			}
			if _, ok := valid[part.ToolCallID]; ok {
				filtered = append(filtered, part)
			}
		}
		if len(filtered) > 0 {
			result = append(result, ccMessage{Role: msg.Role, Content: filtered})
		}
	}
	return result
}

// usageData is the normalized token accounting carried on the finish event.
type usageData struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int
	ReasoningTokens  int
	HasPrompt        bool
	HasCompletion    bool
	HasTotal         bool
	HasCached        bool
	HasReasoning     bool
}

// extractUsage reads CC's camelCase totalUsage (or snake_case usage) shapes.
func extractUsage(data map[string]any) *usageData {
	var raw map[string]any
	if candidate, ok := data["totalUsage"].(map[string]any); ok {
		raw = candidate
	} else if candidate, ok := data["usage"].(map[string]any); ok {
		raw = candidate
	}
	if raw == nil {
		return nil
	}
	usage := &usageData{}
	if value, ok := numberFrom(raw, "promptTokens", "inputTokens", "prompt_tokens"); ok {
		usage.PromptTokens = int(value)
		usage.HasPrompt = true
	}
	if value, ok := numberFrom(raw, "completionTokens", "outputTokens", "completion_tokens"); ok {
		usage.CompletionTokens = int(value)
		usage.HasCompletion = true
	}
	if value, ok := numberFrom(raw, "totalTokens", "total_tokens"); ok {
		usage.TotalTokens = int(value)
		usage.HasTotal = true
	} else if usage.HasPrompt && usage.HasCompletion {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		usage.HasTotal = true
	}
	if value, ok := nestedNumber(raw, "inputTokenDetails", "cacheReadTokens"); ok {
		usage.CachedTokens = int(value)
		usage.HasCached = true
	} else if value, ok := nestedNumber(raw, "promptTokensDetails", "cachedTokens"); ok {
		usage.CachedTokens = int(value)
		usage.HasCached = true
	} else if value, ok := nestedNumber(raw, "prompt_tokens_details", "cached_tokens"); ok {
		usage.CachedTokens = int(value)
		usage.HasCached = true
	}
	if value, ok := nestedNumber(raw, "outputTokenDetails", "reasoningTokens"); ok {
		usage.ReasoningTokens = int(value)
		usage.HasReasoning = true
	} else if value, ok := nestedNumber(raw, "completion_tokens_details", "reasoning_tokens"); ok {
		usage.ReasoningTokens = int(value)
		usage.HasReasoning = true
	}
	if !usage.HasPrompt && !usage.HasCompletion && !usage.HasTotal && !usage.HasCached && !usage.HasReasoning {
		return nil
	}
	return usage
}

func numberFrom(data map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		switch value := data[key].(type) {
		case float64:
			return value, true
		case string:
			trimmed := strings.TrimSpace(value)
			if trimmed == "" {
				continue
			}
			if parsed, err := strconv.ParseFloat(trimmed, 64); err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

func nestedNumber(data map[string]any, object, field string) (float64, bool) {
	nested, ok := data[object].(map[string]any)
	if !ok || nested == nil {
		return 0, false
	}
	if value, ok := nested[field].(float64); ok {
		return value, true
	}
	return 0, false
}
