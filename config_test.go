package main

import (
	"testing"
	"time"
)

func TestYamlScalarAndInt(t *testing.T) {
	raw := []byte("enabled: true\npriority: 1\napi_base: \"https://proxy.internal\"\ncli_version: 0.41.0 # pinned\nidle_timeout_ms: 30000\nmax_retries: 0\n")
	if got := yamlScalar(raw, "api_base"); got != "https://proxy.internal" {
		t.Fatalf("api_base = %q", got)
	}
	if got := yamlScalar(raw, "cli_version"); got != "0.41.0" {
		t.Fatalf("cli_version with comment = %q", got)
	}
	if value, ok := yamlInt(raw, "idle_timeout_ms"); !ok || value != 30000 {
		t.Fatalf("idle = %d %v", value, ok)
	}
	// "enabled: true" must not poison integer reads.
	if _, ok := yamlInt(raw, "enabled"); ok {
		t.Fatal("non-numeric value must not parse as int")
	}
}

func TestSetPluginConfigAppliesDefaults(t *testing.T) {
	defer setPluginConfig(nil)
	setPluginConfig([]byte("api_base: https://alt.example/\nmax_retries: 5\nidle_timeout_ms: 0\n"))
	cfg := activeConfig()
	if cfg.APIBase != "https://alt.example" {
		t.Fatalf("api base trailing slash: %q", cfg.APIBase)
	}
	if cfg.MaxRetries != 5 {
		t.Fatalf("retries = %d", cfg.MaxRetries)
	}
	if cfg.IdleTimeout != 0 {
		t.Fatalf("idle 0 must disable the timer, got %v", cfg.IdleTimeout)
	}
	setPluginConfig(nil)
	cfg = activeConfig()
	if cfg.APIBase != defaultAPIBase || cfg.MaxRetries != defaultMaxRetries || cfg.IdleTimeout != defaultIdleTimeout {
		t.Fatalf("defaults not restored: %+v", cfg)
	}
}

func TestResolveAPIBasePrecedence(t *testing.T) {
	if got := resolveAPIBase(" https://per-auth.example/ ", "https://cfg.example"); got != "https://per-auth.example" {
		t.Fatalf("auth override wins, got %q", got)
	}
	if got := resolveAPIBase("", "https://cfg.example"); got != "https://cfg.example" {
		t.Fatalf("config fallback, got %q", got)
	}
}

func TestCLIVersionPinning(t *testing.T) {
	original := currentConfig
	defer func() {
		configMu.Lock()
		currentConfig = original
		configMu.Unlock()
	}()
	configMu.Lock()
	currentConfig.CLIVersion = "1.2.3"
	configMu.Unlock()
	if got := currentCLIVersion(""); got != "1.2.3" {
		t.Fatalf("pinned version ignored: %q", got)
	}
	configMu.Lock()
	currentConfig.CLIVersion = ""
	cachedVersion = ""
	versionFetchedAt = time.Time{}
	configMu.Unlock()
	// Without the host bridge the fetch fails; must fall back to the constant.
	if got := currentCLIVersion(""); got != defaultFallbackCLIVersion {
		t.Fatalf("fallback version: %q", got)
	}
}
