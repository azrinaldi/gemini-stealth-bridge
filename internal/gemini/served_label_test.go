package gemini

import "testing"

// The frontier slot kept its backend id when upstream renamed the tier, so the
// catalogue says Carbon while the answers still say Argon. Comparing ids alone
// accepted that silently: a client asking for the new tier could not tell it was
// still being served the old one.
func carbonEntry() ModelInfo {
	return ModelInfo{
		ModelID:     "95a4c0abcba24364",
		ModelName:   "gemini-carbon",
		DisplayName: "Most complex work",
		Description: "4 Carbon",
	}
}

func TestServedLabelMismatchDetectsRenameInPlace(t *testing.T) {
	m := carbonEntry()
	cases := []struct {
		label string
		want  bool
	}{
		{"4 Argon", true},
		{"4 Argon High", true},
		{"4 Argon Low", true},
		{"Gemini 4 Argon", true},
		{"4 Carbon", false},
		{"4 Carbon High", false},
		{"gemini-carbon", false},
		{"Most complex work", false},
		{"", false},
	}
	for _, c := range cases {
		if got := ServedLabelMismatch(m, m.ModelID, c.label); got != c.want {
			t.Fatalf("ServedLabelMismatch(%q) = %v, want %v", c.label, got, c.want)
		}
	}
}

// A different backend id is a substitution, not a relabel. The downgrade check
// owns that case, so this one must stay quiet and avoid double reporting.
func TestServedLabelMismatchIgnoresDifferentBackendID(t *testing.T) {
	m := carbonEntry()
	if ServedLabelMismatch(m, "8c46e95b1a07cecc", "3.5 Flash-Lite") {
		t.Fatal("a different model id is a substitution, not a label mismatch")
	}
}

// An account that still serves the pre-rename catalogue must not be flagged.
func TestServedLabelMismatchAcceptsPreRenameCatalogue(t *testing.T) {
	m := ModelInfo{
		ModelID:     "95a4c0abcba24364",
		ModelName:   "gemini-argon",
		DisplayName: "Complex tasks",
		Description: "4 Argon",
	}
	if ServedLabelMismatch(m, m.ModelID, "4 Argon") {
		t.Fatal("argon answers to an argon catalogue entry are not a mismatch")
	}
}

// Every real tier must stay quiet: the versioned label is what upstream sends
// while the display name is a UI phrase, so both have to count as a match.
func TestServedLabelMismatchAcceptsEachTier(t *testing.T) {
	cases := []struct {
		model ModelInfo
		label string
	}{
		{ModelInfo{ModelID: "a", ModelName: "gemini-flash-lite", DisplayName: "Quick answers", Description: "3.5 Flash-Lite"}, "3.5 Flash-Lite"},
		{ModelInfo{ModelID: "b", ModelName: "gemini-flash", DisplayName: "All-around help", Description: "3.8 Flash"}, "3.8 Flash"},
		{ModelInfo{ModelID: "c", ModelName: "gemini-pro", DisplayName: "Detailed research", Description: "3.1 Pro"}, "3.1 Pro"},
	}
	for _, c := range cases {
		if ServedLabelMismatch(c.model, c.model.ModelID, c.label) {
			t.Fatalf("%s served as %q must not be a mismatch", c.model.ModelName, c.label)
		}
	}
}

// A mismatch must not turn a working request into an error: the id still names
// the same backend slot, so the existing downgrade guard keeps accepting it.
func TestIsMatchingServedModelStaysLenientOnRelabel(t *testing.T) {
	m := carbonEntry()
	if !IsMatchingServedModel(m, m.ModelID, "4 Argon") {
		t.Fatal("same backend id must still pass the downgrade guard")
	}
	if !IsMatchingServedModel(m, "", "4 Carbon High") {
		t.Fatal("a matching label with an effort suffix must pass")
	}
	if IsMatchingServedModel(m, "", "4 Argon") {
		t.Fatal("a label naming another tier must not pass")
	}
}
