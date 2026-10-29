package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStateRollingWindow(t *testing.T) {
	store := newStateStore()
	now := time.Now()
	store.recordRequest(requestRecord{AuthID: "a", At: now.Add(-6 * time.Hour)})
	store.recordRequest(requestRecord{AuthID: "a", At: now.Add(-2 * time.Hour)})
	store.recordRequest(requestRecord{AuthID: "a", At: now.Add(-time.Minute)})

	snapshot := store.snapshot()
	if len(snapshot.Auths) != 1 {
		t.Fatalf("auths = %d", len(snapshot.Auths))
	}
	if snapshot.Auths[0].Requests5h != 2 {
		t.Fatalf("requests5h = %d, want 2", snapshot.Auths[0].Requests5h)
	}
	if snapshot.Totals.Requests != 3 {
		t.Fatalf("total requests = %d", snapshot.Totals.Requests)
	}
}

func TestStateTotalsAndReset(t *testing.T) {
	store := newStateStore()
	store.recordRequest(requestRecord{AuthID: "a", Status: 429, PromptTokens: 10, CompletionTokens: 5})
	store.recordRequest(requestRecord{AuthID: "a", PromptTokens: 20, CachedTokens: 3})

	snapshot := store.snapshot()
	if snapshot.Totals.Failures != 1 {
		t.Fatalf("failures = %d", snapshot.Totals.Failures)
	}
	if snapshot.Totals.PromptTokens != 30 || snapshot.Totals.CompletionTokens != 5 || snapshot.Totals.CachedTokens != 3 {
		t.Fatalf("totals = %+v", snapshot.Totals)
	}
	if len(snapshot.Recent) != 2 {
		t.Fatalf("recent = %d", len(snapshot.Recent))
	}
	// Newest first
	if snapshot.Recent[0].PromptTokens != 20 {
		t.Fatalf("recent order = %+v", snapshot.Recent)
	}

	store.reset()
	after := store.snapshot()
	if after.Totals.Requests != 0 || len(after.Recent) != 0 {
		t.Fatalf("reset did not clear: %+v", after.Totals)
	}
	if len(after.Auths) != 1 {
		t.Fatal("reset must keep learned auth metadata")
	}
}

func TestStateRecordCap(t *testing.T) {
	store := newStateStore()
	for i := 0; i < requestHistory+20; i++ {
		store.recordRequest(requestRecord{AuthID: "a"})
	}
	if got := len(store.snapshot().Recent); got != 20 {
		t.Fatalf("recent capped at 20 per snapshot, got %d", got)
	}
	if got := len(store.records); got != requestHistory {
		t.Fatalf("records = %d, want %d", got, requestHistory)
	}
}

func TestManagementRegister(t *testing.T) {
	raw, err := dispatch("management.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	var reg managementRegistration
	if err := json.Unmarshal(decodeEnvelope(t, raw).Result, &reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Resources) != 1 || reg.Resources[0].Path != "/status" {
		t.Fatalf("resources = %+v", reg.Resources)
	}
	if reg.Resources[0].Menu == "" {
		t.Fatal("menu label required for the management sidebar")
	}
	if len(reg.Routes) != 2 {
		t.Fatalf("routes = %+v", reg.Routes)
	}
}

func TestManagementHandleRouting(t *testing.T) {
	pluginState.reset()
	pluginState.recordRequest(requestRecord{AuthID: "commandcode-go-main.json", Model: "m"})

	call := func(method, path string) managementResponse {
		t.Helper()
		req, _ := json.Marshal(managementRequest{Method: method, Path: path})
		raw, err := dispatch("management.handle", req)
		if err != nil {
			t.Fatal(err)
		}
		env := decodeEnvelope(t, raw)
		if !env.OK {
			t.Fatalf("management.handle failed: %v", env.Error)
		}
		var resp managementResponse
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	page := call("GET", "/v0/resource/plugins/commandcode-go/status")
	if page.StatusCode != 200 {
		t.Fatalf("page status = %d", page.StatusCode)
	}
	if !strings.HasPrefix(string(page.Body), "<!doctype html>") {
		t.Fatalf("page body is not HTML: %q", string(page.Body)[:40])
	}
	if csp := page.Headers["Content-Security-Policy"]; len(csp) == 0 || !strings.Contains(csp[0], "default-src 'none'") {
		t.Fatalf("missing CSP: %v", page.Headers)
	}

	status := call("GET", "/v0/management/plugins/commandcode-go/status")
	if status.StatusCode != 200 {
		t.Fatalf("status = %d", status.StatusCode)
	}
	var payload map[string]any
	if err := json.Unmarshal(status.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["totals"]; !ok {
		t.Fatalf("payload = %v", payload)
	}
	// Never leak credentials
	if strings.Contains(string(status.Body), "\"api_key\"") || strings.Contains(string(status.Body), "Bearer ") {
		t.Fatal("status payload must not contain credentials")
	}

	if got := call("POST", "/v0/management/plugins/commandcode-go/status").StatusCode; got != 405 {
		t.Fatalf("wrong method status = %d", got)
	}
	if got := call("POST", "/v0/management/plugins/commandcode-go/reset-stats").StatusCode; got != 200 {
		t.Fatalf("reset status = %d", got)
	}
	if got := call("GET", "/v0/management/plugins/commandcode-go/reset-stats").StatusCode; got != 405 {
		t.Fatalf("reset GET status = %d", got)
	}
	if got := call("GET", "/v0/management/plugins/commandcode-go/nope").StatusCode; got != 404 {
		t.Fatalf("unknown path status = %d", got)
	}
}

func TestRegistrationAdvertisesNewCapabilities(t *testing.T) {
	raw, err := dispatch("plugin.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	var reg registrationResponse
	if err := json.Unmarshal(decodeEnvelope(t, raw).Result, &reg); err != nil {
		t.Fatal(err)
	}
	if !reg.Capabilities.QuotaProvider {
		t.Fatal("quota_provider must be advertised")
	}
	if !reg.Capabilities.ManagementAPI {
		t.Fatal("management_api must be advertised")
	}
}
