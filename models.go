package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// staticModelRequest mirrors pluginapi.StaticModelRequest on the wire.
type staticModelRequest struct {
	Plugin pluginMetadata    `json:"Plugin"`
	Host   hostConfigSummary `json:"Host"`
}

// authModelRequest mirrors pluginapi.AuthModelRequest on the wire.
type authModelRequest struct {
	Plugin         pluginMetadata    `json:"Plugin"`
	AuthID         string            `json:"AuthID"`
	AuthProvider   string            `json:"AuthProvider"`
	StorageJSON    []byte            `json:"StorageJSON"`
	Metadata       map[string]any    `json:"Metadata"`
	Attributes     map[string]string `json:"Attributes"`
	Host           hostConfigSummary `json:"Host"`
	HostCallbackID string            `json:"host_callback_id,omitempty"`
}

type modelResponse struct {
	Provider string          `json:"Provider"`
	Models   []wireModelInfo `json:"Models"`
}

// remoteModel mirrors one item of the CC provider API /models response.
type remoteModel struct {
	ID              string `json:"id"`
	Model           string `json:"model"`
	Object          string `json:"object"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	ContextLength   int    `json:"context_length"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

// handleStaticModels serves the builtin catalog so models are usable even
// before any auth exists; per-auth discovery may merge more ids in.
func handleStaticModels(_ []byte) ([]byte, error) {
	return okEnvelope(modelResponse{Provider: authProviderID, Models: catalogModels()})
}

// handleModelsForAuth refreshes the catalog from the provider API (TTL-guarded,
// deduplicated) when it is stale, then returns the merged model list. Upstream
// failures are non-fatal by design: the static catalog is the offline fallback
// and the reference proxy treats the API as an enhancement, not the source.
func handleModelsForAuth(request []byte) ([]byte, error) {
	var req authModelRequest
	if err := decodeRequest(request, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	if catalogNeedsRefresh() {
		storage, errParse := parseKeyStorage(req.StorageJSON)
		if errParse == nil && strings.TrimSpace(storage.APIKey) != "" {
			apiBase := resolveAPIBase(storage.BaseURL, activeConfig().APIBase)
			if models, errFetch := fetchRemoteModels(req.HostCallbackID, apiBase, storage.APIKey); errFetch == nil && len(models) > 0 {
				applyRemoteCatalog(models)
			}
		}
	}
	return okEnvelope(modelResponse{Provider: authProviderID, Models: catalogModels()})
}

func catalogModels() []wireModelInfo {
	ids := catalogIDs()
	models := make([]wireModelInfo, 0, len(ids))
	for _, id := range ids {
		info := wireModelInfo{
			ID:                         id,
			Object:                     "model",
			OwnedBy:                    authProviderID,
			Type:                       authProviderID,
			DisplayName:                displayNameFor(id),
			Name:                       displayNameFor(id),
			SupportedGenerationMethods: []string{"chat"},
			ContextLength:              contextLengthFor(id),
		}
		if maxOut, ok := staticCatalog.MaxOutputTokens[id]; ok && maxOut > 0 {
			info.MaxCompletionTokens = maxOut
		}
		if efforts, ok := staticCatalog.ReasoningEfforts[id]; ok && len(efforts) > 0 {
			info.Thinking = &wireThinkingSupport{Levels: append([]string(nil), efforts...)}
		}
		if strings.Contains(strings.ToLower(id), "vision") {
			info.SupportedInputModalities = []string{"text", "image"}
		}
		models = append(models, info)
	}
	return models
}

// fetchRemoteModels pulls the live model list from {apiBase}/provider/v1/models.
func fetchRemoteModels(callbackID, apiBase, apiKey string) ([]remoteModel, error) {
	result, errCall := hostCall("host.http.do", hostHTTPRequestMsg{
		Method: http.MethodGet,
		URL:    apiBase + "/provider/v1/models",
		Headers: map[string][]string{
			"Accept":        {"application/json"},
			"Authorization": {"Bearer " + apiKey},
		},
		HostCallbackID: callbackID,
	})
	if errCall != nil {
		return nil, errCall
	}
	var response hostHTTPResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, newStatusError(response.StatusCode, sanitizeErrorText(string(response.Body), apiKey))
	}
	var payload struct {
		Data []remoteModel `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return payload.Data, nil
}
