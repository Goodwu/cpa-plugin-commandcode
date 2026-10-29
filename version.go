package main

import (
	"encoding/json"
	"io"
	"net/http"
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
	result := cachedVersion
	if result == "" {
		result = defaultFallbackCLIVersion
	}
	if time.Since(versionFetchedAt) >= cliVersionRefreshInterval {
		versionFetchedAt = time.Now()
		pluginRuntime.Lock()
		if !pluginRuntime.closing {
			pluginRuntime.wg.Add(1)
			go func() {
				defer pluginRuntime.wg.Done()
				version, err := lookupCLIVersion("")
				versionMu.Lock()
				defer versionMu.Unlock()
				if err == nil && version != "" {
					cachedVersion = version
					versionFetchedAt = time.Now()
				} else {
					versionFetchedAt = time.Now().Add(-cliVersionRefreshInterval + 5*time.Minute)
				}
			}()
		}
		pluginRuntime.Unlock()
	}
	return result
}

var lookupCLIVersion = fetchLatestCLIVersion

func fetchLatestCLIVersion(callbackID string) (string, error) {
	// Version discovery is public metadata and has an independent deadline;
	// it must not inherit a generation callback that may already have ended.
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(npmLatestURL)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", newStatusError(response.StatusCode, "npm registry lookup failed")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return "", err
	}
	var payload struct {
		Version string `json:"version"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return "", errUnmarshal
	}
	if strings.TrimSpace(payload.Version) == "" {
		return "", newStatusError(0, "npm registry response has no version")
	}
	return strings.TrimSpace(payload.Version), nil
}
