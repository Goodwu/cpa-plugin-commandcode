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

// remoteModel mirrors one item of the CC provider API /models response. The
// client view adds the plan-gating fields (category/minimumPlan/hidden); the
// plain view leaves them zero, which the lock evaluation reads as "no gating
// info available" and never locks.
type remoteModel struct {
	ID              string  `json:"id"`
	Model           string  `json:"model"`
	Object          string  `json:"object"`
	Name            string  `json:"name"`
	Description     string  `json:"description"`
	ContextLength   int     `json:"context_length"`
	MaxOutputTokens int     `json:"max_output_tokens"`
	Category        string  `json:"category"`
	MinimumPlan     planRef `json:"minimumPlan"`
	Hidden          bool    `json:"hidden"`
}

// remoteCatalog is one /models payload: the entries plus the client view's
// refreshAfterSeconds hint (0 when absent, e.g. the plain-view fallback).
type remoteCatalog struct {
	Models             []remoteModel
	RefreshAfterSecond int
}

// handleStaticModels serves the builtin catalog so models are usable even
// before any auth exists; per-auth discovery may merge more ids in.
func handleStaticModels(_ []byte) ([]byte, error) {
	return okEnvelope(modelResponse{Provider: authProviderID, Models: catalogModels()})
}

// handleModelsForAuth refreshes the catalog from the provider API (TTL-guarded,
// deduplicated) when it is stale, then returns the merged model list. The
// client view also carries plan-gating fields, so a successful refresh pairs
// with the credential's billing snapshot (plan + credits) to lock models the
// subscription cannot use; any billing failure simply skips locking. Upstream
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
			if catalog, errFetch := fetchRemoteModels(req.HostCallbackID, apiBase, storage.APIKey); errFetch == nil && len(catalog.Models) > 0 {
				planCtx := fetchPlanContext(req.HostCallbackID, apiBase, storage.APIKey, req.AuthID)
				applyRemoteCatalog(catalog.Models, planCtx, catalog.RefreshAfterSecond)
			}
		}
	}
	return okEnvelope(modelResponse{Provider: authProviderID, Models: catalogModels()})
}

// fetchPlanContext builds the lock-evaluation snapshot from the billing
// endpoints the CLI itself uses. It only enables filtering when both calls
// succeed and the subscription names a known plan — mirroring the CLI, which
// leaves every model selectable when its billing context is missing.
func fetchPlanContext(callbackID, apiBase, apiKey, authID string) planContext {
	cliVersion := currentCLIVersion(callbackID)
	credits, status, errCredits := getCredits(callbackID, apiBase, apiKey, cliVersion)
	if errCredits != nil || status != http.StatusOK || credits == nil {
		return planContext{}
	}
	plan := getPlan(callbackID, apiBase, apiKey, cliVersion, authID)
	if _, known := planRanks[plan.planID]; !known {
		return planContext{}
	}
	return planContext{
		filter:        true,
		planID:        plan.planID,
		creditsUnlock: credits.Credits.PurchasedCredits > 0 || credits.Credits.FreeCredits > 0,
	}
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
// The client view (?view=client) is preferred: it is a superset of the plain
// view and adds the plan-gating fields plus the refreshAfterSeconds hint. When
// it fails (e.g. a key that only speaks the plain provider surface) the plain
// view is tried so the model list still refreshes, just without lock info.
func fetchRemoteModels(callbackID, apiBase, apiKey string) (*remoteCatalog, error) {
	if catalog, errClient := fetchModelsView(callbackID, apiBase, apiKey, "client"); errClient == nil {
		return catalog, nil
	}
	return fetchModelsView(callbackID, apiBase, apiKey, "")
}

// fetchModelsView fetches one /provider/v1/models flavor; view "" is the plain
// provider listing (no gating fields, no refresh hint).
func fetchModelsView(callbackID, apiBase, apiKey, view string) (*remoteCatalog, error) {
	url := apiBase + "/provider/v1/models"
	if view != "" {
		url += "?view=" + view
	}
	result, errCall := hostCall("host.http.do", hostHTTPRequestMsg{
		Method: http.MethodGet,
		URL:    url,
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
		Data               []remoteModel `json:"data"`
		RefreshAfterSecond int           `json:"refreshAfterSeconds"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return &remoteCatalog{Models: payload.Data, RefreshAfterSecond: payload.RefreshAfterSecond}, nil
}
