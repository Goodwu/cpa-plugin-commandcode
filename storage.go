package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// keyStorage mirrors the plugin-owned auth file shape:
//
//	{"type":"commandcode-go","api_key":"...","label":"main","base_url":"...","prefix":"..."}
//
// The host stamps "type" with the provider key on save, which is what
// auth.parse matches on.
type keyStorage struct {
	Type    string `json:"type"`
	APIKey  string `json:"api_key"`
	Label   string `json:"label,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	Prefix  string `json:"prefix,omitempty"`
	// BillingAPIKey optionally supplies the CLI login key, used only for
	// read-only billing/quota calls. A provider/API key can run inference but
	// is rejected (401) by /alpha/billing/*, while the CLI login key from
	// ~/.commandcode/auth.json can read usage.
	BillingAPIKey string `json:"billing_api_key,omitempty"`
}

func parseKeyStorage(raw []byte) (*keyStorage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("commandcode-go storage: payload is empty")
	}
	var storage keyStorage
	if err := json.Unmarshal([]byte(trimmed), &storage); err != nil {
		return nil, fmt.Errorf("commandcode-go storage: parse payload: %w", err)
	}
	return &storage, nil
}

// storageType reports the "type" field of a raw auth payload without full parsing.
func storageType(raw []byte) string {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return ""
	}
	return strings.TrimSpace(header.Type)
}

// credentialFileName names the persisted auth file. The label wins when set;
// otherwise a short key hash keeps multi-key pools distinct and stable across
// reloads (a timestamp would re-create the auth entry on every parse).
func credentialFileName(storage *keyStorage) string {
	if label := sanitizeFileSegment(storage.Label); label != "" {
		return fmt.Sprintf("%s-%s.json", pluginID, label)
	}
	digest := sha256.Sum256([]byte(storage.APIKey))
	return fmt.Sprintf("%s-%s.json", pluginID, hex.EncodeToString(digest[:4]))
}

func sanitizeFileSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '@', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// buildAuthData assembles the wire AuthData for a credential. The API key
// lives in StorageJSON only: the executor reads it from the request
// (StorageJSON) and uses it transiently, so nothing copies the secret into
// host-visible attribute maps where it could leak into logs.
func buildAuthData(storage *keyStorage, fileName string) (*authData, error) {
	if storage == nil {
		return nil, fmt.Errorf("commandcode-go: storage is nil")
	}
	raw, err := json.Marshal(storage)
	if err != nil {
		return nil, fmt.Errorf("commandcode-go: encode storage: %w", err)
	}
	label := strings.TrimSpace(storage.Label)
	if label == "" {
		digest := sha256.Sum256([]byte(storage.APIKey))
		label = "commandcode-go-" + hex.EncodeToString(digest[:4])
	}
	prefix := strings.TrimSpace(storage.Prefix)
	baseURL := strings.TrimRight(strings.TrimSpace(storage.BaseURL), "/")
	attributes := map[string]string{
		"auth_kind": "api_key",
	}
	if baseURL != "" {
		attributes["base_url"] = baseURL
	}
	metadata := map[string]any{
		"auth_kind": "api_key",
	}
	if prefix != "" {
		metadata["prefix"] = prefix
		attributes["prefix"] = prefix
	}
	return &authData{
		Provider:    authProviderID,
		ID:          fileName,
		FileName:    fileName,
		Label:       label,
		Prefix:      prefix,
		StorageJSON: raw,
		Metadata:    metadata,
		Attributes:  attributes,
		// Static API keys never expire; schedule the (no-op) refresh far out
		// so the host does not spin on it.
		NextRefreshAfter: time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339),
	}, nil
}
