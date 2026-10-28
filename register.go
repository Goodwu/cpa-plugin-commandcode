package main

import (
	"strings"
)

const (
	// pluginID is the registry identity: plugins.configs.<pluginID>, the dylib
	// file name, and the auth file name prefix. Deliberately NOT "commandcode"
	// so this plugin coexists with the store's API-key provider plugin of that
	// id (which targets the /provider/v1 surface, not /alpha/generate).
	pluginID = "commandcode-go"
	// authProviderID is the provider string stamped on auth records and matched
	// by auth/model/executor identifiers. It defines the provider namespace the
	// host binds this plugin's executor to (providers without a native executor
	// get the plugin executor attached).
	authProviderID = "commandcode-go"
	// schemaVersion is the RPC contract version this plugin speaks.
	schemaVersion = 1
)

// pluginVersion is reported in registration metadata and file names. The
// Makefile overrides it at link time via -ldflags -X; the literal is a
// fallback for plain `go build` invocations.
var pluginVersion = "0.1.0"

type registrationResponse struct {
	SchemaVersion uint32         `json:"schema_version"`
	Metadata      pluginMetadata `json:"metadata"`
	Capabilities  struct {
		AuthProvider          bool     `json:"auth_provider"`
		ModelProvider         bool     `json:"model_provider"`
		Executor              bool     `json:"executor"`
		ExecutorModelScope    string   `json:"executor_model_scope"`
		ExecutorInputFormats  []string `json:"executor_input_formats,omitempty"`
		ExecutorOutputFormats []string `json:"executor_output_formats,omitempty"`
	} `json:"capabilities"`
}

func registrationPayload() registrationResponse {
	var resp registrationResponse
	resp.SchemaVersion = schemaVersion
	resp.Metadata = pluginMetadata{
		Name:             pluginID,
		Version:          pluginVersion,
		Author:           "wuweiwei1",
		GitHubRepository: "https://github.com/wuweiwei1/cpa-plugin-commandcode",
		ConfigFields: []configField{
			{Name: "api_base", Type: "string", Description: "Command Code API base URL (default https://api.commandcode.ai)."},
			{Name: "cli_version", Type: "string", Description: "Pinned x-command-code-version. Empty = auto-refresh from the npm registry."},
			{Name: "idle_timeout_ms", Type: "integer", Description: "Max ms between stream chunks; 0 disables (default 120000)."},
			{Name: "max_retries", Type: "integer", Description: "Upstream retries before the stream starts (default 2)."},
			{Name: "project_slug", Type: "string", Description: "x-project-slug header value (default derived from the host working directory)."},
		},
	}
	resp.Capabilities.AuthProvider = true
	resp.Capabilities.ModelProvider = true
	resp.Capabilities.Executor = true
	resp.Capabilities.ExecutorModelScope = "both"
	resp.Capabilities.ExecutorInputFormats = []string{"chat-completions"}
	// Anthropic clients need the plugin to emit Claude SSE natively: the host
	// registers no chat-completions→anthropic STREAM response transformer, so
	// declaring "anthropic" makes selectExecutorOutputFormat choose passthrough
	// for Claude requests instead of an empty in-host translation.
	resp.Capabilities.ExecutorOutputFormats = []string{"chat-completions", "anthropic"}
	return resp
}

// dispatch routes a host RPC method to its handler.
func dispatch(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req rpcRegistration
		if err := decodeRequest(request, &req); err != nil {
			return errorEnvelope("bad_request", err.Error()), nil
		}
		setPluginConfig(req.ConfigYAML)
		return okEnvelope(registrationPayload())
	case "plugin.shutdown":
		return okEnvelope(struct{}{})
	case "auth.identifier":
		return okEnvelope(map[string]string{"identifier": authProviderID})
	case "auth.parse":
		return handleAuthParse(request)
	case "auth.login.start":
		return errorEnvelope("no_interactive_login",
			"commandcode-go: provide the API key from https://commandcode.ai/settings in an auth file; interactive login is not supported"), nil
	case "auth.login.poll":
		return errorEnvelope("no_interactive_login",
			"commandcode-go: provide the API key from https://commandcode.ai/settings in an auth file; interactive login is not supported"), nil
	case "auth.refresh":
		return handleAuthRefresh(request)
	case "model.static":
		return handleStaticModels(request)
	case "model.for_auth":
		return handleModelsForAuth(request)
	case "executor.identifier":
		return okEnvelope(map[string]string{"identifier": authProviderID})
	case "executor.execute":
		return handleExecute(request)
	case "executor.execute_stream":
		return handleExecuteStream(request)
	case "executor.count_tokens":
		return handleCountTokens(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+strings.TrimSpace(method)), nil
	}
}
