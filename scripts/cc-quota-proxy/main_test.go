package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 真实上游响应样本（2026-10-23 日志，去日志包装后的 payload）。
const realUpstreamBody = `{"code":0,"msg":"","data":{"server_time":1792771167,"plans":[{"user_plan_id":"upl_1","plan_id":"zcode-v3-start-plan-1024","name":"ZCode 1024 Dev Day","description":"","priority":101,"status":"active","starts_at":1792763818,"ends_at":1792976400,"entitlements":[{"entitlement_id":"ent-1","show_name":"GLM-5.3","meter":"model_usage","unit_type":"token","capabilities":["model:glm-5.3"],"grant_units":200000000,"period":"one_time","priority":100,"effective_at":1792767600}]}],"balances":[{"bucket_id":"bucket_1","user_plan_id":"upl_1","plan_id":"zcode-v3-start-plan-1024","entitlement_id":"ent-1","show_name":"GLM-5.3","meter":"model_usage","unit_type":"token","capabilities":["model:glm-5.3"],"priority":100,"plan_priority":101,"entitlement_priority":100,"total_units":200000000,"used_units":11160781,"remaining_units":188839219,"available_units":188839219,"period_start":1792763818,"period_end":1792976400,"expires_at":1792976400}]},"logid":"abc123"}`

func TestInjectBalancesAppendsAndPreserves(t *testing.T) {
	now := time.Unix(1792771167, 0)
	out, ok := injectBalances([]byte(realUpstreamBody), staticQuotaSource, now)
	if !ok {
		t.Fatal("expected injection success")
	}
	var env map[string]any
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("injected body not valid JSON: %v", err)
	}
	// 未知字段保留
	if env["logid"] != "abc123" {
		t.Errorf("logid lost: %v", env["logid"])
	}
	data := env["data"].(map[string]any)
	balances := data["balances"].([]any)
	if len(balances) != 4 {
		t.Fatalf("want 4 balances, got %d", len(balances))
	}
	names := map[string]bool{}
	for _, b := range balances {
		m := b.(map[string]any)
		names[m["show_name"].(string)] = true
		if m["show_name"] == "CommandCode·5h" {
			caps := m["capabilities"].([]any)
			if len(caps) != 1 || caps[0] != "cc-window" {
				t.Errorf("injected balance capabilities must be [cc-window] (non model: prefix), got %v", caps)
			}
			if m["remaining_units"].(float64) != 62_000_000 {
				t.Errorf("unexpected remaining: %v", m["remaining_units"])
			}
		}
	}
	if !names["GLM-5.3"] || !names["CommandCode·5h"] || !names["CommandCode·周"] || !names["CommandCode·月"] {
		t.Errorf("missing expected show_names: %v", names)
	}
	plans := data["plans"].([]any)
	if len(plans) != 2 {
		t.Fatalf("want 2 plans, got %d", len(plans))
	}
	if plans[1].(map[string]any)["name"] != "CommandCode" {
		t.Errorf("injected plan name wrong: %v", plans[1])
	}
	// 原始条目未被破坏
	if plans[0].(map[string]any)["plan_id"] != "zcode-v3-start-plan-1024" {
		t.Errorf("original plan mutated: %v", plans[0])
	}
}

func TestInjectBalancesSkipsBadEnvelope(t *testing.T) {
	cases := []string{
		`{"code":401,"msg":"令牌已过期或验证不正确","success":false}`, // 401 透传
		`{"code":0,"data":[]}`, // data 非对象
		`not json`,             // 非 JSON
		`{}`,                   // 无 data
	}
	for _, c := range cases {
		out, ok := injectBalances([]byte(c), staticQuotaSource, time.Now())
		if ok {
			t.Errorf("should not inject for %q", c)
		}
		if string(out) != c {
			t.Errorf("body must stay untouched on failure: %q -> %q", c, out)
		}
	}
}

func TestInjectBalancesEmptyPlans(t *testing.T) {
	// 无活动期：plans/balances 为空数组或缺失，注入后只有 CommandCode 条目。
	for _, body := range []string{`{"code":0,"data":{"plans":[],"balances":[]}}`, `{"code":0,"data":{}}`} {
		out, ok := injectBalances([]byte(body), staticQuotaSource, time.Now())
		if !ok {
			t.Fatalf("expected inject for %q", body)
		}
		if !strings.Contains(string(out), "CommandCode·5h") {
			t.Errorf("expected CommandCode entry in %q", out)
		}
	}
}

func TestProxyEndToEndInject(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == balancePath {
			if ae := r.Header.Get("Accept-Encoding"); ae != "" && ae != "identity" {
				t.Errorf("balance path must force identity Accept-Encoding, got %q", ae)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Date", "Mon, 02 Nov 2026 00:00:00 GMT")
			io.WriteString(w, realUpstreamBody)
			return
		}
		w.WriteHeader(http.StatusTeapot)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	logger := log.New(io.Discard, "", 0)
	rp, _, err := newProxy("127.0.0.1:0", upstream.URL, "static", logger)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(rp)
	defer front.Close()

	res, err := http.Get(front.URL + balancePath + "?app_version=3.17.2")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	if res.Header.Get("Date") == "" {
		t.Error("date header must be preserved (ZCode clock sync)")
	}
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	data := env["data"].(map[string]any)
	if got := len(data["balances"].([]any)); got != 4 {
		t.Fatalf("want 4 balances after proxy, got %d", got)
	}
	if cl := res.Header.Get("Content-Length"); cl != fmt.Sprintf("%d", len(body)) {
		t.Errorf("Content-Length mismatch: %s vs %d", cl, len(body))
	}

	// 非 balance 路径：纯透传，不动 body。
	res2, err := http.Get(front.URL + "/api/v1/other")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	b2, _ := io.ReadAll(res2.Body)
	if res2.StatusCode != http.StatusTeapot || string(b2) != `{"ok":true}` {
		t.Errorf("passthrough broken: %d %q", res2.StatusCode, b2)
	}
}
