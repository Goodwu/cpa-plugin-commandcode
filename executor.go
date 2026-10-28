package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// resolveAPIBase lets an auth file pin its own upstream base; the plugin
// config provides the default for all credentials.
func resolveAPIBase(storageBase, configBase string) string {
	if base := strings.TrimRight(strings.TrimSpace(storageBase), "/"); base != "" {
		return base
	}
	return strings.TrimRight(configBase, "/")
}

// credential extracts what the executor needs from one execution request.
type credential struct {
	APIKey  string
	APIBase string
}

func credentialFrom(req executorRequest) (credential, error) {
	storage, errParse := parseKeyStorage(req.StorageJSON)
	if errParse != nil {
		// Fall back to attributes/metadata for hosts that route auth data
		// without the storage blob (management-triggered nested execution).
		key := ""
		if storage != nil {
			key = storage.APIKey
		}
		if key == "" {
			key = req.AuthAttributes["api_key"]
		}
		if key == "" {
			if value, ok := req.AuthMetadata["api_key"].(string); ok {
				key = value
			}
		}
		base := req.AuthAttributes["base_url"]
		if key == "" {
			return credential{}, fmt.Errorf("commandcode-go: selected credential has no api_key")
		}
		return credential{APIKey: key, APIBase: base}, nil
	}
	key := strings.TrimSpace(storage.APIKey)
	if key == "" {
		return credential{}, fmt.Errorf("commandcode-go: selected credential has no api_key")
	}
	return credential{APIKey: key, APIBase: storage.BaseURL}, nil
}

// stripModelPrefix removes the plugin's own namespacing prefixes from a
// client-requested model id. With host force-model-prefix, clients send
// "<auth-prefix>/<org>/<model>" or "<provider>/<org>/<model>" and the
// translated payload may keep that prefix; CC rejects it ("Model/provider
// not recognized"). Repeated stripping handles stacked prefixes.
func stripModelPrefix(model, authPrefix string) string {
	model = strings.TrimSpace(model)
	prefixes := make([]string, 0, 2)
	if p := strings.TrimSpace(authPrefix); p != "" && p != authProviderID {
		prefixes = append(prefixes, p+"/")
	}
	prefixes = append(prefixes, authProviderID+"/")
	for changed := true; changed; {
		changed = false
		for _, p := range prefixes {
			if rest, ok := strings.CutPrefix(model, p); ok && rest != "" {
				model = rest
				changed = true
			}
		}
	}
	return model
}

// decodeChatRequest extracts the chat-completions payload the host translated
// for this executor (Payload), falling back to the raw client body when the
// host passed the request through unchanged.
func decodeChatRequest(req executorRequest) (*chatRequest, string, error) {
	raw := req.Payload
	if len(raw) == 0 {
		raw = req.OriginalRequest
	}
	if len(raw) == 0 {
		return nil, "", fmt.Errorf("commandcode-go: executor received an empty payload")
	}
	var chat chatRequest
	if err := json.Unmarshal(raw, &chat); err != nil {
		return nil, "", fmt.Errorf("commandcode-go: decode chat-completions payload: %w", err)
	}
	model := strings.TrimSpace(chat.Model)
	if model == "" {
		model = req.Model
	}
	chat.Model = stripModelPrefix(model, req.AuthAttributes["prefix"])
	return &chat, model, nil
}

