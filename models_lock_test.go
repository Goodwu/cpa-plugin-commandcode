package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

// stubHTTPFor intercepts host.http.do and routes by URL suffix.
func stubHTTPFor(t *testing.T, routes map[string]func(hostHTTPRequestMsg) hostHTTPResponse) {
	t.Helper()
	oldCall := hostCall
	hostCall = func(_ string, payload any) (json.RawMessage, error) {
		msg, _ := payload.(hostHTTPRequestMsg)
		for suffix, respond := range routes {
			if len(msg.URL) >= len(suffix) && msg.URL[len(msg.URL)-len(suffix):] == suffix {
				return mustMarshalJSONEnvelope(respond(msg)), nil
			}
		}
		return mustMarshalJSONEnvelope(hostHTTPResponse{StatusCode: 404, Body: []byte(`{"error":"no route"}`)}), nil
	}
	t.Cleanup(func() { hostCall = oldCall })
}

func mustMarshalJSONEnvelope(response hostHTTPResponse) json.RawMessage {
	raw, _ := json.Marshal(response)
	return raw
}

func TestFetchRemoteModelsPrefersClientView(t *testing.T) {
	stubHTTPFor(t, map[string]func(hostHTTPRequestMsg) hostHTTPResponse{
		"models?view=client": func(msg hostHTTPRequestMsg) hostHTTPResponse {
			return hostHTTPResponse{StatusCode: 200, Body: []byte(`{
				"catalogVersion": "v1",
				"refreshAfterSeconds": 900,
				"data": [
					{"id": "vendor/m", "name": "M", "category": "opensource", "minimumPlan": "individual-go"},
					{"id": "vendor/n", "category": "premium", "minimumPlan": null}
				]
			}`)}
		},
		"models": func(msg hostHTTPRequestMsg) hostHTTPResponse {
			t.Fatalf("plain view must not be fetched when client view succeeds: %s", msg.URL)
			return hostHTTPResponse{}
		},
	})
	catalog, err := fetchRemoteModels("cb", "https://api.example", "key")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if catalog.RefreshAfterSecond != 900 {
		t.Fatalf("refresh hint = %d, want 900", catalog.RefreshAfterSecond)
	}
	if len(catalog.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(catalog.Models))
	}
	if catalog.Models[0].Category != "opensource" || !catalog.Models[0].MinimumPlan.set || catalog.Models[0].MinimumPlan.value != "individual-go" {
		t.Fatalf("client-view gating fields not parsed: %+v", catalog.Models[0])
	}
	if !catalog.Models[1].MinimumPlan.null {
		t.Fatalf("explicit null minimumPlan must parse as null: %+v", catalog.Models[1].MinimumPlan)
	}
}

func TestFetchRemoteModelsFallsBackToPlainView(t *testing.T) {
	stubHTTPFor(t, map[string]func(hostHTTPRequestMsg) hostHTTPResponse{
		"view=client": func(hostHTTPRequestMsg) hostHTTPResponse {
			return hostHTTPResponse{StatusCode: 403, Body: []byte(`{"error":"denied"}`)}
		},
		"v1/models": func(hostHTTPRequestMsg) hostHTTPResponse {
			return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"object":"list","data":[{"id":"vendor/m","context_length":64000}]}`)}
		},
	})
	catalog, err := fetchRemoteModels("cb", "https://api.example", "key")
	if err != nil {
		t.Fatalf("fallback fetch: %v", err)
	}
	if len(catalog.Models) != 1 || catalog.Models[0].ID != "vendor/m" {
		t.Fatalf("plain fallback payload wrong: %+v", catalog.Models)
	}
	if catalog.Models[0].Category != "" || catalog.Models[0].MinimumPlan.set {
		t.Fatalf("plain view must carry no gating fields: %+v", catalog.Models[0])
	}
	if catalog.RefreshAfterSecond != 0 {
		t.Fatalf("plain view has no refresh hint, got %d", catalog.RefreshAfterSecond)
	}
}

func TestFetchPlanContext(t *testing.T) {
	creditsOK := func(msg hostHTTPRequestMsg) hostHTTPResponse {
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"credits":{"purchasedCredits":0,"freeCredits":0,"monthlyCredits":5}}`)}
	}
	subscription := func(planID string) func(hostHTTPRequestMsg) hostHTTPResponse {
		return func(hostHTTPRequestMsg) hostHTTPResponse {
			return hostHTTPResponse{StatusCode: 200, Body: []byte(fmt.Sprintf(`{"success":true,"data":{"planId":%q,"status":"active"}}`, planID))}
		}
	}

	t.Run("known plan filters", func(t *testing.T) {
		stubHTTPFor(t, map[string]func(hostHTTPRequestMsg) hostHTTPResponse{
			"billing/credits":        creditsOK,
			"billing/subscriptions":  subscription("individual-go-v1"),
		})
		ctx := fetchPlanContext("cb", "https://api.example", "key-filter-ok", "auth-1")
		if !ctx.filter || ctx.planID != "individual-go-v1" || ctx.creditsUnlock {
			t.Fatalf("ctx = %+v, want filter for individual-go-v1 without credits", ctx)
		}
	})

	t.Run("credits unlock", func(t *testing.T) {
		stubHTTPFor(t, map[string]func(hostHTTPRequestMsg) hostHTTPResponse{
			"billing/credits": func(hostHTTPRequestMsg) hostHTTPResponse {
				return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"credits":{"purchasedCredits":2.5,"freeCredits":0}}`)}
			},
			"billing/subscriptions": subscription("individual-go-v1"),
		})
		ctx := fetchPlanContext("cb", "https://api.example", "key-credits", "auth-2")
		if !ctx.filter || !ctx.creditsUnlock {
			t.Fatalf("ctx = %+v, want creditsUnlock", ctx)
		}
	})

	t.Run("billing failure disables filtering", func(t *testing.T) {
		stubHTTPFor(t, map[string]func(hostHTTPRequestMsg) hostHTTPResponse{
			"billing/credits": func(hostHTTPRequestMsg) hostHTTPResponse {
				return hostHTTPResponse{StatusCode: 401, Body: []byte(`{"error":"denied"}`)}
			},
		})
		ctx := fetchPlanContext("cb", "https://api.example", "key-401", "auth-3")
		if ctx.filter {
			t.Fatalf("ctx = %+v, want no filtering on billing failure", ctx)
		}
	})

	t.Run("unknown plan disables filtering", func(t *testing.T) {
		stubHTTPFor(t, map[string]func(hostHTTPRequestMsg) hostHTTPResponse{
			"billing/credits":       creditsOK,
			"billing/subscriptions": subscription("enterprise-mystery"),
		})
		ctx := fetchPlanContext("cb", "https://api.example", "key-unknown-plan", "auth-4")
		if ctx.filter {
			t.Fatalf("ctx = %+v, want no filtering for unknown plan", ctx)
		}
	})
}
