package gemini

import "testing"

// The point of these cases is that no model name appears in the expectation.
// A tier the registry renames, or a new one it introduces, must be described by
// what the registry published for it, not by a table in this repo.
func TestThinkingLevelForModelFollowsRegistry(t *testing.T) {
	// A renamed or brand-new tier: the name means nothing to the old tables.
	renamed := ModelInfo{
		ModelID:        "95a4c0abcba24364",
		ModelName:      "gemini-zircon",
		DisplayName:    "Most complex work",
		Description:    "5 Zircon",
		ThinkingLevels: []string{"low", "high"},
	}

	// Its own default is high, and the daemon default high is offered, so high.
	if got := ThinkingLevelForModel(nil, renamed, "high"); got != 2 {
		t.Fatalf("daemon default high: level = %d, want 2", got)
	}
	// Daemon default low is offered too, so it wins when configured.
	if got := ThinkingLevelForModel(nil, renamed, "low"); got != 1 {
		t.Fatalf("daemon default low: level = %d, want 1", got)
	}
	// medium is not advertised for this model: fall back to its own default,
	// which is the first level it lists (low) since it offers no medium.
	if got := ThinkingLevelForModel("medium", renamed, "low"); got != 1 {
		t.Fatalf("unadvertised medium: level = %d, want the model default 1", got)
	}
	// An advertised level the caller asks for is honoured.
	if got := ThinkingLevelForModel("low", renamed, "high"); got != 1 {
		t.Fatalf("explicit low: level = %d, want 1", got)
	}
}

// A model the registry says has no thinking levels must not be given one, no
// matter what its name looks like.
func TestThinkingLevelForModelRespectsNoLevels(t *testing.T) {
	noThinking := ModelInfo{
		ModelName:           "gemini-flash",
		ThinkingPolicyKnown: true,
	}
	if got := ThinkingLevelForModel("high", noThinking, "high"); got != 1 {
		t.Fatalf("level = %d, want 1 for a model with no thinking levels", got)
	}
}

// With no policy published at all, the name heuristic still answers, so an old
// registry keeps working.
func TestThinkingLevelForModelFallsBackToName(t *testing.T) {
	legacy := ModelInfo{ModelName: "gemini-pro"}
	caps := ResolveModelCapabilities(legacy)
	if len(caps.EffortLevels) == 0 {
		t.Fatal("a registry with no policy must still advertise levels by heuristic")
	}
	if got := ThinkingLevelForModel("high", legacy, "low"); got != 2 {
		t.Fatalf("level = %d, want 2", got)
	}
}
