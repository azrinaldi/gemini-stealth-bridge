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
		{ModelInfo{ModelID: "d", ModelName: "gemini-hydrogen", DisplayName: "Everyday help", Description: "4 Hydrogen"}, "4 Hydrogen"},
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

// A tier whose own name ends in "Thinking" must not be reported as a relabel.
// The live 2026-11-13 catalogue advertises `4 Hydrogen Thinking` (id
// 5bf011840784117a); stripping the suffix from the served label alone reduced it
// to "4-hydrogen", which matched no entry and flagged every correct answer.
func TestServedLabelMismatchAcceptsThinkingTierName(t *testing.T) {
	m := ModelInfo{
		ModelID:     "5bf011840784117a",
		ModelName:   "gemini-hydrogen-thinking",
		DisplayName: "Solves complex problems",
		Description: "4 Hydrogen Thinking",
	}
	for _, label := range []string{"4 Hydrogen Thinking", "4 Hydrogen Thinking High", "gemini-hydrogen-thinking"} {
		if ServedLabelMismatch(m, m.ModelID, label) {
			t.Fatalf("%q must not be a mismatch", label)
		}
	}
	if !IsMatchingServedModel(m, m.ModelID, "4 Hydrogen Thinking") {
		t.Fatal("the tier's own label must pass the downgrade guard")
	}
	// "4 Hydrogen" alone is ambiguous between the two tiers: the catalogue lists
	// both and only the backend id separates them. A dropped suffix is reported
	// by the substitution check when the id differs, so it must stay quiet here.
	if ServedLabelMismatch(m, m.ModelID, "4 Hydrogen") {
		t.Fatal("an id-matched answer whose label drops \"Thinking\" must not be a relabel")
	}
}

// The free tier's catalogue entry is `gemini-auto` ("Auto — adapts to your
// needs"): upstream picks a backing model per request and labels the answer with
// the model it chose. Reported live on the VPS pool as id cf41b0e0dd7d53e5 with
// the label "3.5 Flash-Lite", which the relabel guard flagged on every request
// even though the id named the requested slot.
func TestServedLabelMismatchIgnoresRouterTier(t *testing.T) {
	auto := ModelInfo{
		ModelID:     "cf41b0e0dd7d53e5",
		ModelName:   "gemini-auto",
		DisplayName: "Auto",
		Description: "Adapts to your needs",
	}
	for _, label := range []string{"3.5 Flash-Lite", "3.5 Flash-Lite High", "4 Hydrogen", "3.1 Pro"} {
		if ServedLabelMismatch(auto, auto.ModelID, label) {
			t.Fatalf("router tier served as %q must not be a relabel", label)
		}
	}
}

// The exemption is limited to router tiers: a fixed tier that answers under
// another label must still be reported.
func TestServedLabelMismatchStillReportsFixedTier(t *testing.T) {
	if !ServedLabelMismatch(carbonEntry(), carbonEntry().ModelID, "4 Argon") {
		t.Fatal("a fixed tier relabelled as another tier must still be reported")
	}
}
