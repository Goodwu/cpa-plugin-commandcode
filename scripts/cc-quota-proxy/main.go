// cc-quota-proxy：本地反向代理 zcode.z.ai，往 billing/balance 响应注入 CommandCode 余量条目，
// 让 ZCode 订阅卡原生显示 CC 余量（详见 archives/conversations/zcode-native-quota-display.md）。
//
// 用法：
//
//	cc-quota-proxy [-listen 127.0.0.1:8321] [-upstream https://zcode.z.ai] [-inject static|off] [-cpa http://127.0.0.1:8317]
//
// 阶段1（PoC）：-inject static 注入固定假数据验证 UI 渲染；阶段2 接 CPA quota。
// 回滚：launchctl unsetenv ZCODE_BASE_URL 并重启 ZCode 即完全还原。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"time"
)

const balancePath = "/api/v1/zcode-plan/billing/balance"

// injectEntry 是往 data.balances[] 追加的条目；字段名与上游真实响应一致（日志样本 2026-10-23）。
// capabilities 必须留空：非空 "model:xxx" 会被 ZCode 注册为 start plan 可用模型（Q8 逻辑），
// 导致 CC 模型被错误挂到 GLM 账户推理。priority 取最低，不抢占现有条目的 primary 位。
type injectEntry struct {
	BucketID            string   `json:"bucket_id"`
	UserPlanID          string   `json:"user_plan_id"`
	PlanID              string   `json:"plan_id"`
	EntitlementID       string   `json:"entitlement_id"`
	ShowName            string   `json:"show_name"`
	Meter               string   `json:"meter"`
	UnitType            string   `json:"unit_type"`
	Capabilities        []string `json:"capabilities"`
	Priority            int64    `json:"priority"`
	PlanPriority        int64    `json:"plan_priority"`
	EntitlementPriority int64    `json:"entitlement_priority"`
	TotalUnits          float64  `json:"total_units"`
	UsedUnits           float64  `json:"used_units"`
	RemainingUnits      float64  `json:"remaining_units"`
	AvailableUnits      float64  `json:"available_units"`
	ReservedUnits       *float64 `json:"reserved_units"`
	PeriodStart         int64    `json:"period_start"`
	PeriodEnd           int64    `json:"period_end"`
	ExpiresAt           int64    `json:"expires_at"`
}

type injectPlanEntitlement struct {
	EntitlementID string   `json:"entitlement_id"`
	ShowName      string   `json:"show_name"`
	Meter         string   `json:"meter"`
	UnitType      string   `json:"unit_type"`
	Capabilities  []string `json:"capabilities"`
	GrantUnits    float64  `json:"grant_units"`
	Period        string   `json:"period"`
	Priority      int64    `json:"priority"`
	EffectiveAt   int64    `json:"effective_at"`
}

// injectPlan 与 balances 配套注入：UI 侧 AOe 把 plans map 成 {name, plan_id, status}，
// LEe 归一化要求 status=active 且 ends_at 在有效期内。
type injectPlan struct {
	UserPlanID   string                  `json:"user_plan_id"`
	PlanID       string                  `json:"plan_id"`
	Name         string                  `json:"name"`
	Description  string                  `json:"description"`
	Priority     int64                   `json:"priority"`
	Status       string                  `json:"status"`
	StartsAt     int64                   `json:"starts_at"`
	EndsAt       int64                   `json:"ends_at"`
	Entitlements []injectPlanEntitlement `json:"entitlements"`
}

// injectBalances 就地解析并追加条目；返回是否注入成功（失败则保持原 body 不动，宁可少显示不可破坏）。
// 用 map 承载以完整保留上游未知字段（logid 等），注入条目以 typed struct marshal 后追加。
func injectBalances(body []byte, src func(time.Time) ([]injectPlan, []injectEntry), now time.Time) ([]byte, bool) {
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		return body, false
	}
	data, ok := env["data"].(map[string]any)
	if !ok {
		return body, false
	}
	// 信封语义与 isSuccessfulBigModelEnvelope 一致才注入；401/5xx 等原样透传。
	switch code := env["code"].(type) {
	case float64:
		if code != 0 && code != 200 {
			return body, false
		}
	case nil:
		if success, ok := env["success"].(bool); ok && !success {
			return body, false
		}
	default:
		return body, false
	}
	plans, balances := src(now)
	data["plans"] = append(toAnySlice(data["plans"]), mustAny(plans)...)
	data["balances"] = append(toAnySlice(data["balances"]), mustAny(balances)...)
	out, err := json.Marshal(env)
	if err != nil {
		return body, false
	}
	return out, true
}

