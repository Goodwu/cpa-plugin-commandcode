package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ──────────────────────────────────────────
// Host contract (mirrors pluginapi quota types, snake_case per v8)
// ──────────────────────────────────────────

type quotaHostConfig struct {
	AuthDir  string `json:"AuthDir,omitempty"`
	ProxyURL string `json:"ProxyURL,omitempty"`
}

type quotaFetchRequest struct {
	AuthIndex      string            `json:"auth_index"`
	AuthID         string            `json:"auth_id"`
	Provider       string            `json:"provider"`
	StorageJSON    []byte            `json:"storage_json,omitempty"`
	Metadata       map[string]any    `json:"metadata,omitempty"`
	Attributes     map[string]string `json:"attributes,omitempty"`
	Host           quotaHostConfig   `json:"host,omitempty"`
	HostCallbackID string            `json:"host_callback_id,omitempty"`
}

type quotaResetRequest struct {
	AuthIndex      string          `json:"auth_index"`
	AuthID         string          `json:"auth_id"`
	Provider       string          `json:"provider"`
	HostCallbackID string          `json:"host_callback_id,omitempty"`
	Host           quotaHostConfig `json:"host,omitempty"`
}

type quotaSubscription struct {
	Plan     string `json:"plan,omitempty"`
	TierName string `json:"tierName,omitempty"`
	TierID   string `json:"tierId,omitempty"`
}

