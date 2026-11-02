package main

import (
	"testing"
	"time"
)

func TestResolveModel(t *testing.T) {
	cases := map[string]string{
		"":                           "deepseek/deepseek-v4-pro",
		"default":                    "deepseek/deepseek-v4-pro",
		"deepseek-v4-pro":            "deepseek/deepseek-v4-pro",
		"glm5.2":                     "zai-org/GLM-5.2",
		"GLM-5.2":                    "zai-org/GLM-5.2", // bare name, catalog last-segment match
		"Kimi-K3":                    "moonshotai/Kimi-K3",
		"xai/grok-4.5":               "xai/grok-4.5",        // full id passes through
		"custom/vendor-model":        "custom/vendor-model", // unknown org passes through
		"nemotron-3-ultra-550b-a55b": "nvidia/nemotron-3-ultra-550b-a55b",
	}
	for input, want := range cases {
		if got := resolveModel(input); got != want {
			t.Fatalf("resolveModel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestResolveEffortForModel(t *testing.T) {
	if got := resolveEffortForModel("deepseek/deepseek-v4-pro", "low"); got != "high" {
		t.Fatalf("low clips up to high, got %q", got)
	}
	if got := resolveEffortForModel("deepseek/deepseek-v4-pro", "max"); got != "max" {
		t.Fatalf("max must stay reachable, got %q", got)
	}
	if got := resolveEffortForModel("xai/grok-4.5", "xhigh"); got != "high" {
		t.Fatalf("xhigh clips down to high for grok, got %q", got)
	}
	if got := resolveEffortForModel("moonshotai/Kimi-K3", "low"); got != "low" {
		t.Fatalf("uncatalogued effort set passes through, got %q", got)
	}
	if got := resolveEffortForModel("deepseek/deepseek-v4-pro", ""); got != "" {
		t.Fatalf("empty request stays empty, got %q", got)
	}
}

func TestIsClosedModel(t *testing.T) {
	for _, id := range []string{"claude-opus-5", "gpt-5.5", "anthropic/x", "google/gemini-3.7-flash", "openai/gpt-4"} {
		if !isClosedModel(id) {
			t.Fatalf("%q should be closed", id)
		}
	}
	for _, id := range []string{"deepseek/deepseek-v4-pro", "zai-org/GLM-5.2", "moonshotai/Kimi-K3"} {
		if isClosedModel(id) {
			t.Fatalf("%q should be open", id)
		}
	}
}

func TestApplyRemoteCatalogMergesAndFilters(t *testing.T) {
	defer func() {
		catalogMu.Lock()
		currentCat = buildStaticCatalog()
		catalogAt = time.Time{}
		catalogTTL = defaultCatalogTTL
		catalogMu.Unlock()
	}()
	applyRemoteCatalog([]remoteModel{
		{ID: "deepseek/deepseek-v4-pro", Name: "API DeepSeek", ContextLength: 2000000},
		{ID: "brand/new-model", ContextLength: 0},
		{ID: "anthropic/claude", Name: "must be filtered"},
		{ID: "  "},
	}, planContext{}, 0)
	ids := catalogIDs()
	if len(ids) < len(staticCatalog.Builtin) {
		t.Fatalf("static ids must survive, got %d ids", len(ids))
	}
	if displayNameFor("deepseek/deepseek-v4-pro") != "API DeepSeek" {
		t.Fatalf("api name should win, got %q", displayNameFor("deepseek/deepseek-v4-pro"))
	}
	if contextLengthFor("deepseek/deepseek-v4-pro") != 2000000 {
		t.Fatal("api context length should win")
	}
	var foundNew bool
	for _, id := range ids {
		if id == "brand/new-model" {
			foundNew = true
		}
		if id == "anthropic/claude" {
			t.Fatal("closed model leaked into catalog")
		}
	}
	if !foundNew {
		t.Fatal("new api model should be appended")
	}
	if contextLengthFor("brand/new-model") != 128_000 {
		t.Fatal("unknown context falls back to 128k")
	}
}

func planRefFor(value string) planRef {
	return planRef{set: true, value: value}
}

func TestEvaluateModelAccessMatrix(t *testing.T) {
	goPlan := planContext{filter: true, planID: "individual-go-v1"}
	proPlan := planContext{filter: true, planID: "individual-pro"}
	cases := []struct {
		name        string
		category    string
		minimumPlan planRef
		ctx         planContext
		want        bool
	}{
		{"no billing context stays open", "premium", planRefFor("individual-provider"), planContext{}, true},
		{"credits unlock everything", "premium", planRefFor("individual-provider"), planContext{filter: true, planID: "individual-go-v1", creditsUnlock: true}, true},
		{"uncatalogued model stays open", "", planRef{}, goPlan, true},
		{"floor plan is allowed", "opensource", planRefFor("individual-go"), goPlan, true},
		{"higher tier locks", "opensource", planRefFor("individual-goat"), goPlan, false},
		{"top tier allows everything", "premium", planRefFor("individual-ultra"), planContext{filter: true, planID: "individual-ultra"}, true},
		{"pro outranks goat gate", "premium", planRefFor("individual-goat"), proPlan, true},
		{"null minimumPlan locks all", "opensource", planRef{set: true, null: true}, proPlan, false},
		{"category fallback locks premium on go", "premium", planRef{}, goPlan, false},
		{"category fallback allows opensource on go", "opensource", planRef{}, goPlan, true},
		{"category fallback allows premium on pro", "premium", planRef{}, proPlan, true},
		{"unknown minimum plan id falls back to category", "premium", planRefFor("enterprise-custom"), goPlan, false},
		{"unknown minimum plan id, allowed category", "opensource", planRefFor("enterprise-custom"), goPlan, true},
	}
	for _, tc := range cases {
		if got := evaluateModelAccess(tc.category, tc.minimumPlan, tc.ctx); got != tc.want {
			t.Fatalf("%s: evaluateModelAccess(%q, %+v, %+v) = %v, want %v", tc.name, tc.category, tc.minimumPlan, tc.ctx, got, tc.want)
		}
	}
}

// resetCatalog restores the static catalog after a filtering test.
func resetCatalog() {
	catalogMu.Lock()
	currentCat = buildStaticCatalog()
	catalogAt = time.Time{}
	catalogTTL = defaultCatalogTTL
	catalogMu.Unlock()
}

func TestApplyRemoteCatalogLocksModelsByPlan(t *testing.T) {
	defer resetCatalog()
	goCtx := planContext{filter: true, planID: "individual-go-v1"}
	applyRemoteCatalog([]remoteModel{
		{ID: "vendor/open-model", Category: "opensource", MinimumPlan: planRefFor("individual-go")},
		{ID: "vendor/gated-model", Category: "opensource", MinimumPlan: planRefFor("individual-goat")},
		{ID: "vendor/premium-model", Category: "premium"},
		{ID: "vendor/null-model", Category: "opensource", MinimumPlan: planRef{set: true, null: true}},
	}, goCtx, 900)

	ids := catalogIDs()
	for _, id := range ids {
		for _, locked := range []string{"vendor/gated-model", "vendor/premium-model", "vendor/null-model"} {
			if id == locked {
				t.Fatalf("locked model %q leaked into catalog ids", locked)
			}
		}
	}
	var openFound bool
	for _, id := range ids {
		if id == "vendor/open-model" {
			openFound = true
		}
	}
	if !openFound {
		t.Fatal("allowed model must stay listed")
	}
	if required, locked := modelLockReason("vendor/gated-model"); !locked || required != "individual-goat" {
		t.Fatalf("lock reason = %q %v, want individual-goat true", required, locked)
	}
	if _, locked := modelLockReason("vendor/open-model"); locked {
		t.Fatal("allowed model must not report locked")
	}
	if isModelLocked("vendor/premium-model") != true {
		t.Fatal("category-locked model must report locked")
	}
	// Bare-name resolution must skip locked entries and fall through.
	if got := resolveModel("gated-model"); got != "gated-model" {
		t.Fatalf("locked catalog match must be skipped, got %q", got)
	}
	if got := resolveModel("open-model"); got != "vendor/open-model" {
		t.Fatalf("bare name should resolve to the allowed model, got %q", got)
	}
}

func TestApplyRemoteCatalogDegradationsNeverLock(t *testing.T) {
	defer resetCatalog()
	goCtx := planContext{filter: true, planID: "individual-go-v1"}
	gated := []remoteModel{{ID: "vendor/gated-model", Category: "opensource", MinimumPlan: planRefFor("individual-goat")}}

	// Plain-view fallback carries no category: nothing can be evaluated or locked.
	applyRemoteCatalog([]remoteModel{{ID: "vendor/gated-model", ContextLength: 64000}}, goCtx, 0)
	if isModelLocked("vendor/gated-model") {
		t.Fatal("plain view must not lock models")
	}

	// Purchased/free credits unlock every model.
	applyRemoteCatalog(gated, planContext{filter: true, planID: "individual-go-v1", creditsUnlock: true}, 0)
	if isModelLocked("vendor/gated-model") {
		t.Fatal("credits must unlock gated models")
	}

	// Unknown or missing plan context disables filtering entirely.
	applyRemoteCatalog(gated, planContext{}, 0)
	if isModelLocked("vendor/gated-model") {
		t.Fatal("no plan context must not lock models")
	}
	// Lock state clears when gating info disappears again (plain view).
	applyRemoteCatalog(gated, goCtx, 900)
	if !isModelLocked("vendor/gated-model") {
		t.Fatal("sanity: gated model should be locked with a real plan context")
	}
	applyRemoteCatalog([]remoteModel{{ID: "vendor/gated-model"}}, goCtx, 0)
	if isModelLocked("vendor/gated-model") {
		t.Fatal("plain-view refresh must clear the stale lock set")
	}
}

func TestCatalogTTLClosedFromRefreshHint(t *testing.T) {
	defer resetCatalog()
	for _, tc := range []struct {
		hintSeconds int
		want        time.Duration
	}{
		{900, 15 * time.Minute},
		{60, 5 * time.Minute},   // clamped up
		{999999, time.Hour},     // clamped down
		{0, 15 * time.Minute},   // absent keeps default
	} {
		applyRemoteCatalog([]remoteModel{{ID: "vendor/m"}}, planContext{}, tc.hintSeconds)
		catalogMu.RLock()
		got := catalogTTL
		catalogMu.RUnlock()
		if got != tc.want {
			t.Fatalf("hint %ds: catalogTTL = %v, want %v", tc.hintSeconds, got, tc.want)
		}
	}
}
