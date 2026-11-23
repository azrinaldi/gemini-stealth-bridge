package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestRewriteMediaPreservesRawURLInBothTransports(t *testing.T) {
	const raw = "https://lh3.googleusercontent.com/rd-gg-dl/fixture=s512-rw?alr=yes&token=fixture"
	const legacy = "https://lh3.googleusercontent.com/rd-gg-dl/fixture=s1024-rw"
	for _, plain := range []bool{false, true} {
		srv := New(config.Config{MediaPlainFetch: plain})
		frame := gemini.Frame{Text: legacy + " " + raw, GeneratedImages: []gemini.GeneratedImage{{URL: legacy, RawURL: raw}}}
		srv.rewriteFrameMedia(context.Background(), "fixture", &frame)
		want := raw
		for _, entry := range srv.mediaItems {
			if entry.UpstreamURL != want {
				t.Fatalf("plain=%t: upstream URL was modified: %s", plain, entry.UpstreamURL)
			}
		}
		if len(srv.mediaItems) != 1 || strings.Contains(frame.Text, "googleusercontent.com") || !strings.Contains(frame.Text, "/v1/media/") {
			t.Fatalf("media URL was not safely proxied: %s", frame.Text)
		}
	}
}

func TestImageURLFormatKeepsQueryParameters(t *testing.T) {
	const raw = "https://lh3.googleusercontent.com/fixture=s512-rw?alr=yes&token=fixture"
	for format, suffix := range map[string]string{"png": "=s0-rp", "gif": "", "jpeg": "=s1024-rw"} {
		got, _, _ := applyGoogleImageURLFormat(raw, format)
		if want := "https://lh3.googleusercontent.com/fixture" + suffix + "?alr=yes&token=fixture"; got != want {
			t.Fatalf("format=%s: query parameters lost: got=%s want=%s", format, got, want)
		}
	}
}

type mediaDownloadUpstream struct {
	*mockUpstream
	download func(string, string) ([]byte, string, error)
}

func (m *mediaDownloadUpstream) DownloadMedia(_ context.Context, raw, fallback string) ([]byte, string, error) {
	return m.download(raw, fallback)
}

func TestImageGenerationBase64ContractAndSingleFetch(t *testing.T) {
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 16, 9))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, format string
		data         []byte
		err          error
		status       int
	}{
		{"base64 success", "b64_json", pngBytes.Bytes(), nil, http.StatusOK},
		{"base64 failure", "b64_json", nil, errors.New("media HTTP 403"), http.StatusBadGateway},
		{"base64 empty body", "b64_json", nil, nil, http.StatusBadGateway},
		{"URL failure remains retryable", "url", nil, errors.New("media HTTP 403"), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, account := newTestHarness(false, false)
			calls := 0
			account.Client = &mediaDownloadUpstream{
				mockUpstream: clientFromFrames([]gemini.Frame{{Done: true, GeneratedImages: []gemini.GeneratedImage{{URL: "https://lh3.googleusercontent.com/fixture=s512-rw"}}}}),
				download: func(string, string) ([]byte, string, error) {
					calls++
					return tc.data, "image/png", tc.err
				},
			}
			rec := httptest.NewRecorder()
			srv.HandleImageGeneration(context.Background(), rec, map[string]any{"model": testModel.ModelName, "prompt": "fixture", "response_format": tc.format}, AuthScope{OK: true, AccountID: account.ID})
			if rec.Code != tc.status || calls != 1 {
				t.Fatalf("status=%d want=%d downloads=%d body=%s", rec.Code, tc.status, calls, rec.Body.String())
			}
			if tc.status != http.StatusOK {
				if !strings.Contains(rec.Body.String(), "media_download_failed") {
					t.Fatalf("missing explicit base64 error: %s", rec.Body.String())
				}
				return
			}
			if tc.format == "b64_json" {
				var response struct {
					Data []struct {
						Base64        string `json:"b64_json"`
						Width, Height int
					}
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Data) != 1 || response.Data[0].Base64 != base64.StdEncoding.EncodeToString(tc.data) || response.Data[0].Width != 16 || response.Data[0].Height != 9 {
					t.Fatal("base64 bytes or actual dimensions missing")
				}
			}
		})
	}
}