type quotaBucket struct {
	Window            string  `json:"window,omitempty"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}

type quotaGroup struct {
	DisplayName string        `json:"displayName,omitempty"`
	Buckets     []quotaBucket `json:"buckets,omitempty"`
}

type quotaMetric struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Unit     string  `json:"unit,omitempty"`
	Format   string  `json:"format,omitempty"`
	Currency string  `json:"currency,omitempty"`
}

type quotaFetchResponse struct {
	Subscription       *quotaSubscription `json:"subscription,omitempty"`
	Summary            []quotaMetric      `json:"summary,omitempty"`
	ServerTimeOffsetMs int64              `json:"serverTimeOffsetMs,omitempty"`
	Groups             []quotaGroup       `json:"groups,omitempty"`
}

type quotaDescribeResponse struct {
	SupportedProviders []string `json:"supported_providers,omitempty"`
	DisplayName        string   `json:"display_name,omitempty"`
	SupportsReset      bool     `json:"supports_reset,omitempty"`
}

type quotaResetResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// ──────────────────────────────────────────
// Upstream CommandCode billing payloads
// ──────────────────────────────────────────

type ccWindowLimit struct {
	Used     float64 `json:"used"`
	Cap      float64 `json:"cap"`
	Exceeded bool    `json:"exceeded"`
	ResetAt  int64   `json:"resetAt"` // milliseconds
}

type ccCreditsResponse struct {
	Credits struct {
		BelowThreshold   bool     `json:"belowThreshold"`
		MonthlyCredits   *float64 `json:"monthlyCredits"`
		PurchasedCredits float64  `json:"purchasedCredits"`
		FreeCredits      float64  `json:"freeCredits"`
	} `json:"credits"`
	WindowLimits *struct {
		Limited  bool           `json:"limited"`
		Exceeded *bool          `json:"exceeded"`
		FiveHour *ccWindowLimit `json:"fiveHour"`
		Weekly   *ccWindowLimit `json:"weekly"`
	} `json:"windowLimits"`
}

type ccSubscriptionResponse struct {
	Success bool `json:"success"`
	Data    struct {
		PlanID             string `json:"planId"`
		Status             string `json:"status"`
		CurrentPeriodStart string `json:"currentPeriodStart"`
		CurrentPeriodEnd   string `json:"currentPeriodEnd"`
	} `json:"data"`
}

// ──────────────────────────────────────────
// Cache (per auth, short TTL)
// ──────────────────────────────────────────

const (
	quotaCacheTTL = 60 * time.Second
	planCacheTTL  = 5 * time.Minute
)

type quotaCacheEntry struct {
	at   time.Time
	resp quotaFetchResponse
}

type planCacheEntry struct {
	at     time.Time
	planID string
	period string
	status string
}

var quotaCache = struct {
	mu    sync.Mutex
	items map[string]quotaCacheEntry
}{items: map[string]quotaCacheEntry{}}

var planCache = struct {
	mu    sync.Mutex
	items map[string]planCacheEntry
}{items: map[string]planCacheEntry{}}

// ──────────────────────────────────────────
// RPC handlers
// ──────────────────────────────────────────

func handleQuotaDescribe(_ []byte) ([]byte, error) {
	return okEnvelope(quotaDescribeResponse{
		SupportedProviders: []string{authProviderID},
		DisplayName:        "CommandCode",
		SupportsReset:      false,
	})
}

func handleQuotaReset(_ []byte) ([]byte, error) {
	return okEnvelope(quotaResetResponse{
		Success: false,
		Message: "CommandCode does not expose a manual quota reset",
	})
}

func handleQuotaFetch(request []byte) ([]byte, error) {
	var req quotaFetchRequest
	if err := decodeRequest(request, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	if provider := strings.TrimSpace(req.Provider); provider != "" && provider != authProviderID {
		return errorEnvelope("unsupported_provider", "unsupported provider: "+provider), nil
	}
	storage, _ := parseKeyStorage(req.StorageJSON)
	apiKey := ""
	baseURLOverride := ""
	if storage != nil {
		apiKey = strings.TrimSpace(storage.APIKey)
		baseURLOverride = storage.BaseURL
	}
	if apiKey == "" {
		return errorEnvelope("invalid_request", "credential has no api_key"), nil
	}
	apiBase := resolveAPIBase(baseURLOverride, activeConfig().APIBase)
	authID := firstNonEmpty(req.AuthID, req.AuthIndex, "default")
	cacheID := fmt.Sprintf("%s:%x", authID, sha256.Sum256([]byte(apiBase+"\x00"+apiKey)))

	bindQuotaIdentity(authID, cacheID)
	if cached, ok := lookupQuotaCache(cacheID); ok {
		return okEnvelope(cached)
	}

	resp, errFetch := fetchQuotaFromBilling(req.HostCallbackID, apiBase, apiKey, authID)
	if errFetch != nil {
		// Identity validation and snapshot read share the bind lock, so an
		// old request cannot copy the newly selected account's calibration.
		quotaCache.mu.Lock()
		fallbackID := authID
		if quotaIdentities[authID] != cacheID {
			fallbackID = ""
		}
		resp = localQuotaFallback(fallbackID, fmt.Errorf("%s", sanitizeErrorText(errFetch.Error(), apiKey)))
		quotaCache.mu.Unlock()
	}
	storeQuotaCache(cacheID, resp)
	return okEnvelope(resp)
}

// fetchQuotaFromBilling reads the CLI billing endpoint (the same one `cmd`
// uses for its /usage panel) and normalizes window limits into quota buckets.
func fetchQuotaFromBilling(callbackID, apiBase, apiKey, authID string) (quotaFetchResponse, error) {
	cliVersion := currentCLIVersion(callbackID)
	credits, status, errCredits := getCredits(callbackID, apiBase, apiKey, cliVersion)
	if errCredits != nil || status != http.StatusOK || credits == nil {
		msg := "billing request failed"
		if errCredits != nil {
			msg = errCredits.Error()
		} else {
			msg = fmt.Sprintf("billing request returned HTTP %d", status)
		}
		return quotaFetchResponse{}, fmt.Errorf("%s", msg)
	}

	resp := quotaFetchResponse{Summary: []quotaMetric{}}
	if credits.WindowLimits != nil {
		limited := credits.WindowLimits.Limited
		buckets := make([]quotaBucket, 0, 2)
		for _, w := range []struct {
			name  string
			label string
			limit *ccWindowLimit
		}{
			{"5h", "5-hour window", credits.WindowLimits.FiveHour},
			{"7d", "Weekly window", credits.WindowLimits.Weekly},
		} {
			if w.limit == nil || w.limit.Cap <= 0 {
				continue
			}
			remaining := (w.limit.Cap - w.limit.Used) / w.limit.Cap
			if remaining < 0 {
				remaining = 0
			}
			if remaining > 1 {
				remaining = 1
			}
			bucket := quotaBucket{
				Window:            w.name,
				RemainingFraction: remaining,
				Description: fmt.Sprintf("%s: %.2f / %.2f credits used",
					w.label, w.limit.Used, w.limit.Cap),
			}
			if w.limit.ResetAt > 0 {
				bucket.ResetTime = time.UnixMilli(w.limit.ResetAt).UTC().Format(time.RFC3339)
			}
			buckets = append(buckets, bucket)
		}
		if len(buckets) > 0 {
			resp.Groups = []quotaGroup{{
				DisplayName: "Usage limits" + map[bool]string{true: "", false: " (unlimited)"}[limited],
				Buckets:     buckets,
			}}
		}
	}

	if credits.Credits.MonthlyCredits != nil {
		resp.Summary = append(resp.Summary, quotaMetric{Key: "monthly_credits", Label: "Monthly credits left", Value: round2(*credits.Credits.MonthlyCredits), Unit: "credits", Format: "number"})
	}
	if credits.Credits.PurchasedCredits > 0 {
		resp.Summary = append(resp.Summary,
			quotaMetric{Key: "purchased_credits", Label: "Extra credits", Value: round2(credits.Credits.PurchasedCredits), Unit: "credits", Format: "number"},
		)
	}
	if plan := getPlan(callbackID, apiBase, apiKey, cliVersion, authID); plan.planID != "" {
		resp.Subscription = &quotaSubscription{Plan: plan.planID, TierID: plan.planID}
		if bucket := monthlyQuotaBucket(plan, credits.Credits.MonthlyCredits, time.Now()); bucket != nil {
			resp.Groups = append(resp.Groups, quotaGroup{DisplayName: "Monthly credits", Buckets: []quotaBucket{*bucket}})
		}
	}
	quotaCache.mu.Lock()
	identity := fmt.Sprintf("%s:%x", authID, sha256.Sum256([]byte(apiBase+"\x00"+apiKey)))
	if current := quotaIdentities[authID]; current == "" || current == identity {
		rememberLocalQuota(authID, credits)
	}
	quotaCache.mu.Unlock()
	return resp, nil
}

func getCredits(callbackID, apiBase, apiKey, cliVersion string) (*ccCreditsResponse, int, error) {
	response, errCall := hostHTTPDo(callbackID, hostHTTPRequestMsg{
		Method:  http.MethodGet,
		URL:     strings.TrimRight(apiBase, "/") + "/alpha/billing/credits",
		Headers: buildUsageHeaders(apiKey, cliVersion),
	})
	if errCall != nil {
		return nil, 0, errCall
	}
	if response.StatusCode != http.StatusOK {
		return nil, response.StatusCode, nil
	}
	var parsed ccCreditsResponse
	if errUnmarshal := json.Unmarshal(response.Body, &parsed); errUnmarshal != nil {
		return nil, response.StatusCode, fmt.Errorf("decode billing response: %w", errUnmarshal)
	}
	return &parsed, response.StatusCode, nil
}

// Exact plan allocations shipped by command-code CLI 1.88.2 (getPlanInfo).
// Do not infer allocations for unknown plans or include purchased/free credits
// in the subscription's monthly ratio.
func monthlyQuotaBucket(plan planCacheEntry, remaining *float64, now time.Time) *quotaBucket {
	allocations := map[string]float64{
		"individual-go": 10, "individual-go-v1": 10, "individual-goat": 70,
		"individual-pro": 30, "individual-pro-v1": 80, "individual-provider": 15,
		"individual-max": 150, "individual-ultra": 300, "teams-pro": 40,
	}
	total, known := allocations[plan.planID]
	if !known || plan.status != "active" || remaining == nil || math.IsNaN(*remaining) || math.IsInf(*remaining, 0) {
		return nil
	}
	reset, err := time.Parse(time.RFC3339, plan.period)
	if err != nil || !reset.After(now) {
		return nil
	}
	fraction := math.Max(0, math.Min(1, *remaining/total))
	return &quotaBucket{Window: "monthly", RemainingFraction: fraction, ResetTime: reset.UTC().Format(time.RFC3339),
		Description: fmt.Sprintf("Monthly subscription credits: %.2f / %.2f remaining (CLI plan allocation)", *remaining, total)}
}

func getPlan(callbackID, apiBase, apiKey, cliVersion, authID string) planCacheEntry {
	authID = fmt.Sprintf("%s:%x", authID, sha256.Sum256([]byte(apiBase+"\x00"+apiKey)))
	planCache.mu.Lock()
	if entry, ok := planCache.items[authID]; ok && time.Since(entry.at) < planCacheTTL {
		planCache.mu.Unlock()
		return entry
	}
	planCache.mu.Unlock()

	response, errCall := hostHTTPDo(callbackID, hostHTTPRequestMsg{
		Method:  http.MethodGet,
		URL:     strings.TrimRight(apiBase, "/") + "/alpha/billing/subscriptions",
		Headers: buildUsageHeaders(apiKey, cliVersion),
	})
	if errCall != nil || response.StatusCode != http.StatusOK {
		return planCacheEntry{}
	}
	var parsed ccSubscriptionResponse
	if errUnmarshal := json.Unmarshal(response.Body, &parsed); errUnmarshal != nil {
		return planCacheEntry{}
	}
	planID := strings.TrimSpace(parsed.Data.PlanID)
	period := strings.TrimSpace(parsed.Data.CurrentPeriodEnd)
	planCache.mu.Lock()
	entry := planCacheEntry{at: time.Now(), planID: planID, period: period, status: strings.TrimSpace(parsed.Data.Status)}
	planCache.items[authID] = entry
	planCache.mu.Unlock()
	return entry
}

// buildUsageHeaders mirrors the CLI headers for read-only billing calls.
func buildUsageHeaders(apiKey, cliVersion string) map[string][]string {
	if cliVersion == "" {
		cliVersion = defaultFallbackCLIVersion
	}
	return map[string][]string{
		"Accept":                 {"application/json"},
		"User-Agent":             {fmt.Sprintf("commandcode-cli/%s Node.js/%s", cliVersion, cliNodeVersion)},
		"Authorization":          {"Bearer " + apiKey},
		"x-cli-environment":      {"production"},
		"x-command-code-version": {cliVersion},
		"x-co-flag":              {"false"},
		"x-taste-learning":       {"false"},
	}
}

// recordRateLimitFromBody parses CommandCode's 429 envelope:
//
//	{"success":false,"error":{...,"rateLimit":{"limit":3,"remaining":0,
//	  "reset":<unix|unix_ms>,"window":"fiveHour"}}}
//
// The snapshot is the last line of defence when the billing API is unavailable.
func recordRateLimitFromBody(authID, body string) {
	if authID == "" || strings.TrimSpace(body) == "" {
		return
	}
	var payload struct {
		Error struct {
			RateLimit struct {
				Limit     int    `json:"limit"`
				Remaining int    `json:"remaining"`
				Reset     int64  `json:"reset"`
				Window    string `json:"window"`
			} `json:"rateLimit"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal([]byte(body), &payload); errUnmarshal != nil {
		return
	}
	rate := payload.Error.RateLimit
	if rate.Limit <= 0 {
		return
	}
	var resetAt time.Time
	switch {
	case rate.Reset > 1_000_000_000_000: // milliseconds
		resetAt = time.UnixMilli(rate.Reset)
	case rate.Reset > 0: // seconds
		resetAt = time.Unix(rate.Reset, 0)
	}
	pluginState.recordRateLimit(authID, rate.Window, float64(rate.Limit), float64(rate.Remaining), resetAt)
}

