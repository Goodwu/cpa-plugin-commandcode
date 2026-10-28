package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// The official CLI ships frequently and the CC server rejects requests whose
// x-command-code-version looks stale, so the plugin refreshes the advertised
// version from the npm registry at most once a day and falls back to the
// compiled-in constant while offline.
const cliVersionRefreshInterval = 24 * time.Hour

const npmLatestURL = "https://registry.npmjs.org/command-code/latest"

var (
	versionMu        sync.Mutex
	cachedVersion    string
	versionFetchedAt time.Time
)

// currentCLIVersion returns the version to stamp into the CLI fingerprint
// headers. Pinned config wins; then the cached npm value; then the fallback.
func currentCLIVersion(callbackID string) string {
	if pinned := activeConfig().CLIVersion; pinned != "" {
		return pinned
	}
	versionMu.Lock()
	defer versionMu.Unlock()
	if cachedVersion != "" && time.Since(versionFetchedAt) < cliVersionRefreshInterval {
		return cachedVersion
	}
	version, errFetch := fetchLatestCLIVersion(callbackID)
	if errFetch == nil && version != "" {
		cachedVersion = version
		versionFetchedAt = time.Now()
		return cachedVersion
	}
	if cachedVersion != "" {
		return cachedVersion
	}
	// Failed lookups still get a full interval of patience: the fallback
	// constant beats re-fetching on every request while the registry is down.
	versionFetchedAt = time.Now()
	return defaultFallbackCLIVersion
}

func fetchLatestCLIVersion(callbackID string) (string, error) {
	response, errCall := hostHTTPDo(callbackID, hostHTTPRequestMsg{
		Method:  "GET",
		URL:     npmLatestURL,
		Headers: map[string][]string{"Accept": {"application/json"}},
	})
	if errCall != nil {
		return "", errCall
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", newStatusError(response.StatusCode, "npm registry lookup failed")
	}
	var payload struct {
		Version string `json:"version"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		return "", errUnmarshal
	}
	if strings.TrimSpace(payload.Version) == "" {
		return "", newStatusError(0, "npm registry response has no version")
	}
	return strings.TrimSpace(payload.Version), nil
}
