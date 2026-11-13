package gemini

import (
	"reflect"
	"testing"
)

func TestParseThinkingPolicyReadsUpstreamLevels(t *testing.T) {
	// Shape taken from a live otAQ7b response: field 24 is
	// [policyName, [[modelDescriptor, [[enum,label,description,...],...], null, [1]], ...]].
	policy := []any{
		"launch_ultra_mode_pro_policy",
		[]any{
			[]any{
				[]any{"", float64(6), false, nil, "Flash-Lite", "Fastest answers", nil, false, nil, nil, false, nil, "3.5 Flash-Lite"},
				[]any{
					[]any{float64(1), "Low", "Quick and efficient", false},
					[]any{float64(4), "Medium", "Balanced depth", false},
					[]any{float64(2), "High", "Extra thorough", false},
				},
				nil,
				[]any{float64(1)},
			},
			[]any{
				[]any{"", float64(8), false, nil, "Argon", "Complex tasks", nil, true},
				[]any{
					[]any{float64(1), "Low", "Quick and efficient", false},
					[]any{float64(2), "High", "Extra thorough", false},
				},
				nil,
				[]any{float64(1)},
			},
		},
	}

	got := ParseThinkingPolicy(policy)
	want := map[int][]string{
		6: {"low", "medium", "high"},
		8: {"low", "high"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseThinkingPolicy = %v, want %v", got, want)
	}
}

func TestParseThinkingPolicyDropsUnknownEnums(t *testing.T) {
	policy := []any{
		"policy",
		[]any{
			[]any{
				[]any{"", float64(3)},
				[]any{
					[]any{float64(1), "Low"},
					[]any{float64(9), "Mystery"}, // unknown enum must not be advertised
					[]any{float64(2), "High"},
				},
			},
		},
	}
	got := ParseThinkingPolicy(policy)
	if want := []string{"low", "high"}; !reflect.DeepEqual(got[3], want) {
		t.Fatalf("levels = %v, want %v", got[3], want)
	}
}

func TestParseThinkingPolicyHandlesMissingPayload(t *testing.T) {
	for _, in := range []any{nil, "nope", []any{}, []any{"only-name"}} {
		if got := ParseThinkingPolicy(in); len(got) != 0 {
			t.Fatalf("ParseThinkingPolicy(%v) = %v, want empty", in, got)
		}
	}
}

// The registry is authoritative: a model named "flash" that upstream says has no
// thinking levels must not be advertised as a reasoning model.
func TestResolveModelCapabilitiesPrefersRegistryOverName(t *testing.T) {
	noThinking := ModelInfo{ModelName: "gemini-flash", DisplayName: "3.8 Flash", ThinkingPolicyKnown: true}
	caps := ResolveModelCapabilities(noThinking)
	if caps.Reasoning {
		t.Fatal("model without registry levels must not be advertised as reasoning")
	}
	if len(caps.EffortLevels) != 0 {
		t.Fatalf("effort levels = %v, want none", caps.EffortLevels)
	}

	withThinking := ModelInfo{
		ModelName:      "gemini-flash",
		DisplayName:    "3.8 Flash",
		ThinkingLevels: []string{"low", "medium", "high"},
	}
	caps = ResolveModelCapabilities(withThinking)
	if !caps.Reasoning {
		t.Fatal("model with registry levels must be advertised as reasoning")
	}
	if !reflect.DeepEqual(caps.EffortLevels, []string{"low", "medium", "high"}) {
		t.Fatalf("effort levels = %v", caps.EffortLevels)
	}
}

// A lite model that upstream advertises thinking levels must still be reasoning:
// the heuristic "lite means no thinking" is a fallback, not an override.
func TestResolveModelCapabilitiesLiteWithRegistryLevels(t *testing.T) {
	m := ModelInfo{
		ModelName:      "gemini-flash-lite",
		DisplayName:    "3.5 Flash-Lite",
		ThinkingLevels: []string{"low", "medium", "high"},
	}
	caps := ResolveModelCapabilities(m)
	if !caps.Reasoning {
		t.Fatal("upstream advertises thinking levels for this lite model, so it is a reasoning model")
	}
}

func TestResolveModelCapabilitiesFallbackKeepsLiteNonReasoning(t *testing.T) {
	caps := ResolveModelCapabilities(ModelInfo{ModelName: "gemini-flash-lite", DisplayName: "3.5 Flash-Lite"})
	if caps.Reasoning {
		t.Fatal("without registry levels the lite heuristic must still apply")
	}
}

func TestResolveModelLimitsUsesDocumentedValues(t *testing.T) {
	cases := []ModelInfo{
		{DisplayName: "3.5 Flash-Lite"},
		{DisplayName: "3.8 Flash"},
		{DisplayName: "3.1 Pro"},
		{DisplayName: "4 Argon"},
		{DisplayName: "4 Hydrogen"},
	}
	for _, m := range cases {
		lim, source := ResolveModelLimits(m, 1, 1)
		if source != "documented" {
			t.Fatalf("%s: source = %q, want documented", m.DisplayName, source)
		}
		if lim.ContextWindow != 1048576 || lim.MaxOutput != 65536 {
			t.Fatalf("%s: limits = %+v, want 1048576/65536", m.DisplayName, lim)
		}
	}
}

func TestResolveModelLimitsFallsBackForUnknownModel(t *testing.T) {
	lim, source := ResolveModelLimits(ModelInfo{DisplayName: "9.9 Mystery"}, 200000, 32000)
	if source != "config" {
		t.Fatalf("source = %q, want config", source)
	}
	if lim.ContextWindow != 200000 || lim.MaxOutput != 32000 {
		t.Fatalf("limits = %+v, want the configured fallback", lim)
	}
}

func TestDocumentedModelKey(t *testing.T) {
	cases := map[string]string{
		"3.5 Flash-Lite": "gemini-3.5-flash-lite",
		"3.8 Flash":      "gemini-3.8-flash",
		"3.1 Pro":        "gemini-3.1-pro",
		"4 Argon":        "gemini-4-argon",
		"4 Hydrogen":     "gemini-4-hydrogen",
	}
	for in, want := range cases {
		if got := DocumentedModelKey(ModelInfo{DisplayName: in}); got != want {
			t.Fatalf("DocumentedModelKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// The everyday tier was renamed from Flash to Hydrogen on 2026-11-13. A name
// missing from the reasoning tables silently forces thinking level low, so a
// registry that carries no thinking policy would strip the new tier's thinking.
func TestHydrogenIsReasoningModel(t *testing.T) {
	for _, name := range []string{"gemini-hydrogen", "gemini-4-hydrogen"} {
		if !IsReasoningModel(name) {
			t.Fatalf("%s must be a reasoning model", name)
		}
		if got := ThinkingLevelFor(nil, name, "high"); got != 2 {
			t.Fatalf("%s default thinking = %d, want 2 (high)", name, got)
		}
	}
}

// The registry exposes the tier as `gemini-hydrogen` while the versioned id a
// client reads is `gemini-4-hydrogen`. Matching only aliases left the versioned
// id unresolved and returned 404 model_not_found.
func TestMatchesModelAcceptsVersionedDescription(t *testing.T) {
	m := ModelInfo{
		ModelID:     "hydrogen-id",
		ModelName:   "gemini-hydrogen",
		DisplayName: "Everyday help",
		Description: "4 Hydrogen",
	}
	for _, wanted := range []string{"gemini-hydrogen", "gemini-4-hydrogen", "gsb/gemini-4-hydrogen@think=high"} {
		if !MatchesModel(m, wanted) {
			t.Fatalf("MatchesModel(%q) = false, want true", wanted)
		}
	}
}

// The live catalogue advertises both `4 Hydrogen` and `4 Hydrogen Thinking`;
// each must resolve to the same documented limits rather than a config guess.
func TestResolveModelLimitsCoversHydrogenThinking(t *testing.T) {
	m := ModelInfo{
		ModelName:   "gemini-hydrogen-thinking",
		DisplayName: "Solves complex problems",
		Description: "4 Hydrogen Thinking",
	}
	lim, source := ResolveModelLimits(m, 1, 1)
	if source != "documented" {
		t.Fatalf("source = %q, want documented", source)
	}
	if lim.ContextWindow != 1048576 || lim.MaxOutput != 65536 {
		t.Fatalf("limits = %+v", lim)
	}
}
