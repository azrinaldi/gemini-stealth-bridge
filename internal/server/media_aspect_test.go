package server

import (
	"strings"
	"testing"
)

func TestResolveAspectRatioFromSizeDimensions(t *testing.T) {
	// Regression: size used to be matched against a fixed allow-list, so common
	// 16:9 resolutions such as 1280x720 silently produced no aspect ratio.
	cases := map[string]string{
		"1920x1080": "16:9",
		"1280x720":  "16:9",
		"2560x1440": "16:9",
		"3840x2160": "16:9",
		"1200x630":  "16:9",
		"1080x1920": "9:16",
		"720x1280":  "9:16",
		"1024x1024": "1:1",
		"1600x1200": "4:3",
		"1200x1600": "3:4",
		"1280X720":  "16:9",
		"1280*720":  "16:9",
	}
	for size, want := range cases {
		got, err := resolveAspectRatio("", size)
		if err != nil {
			t.Fatalf("size %q: unexpected error: %v", size, err)
		}
		if got != want {
			t.Fatalf("size %q: expected %q, got %q", size, want, got)
		}
	}
}

func TestResolveAspectRatioLabelsAndAliases(t *testing.T) {
	cases := map[string]string{
		"16:9":       "16:9",
		"landscape":  "16:9",
		"widescreen": "16:9",
		"portrait":   "9:16",
		"vertical":   "9:16",
		"square":     "1:1",
		"4:3":        "4:3",
		"3:4":        "3:4",
		"21:9":       "16:9",
		"2:1":        "16:9",
	}
	for in, want := range cases {
		got, err := resolveAspectRatio(in, "")
		if err != nil {
			t.Fatalf("aspect_ratio %q: unexpected error: %v", in, err)
		}
		if got != want {
			t.Fatalf("aspect_ratio %q: expected %q, got %q", in, want, got)
		}
	}
}

func TestResolveAspectRatioAgreeingHints(t *testing.T) {
	got, err := resolveAspectRatio("16:9", "1280x720")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "16:9" {
		t.Fatalf("expected 16:9, got %q", got)
	}
}

func TestResolveAspectRatioRejectsConflictingHints(t *testing.T) {
	// Regression: aspect_ratio used to win silently while size was discarded.
	_, err := resolveAspectRatio("16:9", "1080x1920")
	if err == nil {
		t.Fatal("expected an error for conflicting shape hints")
	}
	if !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("expected a conflict error, got: %v", err)
	}
}

func TestResolveAspectRatioRejectsMalformedInput(t *testing.T) {
	if _, err := resolveAspectRatio("", "banana"); err == nil {
		t.Fatal("expected an error for malformed size")
	}
	if _, err := resolveAspectRatio("wide-ish", ""); err == nil {
		t.Fatal("expected an error for malformed aspect_ratio")
	}
	if _, err := resolveAspectRatio("", ""); err != nil {
		t.Fatalf("empty hints must not error, got: %v", err)
	}
}
