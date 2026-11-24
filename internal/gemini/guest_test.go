package gemini_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/prompt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
	fhttp "github.com/bogdanfinn/fhttp"
)

type guestDoer struct {
	fn func(*fhttp.Request) (*fhttp.Response, error)
}

func (d *guestDoer) Do(r *fhttp.Request) (*fhttp.Response, error) { return d.fn(r) }
func (*guestDoer) CloseIdleConnections()                          {}

func guestFrame(text, label string, done bool) string {
	candidate := []any{"rc_guest", []string{text}, nil, nil, nil, nil, nil, nil, nil}
	if done {
		candidate[8] = []int{2}
	}
	inner, _ := json.Marshal([]any{nil, []string{"c_ignored", "r_ignored"}, nil, nil, []any{candidate}, map[string]any{"43": label, "26": "context_ignored"}})
	outer, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(inner)}})
	return string(outer) + "\n"
}

func minimalGuestPrompt(r *fhttp.Request) (string, error) {
	const endpoint = "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"
	if r.Method != "POST" || r.URL.String() != endpoint {
		return "", fmt.Errorf("unexpected anonymous method or endpoint: %s %s", r.Method, r.URL)
	}
	for key := range r.Header {
		key = strings.ToLower(key)
		if key == "cookie" || key == "authorization" || key == "x-api-key" || key == "x-goog-api-key" || strings.HasPrefix(key, "x-goog-ext-") {
			return "", fmt.Errorf("anonymous request contains forbidden header %s", key)
		}
	}
	if strings.Join(r.Header["content-type"], "") != "application/x-www-form-urlencoded;charset=UTF-8" || strings.Join(r.Header["sec-fetch-mode"], "") != "cors" {
		return "", errors.New("anonymous request must use the normal CORS kind and exact form content type")
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil || len(form) != 1 || len(form["f.req"]) != 1 {
		return "", errors.New("anonymous body must contain only one f.req value")
	}
	var freq []any
	if json.Unmarshal([]byte(form.Get("f.req")), &freq) != nil || len(freq) != 2 || freq[0] != nil {
		return "", errors.New("anonymous f.req must be [null, encoded inner]")
	}
	encoded, ok := freq[1].(string)
	if !ok {
		return "", errors.New("anonymous inner must be a JSON string")
	}
	var inner [][]string
	if json.Unmarshal([]byte(encoded), &inner) != nil || len(inner) != 1 || len(inner[0]) != 1 {
		return "", errors.New("anonymous inner must contain only [[prompt]]")
	}
	return inner[0][0], nil
}

func TestGuestClientExactMinimalRequestAndNoCookiesAcrossTurns(t *testing.T) {
	var calls atomic.Int32
	want := "Quote: \"hello\"\nUnicode: \u00e9 \U0001f642 <tag>&tail"
	client := gemini.NewGuestClient(transport.ClientConfig{CustomDoer: &guestDoer{fn: func(r *fhttp.Request) (*fhttp.Response, error) {
		got, err := minimalGuestPrompt(r)
		if err != nil {
			return nil, err
		}
		if got != want {
			return nil, errors.New("anonymous prompt escaping changed its contents")
		}
		calls.Add(1)
		body := ")]}'\n\n" + guestFrame("O", "Future Model", false) + guestFrame("OK", "Future Model", false) + guestFrame("O", "Future Model", true)
		return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{"Set-Cookie": {"SAPISID=must-not-be-replayed; Path=/; Secure"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}}}, prompt.EstimateTokens)
	defer client.Close()
	for range 2 {
		var snapshots []string
		label := ""
		res, err := client.StreamGenerateLive(context.Background(), want, gemini.StreamOptions{
			Model:    &gemini.ModelInfo{ModelID: "must-not-be-sent", ModelName: "guest-free"},
			Metadata: []any{"c_forbidden", "r_forbidden", "rc_forbidden"}, ThinkingLevel: 2, Language: "id", Temporary: true,
		}, func(update *gemini.LineParseResult) error {
			if update.CID != "" || update.RID != "" || update.ContextToken != "" {
				return errors.New("guest returned reusable conversation metadata")
			}
			if update.AnsweredModelLabel != "" {
				label = update.AnsweredModelLabel
			}
			for _, frame := range update.Frames {
				snapshots = append(snapshots, frame.Text)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != 200 || res.Empty || res.CID != "" || res.RID != "" || res.BootstrapMs != 0 || label != "Future Model" {
			t.Fatalf("unexpected anonymous response: %+v label=%q", res, label)
		}
		if len(snapshots) != 3 || snapshots[0] != "O" || snapshots[1] != "OK" || snapshots[2] != "OK" {
			t.Fatalf("cumulative longest snapshot was lost: %q", snapshots)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2", calls.Load())
	}
}

func TestGuestClientCompletesCleanAnonymousEOFAndPreservesLabel(t *testing.T) {
	client := gemini.NewGuestClient(transport.ClientConfig{CustomDoer: &guestDoer{fn: func(*fhttp.Request) (*fhttp.Response, error) {
		return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader(guestFrame("Answer", "Mutable label", false)))}, nil
	}}}, prompt.EstimateTokens)
	defer client.Close()
	var final gemini.Frame
	_, err := client.StreamGenerateLive(context.Background(), "question", gemini.StreamOptions{}, func(update *gemini.LineParseResult) error {
		for _, frame := range update.Frames {
			final = frame
		}
		return nil
	})
	if err != nil || final.Text != "Answer" || !final.Done || final.AnsweredModelID != "" || final.AnsweredModelLabel != "Mutable label" {
		t.Fatalf("clean anonymous EOF did not complete: %+v, %v", final, err)
	}
}

func TestGuestClientCleanEOFFinalizesRawCandidateThroughSharedSanitizer(t *testing.T) {
	for _, tc := range []struct{ raw, want, thoughts string }{
		{"enough", "enough", ""},
		{"literal [ci", "literal [ci", ""},
		{"literal https:/", "literal https:/", ""},
		{"literal [meaningful label]", "literal [meaningful label]", ""},
		{"Answer with spaces   ", "Answer with spaces   ", ""},
		{"OK [cite: 123", "OK", ""},
		{"OK [cite: 1].", "OK.", ""},
		{"<think>native reasoning</think>enough", "enough", "native reasoning"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			client := gemini.NewGuestClient(transport.ClientConfig{CustomDoer: &guestDoer{fn: func(*fhttp.Request) (*fhttp.Response, error) {
				return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader(guestFrame(tc.raw, "", false)))}, nil
			}}}, prompt.EstimateTokens)
			defer client.Close()
			var final gemini.Frame
			var sent string
			_, err := client.StreamGenerateLive(context.Background(), "question", gemini.StreamOptions{}, func(update *gemini.LineParseResult) error {
				for _, frame := range update.Frames {
					if !strings.HasPrefix(frame.Text, sent) {
						return errors.New("EOF final text revised an emitted sanitized prefix")
					}
					sent, final = frame.Text, frame
				}
				return nil
			})
			if err != nil || !final.Done || final.Text != tc.want || final.Thoughts != tc.thoughts {
				t.Fatalf("raw EOF suffix was lost or leaked: %+v, %v", final, err)
			}
		})
	}
}

func TestGuestClientStopCallbackDoesNotEmitAnEOFFinalization(t *testing.T) {
	client := gemini.NewGuestClient(transport.ClientConfig{CustomDoer: &guestDoer{fn: func(*fhttp.Request) (*fhttp.Response, error) {
		return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader(guestFrame("prefix", "", false) + guestFrame("prefix tail", "", true)))}, nil
	}}}, prompt.EstimateTokens)
	defer client.Close()
	calls := 0
	_, err := client.StreamGenerateLive(context.Background(), "question", gemini.StreamOptions{}, func(*gemini.LineParseResult) error {
		calls++
		return transport.ErrStopStreaming
	})
	if err != nil || calls != 1 {
		t.Fatalf("guest ignored a stopped callback: calls=%d err=%v", calls, err)
	}
}

