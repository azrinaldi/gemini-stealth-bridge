package gemini

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ModelInfo describes a model entry from the live otAQ7b RPC registry or static fallback.
type ModelInfo struct {
	ModelID       string   `json:"modelId"`
	ModelName     string   `json:"modelName"`
	DisplayName   string   `json:"displayName"`
	Aliases       []string `json:"aliases,omitempty"`
	Description   string   `json:"description,omitempty"`
	Capacity      int      `json:"capacity"`
	CapacityField int      `json:"capacityField"`
	ModelNumber   int      `json:"modelNumber"`
	// ThinkingLevels holds the thinking levels this model advertises upstream,
	// ordered as the registry lists them. It is empty when the registry has no
	// policy entry for the model, in which case capability detection falls back
	// to name heuristics.
	ThinkingLevels []string `json:"thinkingLevels,omitempty"`
	// ThinkingLabels holds the human-readable label for each ThinkingLevels entry.
	ThinkingLabels []string `json:"thinkingLabels,omitempty"`
	// ThinkingPolicyKnown reports that the registry published a thinking policy
	// and simply did not list this model. That is different from an older registry
	// with no policy at all: an excluded model genuinely has no thinking levels,
	// while a missing policy means we must fall back to name heuristics.
	ThinkingPolicyKnown bool `json:"thinkingPolicyKnown,omitempty"`
}

// QuotaBucket represents one rate-limit/quota bucket returned by the qpEbW RPC.
type QuotaBucket struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	UsagePercentage *float64 `json:"usagePercentage"`
	Total           int      `json:"total"`
	Remaining       int      `json:"remaining"`
	ResetAt         string   `json:"resetAt,omitempty"`
	Unlimited       bool     `json:"unlimited"`
	Synthetic       bool     `json:"synthetic,omitempty"`
	Strikes         int      `json:"-"`
}

// ModelLimits defines the context window and maximum output tokens for a model.
type ModelLimits struct {
	ContextWindow int `json:"contextWindow"`
	MaxOutput     int `json:"maxOutput"`
}

// documentedModelLimits holds the token limits Google publishes for each model
// reachable through the Gemini web UI. Values come from the per-model pages under
// ai.google.dev/gemini-api/docs/models and are keyed by the documented model id
// derived from the registry's display name ("3.5 Flash-Lite" -> "gemini-3.5-flash-lite").
//
// They are deliberately per-model rather than one global number: a future model
// with a smaller window must not silently inherit another model's limit.
var documentedModelLimits = map[string]ModelLimits{
	"gemini-3.5-flash-lite":  {ContextWindow: 1048576, MaxOutput: 65536},
	"gemini-3.8-flash":       {ContextWindow: 1048576, MaxOutput: 65536},
	"gemini-3.1-pro":         {ContextWindow: 1048576, MaxOutput: 65536},
	"gemini-3.1-pro-preview": {ContextWindow: 1048576, MaxOutput: 65536},
	"gemini-4-carbon":        {ContextWindow: 1048576, MaxOutput: 65536},
	// Retained so an account that still serves the pre-rename catalogue keeps its
	// documented limits rather than falling back to the configured guess.
	"gemini-4-argon": {ContextWindow: 1048576, MaxOutput: 65536},
}

