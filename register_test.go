package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func b64(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }

func decodeEnvelope(t *testing.T, raw []byte) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope decode: %v (%s)", err, raw)
	}
	return env
}

func TestDispatchRegistration(t *testing.T) {
	raw, err := dispatch("plugin.register", []byte(`{"schema_version":1,"config_yaml":"`+b64("enabled: true\n")+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("register failed: %v", env.Error)
	}
	var reg registrationResponse
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.SchemaVersion != schemaVersion {
		t.Fatalf("schema = %d", reg.SchemaVersion)
	}
	if reg.Metadata.Name != pluginID {
		t.Fatalf("name = %q", reg.Metadata.Name)
	}
	if !reg.Capabilities.AuthProvider || !reg.Capabilities.ModelProvider || !reg.Capabilities.Executor {
		t.Fatalf("capabilities = %+v", reg.Capabilities)
	}
	if reg.Capabilities.ExecutorModelScope != "both" {
		t.Fatalf("scope = %q", reg.Capabilities.ExecutorModelScope)
	}
	if strings.Join(reg.Capabilities.ExecutorInputFormats, ",") != "chat-completions" {
		t.Fatalf("input formats = %v", reg.Capabilities.ExecutorInputFormats)
	}
	if strings.Join(reg.Capabilities.ExecutorOutputFormats, ",") != "chat-completions" {
		t.Fatalf("output formats = %v", reg.Capabilities.ExecutorOutputFormats)
	}
}

func TestDispatchIdentifiers(t *testing.T) {
	for _, method := range []string{"auth.identifier", "executor.identifier"} {
		raw, err := dispatch(method, nil)
		if err != nil {
			t.Fatal(err)
		}
		env := decodeEnvelope(t, raw)
		var out struct{ Identifier string }
		_ = json.Unmarshal(env.Result, &out)
		if out.Identifier != authProviderID {
			t.Fatalf("%s -> %q", method, out.Identifier)
		}
	}
}

func TestAuthParseHandledAndReject(t *testing.T) {
	mine, err := dispatch("auth.parse", []byte(`{"RawJSON":"`+b64(`{"type":"commandcode-go","api_key":"cc-secret-1","label":"main"}`)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, mine)
	var parsed authParseResponse
	if err := json.Unmarshal(env.Result, &parsed); err != nil {
		t.Fatal(err)
	}
	if !parsed.Handled {
		t.Fatal("own auth file must be handled")
	}
	if parsed.Auth.Provider != authProviderID || parsed.Auth.FileName != "commandcode-go-main.json" {
		t.Fatalf("auth = %+v", parsed.Auth)
	}
	if !strings.Contains(string(parsed.Auth.StorageJSON), "cc-secret-1") {
		t.Fatal("StorageJSON must carry the credential")
	}

	other, err := dispatch("auth.parse", []byte(`{"RawJSON":"`+b64(`{"type":"xai","access_token":"z"}`)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	env = decodeEnvelope(t, other)
	_ = json.Unmarshal(env.Result, &parsed)
	if parsed.Handled {
		t.Fatal("foreign auth file must not be handled")
	}
}

func TestAuthParseMissingKey(t *testing.T) {
	raw, err := dispatch("auth.parse", []byte(`{"RawJSON":"`+b64(`{"type":"commandcode-go"}`)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if env.OK {
		t.Fatal("missing api_key must be rejected")
	}
}

func TestAuthRefreshNoOp(t *testing.T) {
	raw, err := dispatch("auth.refresh", []byte(`{"AuthID":"commandcode-go-main.json","StorageJSON":"`+b64(`{"type":"commandcode-go","api_key":"k","label":"main"}`)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("refresh failed: %v", env.Error)
	}
	var resp authRefreshResponse
	_ = json.Unmarshal(env.Result, &resp)
	if !strings.Contains(string(resp.Auth.StorageJSON), `"api_key":"k"`) {
		t.Fatalf("storage not preserved: %s", resp.Auth.StorageJSON)
	}
	if resp.NextRefreshAfter == "" {
		t.Fatal("refresh must reschedule")
	}
}

func TestLoginRejected(t *testing.T) {
	for _, method := range []string{"auth.login.start", "auth.login.poll"} {
		raw, err := dispatch(method, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if env := decodeEnvelope(t, raw); env.OK {
			t.Fatalf("%s must not pretend to support login", method)
		}
	}
}

func TestStaticModelsCoverCatalog(t *testing.T) {
	raw, err := dispatch("model.static", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	var resp modelResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Provider != authProviderID {
		t.Fatalf("provider = %q", resp.Provider)
	}
	if len(resp.Models) < 10 {
		t.Fatalf("models = %d", len(resp.Models))
	}
	for _, model := range resp.Models {
		if model.ContextLength <= 0 {
			t.Fatalf("%s missing context length", model.ID)
		}
	}
}

func TestExecuteWithoutHostReturnsBadGateway(t *testing.T) {
	// Without the host bridge (tests run outside CPA) every upstream attempt
	// fails at transport level; the plugin must surface a 502 envelope rather
	// than panic. Retry is disabled to keep the test fast.
	previous := currentConfig
	currentConfig.MaxRetries = 0
	defer func() { currentConfig = previous }()

	payload := map[string]any{"model": "deepseek-v4-pro", "messages": []map[string]any{{"role": "user", "content": "hi"}}}
	rawPayload, _ := json.Marshal(payload)
	req := map[string]any{
		"AuthID":         "commandcode-go-main.json",
		"AuthProvider":   authProviderID,
		"Model":          "deepseek-v4-pro",
		"Format":         "chat-completions",
		"Stream":         false,
		"Payload":        base64.StdEncoding.EncodeToString(rawPayload),
		"StorageJSON":    base64.StdEncoding.EncodeToString([]byte(`{"type":"commandcode-go","api_key":"k"}`)),
		"AuthAttributes": map[string]string{},
		"AuthMetadata":   map[string]any{},
	}
	rawRequest, _ := json.Marshal(req)
	raw, err := dispatch("executor.execute", rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if env.OK {
		t.Fatal("execute must fail without a host bridge")
	}
	if env.Error == nil || env.Error.HTTPStatus != 502 {
		t.Fatalf("error = %+v", env.Error)
	}
}

func TestCountTokensHeuristic(t *testing.T) {
	payload := map[string]any{"model": "m", "messages": []map[string]any{{"role": "user", "content": strings.Repeat("word ", 400)}}}
	rawPayload, _ := json.Marshal(payload)
	req := map[string]any{"Payload": base64.StdEncoding.EncodeToString(rawPayload)}
	rawRequest, _ := json.Marshal(req)
	raw, err := dispatch("executor.count_tokens", rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("count failed: %v", env.Error)
	}
	var resp executorResponse
	_ = json.Unmarshal(env.Result, &resp)
	var counted map[string]any
	_ = json.Unmarshal(resp.Payload, &counted)
	if counted["total_tokens"].(float64) < 100 {
		t.Fatalf("estimate too small: %v", counted)
	}
	if counted["input_tokens"].(float64) != counted["total_tokens"].(float64) {
		t.Fatal("count-only responses should report input==total")
	}
}

func TestUnknownMethod(t *testing.T) {
	raw, err := dispatch("nonsense.method", nil)
	if err != nil {
		t.Fatal(err)
	}
	if env := decodeEnvelope(t, raw); env.OK {
		t.Fatal("unknown method must fail")
	}
}