func toAnySlice(v any) []any {
	s, ok := v.([]any)
	if !ok {
		return nil
	}
	return s
}

func mustAny(v any) []any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var s []any
	if err := json.Unmarshal(b, &s); err != nil {
		return nil
	}
	return s
}

// staticQuotaSource 是阶段1的假数据源：5h 窗口 62%、周窗口 45%、月度 22.7%（对齐 CPA 真实比例）。
// total 取 1 亿量级 token 风格数值，与现有 GLM 条目（亿级）显示格式一致。
// 月度重置时间取用户订阅的真实账期（2026-11-20，见 TASKS.md 月度额度记录）。
func staticQuotaSource(now time.Time) ([]injectPlan, []injectEntry) {
	period5hEnd := now.Add(time.Hour).Unix()
	weekEnd := now.Add(72 * time.Hour).Unix()
	monthEnd := time.Date(now.Year(), now.Month(), 20, 0, 0, 0, 0, time.Local)
	if !monthEnd.After(now) {
		monthEnd = monthEnd.AddDate(0, 1, 0)
	}
	const total = float64(100_000_000)
	ents := []injectPlanEntitlement{
		{EntitlementID: "cc-ent-5h", ShowName: "CommandCode·5h", Meter: "model_usage", UnitType: "token", Capabilities: []string{"cc-window"}, GrantUnits: total, Period: "rolling", Priority: 1, EffectiveAt: now.Add(-4 * time.Hour).Unix()},
		{EntitlementID: "cc-ent-week", ShowName: "CommandCode·周", Meter: "model_usage", UnitType: "token", Capabilities: []string{"cc-window"}, GrantUnits: total, Period: "weekly", Priority: 1, EffectiveAt: now.Add(-3 * 24 * time.Hour).Unix()},
		{EntitlementID: "cc-ent-month", ShowName: "CommandCode·月", Meter: "model_usage", UnitType: "token", Capabilities: []string{"cc-window"}, GrantUnits: total, Period: "monthly", Priority: 1, EffectiveAt: monthEnd.AddDate(0, -1, 0).Unix()},
	}
	plans := []injectPlan{{
		UserPlanID: "cc-poc-upl", PlanID: "cc-poc-plan", Name: "CommandCode",
		Description: "via CPA (PoC static)", Priority: 1, Status: "active",
		StartsAt: now.Add(-4 * time.Hour).Unix(), EndsAt: weekEnd, Entitlements: ents,
	}}
	balances := []injectEntry{
		{
			BucketID: "cc-bucket-5h", UserPlanID: "cc-poc-upl", PlanID: "cc-poc-plan", EntitlementID: "cc-ent-5h",
			ShowName: "CommandCode·5h", Meter: "model_usage", UnitType: "token", Capabilities: []string{"cc-window"},
			Priority: 1, PlanPriority: 1, EntitlementPriority: 1,
			TotalUnits: total, UsedUnits: 38_000_000, RemainingUnits: 62_000_000, AvailableUnits: 62_000_000,
			PeriodStart: now.Add(-4 * time.Hour).Unix(), PeriodEnd: period5hEnd, ExpiresAt: period5hEnd,
		},
		{
			BucketID: "cc-bucket-week", UserPlanID: "cc-poc-upl", PlanID: "cc-poc-plan", EntitlementID: "cc-ent-week",
			ShowName: "CommandCode·周", Meter: "model_usage", UnitType: "token", Capabilities: []string{"cc-window"},
			Priority: 1, PlanPriority: 1, EntitlementPriority: 1,
			TotalUnits: total, UsedUnits: 55_000_000, RemainingUnits: 45_000_000, AvailableUnits: 45_000_000,
			PeriodStart: now.Add(-3 * 24 * time.Hour).Unix(), PeriodEnd: weekEnd, ExpiresAt: weekEnd,
		},
		{
			BucketID: "cc-bucket-month", UserPlanID: "cc-poc-upl", PlanID: "cc-poc-plan", EntitlementID: "cc-ent-month",
			ShowName: "CommandCode·月", Meter: "model_usage", UnitType: "token", Capabilities: []string{"cc-window"},
			Priority: 1, PlanPriority: 1, EntitlementPriority: 1,
			TotalUnits: total, UsedUnits: 77_300_000, RemainingUnits: 22_700_000, AvailableUnits: 22_700_000,
			PeriodStart: monthEnd.AddDate(0, -1, 0).Unix(), PeriodEnd: monthEnd.Unix(), ExpiresAt: monthEnd.Unix(),
		},
	}
	return plans, balances
}