func TestGuestClientRetainsLongestRawSnapshotWhenSanitizedTextPlateaus(t *testing.T) {
	client := gemini.NewGuestClient(transport.ClientConfig{CustomDoer: &guestDoer{fn: func(*fhttp.Request) (*fhttp.Response, error) {
		return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader(guestFrame("OK [cite: 1]", "", false) + guestFrame("OK [", "", false)))}, nil
	}}}, prompt.EstimateTokens)
	defer client.Close()
	var final gemini.Frame
	_, err := client.StreamGenerateLive(context.Background(), "question", gemini.StreamOptions{}, func(update *gemini.LineParseResult) error {
		for _, frame := range update.Frames {
			final = frame
		}
		return nil
	})
	if err != nil || !final.Done || final.Text != "OK" {
		t.Fatalf("shorter raw snapshot replaced the longest candidate at EOF: %+v %v", final, err)
	}
}

func TestGuestPromptHardBoundariesUseBridgeEstimatorAndCodePoints(t *testing.T) {
	prompt.SetTokenEstimatorMode("script")
	for _, tc := range []struct{ name, text, code string }{
		{"empty", "", "invalid_request_error"},
		{"whitespace", " \t\r\n\u2003", "invalid_request_error"},
		{"at-char-limit", strings.Repeat("x", 500000), ""},
		{"over-char-limit", strings.Repeat("x", 500001), "context_length_exceeded"},
		{"at-token-limit", strings.Repeat("\u4e00", 85333), ""},
		{"over-token-limit", strings.Repeat("\u4e00", 85334), "context_length_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := gemini.ValidateGuestPrompt(tc.text, prompt.EstimateTokens(tc.text))
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var ue *transport.UpstreamError
			if !errors.As(err, &ue) || ue.StatusCode != 400 || ue.Code != tc.code {
				t.Fatalf("error = %v, want 400 %s", err, tc.code)
			}
		})
	}
	// This isolates the character boundary from the independent token boundary:
	// multi-byte characters are not counted as bytes or UTF-16 surrogate pairs.
	for _, text := range []string{strings.Repeat("\u00e9", 500000), strings.Repeat("\U0001f642", 500000)} {
		if err := gemini.ValidateGuestPrompt(text, 1); err != nil {
			t.Fatal(err)
		}
		if err := gemini.ValidateGuestPrompt(text+"x", 1); err == nil {
			t.Fatal("extra Unicode code point must exceed the hard character limit")
		}
	}
}

