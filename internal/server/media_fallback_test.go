package server

import (
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

// Regression: OpenAI's images API allows size "auto" (gpt-image-1) and
// pass-through clients forward it. Treating it as malformed would 400 requests
// that worked before shape hints started being reconciled.
func TestResolveAspectRatioTreatsAutoAsAbsent(t *testing.T) {
	for _, c := range [][2]string{
		{"auto", ""},
		{"", "auto"},
		{"Auto", ""},
		{"", "  AUTO "},
		{"auto", "auto"},
	} {
		got, err := resolveAspectRatio(c[0], c[1])
		if err != nil {
			t.Fatalf("ar=%q size=%q: unexpected error: %v", c[0], c[1], err)
		}
		if got != "" {
			t.Fatalf("ar=%q size=%q: expected no ratio, got %q", c[0], c[1], got)
		}
	}
	got, err := resolveAspectRatio("16:9", "auto")
	if err != nil || got != "16:9" {
		t.Fatalf(`aspect_ratio "16:9" with size "auto": expected 16:9, got %q err=%v`, got, err)
	}
}

// Media failover must respect the same group boundary as chat text failover: a
// request that starts on a grouped account retries only inside that group, and
// an ungrouped request never adopts grouped accounts.
func TestPickFallbackAccountRespectsOriginGroup(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	srv := New(cfg)

	a1 := &AccountState{ID: "ungrouped", Models: []gemini.ModelInfo{testModel}}
	a2 := &AccountState{ID: "team-a-1", Models: []gemini.ModelInfo{testModel}, AccountGroup: "team-a"}
	a3 := &AccountState{ID: "team-a-2", Models: []gemini.ModelInfo{testModel}, AccountGroup: "team-a"}
	a4 := &AccountState{ID: "team-b-1", Models: []gemini.ModelInfo{testModel}, AccountGroup: "team-b"}
	accts := []*AccountState{a1, a2, a3, a4}
	srv.SetAccounts(accts)

	if got, _ := srv.pickFallbackAccount(accts, map[string]bool{"ungrouped": true}, "", testModel.ModelName, nil); got != nil {
		t.Fatalf("ungrouped origin must not adopt grouped accounts, got %q", got.ID)
	}
	if got, _ := srv.pickFallbackAccount(accts, map[string]bool{"team-a-1": true}, "team-a", testModel.ModelName, nil); got == nil || got.ID != "team-a-2" {
		t.Fatalf("team-a origin should fail over to team-a-2, got %v", got)
	}
	if got, _ := srv.pickFallbackAccount(accts, map[string]bool{"team-b-1": true}, "team-b", testModel.ModelName, nil); got != nil {
		t.Fatalf("team-b origin has no group-mates; expected no fallback, got %q", got.ID)
	}
}