// rememberLocalQuota keeps the freshest window limits for the local fallback.
func rememberLocalQuota(authID string, credits *ccCreditsResponse) {
	if credits == nil || credits.WindowLimits == nil {
		return
	}
	for window, w := range map[string]*ccWindowLimit{"5h": credits.WindowLimits.FiveHour, "7d": credits.WindowLimits.Weekly} {
		if w == nil || w.Cap <= 0 {
			continue
		}
		remaining := w.Cap - w.Used
		if remaining < 0 {
			remaining = 0
		}
		pluginState.recordRateLimit(authID, window, w.Cap, remaining, time.UnixMilli(w.ResetAt))
	}
}

// localQuotaFallback reports local observation when the billing API is
// unreachable or rejects the credential (provider keys cannot read billing).
func localQuotaFallback(authID string, cause error) quotaFetchResponse {
	snapshot := pluginState.snapshot()
	var auth *authSnapshot
	for i := range snapshot.Auths {
		if snapshot.Auths[i].AuthID == authID {
			auth = &snapshot.Auths[i]
			break
		}
	}

	resp := quotaFetchResponse{
		Summary: []quotaMetric{{
			Key:    "source",
			Label:  "Data source",
			Value:  0,
			Unit:   "local",
			Format: "text",
		}},
	}
	if auth != nil {
		resp.Summary = append(resp.Summary, quotaMetric{
			Key: "local_requests_5h", Label: "Requests in last 5h", Value: float64(auth.Requests5h), Unit: "requests", Format: "number",
		})
		for _, rl := range auth.RateLimits {
			if rl != nil && rl.Limit > 0 && rl.ResetAt.After(time.Now()) {
				fraction := float64(rl.Remaining) / float64(rl.Limit)
				if fraction < 0 {
					fraction = 0
				}
				if fraction > 1 {
					fraction = 1
				}
				bucket := quotaBucket{
					Window:            rl.Window,
					RemainingFraction: fraction,
					Description:       fmt.Sprintf("Calibration: %g / %g remaining", rl.Remaining, rl.Limit),
				}
				if !rl.ResetAt.IsZero() {
					bucket.ResetTime = rl.ResetAt.UTC().Format(time.RFC3339)
				}
				resp.Groups = append(resp.Groups, quotaGroup{DisplayName: "Usage limits (estimated)", Buckets: []quotaBucket{bucket}})
			}
		}
	}
	if cause != nil {
		message := cause.Error()
		if strings.Contains(message, "401") {
			message = "额度需 CLI 登录 key（~/.commandcode/auth.json 的 apiKey）；当前 key 只能推理，显示本地统计"
		}
		resp.Summary = append(resp.Summary, quotaMetric{
			Key: "billing_error", Label: message, Format: "text",
		})
	}
	return resp
}

