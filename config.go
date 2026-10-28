package main

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultAPIBase            = "https://api.commandcode.ai"
	defaultIdleTimeout        = 120 * time.Second
	defaultMaxRetries         = 2
	defaultRetryBackoffMs     = 500
	defaultFallbackCLIVersion = "0.40.3"
)

// pluginConfig holds the plugins.configs.commandcode-go block. The host hands
// it over as raw YAML at register/reconfigure; only flat scalar keys are
// supported (matching the host's documented config surface), so a line scan
// avoids a YAML dependency in the module.
type pluginConfig struct {
	APIBase      string
	CLIVersion   string
	IdleTimeout  time.Duration
	MaxRetries   int
	RetryBackoff time.Duration
	ProjectSlug  string
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		APIBase:      defaultAPIBase,
		IdleTimeout:  defaultIdleTimeout,
		MaxRetries:   defaultMaxRetries,
		RetryBackoff: time.Duration(defaultRetryBackoffMs) * time.Millisecond,
	}
}

var (
	configMu      sync.RWMutex
	currentConfig = defaultPluginConfig()
)

func setPluginConfig(raw []byte) {
	cfg := defaultPluginConfig()
	if value := strings.TrimSpace(yamlScalar(raw, "api_base")); value != "" {
		cfg.APIBase = strings.TrimRight(value, "/")
	}
	if value := strings.TrimSpace(yamlScalar(raw, "cli_version")); value != "" {
		cfg.CLIVersion = value
	}
	if value, ok := yamlInt(raw, "idle_timeout_ms"); ok && value >= 0 {
		cfg.IdleTimeout = time.Duration(value) * time.Millisecond
	}
	if value, ok := yamlInt(raw, "max_retries"); ok && value >= 0 {
		cfg.MaxRetries = int(value)
	}
	if value, ok := yamlInt(raw, "retry_backoff_ms"); ok && value > 0 {
		cfg.RetryBackoff = time.Duration(value) * time.Millisecond
	}
	if value := strings.TrimSpace(yamlScalar(raw, "project_slug")); value != "" {
		cfg.ProjectSlug = value
	}
	configMu.Lock()
	currentConfig = cfg
	configMu.Unlock()
}

func activeConfig() pluginConfig {
	configMu.RLock()
	defer configMu.RUnlock()
	return currentConfig
}

// yamlScalar extracts the scalar value of a top-level "key: value" line.
func yamlScalar(raw []byte, key string) string {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	prefix := key + ":"
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if rest == "" {
			continue
		}
		if idx := strings.Index(rest, " #"); idx >= 0 {
			rest = rest[:idx]
		}
		rest = strings.TrimSpace(rest)
		if len(rest) >= 2 {
			if (rest[0] == '"' && rest[len(rest)-1] == '"') ||
				(rest[0] == '\'' && rest[len(rest)-1] == '\'') {
				rest = rest[1 : len(rest)-1]
			}
		}
		return strings.TrimSpace(rest)
	}
	return ""
}

func yamlInt(raw []byte, key string) (int64, bool) {
	value := strings.TrimSpace(yamlScalar(raw, key))
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}
