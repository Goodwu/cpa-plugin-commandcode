package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAcceptEncodingIsNotDeclared(t *testing.T) {
	body := &ccRequestBody{ThreadID: "t", Config: map[string]any{"workingDir": "/tmp/app"}}
	headers := buildUpstreamHeaders("k", "1.0.0", body)
	// The host HTTP bridge does not decompress error bodies; requesting only
	// identity encoding (by omitting the header) keeps upstream 4xx readable.
	if _, exists := headers["Accept-Encoding"]; exists {
		t.Fatal("Accept-Encoding must not be sent (raw bridge cannot decode br/gzip error bodies)")
	}
}

func TestStripModelPrefix(t *testing.T) {
	cases := []struct {
		model      string
		authPrefix string
		want       string
	}{
		{"cc/deepseek/deepseek-v4-flash", "cc", "deepseek/deepseek-v4-flash"},
		{"cc/zai-org/GLM-5.2", "cc", "zai-org/GLM-5.2"},
		{"commandcode-go/moonshotai/Kimi-K3", "", "moonshotai/Kimi-K3"},
		{"commandcode-go/cc/xai/grok-4.5", "cc", "xai/grok-4.5"},
		{"cc/cc/deepseek/deepseek-v4-flash", "cc", "deepseek/deepseek-v4-flash"},
		{"deepseek/deepseek-v4-flash", "cc", "deepseek/deepseek-v4-flash"},
		{"glm5.2", "cc", "glm5.2"}, // bare alias survives prefix stripping and resolves via catalog
		{"cc/", "cc", "cc/"},       // stripping would empty the id: keep as-is
		{"unknown/vendor-model", "", "unknown/vendor-model"},
	}
	for _, tc := range cases {
		if got := stripModelPrefix(tc.model, tc.authPrefix); got != tc.want {
			t.Fatalf("stripModelPrefix(%q, %q) = %q, want %q", tc.model, tc.authPrefix, got, tc.want)
		}
	}
}

func TestSessionThreadID(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string][]string
		want    string
	}{
		{"x-session-id", map[string][]string{"X-Session-Id": {"sess-abcdef12"}}, "sess-abcdef12"},
		{"claude code", map[string][]string{"X-Claude-Code-Session-Id": {"claude-session-42"}}, "claude-session-42"},
		{"conversation", map[string][]string{"Conversation-Id": {"conv_12345678"}}, "conv_12345678"},
		{"precedence", map[string][]string{
			"X-Session-Id":    {"first-priority"},
			"Conversation-Id": {"second-priority"},
			"Session-Id":      {"third-priority"},
		}, "first-priority"},
		{"too short", map[string][]string{"X-Session-Id": {"abc"}}, ""},
		{"illegal chars", map[string][]string{"X-Session-Id": {"bad session id!"}}, ""},
		{"too long", map[string][]string{"X-Session-Id": {strings.Repeat("a", 200)}}, ""},
		{"missing", nil, ""},
	}
	for _, tc := range cases {
		got := sessionThreadID(executorRequest{Headers: tc.headers})
		if tc.want == "" {
			if !isUUID(got) {
				t.Fatalf("%s: expected UUID fallback, got %q", tc.name, got)
			}
			continue
		}
		if got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				return false
			}
		}
	}
	return true
}