// DocumentedModelKeys lists the documented model ids a registry entry could
// correspond to, most specific first.
//
// The live registry carries the versioned name in the description field ("4
// Carbon") while the display name holds the UI label ("Most complex work"), and
// older registries had the two the other way round. Every candidate is tried so
// a labels-are-descriptive response does not silently downgrade all models to
// the configured fallback.
func DocumentedModelKeys(m ModelInfo) []string {
	seen := make(map[string]bool, 3)
	keys := make([]string, 0, 3)
	for _, raw := range []string{m.Description, m.DisplayName, m.ModelName} {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		name = strings.TrimPrefix(name, "gemini-")
		slug := whitespaceRe.ReplaceAllString(strings.ToLower(name), "-")
		if slug == "" {
			continue
		}
		key := "gemini-" + slug
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys
}

// DocumentedModelKey derives the documented model id for a registry entry, e.g.
// "3.5 Flash-Lite" becomes "gemini-3.5-flash-lite". It returns the first
// candidate, which is only meaningful for display; use DocumentedModelKeys when
// the value drives a lookup.
func DocumentedModelKey(m ModelInfo) string {
	keys := DocumentedModelKeys(m)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

// ResolveModelLimits returns the documented limits for a model, falling back to
// the supplied defaults when Google has not published limits for it yet. Source
// reports which of the two was used so callers can tell a verified number from a
// configured guess.
func ResolveModelLimits(m ModelInfo, fallbackContext, fallbackOutput int) (ModelLimits, string) {
	for _, key := range DocumentedModelKeys(m) {
		if lim, ok := documentedModelLimits[key]; ok {
			return lim, "documented"
		}
	}
	if fallbackContext <= 0 {
		fallbackContext = 1048576
	}
	if fallbackOutput <= 0 {
		fallbackOutput = 65536
	}
	return ModelLimits{ContextWindow: fallbackContext, MaxOutput: fallbackOutput}, "config"
}

// ParseThinkingPolicy converts the otAQ7b field 24 policy entries into a map from
// model number to the thinking levels that model advertises. Each entry looks like
//
//	[modelDescriptor, [[1,"Low","Quick and efficient",...],[4,"Medium",...],[2,"High",...]], null, [1]]
//
// where the first element of every level row is Google's Assistant_Lamda_ThinkingLevel
// enum. Only levels whose enum matches a known name are returned, so an unknown
// future enum is dropped rather than advertised with a guessed name.
func ParseThinkingPolicy(policy any) map[int][]string {
	out := make(map[int][]string)
	rows, ok := policy.([]any)
	if !ok || len(rows) < 2 {
		return out
	}
	entries, ok := rows[1].([]any)
	if !ok {
		return out
	}
	for _, entryAny := range entries {
		entry, ok := entryAny.([]any)
		if !ok || len(entry) < 2 {
			continue
		}
		desc, ok := entry[0].([]any)
		if !ok {
			continue
		}
		modelNumber, ok := intAt(desc, 1)
		if !ok || modelNumber <= 0 {
			continue
		}
		levelRows, ok := entry[1].([]any)
		if !ok {
			continue
		}
		levels := make([]string, 0, len(levelRows))
		for _, rowAny := range levelRows {
			row, ok := rowAny.([]any)
			if !ok || len(row) == 0 {
				continue
			}
			enum, ok := intAt(row, 0)
			if !ok {
				continue
			}
			if name, ok := ThinkingLevelName(enum); ok {
				levels = append(levels, name)
			}
		}
		if len(levels) > 0 {
			out[modelNumber] = levels
		}
	}
	return out
}

// pickDefaultEffort returns the level to advertise as default for a model that
// lists levels. Google documents medium as the default where a model offers it;
// otherwise the model's own ordering decides, so the answer is always a level the
// model actually supports.
func pickDefaultEffort(levels []string) string {
	for _, l := range levels {
		if l == "medium" {
			return "medium"
		}
	}
	if len(levels) > 0 {
		return levels[0]
	}
	return ""
}

// ThinkingLevelName maps Google's Assistant_Lamda_ThinkingLevel enum back to its
// effort name. The registry, not this table, is the source of truth for which
// models expose which levels; this only names the values it sees.
func ThinkingLevelName(enum int) (string, bool) {
	switch enum {
	case 1:
		return "low", true
	case 4:
		return "medium", true
	case 2:
		return "high", true
	}
	return "", false
}

// ModelCapabilities describes the features and reasoning effort support of a model.
type ModelCapabilities struct {
	Reasoning     bool     `json:"reasoning"`
	Vision        bool     `json:"vision"`
	Tools         bool     `json:"tools"`
	Search        bool     `json:"search"`
	EffortLevels  []string `json:"effortLevels,omitempty"`
	DefaultEffort string   `json:"defaultEffort"`
}

// ThinkingLevels maps Gemini Web's 3 thinking levels ("low", "medium", "high") to
// Google's Assistant_Lamda_ThinkingLevel protobuf enum (otAQ7b field 24):
//
//	1 = THINKING_LEVEL_STANDARD ("low", default)
//	4 = THINKING_LEVEL_MEDIUM   ("medium")
//	2 = THINKING_LEVEL_EXTENDED ("high")
var ThinkingLevels = map[string]int{
	"low":    1,
	"medium": 4,
	"high":   2,
}

// SupportedThinkingEfforts lists the 3 valid thinking effort levels in order.
var SupportedThinkingEfforts = []string{"low", "medium", "high"}

var reThinkingCapable = regexp.MustCompile(`(?i)thinking|pro|argon|carbon|flash`)

var explicitReasoningModels = map[string]bool{
	"gemini-pro":                true,
	"gemini-flash":              true,
	"gemini-thinking":           true,
	"gemini-carbon":             true,
	"gemini-4-carbon":           true,
	"gemini-argon":              true,
	"gemini-4-argon":            true,
	"gemini-3-pro":              true,
	"gemini-2.5-pro":            true,
	"gemini-2.5-flash":          true,
	"gemini-2.0-flash-thinking": true,
	"gemini-flash-lite":         false,
	"gemini-2.5-flash-lite":     false,
	"gemini-2.0-flash-lite":     false,
}

// IsReasoningModel reports whether modelName supports extended thinking/reasoning.
func IsReasoningModel(modelName string) bool {
	clean, _ := ParseModelRequest(modelName)
	lower := strings.ToLower(strings.TrimSpace(clean))
	if strings.Contains(lower, "lite") {
		return false
	}
	if val, ok := explicitReasoningModels[lower]; ok {
		return val
	}
	return reThinkingCapable.MatchString(lower)
}

// ResolveModelCapabilities returns the structured capabilities for a ModelInfo.
//
// When the registry advertised thinking levels for the model, those levels are
// authoritative. When the registry published a policy but excluded this model,
// the model has no thinking levels and must not be advertised as reasoning just
// because its name contains "flash". Name heuristics remain as a fallback only
// for registries that carry no thinking policy at all, and for the static list.
func ResolveModelCapabilities(m ModelInfo) ModelCapabilities {
	base := ModelCapabilities{
		Vision:        true,
		Tools:         true,
		Search:        true,
		DefaultEffort: "low",
	}
	if len(m.ThinkingLevels) > 0 {
		base.Reasoning = true
		base.EffortLevels = append([]string(nil), m.ThinkingLevels...)
		// The advertised default must be one of the levels this model lists, or
		// clients would send a value the model cannot represent. Google documents
		// medium as the default where it is available.
		base.DefaultEffort = pickDefaultEffort(m.ThinkingLevels)
		return base
	}
	if m.ThinkingPolicyKnown {
		// The registry answered and left this model out: it has no thinking modes.
		return base
	}

	lowerName := strings.ToLower(strings.TrimSpace(m.ModelName))
	lowerDisp := strings.ToLower(strings.TrimSpace(m.DisplayName))
	if strings.Contains(lowerName, "lite") || strings.Contains(lowerDisp, "lite") {
		return base
	}
	reasoning := IsReasoningModel(m.ModelName)
	if !reasoning && m.DisplayName != "" {
		reasoning = IsReasoningModel(m.DisplayName)
	}
	if reasoning {
		base.Reasoning = true
		base.EffortLevels = append([]string(nil), SupportedThinkingEfforts...)
		base.DefaultEffort = "high"
	}
	return base
}

// ComputeCapacity derives the account's capacity and capacityField (12 or 13)
// from otAQ7b's tierFlags (field 16) and capFlags (field 17).
func ComputeCapacity(tierFlags, capFlags []int) (capacity int, capacityField int) {
	has := func(arr []int, v int) bool {
		for _, x := range arr {
			if x == v {
				return true
			}
		}
		return false
	}
	if has(tierFlags, 21) {
		return 1, 13
	}
	if has(tierFlags, 22) {
		return 2, 13
	}
	if has(capFlags, 115) {
		return 4, 12 // Plus
	}
	if has(tierFlags, 16) || has(capFlags, 106) {
		return 3, 12 // Pro
	}
	if has(tierFlags, 8) || has(capFlags, 19) {
		return 2, 12 // Pro
	}
	return 1, 12 // Free
}

// BuildModelHeader constructs the x-goog-ext-525001261-jspb header JSON array
// for a given ModelInfo, thinking level, and f.sid.
func BuildModelHeader(m ModelInfo, thinkLvl int, fSid string) string {
	var arr []any
	if m.CapacityField == 13 {
		arr = []any{
			1, nil, nil, nil, m.ModelID, nil, nil, 0, []int{4, 5, 6, 8},
			nil, nil, nil, m.Capacity, nil, nil, m.ModelNumber, thinkLvl, fSid,
		}
	} else {
		capVal := m.Capacity
		if capVal <= 0 {
			capVal = 1
		}
		numVal := m.ModelNumber
		if numVal <= 0 {
			numVal = 1
		}
		arr = []any{
			1, nil, nil, nil, m.ModelID, nil, nil, 0, []int{4, 5, 6, 8},
			nil, nil, capVal, nil, nil, numVal, thinkLvl, fSid,
		}
	}
	b, _ := json.Marshal(arr)
	return string(b)
}

// knownModelPrefixes lists router/provider prefixes stripped before matching a model name.
var knownModelPrefixes = []string{
	"gsb/",
	"google/",
	"models/",
	"vx/",
	"vertex/",
}

// ParseModelRequest splits an incoming model identifier (e.g. "gsb/gemini-4-argon@think=high")
// into its normalized model name ("gemini-4-argon") and optional inline thinking override ("high").
func ParseModelRequest(raw string) (cleanModel string, inlineThink string) {
	s := strings.TrimSpace(raw)
	if idx := strings.LastIndex(strings.ToLower(s), "@think="); idx >= 0 {
		inlineThink = strings.TrimSpace(s[idx+len("@think="):])
		s = strings.TrimSpace(s[:idx])
	}
	lower := strings.ToLower(s)
	for _, p := range knownModelPrefixes {
		if strings.HasPrefix(lower, p) {
			s = strings.TrimSpace(s[len(p):])
			break
		}
	}
	return s, inlineThink
}

// MatchesModel reports whether a ModelInfo matches the requested model string
// by modelName, modelId, displayName, or alias (with optional router prefix or @think= suffix).
func MatchesModel(m ModelInfo, wanted string) bool {
	w, _ := ParseModelRequest(wanted)
	if w == "" {
		return false
	}
	if strings.EqualFold(m.ModelName, w) || strings.EqualFold(m.ModelID, w) || strings.EqualFold(m.DisplayName, w) {
		return true
	}
	for _, a := range m.Aliases {
		if strings.EqualFold(a, w) {
			return true
		}
	}
	return false
}

// normalizeServedLabel reduces a served model label to a comparable slug:
// "Gemini 4 Argon High" -> "argon", "3.5 Flash-Lite" -> "flash-lite".
func normalizeServedLabel(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "gemini")
	s = strings.TrimLeft(s, " -")
	return strings.Join(strings.Fields(s), "-")
}

// stripVersionTokens removes pure version tokens ("4", "3.5") from a label slug
// so "4 Carbon" and "carbon" compare equal.
func stripVersionTokens(s string) string {
	parts := strings.Split(s, "-")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		isVer := true
		for i := range len(p) {
			if (p[i] < '0' || p[i] > '9') && p[i] != '.' {
				isVer = false
				break
			}
		}
		if !isVer {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "-")
}

// servedLabelNames lists the slugs a served label may legitimately carry for
// this registry entry: the model name, the display name, and the versioned
// description. The registry moved the tier name into the description ("4
// Carbon") while the display name became a UI phrase ("Most complex work"), so
// comparing against the display name alone would reject every real answer.
func servedLabelNames(m ModelInfo) []string {
	names := make([]string, 0, 3)
	for _, raw := range []string{m.ModelName, m.DisplayName, m.Description} {
		if s := normalizeServedLabel(raw); s != "" {
			names = append(names, s)
		}
	}
	return names
}

// stripThinkingSuffix drops a trailing thinking-level word from a label slug.
// Upstream labels an answer with the effort it used ("4 Argon High"), and that
// suffix says nothing about which tier answered.
func stripThinkingSuffix(s string) string {
	for _, suffix := range []string{"-low", "-medium", "-high", "-thinking"} {
		if strings.HasSuffix(s, suffix) {
			return strings.TrimSuffix(s, suffix)
		}
	}
	return s
}

// IsMatchingServedModel checks whether the upstream frame's answeredModelId /
// answeredModelLabel matches the requested ModelInfo (preventing silent downgrades).
func IsMatchingServedModel(m ModelInfo, servedID, servedLabel string) bool {
	if servedID != "" && m.ModelID != "" {
		if strings.EqualFold(servedID, m.ModelID) {
			return true
		}
	}
	if servedLabel != "" {
		s := stripThinkingSuffix(normalizeServedLabel(servedLabel))
		for _, name := range servedLabelNames(m) {
			if s == name {
				return true
			}
		}
		if servedID == "" || m.ModelID == "" {
			sv := stripVersionTokens(s)
			for _, name := range servedLabelNames(m) {
				if sv != "" && sv == stripVersionTokens(name) {
					return true
				}
			}
		}
	}
	return false
}

// ServedLabelMismatch reports whether the upstream answer was labelled with a
// different tier than the registry entry names, while the model id still
// matched. Upstream renames a tier in place: on 2026-11-10 the slot kept id
// 95a4c0abcba24364 but the catalogue called it Carbon while answers still said
// "4 Argon". The id comparison alone accepted that silently, so a client asking
// for the new tier could not tell it was still served the old one.
func ServedLabelMismatch(m ModelInfo, servedID, servedLabel string) bool {
	if servedLabel == "" {
		return false
	}
	if servedID != "" && m.ModelID != "" && !strings.EqualFold(servedID, m.ModelID) {
		// A different backend id is a substitution, not a relabel; the
		// downgrade check already covers it.
		return false
	}
	s := stripThinkingSuffix(normalizeServedLabel(servedLabel))
	if s == "" {
		return false
	}
	sv := stripVersionTokens(s)
	for _, name := range servedLabelNames(m) {
		if s == name {
			return false
		}
		if sv != "" && sv == stripVersionTokens(name) {
			return false
		}
	}
	return true
}

// resolveDefaultThinkingLevel maps a default effort string or enum number to 1 (low), 4 (medium), or 2 (high).
func resolveDefaultThinkingLevel(defaultEffort string) int {
	switch strings.ToLower(strings.TrimSpace(defaultEffort)) {
	case "medium", "4":
		return 4
	case "high", "2":
		return 2
	default:
		return 1 // "low" (default)
	}
}

// ThinkingLevelForModel resolves the thinking level for a registry entry.
//
// It exists so the level comes from what upstream published for this model
// rather than from its name. The name-based heuristics in ThinkingLevelFor are a
// fallback for a registry that carries no thinking policy; when the registry
// does answer, a model that was renamed or newly introduced is described
// correctly without a code change.
func ThinkingLevelForModel(rawEffort any, m ModelInfo, defaultEffort string) int {
	caps := ResolveModelCapabilities(m)
	if len(caps.EffortLevels) == 0 {
		// No levels advertised for this entry: it cannot think, so any effort the
		// caller asked for would be a request upstream cannot represent.
		return 1
	}
	want := ""
	if rawEffort != nil {
		if s, ok := rawEffort.(string); ok {
			want = strings.ToLower(strings.TrimSpace(s))
		}
	}
	if want == "" {
		want = strings.ToLower(strings.TrimSpace(defaultEffortOf(caps, defaultEffort)))
	}
	level := normalizeEffortKey(want)
	for _, advertised := range caps.EffortLevels {
		if advertised == level {
			return ThinkingLevels[advertised]
		}
	}
	// The requested level is not one this model offers (a tier may carry only
	// low/high). Use the model's own advertised default instead of inventing one.
	if def := caps.DefaultEffort; def != "" {
		if lvl, ok := ThinkingLevels[def]; ok {
			return lvl
		}
	}
	if lvl, ok := ThinkingLevels[caps.EffortLevels[0]]; ok {
		return lvl
	}
	return 1
}

// defaultEffortOf prefers the daemon default when the model offers it, else the
// model's own advertised default.
func defaultEffortOf(caps ModelCapabilities, daemonDefault string) string {
	want := normalizeEffortKey(daemonDefault)
	for _, advertised := range caps.EffortLevels {
		if advertised == want {
			return advertised
		}
	}
	return caps.DefaultEffort
}

// normalizeEffortKey maps the loose spellings a client may send onto the three
// Gemini level names.
func normalizeEffortKey(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "medium", "4", "med":
		return "medium"
	case "high", "2", "max", "xhigh", "extended", "enabled", "adaptive", "true":
		return "high"
	case "low", "1", "0", "min", "minimal", "none", "off", "disabled", "false":
		return "low"
	}
	return ""
}

