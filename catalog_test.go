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
		name         string
		id           string
		category     string
		minimumPlan  planRef
		ctx          planContext
		wantAllowed  bool
		wantRequired string
	}{
		{"no billing context stays open", "m", "premium", planRefFor("individual-provider"), planContext{}, true, ""},
		{"credits unlock everything", "m", "premium", planRefFor("individual-provider"), planContext{filter: true, planID: "individual-go-v1", creditsUnlock: true}, true, ""},
		{"uncatalogued model stays open", "m", "", planRef{}, goPlan, true, ""},
		{"unknown category never gates", "m", "internal-preview", planRefFor("individual-goat"), goPlan, true, ""},
		{"floor plan is allowed", "m", "opensource", planRefFor("individual-go"), goPlan, true, "individual-go"},
		{"higher tier locks", "m", "opensource", planRefFor("individual-goat"), goPlan, false, "individual-goat"},
		{"top tier allows everything", "m", "premium", planRefFor("individual-ultra"), planContext{filter: true, planID: "individual-ultra"}, true, "individual-ultra"},
		{"pro outranks goat gate", "m", "premium", planRefFor("individual-goat"), proPlan, true, "individual-goat"},
		{"null minimumPlan locks all, no tier satisfies", "m", "opensource", planRef{set: true, null: true}, proPlan, false, ""},
		{"category fallback locks premium on go, names Pro", "m", "premium", planRef{}, goPlan, false, "individual-pro"},
		{"category fallback allows opensource on go", "m", "opensource", planRef{}, goPlan, true, "individual-go"},
		{"category fallback allows premium on pro", "m", "premium", planRef{}, proPlan, true, "individual-pro"},
		// CLI chain rankedAccess → embeddedAccess → categoryAccess: an unknown
		// tier value consults the embedded snapshot (xai/grok-4.6 = goat there)
		// before category gating — the reviewer's counterexample.
		{"unknown tier falls back to embedded goat gate", "xai/grok-4.6", "opensource", planRefFor("future-plan"), goPlan, false, "individual-goat"},
		{"unknown tier not in embedded falls to category", "vendor/brand-new", "premium", planRefFor("future-plan"), goPlan, false, "individual-pro"},
		{"unknown tier, embedded category disagrees", "xai/grok-4.6", "premium", planRefFor("future-plan"), goPlan, false, "individual-pro"},
		{"unknown tier, allowed category", "vendor/brand-new", "opensource", planRefFor("enterprise-custom"), goPlan, true, "individual-go"},
	}
	for _, tc := range cases {
		gotAllowed, gotRequired := evaluateModelAccess(tc.id, tc.category, tc.minimumPlan, tc.ctx)
		if gotAllowed != tc.wantAllowed || gotRequired != tc.wantRequired {
			t.Fatalf("%s: evaluateModelAccess(%q, %q, %+v, %+v) = (%v, %q), want (%v, %q)",
				tc.name, tc.id, tc.category, tc.minimumPlan, tc.ctx, gotAllowed, gotRequired, tc.wantAllowed, tc.wantRequired)
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
	if required, locked := modelLockReason("vendor/premium-model"); !locked || required != "individual-pro" {
		t.Fatalf("category lock must name the category floor Pro, got %q %v", required, locked)
	}
	if required, locked := modelLockReason("vendor/null-model"); !locked || required != "" {
		t.Fatalf("null lock has no satisfiable tier, got %q %v", required, locked)
	}
	if _, locked := modelLockReason("vendor/open-model"); locked {
		t.Fatal("allowed model must not report locked")
	}
	if isModelLocked("vendor/premium-model") != true {
		t.Fatal("category-locked model must report locked")
	}
	if _, locked := lockedBareNameReason("gated-model"); !locked {
		t.Fatal("bare name of a locked model must match the lock set")
	}
	if _, locked := lockedBareNameReason("open-model"); locked {
		t.Fatal("bare name of an allowed model must not match")
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

// TestSwitchOffInvalidatesPublishedLockSnapshot covers the hot-reconfigure
// path the second review flagged: a locked snapshot published while the
// switch was on must stop filtering the moment the switch turns off — no
// catalog refresh required — and resume if the switch turns back on.
func TestSwitchOffInvalidatesPublishedLockSnapshot(t *testing.T) {
	defer resetCatalog()
	defer setPluginConfig(nil)
	goCtx := planContext{filter: true, planID: "individual-go-v1"}
	applyRemoteCatalog([]remoteModel{
		{ID: "vendor/gated-model", Category: "opensource", MinimumPlan: planRefFor("individual-goat")},
	}, goCtx, 900)
	if !isModelLocked("vendor/gated-model") {
		t.Fatal("sanity: model must be locked while the switch is on")
	}

	setPluginConfig([]byte("filter_locked_models: false\n"))
	// No refresh happens between the two switch positions (stale snapshot).
	if isModelLocked("vendor/gated-model") {
		t.Fatal("switch off must invalidate the published lock set immediately")
	}
	found := false
	for _, id := range catalogIDs() {
		if id == "vendor/gated-model" {
			found = true
		}
	}
	if !found {
		t.Fatal("switch off must list the previously locked model")
	}
	if got := resolveModel("gated-model"); got != "vendor/gated-model" {
		t.Fatalf("bare name must resolve again while off, got %q", got)
	}
	if env := lockedModelEnvelope("vendor/gated-model"); env != nil {
		t.Fatalf("switch off must not reject requests, got %s", env)
	}

	// A refresh that captured its plan context before the switch flipped may
	// still publish a locked snapshot afterwards; it must stay inert while off.
	applyRemoteCatalog([]remoteModel{
		{ID: "vendor/gated-model", Category: "opensource", MinimumPlan: planRefFor("individual-goat")},
	}, goCtx, 900)
	if isModelLocked("vendor/gated-model") {
		t.Fatal("in-flight refresh must not re-enable filtering while off")
	}

	setPluginConfig([]byte("filter_locked_models: true\n"))
	if !isModelLocked("vendor/gated-model") {
		t.Fatal("switch back on restores the last known lock state")
	}
}

// TestLockedBareNameReasonPicksLowestTier pins the deterministic claim for
// same-suffix collisions: the lowest tier that unlocks one of the models.
func TestLockedBareNameReasonPicksLowestTier(t *testing.T) {
	defer resetCatalog()
	defer setPluginConfig(nil)
	applyRemoteCatalog([]remoteModel{
		{ID: "a/dup", Category: "opensource", MinimumPlan: planRefFor("individual-goat")},
		{ID: "b/dup", Category: "opensource", MinimumPlan: planRefFor("individual-pro")},
		{ID: "c/dup", Category: "opensource", MinimumPlan: planRef{set: true, null: true}},
	}, planContext{filter: true, planID: "individual-go-v1"}, 900)
	required, locked := lockedBareNameReason("dup")
	if !locked || required != "individual-goat" {
		t.Fatalf("bare collision = %q %v, want individual-goat true (lowest claimable tier)", required, locked)
	}
	if _, locked := lockedBareNameReason("unmatched"); locked {
		t.Fatal("unmatched bare name must not lock")
	}
}

// TestCatalogAndLockConsultsRaceFree exercises concurrent consultation and
// publish under the race detector.
func TestCatalogAndLockConsultsRaceFree(t *testing.T) {
	defer resetCatalog()
	defer setPluginConfig(nil)
	goCtx := planContext{filter: true, planID: "individual-go-v1"}
	models := []remoteModel{
		{ID: "vendor/gated-model", Category: "opensource", MinimumPlan: planRefFor("individual-goat")},
		{ID: "vendor/open-model", Category: "opensource", MinimumPlan: planRefFor("individual-go")},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			applyRemoteCatalog(models, goCtx, 900)
		}
	}()
	for i := 0; i < 200; i++ {
		catalogIDs()
		isModelLocked("vendor/gated-model")
		lockedBareNameReason("gated-model")
		displayNameFor("vendor/open-model")
	}
	<-done
}