type proxy struct {
	upstream *url.URL
	inject   string
	logger   *log.Logger
}

func newProxy(listen, upstream, injectMode string, logger *log.Logger) (*httputil.ReverseProxy, *proxy, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, nil, err
	}
	p := &proxy{upstream: u, inject: injectMode, logger: logger}
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.Host = u.Host
			req.URL.Scheme = u.Scheme
			req.URL.Host = u.Host
			// billing/balance 走明文：上游不再压缩，注入才能改 body；其余路径保持原样透传（含流式）。
			if req.URL.Path == balancePath {
				req.Header.Set("Accept-Encoding", "identity") // 显式 identity：Go transport 对空值会自动补 gzip
			}
		},
		ModifyResponse: p.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.logger.Printf("ERROR %s %s upstream=%v", r.Method, r.URL.Path, err)
			http.Error(w, "cc-quota-proxy upstream error", http.StatusBadGateway)
		},
		FlushInterval: -1, // 流式与 websocket 即时透传
	}
	return rp, p, nil
}

func (p *proxy) modifyResponse(res *http.Response) error {
	if res.Request == nil || res.Request.URL.Path != balancePath || p.inject == "off" {
		return nil
	}
	if res.StatusCode != http.StatusOK {
		p.logger.Printf("SKIP %s status=%d (non-200, passthrough)", balancePath, res.StatusCode)
		return nil
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		return fmt.Errorf("read upstream body: %w", err)
	}
	injected := false
	if p.inject == "static" {
		body, injected = injectBalances(body, staticQuotaSource, time.Now())
	}
	if !injected {
		p.logger.Printf("SKIP %s body-not-injectable (len=%d, passthrough)", balancePath, len(body))
	} else {
		p.logger.Printf("INJECT %s +%d bytes", balancePath, len(body))
	}
	res.Body = io.NopCloser(bytes.NewReader(body))
	res.ContentLength = int64(len(body))
	res.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	res.Header.Del("Content-Encoding") // body 已解压重写
	res.Header.Del("ETag")             // 内容已变，避免客户端缓存错配
	return nil
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8321", "local listen address")
	upstream := flag.String("upstream", "https://zcode.z.ai", "zcode origin to proxy")
	inject := flag.String("inject", "static", "inject mode: static | off")
	logPath := flag.String("log", "", "log file path (default stderr)")
	flag.Parse()

	var w io.Writer = os.Stderr
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatalf("open log: %v", err)
		}
		defer f.Close()
		w = io.Writer(f)
	}
	logger := log.New(w, "", log.LstdFlags|log.Lmicroseconds)

	rp, _, err := newProxy(*listen, *upstream, *inject, logger)
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Printf("REQ %s %s%s", r.Method, r.URL.Path, maskQuery(r.URL.RawQuery))
		rp.ServeHTTP(w, r)
	})
	logger.Printf("cc-quota-proxy listening=%s upstream=%s inject=%s", *listen, *upstream, *inject)
	fmt.Fprintf(w, "cc-quota-proxy listening=%s upstream=%s inject=%s\n", *listen, *upstream, *inject)
	if err := http.ListenAndServe(*listen, handler); err != nil {
		logger.Fatalf("serve: %v", err)
	}
}

// maskQuery 隐藏 query 里的敏感参数（当前只有 app_version，保留；其余截断）。
func maskQuery(q string) string {
	if q == "" {
		return ""
	}
	parts := strings.Split(q, "&")
	for i, s := range parts {
		if !strings.HasPrefix(s, "app_version=") {
			parts[i] = "…"
		}
	}
	return "?" + strings.Join(parts, "&")
}
