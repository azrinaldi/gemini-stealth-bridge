package gemini

import "testing"

// The frontier tier was renamed upstream on 2026-11-10: "4 Argon" became "4
// Carbon". A name that is not in the reasoning table silently forces thinking
// level low, so the most capable model would answer with the least effort.
func TestCarbonIsReasoningModel(t *testing.T) {
	for _, name := range []string{"gemini-carbon", "gemini-4-carbon"} {
		if !IsReasoningModel(name) {
			t.Fatalf("%s must be a reasoning model", name)
		}
		if got := ThinkingLevelFor(nil, name, "high"); got != 2 {
			t.Fatalf("%s default thinking = %d, want 2 (high)", name, got)
		}
	}
	// The lite tier must stay non-reasoning: it has no thinking levels.
	if IsReasoningModel("gemini-flash-lite") {
		t.Fatal("lite must stay non-reasoning")
	}
}

// The live registry carries the versioned name in the description while the
// display name is a UI label. Every model must still find its documented limits
// instead of silently falling back to the configured guess.
func TestResolveModelLimitsReadsVersionedDescription(t *testing.T) {
	cases := []ModelInfo{
		{ModelName: "gemini-flash-lite", DisplayName: "Quick answers", Description: "3.5 Flash-Lite"},
		{ModelName: "gemini-flash", DisplayName: "All-around help", Description: "3.8 Flash"},
		{ModelName: "gemini-pro", DisplayName: "Detailed research", Description: "3.1 Pro"},
		{ModelName: "gemini-carbon", DisplayName: "Most complex work", Description: "4 Carbon"},
	}
	for _, m := range cases {
		lim, source := ResolveModelLimits(m, 1, 1)
		if source != "documented" {
			t.Fatalf("%s (%q): source = %q, want documented", m.ModelName, m.Description, source)
		}
		if lim.ContextWindow != 1048576 || lim.MaxOutput != 65536 {
			t.Fatalf("%s: limits = %+v", m.ModelName, lim)
		}
	}
}