func TestSanitizeSessionID(t *testing.T) {
	if id, ok := sanitizeSessionID("  abcdefgh-1234  "); !ok || id != "abcdefgh-1234" {
		t.Fatalf("trim/normalize: %q %v", id, ok)
	}
	for _, bad := range []string{"", "short", "has space", "bad/slash", "x\ny", strings.Repeat("z", 129)} {
		if _, ok := sanitizeSessionID(bad); ok {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

func TestLockedModelEnvelope(t *testing.T) {
	defer resetCatalog()
	defer setPluginConfig(nil)
	applyRemoteCatalog([]remoteModel{
		{ID: "vendor/gated-model", Category: "opensource", MinimumPlan: planRefFor("individual-goat")},
	}, planContext{filter: true, planID: "individual-go-v1"}, 900)

	env := lockedModelEnvelope("vendor/gated-model")
	if env == nil {
		t.Fatal("locked model must produce a rejection envelope")
	}
	var parsed envelope
	if err := json.Unmarshal(env, &parsed); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if parsed.OK || parsed.Error == nil {
		t.Fatalf("expected error envelope, got %s", env)
	}
	if parsed.Error.Code != "model_not_in_plan" || parsed.Error.HTTPStatus != http.StatusForbidden {
		t.Fatalf("code/status = %q/%d, want model_not_in_plan/403", parsed.Error.Code, parsed.Error.HTTPStatus)
	}
	if !strings.Contains(parsed.Error.Message, "GOAT") || !strings.Contains(parsed.Error.Message, "individual-goat") {
		t.Fatalf("message should name the required plan, got %q", parsed.Error.Message)
	}

	// A bare name equal to a locked id's last segment must also be rejected.
	bare := lockedModelEnvelope("gated-model")
	if bare == nil {
		t.Fatal("bare name of a locked model must be rejected")
	}
	if err := json.Unmarshal(bare, &parsed); err != nil || parsed.Error == nil {
		t.Fatalf("bare-name rejection envelope: %v %s", err, bare)
	}
	if !strings.Contains(parsed.Error.Message, "GOAT") {
		t.Fatalf("bare-name message should name the required plan, got %q", parsed.Error.Message)
	}

	if env := lockedModelEnvelope("vendor/other-model"); env != nil {
		t.Fatalf("unlocked model must pass, got %s", env)
	}

	setPluginConfig([]byte("filter_locked_models: false\n"))
	if env := lockedModelEnvelope("vendor/gated-model"); env != nil {
		t.Fatalf("switched-off filter must pass locked models, got %s", env)
	}
}

func TestHandleExecuteRejectsLockedModel(t *testing.T) {
	defer resetCatalog()
	defer setPluginConfig(nil)
	applyRemoteCatalog([]remoteModel{
		{ID: "vendor/gated-model", Category: "opensource", MinimumPlan: planRefFor("individual-goat")},
	}, planContext{filter: true, planID: "individual-go-v1"}, 900)

	oldCall := hostCall
	defer func() { hostCall = oldCall }()
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		t.Fatalf("locked model must not reach the upstream, got host call %s", method)
		return nil, nil
	}

	payload, _ := json.Marshal(map[string]any{"model": "vendor/gated-model", "messages": []map[string]any{{"role": "user", "content": "hi"}}, "stream": false})
	request, _ := json.Marshal(executorRequest{
		AuthID:         "auth-1",
		StorageJSON:    []byte(`{"type":"commandcode-go","api_key":"sk-test"}`),
		Payload:        payload,
		HostCallbackID: "cb-1",
	})
	response, errRPC := handleExecute(request)
	if errRPC != nil {
		t.Fatalf("handleExecute rpc error: %v", errRPC)
	}
	var parsed envelope
	if err := json.Unmarshal(response, &parsed); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if parsed.OK || parsed.Error == nil || parsed.Error.Code != "model_not_in_plan" || parsed.Error.HTTPStatus != http.StatusForbidden {
		t.Fatalf("expected 403 model_not_in_plan envelope, got %s", response)
	}
}

func TestHandleExecuteStreamRejectsLockedModel(t *testing.T) {
	defer resetCatalog()
	defer setPluginConfig(nil)
	applyRemoteCatalog([]remoteModel{
		{ID: "vendor/gated-model", Category: "opensource", MinimumPlan: planRefFor("individual-goat")},
	}, planContext{filter: true, planID: "individual-go-v1"}, 900)

	oldCall := hostCall
	defer func() { hostCall = oldCall }()
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		t.Fatalf("locked model must not reach the upstream, got host call %s", method)
		return nil, nil
	}

	payload, _ := json.Marshal(map[string]any{"model": "vendor/gated-model", "messages": []map[string]any{{"role": "user", "content": "hi"}}, "stream": true})
	request, _ := json.Marshal(executorRequest{
		AuthID:         "auth-1",
		StreamID:       "stream-1",
		StorageJSON:    []byte(`{"type":"commandcode-go","api_key":"sk-test"}`),
		Payload:        payload,
		HostCallbackID: "cb-1",
	})
	response, errRPC := handleExecuteStream(request)
	if errRPC != nil {
		t.Fatalf("handleExecuteStream rpc error: %v", errRPC)
	}
	var parsed envelope
	if err := json.Unmarshal(response, &parsed); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if parsed.OK || parsed.Error == nil || parsed.Error.Code != "model_not_in_plan" || parsed.Error.HTTPStatus != http.StatusForbidden {
		t.Fatalf("expected 403 model_not_in_plan envelope, got %s", response)
	}
}
