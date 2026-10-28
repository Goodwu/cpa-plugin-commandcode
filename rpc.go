package main

import (
	"encoding/json"
	"fmt"
)

// envelope mirrors the pluginabi JSON contract exchanged with the host.
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// rpcRegistration mirrors the host's plugin.register/reconfigure request.
type rpcRegistration struct {
	SchemaVersion uint32 `json:"schema_version"`
	ConfigYAML    []byte `json:"config_yaml"`
}

func okEnvelope(result any) ([]byte, error) {
	raw, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal plugin result: %w", errMarshal)
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// httpErrorEnvelope surfaces an upstream-facing failure with a concrete HTTP
// status so the host maps it onto the client response instead of a 500.
func httpErrorEnvelope(status int, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{
		Code:       "upstream_error",
		Message:    message,
		HTTPStatus: status,
	}})
	return raw
}

// hostConfigSummary mirrors pluginapi.HostConfigSummary on the wire.
type hostConfigSummary struct {
	AuthDir          string                  `json:"AuthDir"`
	ProxyURL         string                  `json:"ProxyURL"`
	ForceModelPrefix bool                    `json:"ForceModelPrefix"`
	OAuthModelAlias  map[string][]modelAlias `json:"OAuthModelAlias"`
	ExcludedModels   map[string][]string     `json:"ExcludedModels"`
}

type modelAlias struct {
	Name  string `json:"Name"`
	Alias string `json:"Alias"`
}

// pluginMetadata mirrors pluginapi.Metadata on the wire.
type pluginMetadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type configField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	EnumValues  []string `json:"EnumValues"`
	Description string   `json:"Description"`
}

// authData mirrors pluginapi.AuthData on the wire.
type authData struct {
	Provider         string            `json:"Provider"`
	ID               string            `json:"ID,omitempty"`
	FileName         string            `json:"FileName,omitempty"`
	Label            string            `json:"Label,omitempty"`
	Prefix           string            `json:"Prefix,omitempty"`
	ProxyURL         string            `json:"ProxyURL,omitempty"`
	Disabled         bool              `json:"Disabled,omitempty"`
	StorageJSON      []byte            `json:"StorageJSON,omitempty"`
	Metadata         map[string]any    `json:"Metadata,omitempty"`
	Attributes       map[string]string `json:"Attributes,omitempty"`
	NextRefreshAfter string            `json:"NextRefreshAfter,omitempty"`
}

// wireModelInfo mirrors pluginapi.ModelInfo on the wire.
type wireModelInfo struct {
	ID                         string               `json:"ID"`
	Object                     string               `json:"Object,omitempty"`
	Created                    int64                `json:"Created,omitempty"`
	OwnedBy                    string               `json:"OwnedBy,omitempty"`
	Type                       string               `json:"Type,omitempty"`
	DisplayName                string               `json:"DisplayName,omitempty"`
	Name                       string               `json:"Name,omitempty"`
	Version                    string               `json:"Version,omitempty"`
	Description                string               `json:"Description,omitempty"`
	InputTokenLimit            int64                `json:"InputTokenLimit,omitempty"`
	OutputTokenLimit           int64                `json:"OutputTokenLimit,omitempty"`
	SupportedGenerationMethods []string             `json:"SupportedGenerationMethods,omitempty"`
	ContextLength              int64                `json:"ContextLength,omitempty"`
	MaxCompletionTokens        int64                `json:"MaxCompletionTokens,omitempty"`
	SupportedParameters        []string             `json:"SupportedParameters,omitempty"`
	SupportedInputModalities   []string             `json:"SupportedInputModalities,omitempty"`
	SupportedOutputModalities  []string             `json:"SupportedOutputModalities,omitempty"`
	Thinking                   *wireThinkingSupport `json:"Thinking,omitempty"`
	UserDefined                bool                 `json:"UserDefined,omitempty"`
}

type wireThinkingSupport struct {
	Min            int      `json:"Min,omitempty"`
	Max            int      `json:"Max,omitempty"`
	ZeroAllowed    bool     `json:"ZeroAllowed,omitempty"`
	DynamicAllowed bool     `json:"DynamicAllowed,omitempty"`
	Levels         []string `json:"Levels,omitempty"`
}

// executorRequest mirrors pluginapi.ExecutorRequest plus the stream fields the
// host adds (rpc_schema.go rpcExecutorRequest). Field names are PascalCase:
// the embedded struct has no JSON tags.
type executorRequest struct {
	AuthID          string              `json:"AuthID"`
	AuthProvider    string              `json:"AuthProvider"`
	Model           string              `json:"Model"`
	Format          string              `json:"Format"`
	Stream          bool                `json:"Stream"`
	Alt             string              `json:"Alt"`
	Headers         map[string][]string `json:"Headers"`
	Query           map[string][]string `json:"Query"`
	OriginalRequest []byte              `json:"OriginalRequest"`
	SourceFormat    string              `json:"SourceFormat"`
	Payload         []byte              `json:"Payload"`
	Metadata        map[string]any      `json:"Metadata"`
	StorageJSON     []byte              `json:"StorageJSON"`
	AuthMetadata    map[string]any      `json:"AuthMetadata"`
	AuthAttributes  map[string]string   `json:"AuthAttributes"`
	StreamID        string              `json:"stream_id,omitempty"`
	HostCallbackID  string              `json:"host_callback_id,omitempty"`
}

// executorResponse mirrors pluginapi.ExecutorResponse on the wire.
type executorResponse struct {
	Payload  []byte              `json:"Payload"`
	Headers  map[string][]string `json:"Headers,omitempty"`
	Metadata map[string]any      `json:"Metadata,omitempty"`
}

// executorStreamOpenResponse mirrors rpcExecutorStreamResponse: the plugin has
// emitted everything through host.stream.emit, so it returns an empty body and
// only the stream headers.
type executorStreamOpenResponse struct {
	Headers map[string][]string `json:"headers,omitempty"`
}

func decodeRequest(request []byte, target any) error {
	if len(request) == 0 {
		return nil
	}
	if err := json.Unmarshal(request, target); err != nil {
		return fmt.Errorf("decode plugin request: %w", err)
	}
	return nil
}
