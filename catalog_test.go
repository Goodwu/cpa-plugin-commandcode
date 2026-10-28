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
		catalogMu.Unlock()
	}()
	applyRemoteCatalog([]remoteModel{
		{ID: "deepseek/deepseek-v4-pro", Name: "API DeepSeek", ContextLength: 2000000},
		{ID: "brand/new-model", ContextLength: 0},
		{ID: "anthropic/claude", Name: "must be filtered"},
		{ID: "  "},
	})
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
