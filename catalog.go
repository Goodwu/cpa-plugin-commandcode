package main

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// catalog.json is the hand-maintained model metadata vendored from the
// reference proxy: builtin ids (stable default order), short aliases, context
// windows, display names, per-model reasoning-effort sets, and max output
// tokens. The provider API (when reachable) only adds/overrides ids, names,
// and context windows — everything else lives here.
//
//go:embed catalog.json
var catalogJSON []byte

type catalogData struct {
	ClosedModelOrgs  []string            `json:"closedModelOrgs"`
	Builtin          []string            `json:"builtin"`
	ShortAliases     map[string]string   `json:"shortAliases"`
	ContextWindows   map[string]int64    `json:"contextWindows"`
	ModelNames       map[string]string   `json:"modelNames"`
	ReasoningEfforts map[string][]string `json:"reasoningEfforts"`
	MaxOutputTokens  map[string]int64    `json:"maxOutputTokens"`
}

// effortRank orders effort levels low -> max; unknown values rank as medium so
// clipping still picks a sensible nearest level.
var effortRank = map[string]int{"low": 0, "medium": 1, "high": 2, "xhigh": 3, "max": 4}

func rankEffort(effort string) int {
	if r, ok := effortRank[effort]; ok {
		return r
	}
	return 1
}

var staticCatalog catalogData

// init, not a var initializer: the initializer for currentCat would run
// before this unmarshal and bake in an empty catalog.
func init() {
	_ = json.Unmarshal(catalogJSON, &staticCatalog)
	if staticCatalog.Builtin == nil {
		staticCatalog.Builtin = []string{}
	}
	currentCat = buildStaticCatalog()
	staticOrgSet = buildOrgSet()
}

// modelCatalog is the resolved (static or API-merged) view used by resolution.
type modelCatalog struct {
	ids         []string
	displayName map[string]string
	contextLen  map[string]int64
}

var (
	catalogMu    sync.RWMutex
	currentCat   *modelCatalog
	catalogAt    time.Time
	catalogTTL   = time.Hour
	staticOrgSet map[string]struct{}
)

func buildOrgSet() map[string]struct{} {
	orgs := map[string]struct{}{}
	for _, org := range staticCatalog.ClosedModelOrgs {
		orgs[strings.ToLower(org)] = struct{}{}
	}
	if len(orgs) == 0 {
		for _, org := range []string{"anthropic", "openai", "google", "gemini"} {
			orgs[org] = struct{}{}
		}
	}
	return orgs
}

func buildStaticCatalog() *modelCatalog {
	cat := &modelCatalog{
		ids:         append([]string(nil), staticCatalog.Builtin...),
		displayName: make(map[string]string, len(staticCatalog.Builtin)),
		contextLen:  make(map[string]int64, len(staticCatalog.Builtin)),
	}
	for _, id := range cat.ids {
		name := staticCatalog.ModelNames[id]
		if name == "" {
			name = id[strings.LastIndex(id, "/")+1:]
		}
		cat.displayName[id] = name
		size := staticCatalog.ContextWindows[id]
		if size == 0 {
			size = 128_000
		}
		cat.contextLen[id] = size
	}
	return cat
}

// catalogIDs returns the active catalog ids (static order first).
func catalogIDs() []string {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	return append([]string(nil), currentCat.ids...)
}