func TestGuestClientRejectsLocalLimitsAndUploadsWithoutUpstream(t *testing.T) {
	var calls atomic.Int32
	client := gemini.NewGuestClient(transport.ClientConfig{CustomDoer: &guestDoer{fn: func(*fhttp.Request) (*fhttp.Response, error) {
		calls.Add(1)
		return nil, errors.New("local validation unexpectedly reached upstream")
	}}}, prompt.EstimateTokens)
	defer client.Close()
	for _, text := range []string{"", " \n", strings.Repeat("x", 600000), strings.Repeat("\u4e00", 90000)} {
		if _, err := client.StreamGenerateLive(context.Background(), text, gemini.StreamOptions{}, nil); err == nil {
			t.Fatal("invalid anonymous prompt was accepted")
		}
	}
	if _, err := client.StreamGenerateLive(context.Background(), "valid", gemini.StreamOptions{Files: []gemini.UploadedFile{{Path: "/forbidden"}}}, nil); err == nil {
		t.Fatal("guest attachment was accepted")
	}
	if _, err := client.UploadFile(context.Background(), []byte("image"), "image.png", "image/png"); err == nil {
		t.Fatal("guest upload was accepted")
	}
	if err := client.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	models, err := client.AvailableModels(context.Background(), true)
	if err != nil || len(models.Models) != 1 || models.Models[0].ModelID != "" || models.Models[0].Capacity != 0 || models.Models[0].ModelNumber != 0 || models.Models[0].CapacityField != 0 {
		t.Fatalf("guest fabricated registry fields: %+v, %v", models, err)
	}
	client.GetQuota(context.Background())
	client.RotateCookies(context.Background())
	client.ReloadFromDiskIfNewer()
	client.InvalidateAuth()
	if calls.Load() != 0 {
		t.Fatalf("guest non-generation machinery made %d upstream calls", calls.Load())
	}
}

func TestGuestClientWarningLogsOnlyCounts(t *testing.T) {
	var output bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(oldOutput)
	client := gemini.NewGuestClient(transport.ClientConfig{CustomDoer: &guestDoer{fn: func(*fhttp.Request) (*fhttp.Response, error) {
		return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader(guestFrame("OK", "", true)))}, nil
	}}}, prompt.EstimateTokens)
	defer client.Close()
	for _, text := range []string{strings.Repeat("x", 350001) + "PRIVATE-PROMPT-MARKER", strings.Repeat("\u4e00", 66667)} {
		if _, err := client.StreamGenerateLive(context.Background(), text, gemini.StreamOptions{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(output.String(), "[guest] prompt above safe zone") != 2 || strings.Contains(output.String(), "PRIVATE-PROMPT-MARKER") || strings.Contains(output.String(), "\u4e00") {
		t.Fatalf("warning did not report counts privately: %q", output.String())
	}
}

func TestGuestClientWatchdogUsesCandidateProgressNotHeadersOrKeepalives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		writerDone := make(chan struct{})
		client := gemini.NewGuestClient(transport.ClientConfig{CustomDoer: &guestDoer{fn: func(r *fhttp.Request) (*fhttp.Response, error) {
			return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: reader}, nil
		}}}, prompt.EstimateTokens)
		defer client.Close()
		defer func() { writer.Close(); <-writerDone }()
		go func() {
			defer close(writerDone)
			for range 10 {
				if _, err := io.WriteString(writer, "[[]]\n"); err != nil {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
		startedAt := time.Now()
		_, err := client.StreamGenerateLive(context.Background(), "question", gemini.StreamOptions{NoFrameTimeout: 300 * time.Millisecond}, nil)
		var ue *transport.UpstreamError
		if !errors.As(err, &ue) || ue.Code != "upstream_stalled" || time.Since(startedAt) != 300*time.Millisecond {
			t.Fatalf("watchdog result: %v after %s", err, time.Since(startedAt))
		}
	})
}
