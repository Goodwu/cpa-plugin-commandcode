package main

import (
	"strings"
)

// authParseRequest mirrors pluginapi.AuthParseRequest on the wire.
type authParseRequest struct {
	Provider string            `json:"Provider"`
	Path     string            `json:"Path"`
	FileName string            `json:"FileName"`
	RawJSON  []byte            `json:"RawJSON"`
	Host     hostConfigSummary `json:"Host"`
}

type authParseResponse struct {
	Handled bool       `json:"Handled"`
	Auth    authData   `json:"Auth"`
	Auths   []authData `json:"Auths,omitempty"`
}

type authRefreshRequest struct {
	AuthID       string            `json:"AuthID"`
	AuthProvider string            `json:"AuthProvider"`
	StorageJSON  []byte            `json:"StorageJSON"`
	Metadata     map[string]any    `json:"Metadata"`
	Attributes   map[string]string `json:"Attributes"`
	Host         hostConfigSummary `json:"Host"`
}

type authRefreshResponse struct {
	Auth             authData `json:"Auth"`
	NextRefreshAfter string   `json:"NextRefreshAfter,omitempty"`
}

func handleAuthParse(request []byte) ([]byte, error) {
	var req authParseRequest
	if err := decodeRequest(request, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	if storageType(req.RawJSON) != authProviderID {
		return okEnvelope(authParseResponse{Handled: false})
	}
	storage, err := parseKeyStorage(req.RawJSON)
	if err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	if strings.TrimSpace(storage.APIKey) == "" {
		return errorEnvelope("bad_request", "commandcode-go: auth file has no api_key"), nil
	}
	fileName := strings.TrimSpace(req.FileName)
	if fileName == "" {
		fileName = credentialFileName(storage)
	}
	data, err := buildAuthData(storage, fileName)
	if err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	return okEnvelope(authParseResponse{Handled: true, Auth: *data})
}

// handleAuthRefresh keeps the credential unchanged: CommandCode API keys do
// not rotate. The host may still schedule refreshes; answering with the same
// storage and a fresh far-future window is the stable no-op.
func handleAuthRefresh(request []byte) ([]byte, error) {
	var req authRefreshRequest
	if err := decodeRequest(request, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	storage, err := parseKeyStorage(req.StorageJSON)
	if err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	fileName := strings.TrimSpace(req.AuthID)
	if fileName == "" || !strings.HasSuffix(fileName, ".json") {
		fileName = credentialFileName(storage)
	}
	data, err := buildAuthData(storage, fileName)
	if err != nil {
		return errorEnvelope("refresh_failed", err.Error()), nil
	}
	return okEnvelope(authRefreshResponse{Auth: *data, NextRefreshAfter: data.NextRefreshAfter})
}
