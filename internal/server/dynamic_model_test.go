package server

import (
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

// Model selection follows the registry. These entries are names the static
// tables have never heard of, so a passing result can only come from the list
// the account actually serves.
func TestResolveModelOnAccountFollowsRegistry(t *testing.T) {
	a := &AccountState{}
	a.Models = []gemini.ModelInfo{
		{ModelName: "gemini-zircon", ThinkingPolicyKnown: true},
		{ModelName: "gemini-topaz", ThinkingPolicyKnown: true},
	}

	// An unspecified request takes the account's first served model, whatever it
	// is called. Nothing written out can go stale here.
	got := resolveModelOnAccount(a, "")
	if got == nil || got.ModelName != "gemini-zircon" {
		t.Fatalf("unspecified: got %v, want gemini-zircon", got)
	}
	// A name the registry serves resolves by name.
	got = resolveModelOnAccount(a, "gemini-topaz")
	if got == nil || got.ModelName != "gemini-topaz" {
		t.Fatalf("by name: got %v, want gemini-topaz", got)
	}
	// A name it does not serve resolves to nothing, so the caller reports the
	// model as unavailable instead of silently substituting.
	if got := resolveModelOnAccount(a, "gemini-absent"); got != nil {
		t.Fatalf("absent: got %v, want nil", got)
	}
	// An account with no models answers nil instead of panicking.
	if got := resolveModelOnAccount(&AccountState{}, ""); got != nil {
		t.Fatalf("empty: got %v, want nil", got)
	}
}

// A search with no model override uses the account's default model: one rule,
// not a second list that can disagree with it.
func TestResolveSearchModelUsesAccountDefault(t *testing.T) {
	a := &AccountState{}
	a.Models = []gemini.ModelInfo{
		{ModelName: "gemini-beryl", ThinkingPolicyKnown: true},
		{ModelName: "gemini-onyx", ThinkingPolicyKnown: true},
	}
	picked := resolveSearchModel(a, "")
	if picked == nil || picked.ModelName != "gemini-beryl" {
		t.Fatalf("got %v, want the account default gemini-beryl", picked)
	}
	// An explicit model still wins when the account serves it.
	picked = resolveSearchModel(a, "gemini-onyx")
	if picked == nil || picked.ModelName != "gemini-onyx" {
		t.Fatalf("override: got %v, want gemini-onyx", picked)
	}
}