// ThinkingLevelFor resolves the thinking level from a request body effort value
// and model ID, strictly using Gemini Web's 3 levels: low (1), medium (4), high (2),
// with default low (1).
//
// Prefer ThinkingLevelForModel when a ModelInfo is available: this variant has
// only a name to go on, so it falls back to name heuristics.
func ThinkingLevelFor(rawEffort any, modelName, defaultEffort string) int {
	defLvl := resolveDefaultThinkingLevel(defaultEffort)
	if rawEffort == nil || rawEffort == "" {
		if IsReasoningModel(modelName) {
			return defLvl
		}
		return 1
	}
	switch v := rawEffort.(type) {
	case string:
		key := strings.ToLower(strings.TrimSpace(v))
		if key == "" {
			if IsReasoningModel(modelName) {
				return defLvl
			}
			return 1
		}
		if lvl, exists := ThinkingLevels[key]; exists {
			return lvl
		}
		// Normalize numeric or router-clamped strings into the 3 Gemini levels (low=1, medium=4, high=2)
		switch key {
		case "0", "1", "low", "min", "minimal", "none", "off", "disabled", "false":
			return 1
		case "4", "medium", "med":
			return 4
		case "2", "high", "max", "xhigh", "extended", "enabled", "adaptive", "true":
			return 2
		default:
			return defLvl
		}
	case float64:
		switch int(v) {
		case 4:
			return 4
		case 2:
			return 2
		default:
			return 1
		}
	case int:
		switch v {
		case 4:
			return 4
		case 2:
			return 2
		default:
			return 1
		}
	}
	return defLvl
}
