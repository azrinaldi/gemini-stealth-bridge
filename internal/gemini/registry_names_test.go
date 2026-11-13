package gemini

import (
	"slices"
	"testing"
)

// The live registry keeps the short tier name in the category ("Carbon") while
// the description carries the versioned product name ("4 Carbon"). Publishing
// the bare spelling as the id made a downstream router resolve it against a
// capability table keyed by versioned ids, miss, and silently drop
// reasoning_effort and the 1M window. The versioned spelling is the id now; the
// bare one has to keep resolving as an alias for pinned configurations.
func TestRegistryModelNames(t *testing.T) {
	cases := []struct {
		name        string
		modelID     string
		category    string
		displayName string
		description string
		wantID      string
		wantAliases []string
	}{
		{
			// The 2026-11-13 catalogue: category "Carbon", description "4 Carbon",
			// display "Most complex work".
			name:        "category and description disagree on the version",
			modelID:     "95a4c0abcba24364",
			category:    "Carbon",
			displayName: "Most complex work",
			description: "4 Carbon",
			wantID:      "gemini-4-carbon",
			wantAliases: []string{"gemini-carbon", "gemini-most-complex-work"},
		},
		{
			// Hydrogen is the one tier whose category already carries the version,
			// so the id was versioned before this change and there is no bare
			// spelling to preserve.
			name:        "hydrogen is already versioned in the category",
			modelID:     "56fdd199312815e2",
			category:    "4 Hydrogen",
			displayName: "Everyday help",
			description: "4 Hydrogen",
			wantID:      "gemini-4-hydrogen",
			wantAliases: []string{"gemini-everyday-help"},
		},
		{
			name:        "pro keeps its documented version",
			modelID:     "e6fa609c3fa255c0",
			category:    "Pro",
			displayName: "Detailed research",
			description: "3.1 Pro",
			wantID:      "gemini-3.1-pro",
			wantAliases: []string{"gemini-pro", "gemini-detailed-research"},
		},
		{
			name:        "flash-lite keeps its documented version",
			modelID:     "8c46e95b1a07cecc",
			category:    "Flash-Lite",
			displayName: "Quick answers",
			description: "3.5 Flash-Lite",
			wantID:      "gemini-3.5-flash-lite",
			wantAliases: []string{"gemini-flash-lite", "gemini-quick-answers"},
		},
		{
			// No description to version from: the category is the only name there
			// is, so it stays the id and must not turn into a self-alias.
			name:        "no versioned description falls back to the category",
			modelID:     "abc123",
			category:    "3.7 Flash",
			displayName: "3.7 Flash",
			description: "",
			wantID:      "gemini-3.7-flash",
			wantAliases: []string{},
		},
		{
			name:        "empty category uses the model id",
			modelID:     "deadbeef",
			category:    "",
			displayName: "",
			description: "",
			wantID:      "gemini-deadbeef",
			wantAliases: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotAliases := registryModelNames(tc.modelID, tc.category, tc.displayName, tc.description)
			if gotID != tc.wantID {
				t.Errorf("id = %q, want %q", gotID, tc.wantID)
			}
			if !slices.Equal(gotAliases, tc.wantAliases) {
				t.Errorf("aliases = %v, want %v", gotAliases, tc.wantAliases)
			}
			// An alias must never repeat the id or another alias.
			seen := map[string]bool{gotID: true}
			for _, a := range gotAliases {
				if seen[a] {
					t.Errorf("duplicate alias %q in %v (id %q)", a, gotAliases, gotID)
				}
				seen[a] = true
			}
		})
	}
}

// The LookupModel path is what a client request goes through, so the alias the
// id used to be has to match the same entry it named before the change.
func TestBareAliasStillMatchesVersionedID(t *testing.T) {
	model := ModelInfo{
		ModelID:     "95a4c0abcba24364",
		ModelName:   "gemini-4-carbon",
		DisplayName: "Most complex work",
		Description: "4 Carbon",
		Aliases:     []string{"gemini-carbon", "gemini-most-complex-work"},
	}
	for _, wanted := range []string{"gemini-4-carbon", "gemini-carbon", "gemini-most-complex-work", "gsb/gemini-carbon", "gemini-carbon@think=high"} {
		if !MatchesModel(model, wanted) {
			t.Errorf("MatchesModel(%q) = false, want true", wanted)
		}
	}
	if MatchesModel(model, "gemini-argon") {
		t.Error("MatchesModel(gemini-argon) = true: argon is not a name in this catalogue")
	}
}
