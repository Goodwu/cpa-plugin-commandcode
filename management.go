package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
)

//go:embed web/status.html
var statusPageHTML []byte

// managementRequest mirrors pluginapi.ManagementRequest on the wire.
type managementRequest struct {
	Method  string              `json:"Method"`
	Path    string              `json:"Path"`
	Headers map[string][]string `json:"Headers"`
	Query   map[string][]string `json:"Query"`
	Body    []byte              `json:"Body"`
}

// managementResponse mirrors pluginapi.ManagementResponse on the wire.
type managementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

type managementResource struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description,omitempty"`
}

type managementRegistration struct {
	Routes    []managementRoute    `json:"routes,omitempty"`
	Resources []managementResource `json:"resources,omitempty"`
}

const (
	statusResourcePath = "/status"
	statusRoutePath    = "/plugins/" + pluginID + "/status"
	resetRoutePath     = "/plugins/" + pluginID + "/reset-stats"
	resourcePrefix     = "/v0/resource/plugins/" + pluginID
	managementPrefix   = "/v0/management"
)

func handleManagementRegister(_ []byte) ([]byte, error) {
	return okEnvelope(managementRegistration{
		Resources: []managementResource{{
			Path:        statusResourcePath,
			Menu:        "CommandCode",
			Description: "CommandCode plugin status, quota windows and request statistics.",
		}},
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: statusRoutePath, Description: "Status snapshot as JSON."},
			{Method: http.MethodPost, Path: resetRoutePath, Description: "Clear locally collected statistics."},
		},
	})
}

func handleManagementHandle(request []byte) ([]byte, error) {
	var req managementRequest
	if err := decodeRequest(request, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	path := strings.TrimSpace(req.Path)

	switch {
	case strings.HasSuffix(path, resourcePrefix+statusResourcePath):
		return okEnvelope(managementResponse{
			StatusCode: http.StatusOK,
			Headers:    pageHeaders(),
			Body:       statusPageHTML,
		})
	case strings.HasSuffix(path, managementPrefix+statusRoutePath):
		if req.Method != http.MethodGet {
			return okEnvelope(jsonResponse(http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"}))
		}
		return okEnvelope(jsonResponse(http.StatusOK, statusPayload(req.Headers)))
	case strings.HasSuffix(path, managementPrefix+resetRoutePath):
		if req.Method != http.MethodPost {
			return okEnvelope(jsonResponse(http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"}))
		}
		pluginState.reset()
		return okEnvelope(jsonResponse(http.StatusOK, map[string]any{"ok": true}))
	default:
		return okEnvelope(jsonResponse(http.StatusNotFound, map[string]string{"error": "not found"}))
	}
}

// statusPayload assembles the JSON snapshot the page renders.
func statusPayload(headers map[string][]string) map[string]any {
	snapshot := pluginState.snapshot()
	cfg := activeConfig()
	ids := catalogIDs()

	cliVersion := activeConfig().CLIVersion
	if cliVersion == "" {
		cliVersion = "auto (npm refresh)"
	}

	authList := make([]map[string]any, 0, len(snapshot.Auths))
	for _, auth := range snapshot.Auths {
		entry := map[string]any{
			"auth_id":      auth.AuthID,
			"label":        auth.Label,
			"prefix":       auth.Prefix,
			"disabled":     auth.Disabled,
			"requests_5h":  auth.Requests5h,
			"last_request": auth.LastRequestAt,
		}
		if quotaEntry, ok := lookupQuotaCache(auth.AuthID); ok {
			entry["quota"] = quotaEntry
		}
		if auth.RateLimit != nil {
			entry["rate_limit_seen"] = auth.RateLimit.SeenAt.Format("2006-01-02T15:04:05Z07:00")
		}
		authList = append(authList, entry)
	}

	return map[string]any{
		"plugin": map[string]any{
			"id":           pluginID,
			"version":      pluginVersion,
			"schema":       schemaVersion,
			"uptime_sec":   snapshot.UptimeSec,
			"started_at":   snapshot.StartedAt,
			"host_version": http.Header(headers).Get("X-Cpa-Version"),
		},
		"upstream": map[string]any{
			"api_base":        cfg.APIBase,
			"cli_version":     cliVersion,
			"idle_timeout_ms": int64(cfg.IdleTimeout / 1e6),
			"max_retries":     cfg.MaxRetries,
		},
		"auths":  authList,
		"totals": snapshot.Totals,
		"models": map[string]any{
			"total":  len(ids),
			"sample": firstN(ids, 12),
		},
		"recent": snapshot.Recent,
	}
}

func firstN(values []string, n int) []string {
	if len(values) <= n {
		return values
	}
	return values[:n]
}

func jsonResponse(status int, payload any) managementResponse {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		raw = []byte(`{"error":"marshal failure"}`)
	}
	return managementResponse{
		StatusCode: status,
		Headers: map[string][]string{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		Body: raw,
	}
}

func pageHeaders() map[string][]string {
	return map[string][]string{
		"Content-Type": {"text/html; charset=utf-8"},
		// The page uses inline styles/scripts only; it never loads third-party
		// assets, which would inherit same-origin access to management storage.
		"Content-Security-Policy": {
			"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:",
		},
		"Cache-Control": {"no-store"},
	}
}
