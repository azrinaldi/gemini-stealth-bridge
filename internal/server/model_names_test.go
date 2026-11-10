package server

import (
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

// The Web UI connection chip used to carry a written-out model list, so it still
// advertised "gemini-argon" after upstream renamed the tier to Carbon. The chip
// must be fed from the live registry instead.
func TestModelNamesFollowsRegistry(t *testing.T) {
	got := modelNames(nil)
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
	if names := modelNames([]gemini.ModelInfo{
		{ModelName: "gemini-carbon"},
		{ModelName: "  "},
		{ModelName: "gemini-flash"},
	}); len(names) != 2 || names[0] != "gemini-carbon" || names[1] != "gemini-flash" {
		t.Fatalf("names = %v, want [gemini-carbon gemini-flash]", names)
	}
}
