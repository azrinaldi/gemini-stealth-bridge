package server

import "testing"

// The frontier tier is billed against the "Gemini Pro" bucket. After the
// 2026-11-10 rename to "4 Carbon" an empty name match would drop the family
// tokens entirely, so an exhausted bucket would be reported against the wrong
// model (or not at all).
func TestModelFamilyTokensCoversFrontierTier(t *testing.T) {
	for _, name := range []string{"gemini-argon", "gemini-4-argon", "gemini-carbon", "gemini-4-carbon"} {
		family := modelFamilyTokens(name)
		if len(family) == 0 {
			t.Fatalf("%s: family tokens must not be empty", name)
		}
		found := false
		for _, tok := range family {
			if tok == "pro" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s: family = %v, want a pro token", name, family)
		}
	}
}
