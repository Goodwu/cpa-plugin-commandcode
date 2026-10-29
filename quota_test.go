package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestQuotaDescribeAndIdentifier(t *testing.T) {
	raw, err := dispatch("quota.identifier", nil)
	if err != nil {
		t.Fatal(err)
	}
	var id struct{ Identifier string }
	_ = json.Unmarshal(decodeEnvelope(t, raw).Result, &id)
	if id.Identifier != authProviderID {
		t.Fatalf("identifier = %q", id.Identifier)
	}

	raw, err = dispatch("quota.describe", nil)
	if err != nil {
		t.Fatal(err)
	}
	var desc quotaDescribeResponse
	if err := json.Unmarshal(decodeEnvelope(t, raw).Result, &desc); err != nil {
		t.Fatal(err)
	}
	if len(desc.SupportedProviders) != 1 || desc.SupportedProviders[0] != authProviderID {
		t.Fatalf("providers = %v", desc.SupportedProviders)
	}
	if desc.SupportsReset {
		t.Fatal("CommandCode has no reset API")
	}
}

func TestRecordRateLimitFromBody(t *testing.T) {
	pluginState.reset()
	body := `{"success":false,"error":{"code":"RATE_LIMITED","status":429,"rateLimit":{"limit":3,"remaining":0,"reset":1793178541,"window":"fiveHour"}}}`
	recordRateLimitFromBody("auth-a", body)

	snapshot := pluginState.snapshot()
	var rate *rateLimitSnapshot
	for i := range snapshot.Auths {
		if snapshot.Auths[i].AuthID == "auth-a" {
			rate = snapshot.Auths[i].RateLimit
		}
	}
	if rate == nil || rate.Limit != 3 || rate.Remaining != 0 || rate.Window != "fiveHour" {
		t.Fatalf("rate limit = %+v", rate)
	}
	if rate.ResetAt.Unix() != 1793178541 {
		t.Fatalf("reset seconds = %d", rate.ResetAt.Unix())
	}

	// Milliseconds variant
	recordRateLimitFromBody("auth-b", `{"error":{"rateLimit":{"limit":6,"remaining":4,"reset":1793264003768,"window":"weekly"}}}`)
	for _, auth := range pluginState.snapshot().Auths {
		if auth.AuthID == "auth-b" {
			if auth.RateLimit.ResetAt.UnixMilli() != 1793264003768 {
				t.Fatalf("reset ms = %d", auth.RateLimit.ResetAt.UnixMilli())
			}
		}
	}

	// Garbage must not panic or create records
	recordRateLimitFromBody("auth-c", "not json")
	recordRateLimitFromBody("", body)
	for _, auth := range pluginState.snapshot().Auths {
		if auth.AuthID == "auth-c" {
			t.Fatal("garbage body must not be recorded")
		}
	}
	pluginState.reset()
}

func TestQuotaFetchFallsBackToLocalState(t *testing.T) {
	quotaCache.mu.Lock()
	quotaCache.items = map[string]quotaCacheEntry{}
	quotaCache.mu.Unlock()
	pluginState.reset()

	// Local observation: one request plus a 429 calibration.
	pluginState.recordRequest(requestRecord{AuthID: "a.json", Model: "m", At: time.Now()})
	recordRateLimitFromBody("a.json", `{"error":{"rateLimit":{"limit":3,"remaining":1,"reset":1793178541,"window":"fiveHour"}}}`)

	payload := map[string]any{
		"auth_id":      "a.json",
		"provider":     authProviderID,
		"storage_json": b64(`{"type":"commandcode-go","api_key":"k"}`),
	}
	rawRequest, _ := json.Marshal(payload)
	raw, err := dispatch("quota.fetch", rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("quota.fetch failed: %v", env.Error)
	}
	var resp quotaFetchResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Groups) != 1 || len(resp.Groups[0].Buckets) != 1 {
		t.Fatalf("expected one local bucket, got %+v", resp.Groups)
	}
	bucket := resp.Groups[0].Buckets[0]
	if bucket.Window != "5h" {
		t.Fatalf("window = %q", bucket.Window)
	}
	if bucket.RemainingFraction < 0.33 || bucket.RemainingFraction > 0.34 {
		t.Fatalf("remaining = %v", bucket.RemainingFraction)
	}
	pluginState.reset()
}

func TestQuotaFetchRejectsForeignProvider(t *testing.T) {
	raw, err := dispatch("quota.fetch", []byte(`{"provider":"xai","storage_json":"`+b64(`{"api_key":"k"}`)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if env := decodeEnvelope(t, raw); env.OK {
		t.Fatal("foreign provider must be rejected")
	}
}

func TestBuildUsageHeaders(t *testing.T) {
	headers := buildUsageHeaders("key-1", "1.2.3")
	if headers["Authorization"][0] != "Bearer key-1" {
		t.Fatalf("auth = %v", headers["Authorization"])
	}
	if headers["x-command-code-version"][0] != "1.2.3" {
		t.Fatalf("version = %v", headers["x-command-code-version"])
	}
	if !strings.Contains(headers["User-Agent"][0], "commandcode-cli/1.2.3") {
		t.Fatalf("ua = %v", headers["User-Agent"])
	}
}

func TestQuotaCacheTTL(t *testing.T) {
	quotaCache.mu.Lock()
	quotaCache.items = map[string]quotaCacheEntry{}
	quotaCache.mu.Unlock()

	storeQuotaCache("a", quotaFetchResponse{Summary: []quotaMetric{{Key: "x", Value: 1}}})
	if _, ok := lookupQuotaCache("a"); !ok {
		t.Fatal("fresh entry should hit")
	}
	quotaCache.mu.Lock()
	quotaCache.items["a"] = quotaCacheEntry{at: time.Now().Add(-2 * quotaCacheTTL)}
	quotaCache.mu.Unlock()
	if _, ok := lookupQuotaCache("a"); ok {
		t.Fatal("stale entry should miss")
	}
}