// handleExecute runs one non-streaming generation: the upstream is always
// streaming, so the same event pump feeds the OpenAI non-streaming aggregator.
// Nothing has been sent to the client at this point, so every failure path can
// return a clean HTTP error envelope.
func handleExecute(request []byte) ([]byte, error) {
	var req executorRequest
	if err := decodeRequest(request, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	creds, errCred := credentialFrom(req)
	if errCred != nil {
		return errorEnvelope("invalid_request", errCred.Error()), nil
	}
	chat, _, errDecode := decodeChatRequest(req)
	if errDecode != nil {
		return errorEnvelope("bad_request", errDecode.Error()), nil
	}
	cfg := activeConfig()
	body := toCCRequest(chat, newUUID())
	open, errOpen := openUpstreamStream(req.HostCallbackID, resolveAPIBase(creds.APIBase, cfg.APIBase), creds.APIKey, &body)
	if errOpen != nil {
		return executorFailureEnvelope(errOpen), nil
	}

	var collected []ccEvent
	pumpErr := pumpUpstream(open.StreamID, cfg.IdleTimeout, func(event ccEvent) error {
		collected = append(collected, event)
		return nil
	})
	hostHTTPStreamClose(open.StreamID)

	agg := &aggregateResult{callIndex: map[string]int{}}
	for _, event := range collected {
		applyAggregateEvent(agg, event)
	}
	if agg.UpstreamErr != "" {
		return httpErrorEnvelope(http.StatusBadGateway, "commandcode-go: "+sanitizeErrorText(agg.UpstreamErr, creds.APIKey)), nil
	}
	if pumpErr != nil {
		return executorFailureEnvelope(pumpErr), nil
	}
	if agg.Finish == "" {
		agg.Finish = "stop"
	}
	// Anthropic clients with output format "anthropic" get a native Claude
	// message; the host passes plugin output through when it matches the
	// requested format (and its chat→claude non-stream transformer is
	// available for other paths, but streaming has no such transformer).
	var response map[string]any
	if isClaudeFormat(req.Format) {
		response = buildClaudeNonStreaming(body.Params.Model, agg)
	} else {
		response = buildNonStreamingResponse(body.Params.Model, agg)
	}
	payload := marshalJSON(response)
	return okEnvelope(executorResponse{
		Payload: payload,
		Headers: map[string][]string{"content-type": {"application/json"}},
	})
}

// handleExecuteStream streams one generation. The RPC stays open while the
// pump pushes chat.completion.chunk payloads through host.stream.emit; the
// host translates them toward the client protocol.
func handleExecuteStream(request []byte) ([]byte, error) {
	var req executorRequest
	if err := decodeRequest(request, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	if req.StreamID == "" {
		return errorEnvelope("invalid_request", "commandcode-go: executor.execute_stream requires stream_id"), nil
	}
	creds, errCred := credentialFrom(req)
	if errCred != nil {
		return errorEnvelope("invalid_request", errCred.Error()), nil
	}
	chat, _, errDecode := decodeChatRequest(req)
	if errDecode != nil {
		return errorEnvelope("bad_request", errDecode.Error()), nil
	}
	cfg := activeConfig()
	body := toCCRequest(chat, newUUID())
	open, errOpen := openUpstreamStream(req.HostCallbackID, resolveAPIBase(creds.APIBase, cfg.APIBase), creds.APIKey, &body)
	if errOpen != nil {
		return executorFailureEnvelope(errOpen), nil
	}

	// Select the output encoder by the format the host asked this executor to
	// produce. Claude clients get raw Anthropic SSE frames (the host has no
	// chat→claude STREAM transformer, so passthrough of chat chunks would be
	// empty); everything else gets chat.completion.chunk.
	var encoder chunkEmitter
	var term terminator
	if isClaudeFormat(req.Format) {
		ce := newClaudeEncoder(body.Params.Model)
		encoder, term = ce, ce
	} else {
		oe := newStreamEncoder(body.Params.Model)
		encoder, term = oe, oe
	}

	// The host only consumes stream-bridge chunks AFTER this RPC returns, and
	// the bridge queue holds 16 — pumping synchronously here deadlocks on the
	// 17th chunk (rpc_client_stream.go: "Async streaming plugins can return
	// before they finish emitting chunks, so keep callbacks alive until the
	// stream ends"). Open the upstream synchronously so pre-stream failures
	// still carry HTTP status, then return and pump in the background.
	go pumpStreamAsync(req.StreamID, open.StreamID, cfg.IdleTimeout, encoder, term)

	return okEnvelope(executorStreamOpenResponse{
		Headers: map[string][]string{"content-type": {"text/event-stream"}},
	})
}

// pumpStreamAsync drains the upstream NDJSON stream and forwards encoded
// chunks to the host stream bridge until finish, error, or disconnect.
func pumpStreamAsync(outputStreamID, upstreamStreamID string, idleTimeout time.Duration, encoder chunkEmitter, term terminator) {
	defer hostHTTPStreamClose(upstreamStreamID)

	emitChunk := func(chunk []byte) error {
		return hostStreamEmit(outputStreamID, chunk)
	}

	disconnected := false
	pumpErr := pumpUpstream(upstreamStreamID, idleTimeout, func(event ccEvent) error {
		chunks, errEncode := encoder.emit(event)
		if errEncode != nil {
			return errEncode
		}
		for _, chunk := range chunks {
			if err := emitChunk(chunk); err != nil {
				if hostStreamGone(err) {
					disconnected = true
					return errStreamDisconnected
				}
				hostLog("warn", fmt.Sprintf("commandcode-go: stream emit failed: %v", err))
				return err
			}
		}
		return nil
	})

	if disconnected {
		// The client is gone: nothing left to deliver, just release upstream.
		hostStreamClose(outputStreamID, "")
		return
	}

	if pumpErr != nil {
		if !encoder.finished() {
			for _, chunk := range term.failStream(pumpErr.Error()) {
				_ = hostStreamEmit(outputStreamID, chunk)
			}
		}
		hostStreamClose(outputStreamID, "")
		return
	}
	if !encoder.finished() {
		// Upstream closed without a finish event: synthesize the terminator so
		// client SDKs never see a truncated stream shape.
		for _, chunk := range term.forceFinish() {
			_ = hostStreamEmit(outputStreamID, chunk)
		}
	}
	hostStreamClose(outputStreamID, "")
}

var errStreamDisconnected = fmt.Errorf("commandcode-go: client stream closed")

// executorFailureEnvelope maps an upstream error to the most faithful HTTP
// status: pass CC's own status through when we have one, otherwise 502.
func executorFailureEnvelope(err error) []byte {
	var failure *upstreamFailure
	if asFailure(err, &failure) {
		status := failure.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		return httpErrorEnvelope(status, "commandcode-go: "+failure.Message)
	}
	return httpErrorEnvelope(http.StatusBadGateway, "commandcode-go: "+err.Error())
}

func asFailure(err error, target **upstreamFailure) bool {
	for err != nil {
		if failure, ok := err.(*upstreamFailure); ok {
			*target = failure
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}

// handleCountTokens answers token counts with a conservative heuristic
// estimate: byte length of the text content divided by four, plus per-message
// framing overhead. The upstream offers no counting endpoint.
func handleCountTokens(request []byte) ([]byte, error) {
	var req executorRequest
	if err := decodeRequest(request, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	raw := req.Payload
	if len(raw) == 0 {
		raw = req.OriginalRequest
	}
	var chat chatRequest
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &chat)
	}
	var text strings.Builder
	for _, msg := range chat.Messages {
		text.Write(msg.Content)
		for _, call := range msg.ToolCalls {
			text.WriteString(call.Function.Name)
			text.WriteString(call.Function.Arguments)
		}
	}
	for _, tool := range chat.Tools {
		schema, _ := json.Marshal(tool.Function.Parameters)
		text.Write(schema)
		text.WriteString(tool.Function.Description)
		text.WriteString(tool.Function.Name)
	}
	estimate := utf8.RuneCountInString(text.String())/4 + len(chat.Messages)*8 + 16
	if estimate < 1 {
		estimate = 1
	}
	payload := map[string]any{
		"input_tokens":      estimate,
		"prompt_tokens":     estimate,
		"completion_tokens": 0,
		"total_tokens":      estimate,
	}
	return okEnvelope(executorResponse{
		Payload: marshalJSON(payload),
		Headers: map[string][]string{"content-type": {"application/json"}},
	})
}