// applyRemoteCatalog merges the provider API list over the static catalog:
// static entries keep their positions and metadata; API entries win for
// display name and context window and may add new ids. Closed orgs
// (anthropic/openai/google/...) are filtered out — this plugin only targets
// the open-weight catalog. Junk entries are skipped without discarding the
// refresh. An empty or failed result keeps the current catalog unchanged.
func applyRemoteCatalog(remote []remoteModel) {
	filtered := make([]remoteModel, 0, len(remote))
	for _, model := range remote {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			id = strings.TrimSpace(model.Model)
		}
		if id == "" || isClosedModel(id) {
			continue
		}
		model.ID = id
		filtered = append(filtered, model)
	}
	if len(filtered) == 0 {
		return
	}
	byID := make(map[string]remoteModel, len(filtered))
	for _, model := range filtered {
		if _, seen := byID[strings.ToLower(model.ID)]; !seen {
			byID[strings.ToLower(model.ID)] = model
		}
	}
	merged := &modelCatalog{
		ids:         make([]string, 0, len(staticCatalog.Builtin)+len(filtered)),
		displayName: map[string]string{},
		contextLen:  map[string]int64{},
	}
	seen := map[string]struct{}{}
	for _, id := range staticCatalog.Builtin {
		merged.ids = append(merged.ids, id)
		seen[strings.ToLower(id)] = struct{}{}
		name := staticCatalog.ModelNames[id]
		size := staticCatalog.ContextWindows[id]
		if remote, ok := byID[strings.ToLower(id)]; ok {
			if remote.Name != "" {
				name = remote.Name
			}
			if remote.ContextLength > 0 {
				size = int64(remote.ContextLength)
			}
		}
		if name == "" {
			name = id[strings.LastIndex(id, "/")+1:]
		}
		if size == 0 {
			size = 128_000
		}
		merged.displayName[id] = name
		merged.contextLen[id] = size
	}
	for _, model := range filtered {
		if _, dup := seen[strings.ToLower(model.ID)]; dup {
			continue
		}
		seen[strings.ToLower(model.ID)] = struct{}{}
		merged.ids = append(merged.ids, model.ID)
		name := model.Name
		if name == "" {
			name = model.ID[strings.LastIndex(model.ID, "/")+1:]
		}
		size := int64(model.ContextLength)
		if size == 0 {
			size = 128_000
		}
		merged.displayName[model.ID] = name
		merged.contextLen[model.ID] = size
	}
	catalogMu.Lock()
	currentCat = merged
	catalogAt = time.Now()
	catalogMu.Unlock()
}

func catalogNeedsRefresh() bool {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	return time.Since(catalogAt) >= catalogTTL
}

// isClosedModel detects models the CC bridge serves for paid provider tiers
// (bare "claude-*"/"gpt-*" ids or closed org prefixes) that /alpha/generate
// traffic should not target.
func isClosedModel(id string) bool {
	lower := strings.ToLower(strings.TrimSpace(id))
	if strings.HasPrefix(lower, "claude-") || strings.HasPrefix(lower, "gpt-") {
		return true
	}
	org, _, _ := strings.Cut(lower, "/")
	_, closed := staticOrgSet[org]
	return closed
}

// resolveModel maps an alias or bare name to the canonical catalog id. Full
// ids pass through untouched (the upstream does not validate against a fixed
// list, so unknown "org/model" ids remain usable).
func resolveModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" || model == "default" {
		ids := catalogIDs()
		if len(ids) > 0 {
			return ids[0]
		}
		return "deepseek/deepseek-v4-pro"
	}
	if aliased, ok := staticCatalog.ShortAliases[model]; ok {
		return aliased
	}
	if aliased, ok := staticCatalog.ShortAliases[strings.ToLower(model)]; ok {
		return aliased
	}
	if strings.Contains(model, "/") {
		return model
	}
	lower := strings.ToLower(model)
	for _, id := range catalogIDs() {
		last := id[strings.LastIndex(id, "/")+1:]
		if strings.EqualFold(last, lower) {
			return id
		}
	}
	return model
}

// resolveEffortForModel clips a requested reasoning effort to the model's
// supported set: nearest valid level at or below the request, else the lowest
// supported. Uncatalogued models pass the request through unchanged.
func resolveEffortForModel(canonical, requested string) string {
	if requested == "" {
		return ""
	}
	supported, ok := staticCatalog.ReasoningEfforts[canonical]
	if !ok || len(supported) == 0 {
		return requested
	}
	for _, level := range supported {
		if level == requested {
			return requested
		}
	}
	reqRank := rankEffort(requested)
	best := ""
	for _, level := range supported {
		r := rankEffort(level)
		if r <= reqRank && (best == "" || r > rankEffort(best)) {
			best = level
		}
	}
	if best != "" {
		return best
	}
	for _, level := range supported {
		if best == "" || rankEffort(level) < rankEffort(best) {
			best = level
		}
	}
	return best
}

func displayNameFor(id string) string {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	if name, ok := currentCat.displayName[id]; ok && name != "" {
		return name
	}
	if name, ok := staticCatalog.ModelNames[id]; ok && name != "" {
		return name
	}
	return id[strings.LastIndex(id, "/")+1:]
}

func contextLengthFor(id string) int64 {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	if size, ok := currentCat.contextLen[id]; ok && size > 0 {
		return size
	}
	if size, ok := staticCatalog.ContextWindows[id]; ok && size > 0 {
		return size
	}
	return 128_000
}