func lookupQuotaCache(authID string) (quotaFetchResponse, bool) {
	quotaCache.mu.Lock()
	defer quotaCache.mu.Unlock()
	if key := quotaIdentities[authID]; key != "" {
		authID = key
	}
	entry, ok := quotaCache.items[authID]
	if !ok || time.Since(entry.at) >= quotaCacheTTL {
		return quotaFetchResponse{}, false
	}
	return entry.resp, true
}

func storeQuotaCache(authID string, resp quotaFetchResponse) {
	quotaCache.mu.Lock()
	quotaCache.items[authID] = quotaCacheEntry{at: time.Now(), resp: resp}
	quotaCache.mu.Unlock()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func round2(value float64) float64 {
	if parsed, err := strconv.ParseFloat(fmt.Sprintf("%.2f", value), 64); err == nil {
		return parsed
	}
	return value
}

// Aliases let the management snapshot resolve the current credential cache.
var quotaIdentities = map[string]string{}

func bindQuotaIdentity(authID, identity string) {
	quotaCache.mu.Lock()
	old := quotaIdentities[authID]
	quotaIdentities[authID] = identity
	quotaCache.mu.Unlock()
	if old != "" && old != identity {
		pluginState.mu.Lock()
		if entry := pluginState.auths[authID]; entry != nil {
			entry.RateLimit = nil
			entry.RateLimits = nil
		}
		pluginState.mu.Unlock()
	}
}
