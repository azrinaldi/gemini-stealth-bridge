package gemini

import (
	"context"
	"errors"
	"fmt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
	fhttp "github.com/bogdanfinn/fhttp"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"
)

func TestParseFrameLineSkipsNullKeepaliveAndExtractsMetadata(t *testing.T) {
	keepalive := `[["wrb.fr",null,null,null,null,[3]]]`
	r, err := ParseFrameLine(keepalive)
	if err != nil {
		t.Fatalf("unexpected error on keepalive frame: %v", err)
	}
	if r == nil || len(r.Frames) != 0 {
		t.Fatalf("expected empty frames on keepalive, got %+v", r)
	}

	quotaFrame := `[["wrb.fr",null,null,null,null,[8,null,[["type.googleapis.com/assistant.lamda.BardFrontendService",[[1037]]]]]]]`
	_, err = ParseFrameLine(quotaFrame)
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 429 || ue.Code != "rate_limit_error" {
		t.Fatalf("expected 429 rate_limit_error on 1037 frame, got %v", err)
	}

	// 1185 is an upstream turn failure, not a quota signal: it must stay
	// retryable so the pool can fail over to a healthier account.
	degradedFrame := `[["wrb.fr",null,null,null,null,[8,null,[["type.googleapis.com/assistant.lamda.BardFrontendService",[[1185]]]]]]]`
	_, err = ParseFrameLine(degradedFrame)
	if !errors.As(err, &ue) || ue.StatusCode != 502 || ue.Code != "upstream_degraded" {
		t.Fatalf("expected 502 upstream_degraded on 1185 frame, got %v", err)
	}
	if !strings.Contains(ue.Message, "1185") {
		t.Fatalf("expected the message to keep the upstream code for diagnostics, got %q", ue.Message)
	}
}

func TestParseFrameLineExtractsAnsweredModelAndThoughts(t *testing.T) {
	inner := `[null,["c_123","r_456"],null,null,[["rc_789",["Hello world"],null,null,null,null,null,null,[2],{"38":[["thinking step"]]}]],{"26":"ctx_tok_1","40":"954c0abcba24364","43":"4 Argon"}]`
	innerEsc, _ := marshalJSONString(inner)
	line := `[["wrb.fr",null,` + innerEsc + `]]`

	r, err := ParseFrameLine(line)
	if err != nil {
		t.Fatalf("ParseFrameLine failed: %v", err)
	}
	if r.CID != "c_123" || r.RID != "r_456" || r.ContextToken != "ctx_tok_1" {
		t.Fatalf("unexpected ids: %+v", r)
	}
	if r.AnsweredModelID != "954c0abcba24364" || r.AnsweredModelLabel != "4 Argon" {
		t.Fatalf("unexpected answered model: id=%q label=%q", r.AnsweredModelID, r.AnsweredModelLabel)
	}
	if len(r.Frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(r.Frames))
	}
	f := r.Frames[0]
	if f.RCID != "rc_789" || f.Text != "Hello world" || f.Thoughts != "thinking step" || !f.Done {
		t.Fatalf("unexpected candidate frame: %+v", f)
	}
}

func TestGeminiErrorReplyHeuristics(t *testing.T) {
	if GeminiErrorReply("Sorry, something went wrong. Please try your request again.") == "" {
		t.Fatal("expected canned apology to match")
	}
	if GeminiErrorReply("Sorry, something went wrong. Please try your request again. If the problem persists, contact support.") == "" {
		t.Fatal("expected canned apology with short trailer to match")
	}
	longExplanation := "Sorry, something went wrong. Please try your request again. That message is what the upstream returns when its quota is spent, and here is the full explanation of why that happens and what to do about it in practice."
	if GeminiErrorReply(longExplanation) != "" {
		t.Fatal("expected long explanation (>200 chars) not to be mistaken for an error reply")
	}
	partialWithError := "```python\nprint(144)\n```\n\nI encountered an error doing what you asked. Could you try again?"
	if GeminiErrorReply(partialWithError) == "" {
		t.Fatal("expected appended error sentence in <=400 char reply to match")
	}
}

func TestBuildInnerRequestAndModelHeader(t *testing.T) {
	ac := NewAccountClient("acct", "SAPISID=synthetic", "", nil, nil)
	ac.SetTokens("at_tok", "bl_tok", "fsid_123")
	m := &ModelInfo{
		ModelID:       "954c0abcba24364",
		ModelName:     "gemini-argon",
		DisplayName:   "4 Argon",
		Capacity:      2,
		CapacityField: 12,
		ModelNumber:   4,
	}
	inner, uuidVal, hdr := ac.BuildInnerRequest("Hi", StreamOptions{
		Model:         m,
		ThinkingLevel: 2,
		Temporary:     true,
		DisableTools:  true,
		Metadata:      []any{"cid1", "rid1", "rcid1", nil, nil, nil, nil, nil, nil, "ctx1"},
	})
	if uuidVal == "" || !strings.Contains(inner, uuidVal) {
		t.Fatalf("expected uuid in inner payload: %s", inner)
	}
	if !strings.Contains(hdr, `"954c0abcba24364"`) || !strings.Contains(hdr, `"fsid_123"`) {
		t.Fatalf("unexpected model header: %s", hdr)
	}
	innerOn, _, _ := ac.BuildInnerRequest("Hi", StreamOptions{
		Model:           m,
		ThinkingLevel:   1,
		PersonalContext: "on",
	})
	if strings.Contains(innerOn, ",1,1,null,[") {
		t.Fatalf("expected PersonalContext=on to set inner[67]/inner[68] to 0, got: %s", innerOn)
	}
}

func marshalJSONString(s string) (string, error) {
	b := strings.Builder{}
	b.WriteByte('"')
	for i := range len(s) {
		switch s[i] {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(s[i])
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}

type stubGeminiDoer struct {
	fn func(req *fhttp.Request) (*fhttp.Response, error)
}

func (d *stubGeminiDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	return d.fn(req)
}

func (d *stubGeminiDoer) CloseIdleConnections() {}

func TestParseModelRequestAndStrictThreeThinkingLevels(t *testing.T) {
	clean, think := ParseModelRequest("gsb/gemini-4-argon@think=high")
	if clean != "gemini-4-argon" || think != "high" {
		t.Fatalf("unexpected ParseModelRequest result: clean=%q think=%q", clean, think)
	}
	m := ModelInfo{
		ModelID:     "95a4c0abcba24364",
		ModelName:   "gemini-argon",
		DisplayName: "4 Argon",
		Aliases:     []string{"gemini-4-argon"},
	}
	for _, wanted := range []string{"gemini-argon", "gemini-4-argon", "gsb/gemini-argon", "gsb/gemini-4-argon@think=medium"} {
		if !MatchesModel(m, wanted) {
			t.Fatalf("expected MatchesModel to match %q", wanted)
		}
	}
	if ThinkingLevelFor("low", "gemini-argon", "low") != 1 {
		t.Fatal("expected low -> 1")
	}
	if ThinkingLevelFor("medium", "gemini-argon", "low") != 4 {
		t.Fatal("expected medium -> 4")
	}
	if ThinkingLevelFor("high", "gemini-argon", "low") != 2 {
		t.Fatal("expected high -> 2")
	}
	if ThinkingLevelFor(nil, "gemini-argon", "4") != 4 {
		t.Fatal("expected numeric defaultEffort '4' (medium) -> 4")
	}
	if ThinkingLevelFor(nil, "gemini-argon", "2") != 2 {
		t.Fatal("expected numeric defaultEffort '2' (high) -> 2")
	}
}

func TestBootstrapSelfHealsViaRotateCookiesAndStreamOmitsSAPISIDHash(t *testing.T) {
	appCalls := 0
	rotated := false
	var streamAuthHeader string

	doer := &stubGeminiDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.HasSuffix(u, "/app"):
				appCalls++
				if !rotated {
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{},
						Body:       io.NopCloser(strings.NewReader(`<html>signed out</html>`)),
					}, nil
				}
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_fresh_123","cfb2h":"boq_test","FdrFJe":"999","oPEP7c":"u@example.com"}`)),
				}, nil
			case strings.Contains(u, "RotateCookies"):
				rotated = true
				return &fhttp.Response{
					StatusCode: 200,
					Header: fhttp.Header{
						"Set-Cookie": []string{"__Secure-1PSIDTS=renewed_ts; Path=/; Secure; HttpOnly"},
					},
					Body: io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			case strings.Contains(u, "StreamGenerate"):
				streamAuthHeader = req.Header.Get("authorization")
				inner := `[null,["c_1","r_1"],null,null,[["rc_1",["ok"],null,null,null,null,null,null,[2]]],{"40":"95a4c0abcba24364","43":"4 Argon"}]`
				esc, _ := marshalJSONString(inner)
				body := ")]}'\n\n" + `[["wrb.fr",null,` + esc + `]]` + "\n"
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}

	pool := transport.NewPool(transport.PoolOptions{
		ClientConfig: transport.ClientConfig{CustomDoer: doer},
		MaxSessions:  2,
	})
	ac := NewAccountClient("acct1", "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=stale", "", pool, cookies.NewRotator())
	if err := ac.Bootstrap(context.Background()); err != nil {
		t.Fatalf("expected Bootstrap to self-heal via RotateCookies, got err: %v", err)
	}
	if appCalls != 2 || !rotated {
		t.Fatalf("expected 2 /app calls with RotateCookies in between, got appCalls=%d rotated=%v", appCalls, rotated)
	}
	_, err := ac.StreamGenerateLive(context.Background(), "hello", StreamOptions{
		Model:         &ModelInfo{ModelID: "95a4c0abcba24364", ModelName: "gemini-argon", Capacity: 2, ModelNumber: 4},
		ThinkingLevel: 1,
	}, nil)
	if err != nil {
		t.Fatalf("StreamGenerateLive failed: %v", err)
	}
	if streamAuthHeader != "" {
		t.Fatalf("expected StreamGenerate to omit Authorization header, got %q", streamAuthHeader)
	}
}

func TestParseFrameLineStripsInlineThinkAndCodeReferenceIntoThoughts(t *testing.T) {
	inner := `[null,["c_1","r_1"],null,null,[["rc_1",["<think> Planning step 1 </think>\n\n` + "```python?code_reference&code_event_index=1\\nprint(19 * 27)\\n```\\n\\n```text?code_stdout&code_event_index=1\\n513\\n```\\n\\n" + `Assistant: Final clean answer."],null,null,null,null,null,null,[2]]],{"40":"95a4c0abcba24364","43":"4 Argon"}]`
	esc, _ := marshalJSONString(inner)
	line := `[["wrb.fr",null,` + esc + `]]`
	res, err := ParseFrameLine(line)
	if err != nil {
		t.Fatalf("ParseFrameLine failed: %v", err)
	}
	if len(res.Frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(res.Frames))
	}
	f := res.Frames[0]
	if f.Text != "Final clean answer." {
		t.Fatalf("expected clean text without <think>, code_reference, or Assistant:, got %q", f.Text)
	}
	if !strings.Contains(f.Thoughts, "Planning step 1") || !strings.Contains(f.Thoughts, "print(19 * 27)") || !strings.Contains(f.Thoughts, "513") {
		t.Fatalf("expected inline <think> and code_reference moved into Thoughts, got %q", f.Thoughts)
	}
}

func TestFinalUnclosedThinkAndEchoedHarnessReminderNeverLeak(t *testing.T) {
	rawText := "[Harness reminder: Available external tools: read_file(path).\nExternal harness JSON tool calls (`{\"tool_calls\":[{\"name\":\"<tool>\",\"arguments\":{...}}]}`) are plain-text JSON replies.\nIf more files, searches, edits, or commands are needed to complete the task, output ONLY the ```json {\"tool_calls\":[...]} ``` block.]\n\nVisible answer.<think>private trailing reasoning"
	inner := `[null,["c_1","r_1"],null,null,[["rc_1",[""],null,null,null,null,null,null,[2]]],{"40":"95a4c0abcba24364","43":"4 Argon"}]`
	inner = strings.Replace(inner, `[""]`, `[`+mustQuoteJSON(t, rawText)+`]`, 1)
	esc, _ := marshalJSONString(inner)
	line := `[["wrb.fr",null,` + esc + `]]`

	res, err := ParseFrameLine(line)
	if err != nil {
		t.Fatalf("ParseFrameLine failed: %v", err)
	}
	if len(res.Frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(res.Frames))
	}
	f := res.Frames[0]
	if f.Text != "Visible answer." {
		t.Fatalf("expected clean 'Visible answer.', got %q", f.Text)
	}
	if !strings.Contains(f.Thoughts, "private trailing reasoning") {
		t.Fatalf("expected trailing unclosed <think> moved to Thoughts, got %q", f.Thoughts)
	}
}

func TestParseFrameLineExtractsInlineThinkingProcessMonotonicallyAcrossStreamingFrames(t *testing.T) {
	f1Text := "Thinking Process:\n\n1.  **Analyze Current State:**\n    *   `WaterAndSky.ts` was written cleanly.\n    *   The rule violation was `public get muted(): boolean { return this.isMuted; }`.\n```json"
	f2Text := f1Text + "\n{\"tool_calls\":[{\"name\":\"edit\",\"arguments\":{\"path\":\"src/audio/NatureAudioEngine.ts\"}}]}\n```"

	buildLine := func(rawText string, status int) string {
		inner := `[null,["c_1","r_1"],null,null,[["rc_1",[""],null,null,null,null,null,null,[` + strings.TrimSpace(string(rune('0'+status))) + `]]],{"40":"95a4c0abcba24364","43":"4 Argon"}]`
		inner = strings.Replace(inner, `[""]`, `[`+mustQuoteJSON(t, rawText)+`]`, 1)
		esc, _ := marshalJSONString(inner)
		return `[["wrb.fr",null,` + esc + `]]`
	}

	res1, err := ParseFrameLine(buildLine(f1Text, 1))
	if err != nil || len(res1.Frames) != 1 {
		t.Fatalf("frame 1 parse failed: %v", err)
	}
	if res1.Frames[0].Text != "" {
		t.Fatalf("expected empty Text during streaming Thinking Process with partial fence held back, got %q", res1.Frames[0].Text)
	}
	if !strings.Contains(res1.Frames[0].Thoughts, "public get muted(): boolean { return this.isMuted; }") || strings.Contains(res1.Frames[0].Thoughts, "```json") {
		t.Fatalf("unexpected frame 1 Thoughts: %q", res1.Frames[0].Thoughts)
	}

	res2, err := ParseFrameLine(buildLine(f2Text, 2))
	if err != nil || len(res2.Frames) != 1 {
		t.Fatalf("frame 2 parse failed: %v", err)
	}
	if !strings.HasPrefix(res2.Frames[0].Thoughts, res1.Frames[0].Thoughts) {
		t.Fatalf("expected frame 2 Thoughts (%q) to have frame 1 Thoughts (%q) as prefix", res2.Frames[0].Thoughts, res1.Frames[0].Thoughts)
	}
	if strings.Contains(res2.Frames[0].Text, "Thinking Process:") || !strings.HasPrefix(res2.Frames[0].Text, "```json\n{\"tool_calls\"") {
		t.Fatalf("expected frame 2 Text to contain only the tool_calls block, got %q", res2.Frames[0].Text)
	}
}

func mustQuoteJSON(t *testing.T, s string) string {
	t.Helper()
	q, err := marshalJSONString(s)
	if err != nil {
		t.Fatalf("marshalJSONString: %v", err)
	}
	return q
}

func BenchmarkSanitizeCandidateTextAndThoughts_24KBStream(b *testing.B) {
	chunk := "export class NatureRenderer {\n  private scene: THREE.Scene;\n  public update(dt: number): void {\n    const factor = dt * 0.016;\n  }\n}\n"
	frames := make([]string, 50)
	acc := "Thinking Process:\n\n1. **Designing Scene Graph**:\n   * Keep draw calls below 45.\n   * Lock pixelRatio to 1.\n\nFinal Answer:\n"
	for i := range 50 {
		acc += strings.Repeat(chunk, 4)
		frames[i] = acc
	}
	b.ReportAllocs()
	for b.Loop() {
		for idx, f := range frames {
			_, _ = sanitizeCandidateTextAndThoughts(f, "", idx == len(frames)-1)
		}
	}
}

func TestMultilingualThinkingExtraction(t *testing.T) {
	cases := []struct {
		name        string
		rawText     string
		wantThought string
		wantText    string
	}{
		{
			name:        "SimplifiedChineseWithFullwidthColon",
			rawText:     "思考过程：\n1. 分析问题\n\n最终答案：\n这是最终回复。",
			wantThought: "1. 分析问题",
			wantText:    "这是最终回复。",
		},
		{
			name:        "EmojiMarkdownThinking",
			rawText:     "💭 **Thinking Process**:\nEvaluating options A and B.\n\nAnswer: Option A.",
			wantThought: "Evaluating options A and B.",
			wantText:    "Option A.",
		},
		{
			name:        "KoreanThinkingProcess",
			rawText:     "생각 과정:\n단계별 분석입니다.\n\n최종 답변: 완료되었습니다.",
			wantThought: "단계별 분석입니다.",
			wantText:    "완료되었습니다.",
		},
		{
			name:        "JapaneseThinkingProcess",
			rawText:     "思考プロセス：\nまず要件を確認します。\n\n回答：実装が完了しました。",
			wantThought: "まず要件を確認します。",
			wantText:    "実装が完了しました。",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Streaming partial chunks must never emit invalid UTF-8 (\ufffd) or leak thinking headers
			runes := []rune(tc.rawText)
			for i := 1; i < len(runes); i++ {
				partial := string(runes[:i])
				txt, th := sanitizeCandidateTextAndThoughts(partial, "", false)
				if !utf8.ValidString(txt) || strings.ContainsRune(txt, utf8.RuneError) {
					t.Fatalf("invalid UTF-8 in streaming text at rune %d: %q", i, txt)
				}
				if !utf8.ValidString(th) || strings.ContainsRune(th, utf8.RuneError) {
					t.Fatalf("invalid UTF-8 in streaming thoughts at rune %d: %q", i, th)
				}
			}

			finalTxt, finalTh := sanitizeCandidateTextAndThoughts(tc.rawText, "", true)
			if finalTxt != tc.wantText {
				t.Fatalf("got text=%q, want %q", finalTxt, tc.wantText)
			}
			if finalTh != tc.wantThought {
				t.Fatalf("got thought=%q, want %q", finalTh, tc.wantThought)
			}
		})
	}
}

func TestBootstrapRotation429NotAuthError(t *testing.T) {
	for _, tc := range []struct {
		name        string
		appLimited  bool
		wantMessage string
	}{
		{"app", true, "upstream rate limited"},
		{"rotation", false, "cookie rotation rate limited (HTTP 429)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var appCalls, rotationCalls int
			doer := &stubGeminiDoer{fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				switch {
				case strings.HasSuffix(req.URL.Path, "/app"):
					appCalls++
					if tc.appLimited {
						return &fhttp.Response{
							StatusCode: 429,
							Header:     fhttp.Header{"Retry-After": {"120"}},
							Body:       io.NopCloser(strings.NewReader("rate limited")),
						}, nil
					}
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{"Retry-After": {"2"}},
						Body:       io.NopCloser(strings.NewReader("<html>no token</html>")),
					}, nil
				case req.URL.Path == "/RotateCookies":
					rotationCalls++
					return &fhttp.Response{
						StatusCode: 429,
						Header: fhttp.Header{
							"Retry-After": {"120"},
							"Set-Cookie":  {"__Secure-1PSIDTS=fixture-new; Path=/; Secure"},
						},
						Body: io.NopCloser(strings.NewReader("rate limited")),
					}, nil
				default:
					t.Fatalf("unexpected request to %s", req.URL.Path)
					return nil, nil
				}
			}}
			pool := transport.NewPool(transport.PoolOptions{
				ClientConfig: transport.ClientConfig{CustomDoer: doer},
				MaxSessions:  2,
			})
			ac := NewAccountClient("acct_429", "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts1", "", pool, cookies.NewRotator())
			err := ac.Bootstrap(context.Background())
			var ue *transport.UpstreamError
			if !errors.As(err, &ue) || ue.StatusCode != 429 || ue.Code != "rate_limit_error" || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("expected 429 rate_limit_error with %q, got %v", tc.wantMessage, err)
			}
			if got := ue.Headers["retry-after"]; got != "120" {
				t.Errorf("retry-after = %q, want 120 from the throttled request", got)
			}
			wantRotations := 1
			if tc.appLimited {
				wantRotations = 0
			}
			if appCalls != 1 || rotationCalls != wantRotations {
				t.Errorf("got %d page requests and %d rotations, want 1 and %d", appCalls, rotationCalls, wantRotations)
			}
		})
	}
}

func TestConcurrentStreamCookieGenerationGuard(t *testing.T) {
	streamStarted := make(chan struct{})
	releaseStream := make(chan struct{})

	doer := &stubGeminiDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.Contains(u, "StreamGenerate"):
				close(streamStarted)
				<-releaseStream
				inner := `[null,["c_1","r_1"],null,null,[["rc_1",["done"],null,null,null,null,null,null,[2]]],{"40":"95a4c0abcba24364","43":"4 Argon"}]`
				esc, _ := marshalJSONString(inner)
				body := ")]}'\n\n" + `[["wrb.fr",null,` + esc + `]]` + "\n"
				return &fhttp.Response{
					StatusCode: 200,
					Header: fhttp.Header{
						"Set-Cookie": []string{"__Secure-1PSIDTS=stale_stream_ts; Path=/; Secure; HttpOnly"},
					},
					Body: io.NopCloser(strings.NewReader(body)),
				}, nil
			case strings.Contains(u, "RotateCookies"):
				return &fhttp.Response{
					StatusCode: 200,
					Header: fhttp.Header{
						"Set-Cookie": []string{"__Secure-1PSIDTS=newer_rotated_ts; Path=/; Secure; HttpOnly"},
					},
					Body: io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}

	pool := transport.NewPool(transport.PoolOptions{
		ClientConfig: transport.ClientConfig{CustomDoer: doer},
		MaxSessions:  2,
		MaxQueue:     4,
	})
	ac := NewAccountClient("acct_gen", "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=initial_ts", "", pool, cookies.NewRotator())
	ac.SetTokens("at_valid", "bl_valid", "fsid_1")

	doneCh := make(chan error, 1)
	go func() {
		_, err := ac.StreamGenerateLive(context.Background(), "hi", StreamOptions{
			Model:         &ModelInfo{ModelID: "95a4c0abcba24364", ModelName: "gemini-argon"},
			ThinkingLevel: 1,
		}, nil)
		doneCh <- err
	}()

	<-streamStarted
	// While stream (started at generation G0) is in flight, RotateCookies updates jar to generation G1
	rotRes, err := ac.RotateCookies(context.Background())
	if err != nil || !rotRes.Rotated {
		t.Fatalf("RotateCookies failed: res=%+v err=%v", rotRes, err)
	}
	close(releaseStream)
	if sErr := <-doneCh; sErr != nil {
		t.Fatalf("StreamGenerateLive failed: %v", sErr)
	}

	if !strings.Contains(ac.CookieHeader(), "__Secure-1PSIDTS=newer_rotated_ts") {
		t.Fatalf("stale stream Set-Cookie overwrote newer rotated cookie! got: %s", ac.CookieHeader())
	}
}

func TestKoreanInlineThinkingHeaderAndIndonesianCannedErrorReply(t *testing.T) {
	rawText := " 생각이: Let's mark `Analyze fleet operations` as done and move to `Analyze rush hour`.\n```json\n{\"tool_calls\":[{\"name\":\"todo\",\"arguments\":{\"op\":\"done\"}}]}\n```"
	cleanText, cleanThoughts := sanitizeCandidateTextAndThoughts(rawText, "", true)
	if !strings.Contains(cleanThoughts, "Let's mark `Analyze fleet operations` as done") {
		t.Fatalf("expected inline Korean thinking header routed to thoughts, got thoughts=%q text=%q", cleanThoughts, cleanText)
	}
	if !strings.HasPrefix(cleanText, "```json") {
		t.Fatalf("expected cleanText to start with tool_calls fence, got %q", cleanText)
	}

	for _, refusal := range []string{
		"Saya adalah model bahasa, dan saya tidak dirancang untuk itu.",
		"Saya hanya model bahasa dan tidak bisa membantu Anda untuk itu.",
		"Saya tidak bisa membantu untuk itu karena saya hanya model bahasa dan tidak punya informasi atau kemampuan yang diperlukan.",
		"Saya tidak bisa membantu Anda karena saya hanya model bahasa dan tidak mampu memahami dan merespons hal tersebut.",
		"Saya tidak diprogram untuk melakukan hal itu.",
		"I'm a language model, and I'm not designed to do that.",
		"I'm not programmed to do that.",
		"I can't help with that because I'm only a language model and don't have the necessary information or abilities.",
	} {
		if got := GeminiErrorReply(refusal); got == "" {
			t.Fatalf("expected canned refusal %q to be recognized by GeminiErrorReply", refusal)
		}
	}
}

func TestThoughtBubbleEmojiInlineThinkingAndFinalAnswerSplit(t *testing.T) {
	// 1. Standalone 💭\n before tool_calls with trailing )] artifact
	toolTurn := "💭\nLet's check why 91 GISTARU folders showed `0 lines in features.ndjson.gz`! )]\n```json\n{\"tool_calls\":[{\"name\":\"eval\",\"arguments\":{\"code\":\"print(1)\"}}]}\n```"
	cleanText, cleanThoughts := sanitizeCandidateTextAndThoughts(toolTurn, "", true)
	if cleanThoughts != "Let's check why 91 GISTARU folders showed `0 lines in features.ndjson.gz`!" {
		t.Fatalf("expected stripped thought without 💭 or )], got %q", cleanThoughts)
	}
	if !strings.HasPrefix(cleanText, "```json") {
		t.Fatalf("expected tool block in cleanText, got %q", cleanText)
	}

	// 2. Standalone 💭\n monologue followed by meta-sentence and final answer (like 24312863 and f5a8746f)
	finalTurn := "💭\nAll todo items are now marked complete (`3/3 done, 0 open`).\nLet's write a brief, crisp closing note confirming that the analysis and blueprint above are complete and ready for execution whenever the user approves.✅ Seluruh tahap analisis dan perancangan struktur direktori baru (`datasets-clean/`) telah selesai."
	ansText, ansThoughts := sanitizeCandidateTextAndThoughts(finalTurn, "", true)
	if !strings.HasPrefix(ansText, "✅ Seluruh tahap analisis") {
		t.Fatalf("expected final answer starting with ✅, got %q", ansText)
	}
	if strings.Contains(ansThoughts, "💭") || !strings.Contains(ansThoughts, "All todo items are now marked complete") || strings.Contains(ansThoughts, "Seluruh tahap analisis") {
		t.Fatalf("expected clean thoughts without 💭 or final answer leak, got %q", ansThoughts)
	}

	// 3. Glued markdown heading seam after internal colon outline (session 01a15c4b turn 3)
	turn3 := "💭\nThe user is asking in Indonesian:\n\"kam utau an repo ini fungsinya apa\"\n\nLet's structure the answer following the engineering persona and instructions:\n1. Briefly confirm `gemini-stealth-bridge` (`gsb`) function.\n2. Explain what **Task Mode** is.\n\nLet's keep it clear, concrete, in Indonesian, matching the terse technical style.### 1. Fungsi Repo Ini (`gemini-stealth-bridge` / `gsb`)\n`gsb` adalah reverse-proxy yang mengubah sesi web `gemini.google.com` menjadi endpoint API standar."
	t3Text, t3Thoughts := sanitizeCandidateTextAndThoughts(turn3, "", true)
	if !strings.HasPrefix(t3Text, "### 1. Fungsi Repo Ini") {
		t.Fatalf("turn3: expected answer starting at ### 1. Fungsi Repo Ini, got %q", t3Text)
	}
	if strings.Contains(t3Thoughts, "### 1. Fungsi Repo Ini") || !strings.HasSuffix(t3Thoughts, "matching the terse technical style.") {
		t.Fatalf("turn3: expected thoughts to stop cleanly before ### 1., got %q", t3Thoughts)
	}

	// 4. JSPB ']/[' delimiter seam after internal colon outline (session 01a15c4b turn 4)
	turn4 := "💭\nThe user asks: \"jadi kenapa gemini task mode ga cocok\".\nLet's explain concisely, technically, and clearly in Indonesian why Gemini Task Mode does not fit `gsb`:\n\n1. **Bentrok Orkestrasi**\n5. **Syarat Akun**: mayoritas akun di dalam *account pool* akan gagal (`403`/ineligible).']/['Gemini **Task Mode** tidak cocok dijadikan backend untuk `gsb` karena 5 alasan teknis utama:\n\n### 1. Bentrok Orkestrasi"
	// Simulate mid-stream frame ending right at partial delimiter ".']"
	partialTurn4 := turn4[:strings.Index(turn4, "']/['")+2]
	p4Text, p4Thoughts := sanitizeCandidateTextAndThoughts(partialTurn4, "", false)
	if p4Text != "" || strings.HasSuffix(p4Thoughts, "']") {
		t.Fatalf("turn4 partial: expected held-back delimiter without '], got text=%q thoughts=%q", p4Text, p4Thoughts)
	}
	t4Text, t4Thoughts := sanitizeCandidateTextAndThoughts(turn4, "", true)
	if !strings.HasPrefix(t4Text, "Gemini **Task Mode** tidak cocok") {
		t.Fatalf("turn4: expected answer starting after ']/[' delimiter, got %q", t4Text)
	}
	if strings.Contains(t4Thoughts, "Gemini **Task Mode** tidak cocok") || strings.Contains(t4Thoughts, "']/['") {
		t.Fatalf("turn4: expected thoughts without answer or delimiter, got %q", t4Thoughts)
	}
	if !strings.HasPrefix(t4Thoughts, p4Thoughts) {
		t.Fatalf("turn4: expected final thoughts %q to extend partial thoughts %q monotonically", t4Thoughts, p4Thoughts)
	}
}

func TestExtractStreamEnvelopesSingleArrayNoNewlines(t *testing.T) {
	chunk1 := ")]}'\n\n[[\"wrb.fr\",null,\"[null,[\\\"c_1\\\",\\\"r_1\\\"]]\",null,null,null,\"generic\"]"
	envs, rem, inOuter := extractStreamEnvelopes(chunk1, false, false)
	if len(envs) != 1 || !inOuter || rem != "" {
		t.Fatalf("chunk1: expected 1 envelope, inOuter=true, rem=\"\", got envs=%d inOuter=%v rem=%q", len(envs), inOuter, rem)
	}

	// Split second element across chunk2a and chunk2b with no newlines
	chunk2a := ",[\"wrb.fr\",null,\"[null,[\\\"c_1\\\",\\\"r_1\\\"],null,null,[[\\\"rc_1\\\",[\\\"Hello "
	envs, rem, inOuter = extractStreamEnvelopes(rem+chunk2a, inOuter, false)
	if len(envs) != 0 || !inOuter || rem == "" {
		t.Fatalf("chunk2a: expected 0 envelopes and buffered partial element, got envs=%d inOuter=%v rem=%q", len(envs), inOuter, rem)
	}

	chunk2b := "world\\\"],null,null,null,null,null,null,[1]]]]\"]"
	envs, rem, inOuter = extractStreamEnvelopes(rem+chunk2b, inOuter, false)
	if len(envs) != 1 || !inOuter || rem != "" {
		t.Fatalf("chunk2b: expected 1 completed envelope, got envs=%d inOuter=%v rem=%q", len(envs), inOuter, rem)
	}
	parsed, err := ParseFrameLine(envs[0])
	if err != nil || parsed == nil || len(parsed.Frames) != 1 || parsed.Frames[0].Text != "Hello world" {
		t.Fatalf("chunk2b ParseFrameLine: got %+v err=%v", parsed, err)
	}

	// Trailer chunk closing the outer array
	chunk3 := ",[\"di\",123],[\"af.httprm\",123,\"456\",50]]"
	envs, rem, inOuter = extractStreamEnvelopes(rem+chunk3, inOuter, false)
	if len(envs) != 2 || inOuter || rem != "" {
		t.Fatalf("chunk3: expected 2 trailer envelopes and inOuter=false, got envs=%d inOuter=%v rem=%q", len(envs), inOuter, rem)
	}
	pEnd, err := ParseFrameLine(envs[0])
	if err != nil || pEnd == nil || !pEnd.StreamEnd {
		t.Fatalf("expected ['di',...] to mark StreamEnd=true, got %+v err=%v", pEnd, err)
	}
}

// Gemini leaves a placeholder link where a rendered attachment belongs. The path
// can carry several segments - an image search returns
// `image_collection/image_retrieval/<id>` - and the single-segment pattern that
// shipped leaked the placeholder into the visible answer.
func TestPlaceholderStrippedForMultiSegmentPath(t *testing.T) {
	for _, ph := range []string{
		"https://googleusercontent.com/image_generation_content/abc123",
		"https://googleusercontent.com/image_collection/image_retrieval/123456",
		"https://googleusercontent.com/generated_video_content/xyz",
		"https://googleusercontent.com/some/new/path/42",
	} {
		inner := `[null,["c_1","r_1"],null,null,[["rc_1",["Here you go\n` + ph + `"],null,null,null,{"9":[2]}]]]`
		innerEsc, _ := marshalJSONString(inner)
		line := `[["wrb.fr",null,` + innerEsc + `]]`

		res, err := ParseFrameLine(line)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", ph, err)
		}
		if len(res.Frames) == 0 {
			t.Fatalf("%s: no frames parsed", ph)
		}
		if got := res.Frames[0].Text; strings.Contains(got, "googleusercontent.com") {
			t.Errorf("%s: placeholder survived into text: %q", ph, got)
		}
	}
}

// A card reply puts only a card_content link in the text slot; the answer body
// is field 22. Stripping the link without substituting the body delivered an
// empty answer to the client.
func TestCardContentReplacedByBody(t *testing.T) {
	inner := `[null,["c_1","r_1"],null,null,[["rc_1",["https://googleusercontent.com/card_content/987654"],null,null,null,{"9":[2],"23":["The card body answer."]}]]]`
	innerEsc, _ := marshalJSONString(inner)
	line := `[["wrb.fr",null,` + innerEsc + `]]`

	res, err := ParseFrameLine(line)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if len(res.Frames) == 0 {
		t.Fatal("no frames parsed")
	}
	got := res.Frames[0].Text
	if strings.Contains(got, "card_content") {
		t.Errorf("card link survived: %q", got)
	}
	if !strings.Contains(got, "The card body answer.") {
		t.Errorf("card body was not substituted; got %q", got)
	}
}

// Gemini leaves a placeholder link where a rendered attachment belongs. The path
// can carry several segments - an image search returns
// `image_collection/image_retrieval/<id>` - and the single-segment pattern that
// shipped leaked the placeholder into the visible answer.
func TestPlaceholderDoesNotEatRealMedia(t *testing.T) {
	for _, keep := range []string{
		"https://lh3.googleusercontent.com/gg-dl/abc=s2048",
		"https://contribution.usercontent.google.com/download?c=xyz",
	} {
		inner := `[null,["c_1","r_1"],null,null,[["rc_1",["![img](` + keep + `)"],null,null,null,{"9":[2]}]]]`
		esc, _ := marshalJSONString(inner)
		res, err := ParseFrameLine(`[["wrb.fr",null,` + esc + `]]`)
		if err != nil {
			t.Fatalf("%s: %v", keep, err)
		}
		if len(res.Frames) == 0 {
			t.Fatalf("%s: no frames parsed", keep)
		}
		if !strings.Contains(res.Frames[0].Text, keep) {
			t.Errorf("real media URL was stripped: %s -> %q", keep, res.Frames[0].Text)
		}
	}
}

// When a card reply carries no body in field 22, the link must survive rather
// than be stripped to nothing. An empty answer costs a retry; a followable link
// does not. Image and grounding extraction must still run.
func TestCardContentKeepsLinkWhenBodyMissing(t *testing.T) {
	inner := `[null,["c_1","r_1"],null,null,[["rc_1",["https://googleusercontent.com/card_content/555"],null,null,null,{"9":[2]}]]]`
	esc, _ := marshalJSONString(inner)
	res, err := ParseFrameLine(`[["wrb.fr",null,` + esc + `]]`)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if len(res.Frames) == 0 {
		t.Fatal("no frames parsed")
	}
	got := res.Frames[0].Text
	if !strings.Contains(got, "card_content") {
		t.Errorf("card link was stripped even though field 22 was empty: %q", got)
	}
}

type scriptedChunkReader struct {
	chunks []scriptedChunk
	idx    int
	off    int
}

type scriptedChunk struct {
	waitCtx bool
	delay   time.Duration
	data    string
	err     error
}

func (r *scriptedChunkReader) Read(p []byte) (int, error) {
	return 0, errors.New("unexpected Read without ctx")
}

type ctxScriptedReadCloser struct {
	ctx    context.Context
	chunks []scriptedChunk
	idx    int
	off    int
}

func (r *ctxScriptedReadCloser) Read(p []byte) (int, error) {
	for r.idx < len(r.chunks) {
		cur := &r.chunks[r.idx]
		if cur.delay > 0 {
			time.Sleep(cur.delay)
			cur.delay = 0
		}
		if cur.waitCtx {
			cur.waitCtx = false
			<-r.ctx.Done()
		}
		if r.off < len(cur.data) {
			n := copy(p, cur.data[r.off:])
			r.off += n
			return n, nil
		}
		err := cur.err
		r.idx++
		r.off = 0
		if err != nil {
			return 0, err
		}
	}
	return 0, io.EOF
}

func (r *ctxScriptedReadCloser) Close() error { return nil }

func TestStreamGenerateNoFalseStallWhenFrameArrivesAtWatchdogExpiry(t *testing.T) {
	inner := `[null,["c_1","r_1"],null,null,[["rc_1",["valid answer"],null,null,null,null,null,null,[2]]],{"26":"ctx1","40":"95a4c0abcba24364","43":"4 Argon"}]`
	esc, _ := marshalJSONString(inner)
	frameLine := ")]}'\n\n" + `[["wrb.fr",null,` + esc + `]]` + "\n"

	doer := &stubGeminiDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{},
				Body: &ctxScriptedReadCloser{
					ctx: req.Context(),
					chunks: []scriptedChunk{
						{waitCtx: true, data: frameLine, err: io.EOF},
					},
				},
			}, nil
		},
	}
	pool := transport.NewPool(transport.PoolOptions{
		ClientConfig: transport.ClientConfig{CustomDoer: doer},
		MaxSessions:  2,
	})
	ac := NewAccountClient("acct1", "SAPISID=sap1", "", pool, nil)
	ac.SetTokens("at_tok", "bl_tok", "fsid_1")

	var gotFrames int
	res, err := ac.StreamGenerateLive(context.Background(), "hello", StreamOptions{
		Model:          &ModelInfo{ModelID: "95a4c0abcba24364", ModelName: "gemini-argon", Capacity: 2, ModelNumber: 4},
		ThinkingLevel:  1,
		NoFrameTimeout: 10 * time.Millisecond,
	}, func(update *LineParseResult) error {
		gotFrames += len(update.Frames)
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error when valid frame delivered after watchdog expiry, got %v", err)
	}
	if res == nil || res.Status != 200 || res.Empty || gotFrames == 0 {
		t.Fatalf("unexpected response: res=%+v gotFrames=%d", res, gotFrames)
	}
}

func TestStreamGeneratePartialFrameAtWatchdogExpiryNotClientCancelled(t *testing.T) {
	inner := `[null,["c_1","r_1"],null,null,[["rc_1",["partial"],null,null,null,null,null,null,[1]]],{"40":"95a4c0abcba24364"}]`
	esc, _ := marshalJSONString(inner)
	partialLine := ")]}'\n\n" + `[["wrb.fr",null,` + esc + `]]` + "\n"

	doer := &stubGeminiDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{},
				Body: &ctxScriptedReadCloser{
					ctx: req.Context(),
					chunks: []scriptedChunk{
						{waitCtx: true, data: partialLine},
						{err: context.Canceled},
					},
				},
			}, nil
		},
	}
	pool := transport.NewPool(transport.PoolOptions{
		ClientConfig: transport.ClientConfig{CustomDoer: doer},
		MaxSessions:  2,
	})
	ac := NewAccountClient("acct1", "SAPISID=sap1", "", pool, nil)
	ac.SetTokens("at_tok", "bl_tok", "fsid_1")

	_, err := ac.StreamGenerateLive(context.Background(), "hello", StreamOptions{
		Model:          &ModelInfo{ModelID: "95a4c0abcba24364", ModelName: "gemini-argon", Capacity: 2, ModelNumber: 4},
		ThinkingLevel:  1,
		NoFrameTimeout: 10 * time.Millisecond,
	}, nil)
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("expected UpstreamError, got %v", err)
	}
	if ue.Code == "client_cancelled" || ue.Code == "upstream_stalled" {
		t.Fatalf("expected partial frame at watchdog expiry NOT to return client_cancelled or upstream_stalled, got %d %s", ue.StatusCode, ue.Code)
	}
	if ue.StatusCode != 502 || ue.Code != "incomplete_upstream_response" {
		t.Fatalf("expected 502 incomplete_upstream_response, got %d %s", ue.StatusCode, ue.Code)
	}
}

func TestStreamGenerateKeepaliveOnlyReturnsUpstreamStalled(t *testing.T) {
	keepalive := ")]}'\n\n" + `[["wrb.fr",null,null,null,null,[3]]]` + "\n"
	doer := &stubGeminiDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{},
				Body: &ctxScriptedReadCloser{
					ctx: req.Context(),
					chunks: []scriptedChunk{
						{data: keepalive},
						{waitCtx: true, err: context.Canceled},
					},
				},
			}, nil
		},
	}
	pool := transport.NewPool(transport.PoolOptions{
		ClientConfig: transport.ClientConfig{CustomDoer: doer},
		MaxSessions:  2,
	})
	ac := NewAccountClient("acct1", "SAPISID=sap1", "", pool, nil)
	ac.SetTokens("at_tok", "bl_tok", "fsid_1")

	_, err := ac.StreamGenerateLive(context.Background(), "hello", StreamOptions{
		Model:          &ModelInfo{ModelID: "95a4c0abcba24364", ModelName: "gemini-argon", Capacity: 2, ModelNumber: 4},
		ThinkingLevel:  1,
		NoFrameTimeout: 10 * time.Millisecond,
	}, nil)
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 504 || ue.Code != "upstream_stalled" {
		t.Fatalf("expected 504 upstream_stalled for keepalive-only stream, got %v", err)
	}
}

func TestStreamGenerateWatchdogRequiresMeaningfulCandidate(t *testing.T) {
	const timeout = 10 * time.Second
	line := func(payload string) string {
		esc, _ := marshalJSONString(payload)
		return `[["wrb.fr",null,` + esc + `]]` + "\n"
	}
	empty := line(`[null,null,null,null,[["rc_1",[""],null,null,null,null,null,null,[1]]]]`)
	activity := line(`[null,null,null,null,null,null,["Searching the web"]]`)
	done := line(`[null,null,null,null,[["rc_1",["answer"],null,null,null,null,null,null,[2]]],{"26":"ctx1"}]`)
	for _, tc := range []struct {
		name      string
		chunks    []scriptedChunk
		wantStall bool
	}{
		{"empty candidate", []scriptedChunk{{data: empty}, {waitCtx: true, err: context.Canceled}}, true},
		{"repeated activity", []scriptedChunk{{data: activity}, {delay: 6 * time.Second, data: activity}, {waitCtx: true, err: context.Canceled}}, true},
		{"new activity extends deadline", []scriptedChunk{{delay: 6 * time.Second, data: activity}, {delay: 6 * time.Second, data: done}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				doer := &stubGeminiDoer{fn: func(req *fhttp.Request) (*fhttp.Response, error) {
					return &fhttp.Response{StatusCode: 200, Body: &ctxScriptedReadCloser{ctx: req.Context(), chunks: tc.chunks}}, nil
				}}
				pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}})
				ac := NewAccountClient("fixture", "SAPISID=fixture", "", pool, nil)
				ac.SetTokens("at", "bl", "fsid")
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				start := time.Now()
				var answer string
				_, err := ac.StreamGenerateLive(ctx, "hello", StreamOptions{NoFrameTimeout: timeout}, func(update *LineParseResult) error {
					for _, f := range update.Frames {
						if f.Done {
							answer = f.Text
						}
					}
					return nil
				})
				if tc.wantStall {
					var ue *transport.UpstreamError
					if !errors.As(err, &ue) || ue.StatusCode != 504 || ue.Code != "upstream_stalled" || time.Since(start) != timeout {
						t.Fatalf("expected upstream_stalled at %s, got %v after %s", timeout, err, time.Since(start))
					}
				} else if err != nil || answer != "answer" {
					t.Fatalf("expected completed answer after new activity, got %q, %v", answer, err)
				}
			})
		})
	}
}

func fhttpHeader(req *fhttp.Request, key string) string {
	if vals := req.Header[strings.ToLower(key)]; len(vals) > 0 {
		return vals[0]
	}
	return req.Header.Get(key)
}

func TestStreamGenerateUsesPostBootstrapSelfHealedCookie(t *testing.T) {
	var streamCookie string
	rotated := false
	doer := &stubGeminiDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.HasSuffix(u, "/app"):
				if !rotated {
					return &fhttp.Response{StatusCode: 200, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader(`<html>expired</html>`))}, nil
				}
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_healed","cfb2h":"boq_1","FdrFJe":"111"}`)),
				}, nil
			case strings.Contains(u, "RotateCookies"):
				rotated = true
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=healed_ts; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader("[]")),
				}, nil
			case strings.Contains(u, "StreamGenerate"):
				streamCookie = fhttpHeader(req, "cookie")
				inner := `[null,["c_1","r_1"],null,null,[["rc_1",["ok"],null,null,null,null,null,null,[2]]],{"40":"95a4c0abcba24364","43":"4 Argon"}]`
				esc, _ := marshalJSONString(inner)
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader(")]}'\n\n" + `[["wrb.fr",null,` + esc + `]]` + "\n")),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}

	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
	defer pool.Reset()

	// Pre-populate session in pool with the stale cookie before StreamGenerateLive runs.
	staleCookie := "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=stale_ts"
	if _, err := pool.AcquireWithProxyWithGen(context.Background(), "acct_heal", staleCookie, "", 1); err != nil {
		t.Fatal(err)
	}

	ac := NewAccountClient("acct_heal", staleCookie, "", pool, cookies.NewRotator())
	_, err := ac.StreamGenerateLive(context.Background(), "hello", StreamOptions{
		Model:         &ModelInfo{ModelID: "95a4c0abcba24364", ModelName: "gemini-argon"},
		ThinkingLevel: 1,
	}, nil)
	if err != nil {
		t.Fatalf("StreamGenerateLive failed: %v", err)
	}
	if !strings.Contains(streamCookie, "__Secure-1PSIDTS=healed_ts") {
		t.Fatalf("expected StreamGenerate to send post-bootstrap healed cookie, got %q", streamCookie)
	}
	if !strings.Contains(ac.CookieHeader(), "__Secure-1PSIDTS=healed_ts") {
		t.Fatalf("expected AccountClient to retain healed cookie, got %q", ac.CookieHeader())
	}
}

func TestUploadFileDoesNotClobberConcurrentRotation(t *testing.T) {
	leaseEntered := make(chan struct{})
	releaseLease := make(chan struct{})
	uploadStarted := make(chan struct{})
	releaseUpload := make(chan struct{})
	var uploadReqCookie string
	var rotCalls int

	doer := &stubGeminiDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.Contains(u, "content-push.googleapis.com"):
				uploadReqCookie = fhttpHeader(req, "cookie")
				close(uploadStarted)
				<-releaseUpload
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=stale_upload_ts; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader("/contrib_service/ok.png")),
				}, nil
			case strings.Contains(u, "RotateCookies"):
				rotCalls++
				ts := "rotated_while_queued"
				if rotCalls > 1 {
					ts = "rotated_while_upload_inflight"
				}
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=" + ts + "; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader("[]")),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}

	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2, MaxQueue: 4})
	defer pool.Reset()

	ac := NewAccountClient("acct_up", "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=init_ts", "", pool, cookies.NewRotator())
	sess, err := ac.acquireSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Hold the session lease first so UploadFile blocks waiting in sess.Lease.
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- sess.Lease(context.Background(), func(_ string) error {
			close(leaseEntered)
			<-releaseLease
			return nil
		})
	}()
	<-leaseEntered

	upDone := make(chan error, 1)
	go func() {
		_, uErr := ac.UploadFile(context.Background(), []byte("pngdata"), "a.png", "image/png")
		upDone <- uErr
	}()

	// Rotate cookies while UploadFile is queued in Lease, then release holder.
	time.Sleep(25 * time.Millisecond)
	if rotRes, err := ac.RotateCookies(context.Background()); err != nil || !rotRes.Rotated {
		t.Fatalf("RotateCookies #1 failed: res=%+v err=%v", rotRes, err)
	}
	close(releaseLease)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	<-uploadStarted
	if !strings.Contains(uploadReqCookie, "__Secure-1PSIDTS=rotated_while_queued") {
		t.Fatalf("expected UploadFile to send latest rotated cookie instead of stale leasedCookie, got %q", uploadReqCookie)
	}

	// Rotate cookies AGAIN while upload HTTP call is in flight (before Set-Cookie absorption).
	if rotRes, err := ac.RotateCookies(context.Background()); err != nil || !rotRes.Rotated {
		t.Fatalf("RotateCookies #2 failed: res=%+v err=%v", rotRes, err)
	}
	close(releaseUpload)
	if err := <-upDone; err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}
	if !strings.Contains(ac.CookieHeader(), "__Secure-1PSIDTS=rotated_while_upload_inflight") {
		t.Fatalf("UploadFile Set-Cookie clobbered in-flight rotation: %q", ac.CookieHeader())
	}
}

func TestUploadFileFallbackPreservesConcurrentRotation(t *testing.T) {
	initEntered := make(chan struct{})
	releaseInit := make(chan struct{})
	putEntered := make(chan struct{})
	releasePut := make(chan struct{})
	var putCookie string
	var rotCalls int

	doer := &stubGeminiDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.Contains(u, "content-push.googleapis.com"):
				return &fhttp.Response{StatusCode: 500, Header: fhttp.Header{}, Body: io.NopCloser(strings.NewReader("fail"))}, nil
			case strings.Contains(u, "push.clients6.google.com/upload/") && fhttpHeader(req, "x-goog-upload-command") == "start":
				close(initEntered)
				<-releaseInit
				return &fhttp.Response{
					StatusCode: 200,
					Header: fhttp.Header{
						"X-Goog-Upload-Url": []string{"https://push.clients6.google.com/upload/put1"},
						"Set-Cookie":        []string{"__Secure-1PSIDTS=stale_init_ts; Path=/; Secure"},
					},
					Body: io.NopCloser(strings.NewReader("")),
				}, nil
			case strings.Contains(u, "push.clients6.google.com/upload/put1"):
				putCookie = fhttpHeader(req, "cookie")
				close(putEntered)
				<-releasePut
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=stale_put_ts; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader("/contrib_service/fallback.png")),
				}, nil
			case strings.Contains(u, "RotateCookies"):
				rotCalls++
				ts := "rot_during_init"
				if rotCalls > 1 {
					ts = "rot_during_put"
				}
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=" + ts + "; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader("[]")),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}

	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2, MaxQueue: 4})
	defer pool.Reset()
	ac := NewAccountClient("acct_up_fb", "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=init_ts", "", pool, cookies.NewRotator())

	upDone := make(chan error, 1)
	go func() {
		_, err := ac.UploadFile(context.Background(), []byte("pngdata"), "b.png", "image/png")
		upDone <- err
	}()
	<-initEntered
	if rotRes, err := ac.RotateCookies(context.Background()); err != nil || !rotRes.Rotated {
		t.Fatalf("RotateCookies #1 failed: res=%+v err=%v", rotRes, err)
	}
	close(releaseInit)

	<-putEntered
	if rotRes, err := ac.RotateCookies(context.Background()); err != nil || !rotRes.Rotated {
		t.Fatalf("RotateCookies #2 failed: res=%+v err=%v", rotRes, err)
	}
	close(releasePut)

	if err := <-upDone; err != nil {
		t.Fatalf("UploadFile fallback failed: %v", err)
	}
	if !strings.Contains(putCookie, "__Secure-1PSIDTS=rot_during_init") {
		t.Fatalf("expected PUT step to read latest rotated cookie, got %q", putCookie)
	}
	if !strings.Contains(ac.CookieHeader(), "__Secure-1PSIDTS=rot_during_put") {
		t.Fatalf("fallback upload Set-Cookie clobbered rotated cookie: %q", ac.CookieHeader())
	}
}

func TestPersistStateSerializedNoStaleOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acct.json")
	ac := NewAccountClient("acct_persist", "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts0", path, nil, nil)
	ac.persistState()

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if idx%2 == 0 {
				ac.SetDisabled(idx%4 == 0)
			} else {
				ac.SetSurfaceStatus("geminiWeb", "ok")
			}
		}(i)
	}
	wg.Wait()

	// Final rotated cookie write followed by concurrent status updates must preserve latest cookie on disk.
	ac.absorbAndPersistCookies([]string{"__Secure-1PSIDTS=final_ts; Path=/; Secure"})
	var wg2 sync.WaitGroup
	for range 10 {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			ac.SetDisabled(false)
		}()
	}
	wg2.Wait()

	rec, _, err := cookies.LoadAccountFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Cookies, "__Secure-1PSIDTS=final_ts") {
		t.Fatalf("disk lost final cookie during concurrent persistState: %q", rec.Cookies)
	}
}

func TestReloadFromDiskIfNewerAllFieldsAndMtimeRecheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reload.json")
	if _, err := cookies.SaveCredentialFile(path, cookies.CredentialFile{
		Name:         "reload",
		Email:        "user@example.com",
		Cookies:      "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts1",
		AT:           "at1",
		AuthUser:     1,
		Disabled:     false,
		AccountGroup: "team-a",
		ProxyURL:     "http://127.0.0.1:8001",
		Surfaces:     map[string]string{"geminiWeb": "ok"},
	}); err != nil {
		t.Fatal(err)
	}

	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: &stubGeminiDoer{}}, MaxSessions: 2})
	defer pool.Reset()
	ac, err := LoadAccountFromFile(path, pool, cookies.NewRotator())
	if err != nil {
		t.Fatal(err)
	}
	ac.mu.Lock()
	ac.modelsCache = &AvailableModelsResult{Status: 200}
	ac.mu.Unlock()

	// Unchanged mtime returns false and preserves modelsCache.
	if reloaded := ac.ReloadFromDiskIfNewer(); reloaded {
		t.Fatal("expected ReloadFromDiskIfNewer=false when mtime unchanged")
	}
	ac.mu.RLock()
	hasCache := ac.modelsCache != nil
	ac.mu.RUnlock()
	if !hasCache {
		t.Fatal("expected modelsCache preserved on unchanged mtime")
	}

	// Update file with newer mtime and changed metadata + cookies.
	time.Sleep(15 * time.Millisecond)
	if _, err := cookies.SaveCredentialFile(path, cookies.CredentialFile{
		Name:         "reload",
		Email:        "user@example.com",
		Cookies:      "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts2",
		AT:           "at2",
		AuthUser:     2,
		Disabled:     true,
		AccountGroup: "team-b",
		ProxyURL:     "http://127.0.0.1:8002",
		Surfaces:     map[string]string{"geminiWeb": "blocked"},
	}); err != nil {
		t.Fatal(err)
	}
	// Ensure mtime strictly advances even on coarse filesystems.
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, future, future)

	if reloaded := ac.ReloadFromDiskIfNewer(); !reloaded {
		t.Fatal("expected ReloadFromDiskIfNewer=true when file updated")
	}
	ac.mu.RLock()
	gotDisabled := ac.disabled
	gotGroup := ac.accountGroup
	gotProxy := ac.proxyURL
	gotSurf := ac.surfaces["geminiWeb"]
	gotAuthUser := ac.authUser
	gotAT := ac.at
	gotCache := ac.modelsCache
	ac.mu.RUnlock()
	if !gotDisabled || gotGroup != "team-b" || gotProxy != "http://127.0.0.1:8002" || gotSurf != "blocked" || gotAuthUser != 2 || gotAT != "at2" || gotCache != nil {
		t.Fatalf("reload mismatch: disabled=%v group=%q proxy=%q surf=%q authUser=%d at=%q cache=%v",
			gotDisabled, gotGroup, gotProxy, gotSurf, gotAuthUser, gotAT, gotCache)
	}

	// Re-clearing Disabled=false on disk must clear ac.disabled in memory.
	time.Sleep(15 * time.Millisecond)
	if _, err := cookies.SaveCredentialFile(path, cookies.CredentialFile{
		Name:     "reload",
		Email:    "user@example.com",
		Cookies:  "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts2",
		AuthUser: 2,
		Disabled: false,
	}); err != nil {
		t.Fatal(err)
	}
	future2 := time.Now().Add(4 * time.Second)
	_ = os.Chtimes(path, future2, future2)
	if !ac.ReloadFromDiskIfNewer() {
		t.Fatal("expected ReloadFromDiskIfNewer=true on Disabled=false update")
	}
	ac.mu.RLock()
	stillDisabled := ac.disabled
	ac.mu.RUnlock()
	if stillDisabled {
		t.Fatal("expected Disabled=false on disk to clear ac.disabled")
	}

	// Concurrent ReloadFromDiskIfNewer + persistState race check.
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				ac.ReloadFromDiskIfNewer()
				ac.persistState()
			}
		}()
	}
	wg.Wait()
}

func TestRotateOnClientInFlightRejectsStaleOverwriteOnDiskReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rot_reload.json")
	initCookie := "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts_init"
	if _, err := cookies.SaveCredentialFile(path, cookies.CredentialFile{
		Name:    "rot_reload",
		Email:   "u@example.com",
		Cookies: initCookie,
	}); err != nil {
		t.Fatal(err)
	}

	rotStarted := make(chan struct{})
	releaseRot := make(chan struct{})
	doer := &stubGeminiDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			close(rotStarted)
			<-releaseRot
			return &fhttp.Response{
				StatusCode: 200,
				Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=ts_from_inflight_rot; Path=/; Secure"}},
				Body:       io.NopCloser(strings.NewReader("[]")),
			}, nil
		},
	}
	sc, _ := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
	ac, err := LoadAccountFromFile(path, nil, cookies.NewRotator())
	if err != nil {
		t.Fatal(err)
	}

	rotCh := make(chan *cookies.RotateResult, 1)
	go func() {
		r, _ := ac.rotateOnClient(context.Background(), sc)
		rotCh <- r
	}()
	<-rotStarted

	// External disk reload advances cookieGen while rotateOnClient is in flight.
	if _, err := cookies.SaveCredentialFile(path, cookies.CredentialFile{
		Name:    "rot_reload",
		Email:   "u@example.com",
		Cookies: "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts_disk_relogin",
	}); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, future, future)
	if !ac.ReloadFromDiskIfNewer() {
		t.Fatal("expected ReloadFromDiskIfNewer to pick up new disk cookie")
	}

	close(releaseRot)
	res := <-rotCh
	if res == nil || res.Rotated {
		t.Fatalf("expected stale in-flight rotateOnClient to return Rotated=false, got %+v", res)
	}
	if !strings.Contains(ac.CookieHeader(), "__Secure-1PSIDTS=ts_disk_relogin") {
		t.Fatalf("expected disk relogin cookie preserved, got %q", ac.CookieHeader())
	}
}

func TestRotateOnClientConcurrentWithStreamAbsorption(t *testing.T) {
	rotStarted := make(chan struct{})
	releaseRot := make(chan struct{})
	doer := &stubGeminiDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			if strings.Contains(req.URL.String(), "RotateCookies") {
				close(rotStarted)
				<-releaseRot
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=rot_ts; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader("[]")),
				}, nil
			}
			return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	}
	sc, _ := transport.NewStealthClient(transport.ClientConfig{CustomDoer: doer})
	ac := NewAccountClient("acct_conc", "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=init_ts", "", nil, cookies.NewRotator())

	rotCh := make(chan *cookies.RotateResult, 1)
	go func() {
		r, _ := ac.rotateOnClient(context.Background(), sc)
		rotCh <- r
	}()
	<-rotStarted

	// Stream response Set-Cookie absorbed while rotateOnClient is in flight.
	if !ac.absorbAndPersistCookiesWithGen([]string{"__Secure-1PSIDTS=stream_newer_ts; Path=/; Secure"}, ac.cookieGen.Load(), true) {
		t.Fatal("expected stream Set-Cookie absorption to succeed")
	}
	close(releaseRot)
	res := <-rotCh
	if res == nil || res.Rotated {
		t.Fatalf("expected concurrent rotateOnClient not to clobber stream absorption (Rotated=false), got %+v", res)
	}
	if !strings.Contains(ac.CookieHeader(), "__Secure-1PSIDTS=stream_newer_ts") {
		t.Fatalf("expected stream_newer_ts preserved, got %q", ac.CookieHeader())
	}
}

func TestBootstrapFollowsSafeSameOriginRedirects(t *testing.T) {
	t.Run("safe_locale_and_relative_redirects_succeed", func(t *testing.T) {
		var hops int
		doer := &stubGeminiDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				u := req.URL.String()
				hops++
				switch u {
				case "https://gemini.google.com/app":
					return &fhttp.Response{
						StatusCode: 302,
						Header:     fhttp.Header{"Location": []string{"https://gemini.google.com/app?hl=id"}},
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				case "https://gemini.google.com/app?hl=id":
					return &fhttp.Response{
						StatusCode: 307,
						Header:     fhttp.Header{"Location": []string{"/app?gl=ID"}},
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				case "https://gemini.google.com/app?gl=ID":
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{},
						Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_redirect_ok","cfb2h":"boq_r","FdrFJe":"777"}`)),
					}, nil
				default:
					return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
			},
		}
		pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
		defer pool.Reset()
		ac := NewAccountClient("acct_redir", "SAPISID=sap1; __Secure-1PSID=p1", "", pool, cookies.NewRotator())
		if err := ac.Bootstrap(context.Background()); err != nil {
			t.Fatalf("expected safe same-origin redirects to succeed, got %v", err)
		}
		if hops != 3 {
			t.Fatalf("expected 3 hops, got %d", hops)
		}
	})

	t.Run("signin_redirect_strips_query_secret", func(t *testing.T) {
		doer := &stubGeminiDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				if strings.Contains(req.URL.String(), "RotateCookies") {
					return &fhttp.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("unauth"))}, nil
				}
				return &fhttp.Response{
					StatusCode: 302,
					Header:     fhttp.Header{"Location": []string{"https://accounts.google.com/ServiceLogin?continue=x&secret=MY_SECRET"}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			},
		}
		pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
		defer pool.Reset()
		ac := NewAccountClient("acct_signin", "SAPISID=sap1; __Secure-1PSID=p1", "", pool, cookies.NewRotator())
		err := ac.Bootstrap(context.Background())
		var ue *transport.UpstreamError
		if !errors.As(err, &ue) || ue.StatusCode != 401 || ue.Code != "auth_redirect" {
			t.Fatalf("expected 401 auth_redirect, got %v", err)
		}
		if !strings.Contains(ue.Message, "accounts.google.com/ServiceLogin") || strings.Contains(ue.Message, "MY_SECRET") {
			t.Fatalf("unexpected error message (must sanitize secret): %q", ue.Message)
		}
	})

	t.Run("consent_redirect_not_auth_redirect_and_loop_bounded", func(t *testing.T) {
		var loopCalls int
		doer := &stubGeminiDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				u := req.URL.String()
				if strings.Contains(u, "RotateCookies") {
					return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("[]"))}, nil
				}
				if strings.Contains(u, "consent") {
					return &fhttp.Response{
						StatusCode: 302,
						Header:     fhttp.Header{"Location": []string{"https://consent.google.com/ml?continue=https://accounts.google.com/ServiceLogin"}},
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				}
				loopCalls++
				return &fhttp.Response{
					StatusCode: 302,
					Header:     fhttp.Header{"Location": []string{fmt.Sprintf("https://gemini.google.com/app?hop=%d", loopCalls)}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			},
		}
		pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
		defer pool.Reset()
		ac := NewAccountClient("acct_loop", "SAPISID=sap1; __Secure-1PSID=p1", "", pool, cookies.NewRotator())
		err := ac.Bootstrap(context.Background())
		var ue *transport.UpstreamError
		if !errors.As(err, &ue) || ue.Code == "auth_redirect" {
			t.Fatalf("expected bounded loop error != auth_redirect, got %v", err)
		}
		if loopCalls > 6 {
			t.Fatalf("expected bounded redirect loop <= 6 hops, got %d", loopCalls)
		}
	})
}

func TestAvailableModelsRetryChecksNon200Status(t *testing.T) {
	for _, retryStatus := range []struct {
		httpStatus int
		wantCode   string
	}{
		{429, "rate_limit_error"},
		{503, "model_discovery_error"},
	} {
		t.Run(fmt.Sprintf("retry_%d", retryStatus.httpStatus), func(t *testing.T) {
			var batchCalls int
			doer := &stubGeminiDoer{
				fn: func(req *fhttp.Request) (*fhttp.Response, error) {
					u := req.URL.String()
					if strings.HasSuffix(u, "/app") {
						return &fhttp.Response{
							StatusCode: 200,
							Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_ok","cfb2h":"boq_1","FdrFJe":"111"}`)),
						}, nil
					}
					if strings.Contains(u, "batchexecute") {
						batchCalls++
						if batchCalls == 1 {
							inner := `[null,null,null,null,null,null,null,null,null,null,null,null,null,null,1016]`
							esc, _ := marshalJSONString(inner)
							return &fhttp.Response{
								StatusCode: 200,
								Body:       io.NopCloser(strings.NewReader(")]}'\n\n" + `[["wrb.fr","otAQ7b",` + esc + `]]`)),
							}, nil
						}
						return &fhttp.Response{
							StatusCode: retryStatus.httpStatus,
							Body:       io.NopCloser(strings.NewReader("retry failed")),
						}, nil
					}
					return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
				},
			}
			pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
			defer pool.Reset()
			ac := NewAccountClient("acct_retry", "SAPISID=sap1; __Secure-1PSID=p1", "", pool, cookies.NewRotator())
			ac.SetTokens("at_init", "boq_1", "111")

			_, err := ac.AvailableModels(context.Background(), true)
			var ue *transport.UpstreamError
			if !errors.As(err, &ue) || ue.StatusCode != retryStatus.httpStatus || ue.Code != retryStatus.wantCode {
				t.Fatalf("expected HTTP %d code=%q, got %v", retryStatus.httpStatus, retryStatus.wantCode, err)
			}
			if strings.Contains(ue.Message, "malformed upstream model registry") {
				t.Fatalf("unexpected malformed registry error on HTTP %d: %q", retryStatus.httpStatus, ue.Message)
			}
		})
	}
}

func TestAuthUserSecondaryAccountIsolation(t *testing.T) {
	t.Run("rotates_before_u0_fallback_and_passes_authuser_everywhere", func(t *testing.T) {
		rotated := false
		var sawU0 bool
		var graphqlAuthUser string
		var pollSourcePath, pollAuthUser string

		doer := &stubGeminiDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				u := req.URL.String()
				switch {
				case req.URL.Path == "/u/2/app":
					if !rotated {
						// Stale __Secure-1PSIDTS on /u/2/app bounces to /app; must rotate BEFORE falling back.
						return &fhttp.Response{
							StatusCode: 302,
							Header:     fhttp.Header{"Location": []string{"https://gemini.google.com/app"}},
							Body:       io.NopCloser(strings.NewReader("")),
						}, nil
					}
					if req.URL.Query().Get("hl") == "" {
						return &fhttp.Response{
							StatusCode: 302,
							Header:     fhttp.Header{"Location": []string{"https://gemini.google.com/u/2/app?hl=id"}},
							Body:       io.NopCloser(strings.NewReader("")),
						}, nil
					}
					return &fhttp.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_u2","cfb2h":"boq_u2","FdrFJe":"222"}`)),
					}, nil
				case req.URL.Path == "/app":
					sawU0 = true
					return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"SNlM0e":"at_u0"}`))}, nil
				case strings.Contains(u, "RotateCookies"):
					rotated = true
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=u2_fresh; Path=/; Secure"}},
						Body:       io.NopCloser(strings.NewReader("[]")),
					}, nil
				case strings.Contains(u, "GEMINI_WEB_GRAPHQL"):
					graphqlAuthUser = req.URL.Query().Get("authuser")
					return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[{"data":{"viewer":{"rpcs":{"getUsageInfo":{"pools":[{"poolType":"GXU_FIVE_HOURLY","percentageUsed":0.1}]}}}}}]`))}, nil
				case strings.Contains(u, "rpcids=hNvQHb"):
					pollSourcePath = req.URL.Query().Get("source-path")
					pollAuthUser = req.URL.Query().Get("authuser")
					inner := `[[[["c_vid123","r_1"],null,null,[[["rc_v",["video ready"],null,null,null,null,null,null,[2]]]]]]]`
					esc, _ := marshalJSONString(inner)
					return &fhttp.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader(")]}'\n\n" + `[["wrb.fr","hNvQHb",` + esc + `]]`)),
					}, nil
				default:
					return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("[]"))}, nil
				}
			},
		}

		pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
		defer pool.Reset()
		ac := NewAccountClient("acct_u2", "SAPISID=sap2; __Secure-1PSID=p2; __Secure-1PSIDTS=stale", "", pool, cookies.NewRotator())
		ac.SetAuthUser(2)
		if err := ac.Bootstrap(context.Background()); err != nil {
			t.Fatalf("Bootstrap failed: %v", err)
		}
		if sawU0 {
			t.Fatal("secondary account hijacked to /app before rotation")
		}
		if ac.AuthUser() != 2 {
			t.Fatalf("expected AuthUser=2, got %d", ac.AuthUser())
		}

		buckets := ac.GetQuota(context.Background())
		if len(buckets) == 0 || graphqlAuthUser != "2" {
			t.Fatalf("expected GraphQL quota with authuser=2, got buckets=%d authuser=%q", len(buckets), graphqlAuthUser)
		}

		// Exercise pollAsyncVideoGenerationLocked directly with short context/wait.
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		sess, _ := ac.acquireSession(ctx)
		polled, err := ac.pollAsyncVideoGenerationLocked(ctx, sess.Client, ac.CookieHeader(), "at_u2", "boq_u2", "222", "en", "c_vid123")
		if err != nil || polled == nil || len(polled.Frames) == 0 {
			t.Fatalf("pollAsyncVideoGenerationLocked failed: polled=%+v err=%v", polled, err)
		}
		if pollSourcePath != "/u/2/app/vid123" || pollAuthUser != "2" {
			t.Fatalf("expected source-path=/u/2/app/vid123 and authuser=2, got source-path=%q authuser=%q", pollSourcePath, pollAuthUser)
		}
	})

	t.Run("rejects_unresolved_secondary_slot_after_rotation", func(t *testing.T) {
		var sawRotate, sawU0 bool
		doer := &stubGeminiDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				switch {
				case req.URL.Path == "/u/2/app":
					return &fhttp.Response{
						StatusCode: 302,
						Header:     fhttp.Header{"Location": []string{"https://gemini.google.com/app"}},
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				case req.URL.Path == "/app":
					sawU0 = true
					return &fhttp.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_fallback_u0","cfb2h":"boq_0","FdrFJe":"000"}`)),
					}, nil
				case strings.Contains(req.URL.String(), "RotateCookies"):
					sawRotate = true
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=fresh; Path=/; Secure"}},
						Body:       io.NopCloser(strings.NewReader("[]")),
					}, nil
				default:
					return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
			},
		}
		pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
		defer pool.Reset()
		ac := NewAccountClient("acct_u2_fb", "SAPISID=sap2; __Secure-1PSID=p2; __Secure-1PSIDTS=old", "", pool, cookies.NewRotator())
		ac.SetAuthUser(2)
		if err := ac.Bootstrap(context.Background()); err == nil {
			t.Fatal("expected bootstrap to reject the unresolved secondary slot")
		}
		if !sawRotate || sawU0 || ac.AuthUser() != 2 || ac.at != "" {
			t.Fatalf("expected rotation without primary bootstrap: rotated=%v primary=%v authUser=%d", sawRotate, sawU0, ac.AuthUser())
		}
	})
}

func TestBootstrapRaceWithReloadFromDiskIfNewerNeverPairsStaleAT(t *testing.T) {
	writeNewerCred := func(t *testing.T, path, cookie, at string, bump time.Duration) {
		t.Helper()
		if _, err := cookies.SaveCredentialFile(path, cookies.CredentialFile{
			Kind:    "gemini-web",
			Name:    "acct_boot_race",
			Cookies: cookie,
			AT:      at,
			BL:      "boq_disk",
			FSid:    "333",
		}); err != nil {
			t.Fatalf("SaveCredentialFile: %v", err)
		}
		future := time.Now().Add(bump)
		if err := os.Chtimes(path, future, future); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}

	t.Run("re_fetches_app_when_reload_occurs_during_initial_fetch", func(t *testing.T) {
		credPath := filepath.Join(t.TempDir(), "acct.json")
		if _, err := cookies.SaveCredentialFile(credPath, cookies.CredentialFile{
			Kind:    "gemini-web",
			Name:    "acct_boot_race",
			Cookies: "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts_old",
		}); err != nil {
			t.Fatalf("initial SaveCredentialFile: %v", err)
		}

		firstAppStarted := make(chan struct{})
		releaseFirstApp := make(chan struct{})
		var mu sync.Mutex
		var seenCookies []string

		doer := &stubGeminiDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				cookie := fhttpHeader(req, "cookie")
				mu.Lock()
				seenCookies = append(seenCookies, cookie)
				mu.Unlock()
				if strings.Contains(cookie, "ts_old") {
					close(firstAppStarted)
					<-releaseFirstApp
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{},
						Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_from_old_cookie","cfb2h":"boq_old","FdrFJe":"111"}`)),
					}, nil
				}
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_from_new_cookie","cfb2h":"boq_new","FdrFJe":"222"}`)),
				}, nil
			},
		}

		pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
		defer pool.Reset()
		ac, err := LoadAccountFromFile(credPath, pool, cookies.NewRotator())
		if err != nil {
			t.Fatalf("LoadAccountFromFile: %v", err)
		}

		bootErr := make(chan error, 1)
		go func() {
			bootErr <- ac.Bootstrap(context.Background())
		}()

		<-firstAppStarted
		writeNewerCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts_new_disk", "at_new_disk", 2*time.Second)
		if !ac.ReloadFromDiskIfNewer() {
			t.Fatal("expected ReloadFromDiskIfNewer to reload newer credential")
		}
		close(releaseFirstApp)

		if err := <-bootErr; err != nil {
			t.Fatalf("Bootstrap failed: %v", err)
		}

		ac.mu.RLock()
		gotCookie := ac.cookieHeader
		gotAT := ac.at
		gotBootAt := ac.bootstrappedAt
		ac.mu.RUnlock()

		if !strings.Contains(gotCookie, "ts_new_disk") {
			t.Fatalf("expected cookieHeader to hold ts_new_disk, got %q", gotCookie)
		}
		if gotAT == "at_from_old_cookie" || gotAT != "at_from_new_cookie" {
			t.Fatalf("expected AT from new cookie (never at_from_old_cookie), got %q", gotAT)
		}
		if gotBootAt.IsZero() {
			t.Fatal("expected bootstrappedAt to be set after consistent retry fetch")
		}
		if len(seenCookies) != 2 {
			t.Fatalf("expected 2 /app fetches (initial + consistent retry), got %d", len(seenCookies))
		}
	})

	t.Run("skips_commit_if_reload_also_races_with_retry_fetch", func(t *testing.T) {
		credPath := filepath.Join(t.TempDir(), "acct2.json")
		if _, err := cookies.SaveCredentialFile(credPath, cookies.CredentialFile{
			Kind:    "gemini-web",
			Name:    "acct_boot_race2",
			Cookies: "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts_old",
		}); err != nil {
			t.Fatalf("initial SaveCredentialFile: %v", err)
		}

		firstStarted := make(chan struct{})
		releaseFirst := make(chan struct{})
		secondStarted := make(chan struct{})
		releaseSecond := make(chan struct{})

		doer := &stubGeminiDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				cookie := fhttpHeader(req, "cookie")
				switch {
				case strings.Contains(cookie, "ts_old"):
					close(firstStarted)
					<-releaseFirst
					return &fhttp.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_from_old_cookie","cfb2h":"boq_old","FdrFJe":"111"}`)),
					}, nil
				case strings.Contains(cookie, "ts_disk_1"):
					close(secondStarted)
					<-releaseSecond
					return &fhttp.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_from_disk_1","cfb2h":"boq_1","FdrFJe":"111"}`)),
					}, nil
				default:
					return &fhttp.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("unexpected"))}, nil
				}
			},
		}

		pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
		defer pool.Reset()
		ac, err := LoadAccountFromFile(credPath, pool, cookies.NewRotator())
		if err != nil {
			t.Fatalf("LoadAccountFromFile: %v", err)
		}

		bootErr := make(chan error, 1)
		go func() {
			bootErr <- ac.Bootstrap(context.Background())
		}()

		<-firstStarted
		writeNewerCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts_disk_1", "at_disk_1", 2*time.Second)
		if !ac.ReloadFromDiskIfNewer() {
			t.Fatal("expected first ReloadFromDiskIfNewer to succeed")
		}
		close(releaseFirst)

		<-secondStarted
		writeNewerCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts_disk_2", "at_disk_2", 4*time.Second)
		if !ac.ReloadFromDiskIfNewer() {
			t.Fatal("expected second ReloadFromDiskIfNewer to succeed")
		}
		close(releaseSecond)

		if err := <-bootErr; err != nil {
			t.Fatalf("Bootstrap failed: %v", err)
		}

		ac.mu.RLock()
		gotCookie := ac.cookieHeader
		gotAT := ac.at
		gotBootAt := ac.bootstrappedAt
		ac.mu.RUnlock()

		if !strings.Contains(gotCookie, "ts_disk_2") {
			t.Fatalf("expected cookieHeader to preserve ts_disk_2, got %q", gotCookie)
		}
		if gotAT != "at_disk_2" {
			t.Fatalf("expected AT to stay at_disk_2 (never overwritten by stale /app), got %q", gotAT)
		}
		if !gotBootAt.IsZero() {
			t.Fatalf("expected bootstrappedAt to remain zero when retry raced, got %v", gotBootAt)
		}
	})
}

func TestBootstrapMidRedirectReloadAbortsStaleChain(t *testing.T) {
	writeCred := func(t *testing.T, path, cookie, at string, authUser int, bump time.Duration) {
		t.Helper()
		if _, err := cookies.SaveCredentialFile(path, cookies.CredentialFile{
			Kind:     "gemini-web",
			Name:     "acct_redir_race",
			Cookies:  cookie,
			AT:       at,
			AuthUser: authUser,
			BL:       "boq_disk",
			FSid:     "333",
		}); err != nil {
			t.Fatalf("SaveCredentialFile: %v", err)
		}
		if bump > 0 {
			future := time.Now().Add(bump)
			if err := os.Chtimes(path, future, future); err != nil {
				t.Fatalf("Chtimes: %v", err)
			}
		}
	}

	t.Run("authUser_and_cookies_updated_during_hop0_redirect", func(t *testing.T) {
		credPath := filepath.Join(t.TempDir(), "acct_u2.json")
		writeCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts1", "", 2, 0)

		hop0Started := make(chan struct{})
		releaseHop0 := make(chan struct{})
		var mu sync.Mutex
		var fetchedURLs []string
		var sawStaleU2Redir bool

		doer := &stubGeminiDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				u := req.URL.String()
				mu.Lock()
				fetchedURLs = append(fetchedURLs, u)
				mu.Unlock()

				switch u {
				case "https://gemini.google.com/u/2/app":
					close(hop0Started)
					<-releaseHop0
					return &fhttp.Response{
						StatusCode: 302,
						Header: fhttp.Header{
							"Location":   []string{"https://gemini.google.com/u/2/app?hl=en"},
							"Set-Cookie": []string{"__Secure-1PSIDTS=stale_hop0; Path=/; Secure"},
						},
						Body: io.NopCloser(strings.NewReader("")),
					}, nil
				case "https://gemini.google.com/u/2/app?hl=en":
					mu.Lock()
					sawStaleU2Redir = true
					mu.Unlock()
					return &fhttp.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_from_u2","cfb2h":"boq_u2","FdrFJe":"222"}`)),
					}, nil
				case "https://gemini.google.com/u/3/app":
					cookie := fhttpHeader(req, "cookie")
					if !strings.Contains(cookie, "ts2") {
						t.Errorf("expected /u/3/app to use ts2, got %q", cookie)
					}
					return &fhttp.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_from_u3","cfb2h":"boq_u3","FdrFJe":"333"}`)),
					}, nil
				default:
					return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
			},
		}

		pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
		defer pool.Reset()
		ac, err := LoadAccountFromFile(credPath, pool, cookies.NewRotator())
		if err != nil {
			t.Fatalf("LoadAccountFromFile: %v", err)
		}

		bootCh := make(chan error, 1)
		go func() {
			bootCh <- ac.Bootstrap(context.Background())
		}()

		<-hop0Started
		writeCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts2", "", 3, 2*time.Second)
		if !ac.ReloadFromDiskIfNewer() {
			t.Fatal("expected ReloadFromDiskIfNewer to succeed")
		}
		close(releaseHop0)

		if err := <-bootCh; err != nil {
			t.Fatalf("Bootstrap failed: %v", err)
		}
		mu.Lock()
		staleHit := sawStaleU2Redir
		urlsCopy := append([]string(nil), fetchedURLs...)
		mu.Unlock()
		if staleHit {
			t.Fatalf("stale redirect hop /u/2/app?hl=en should not have been followed; urls=%v", urlsCopy)
		}

		ac.mu.RLock()
		gotAT := ac.at
		gotAU := ac.authUser
		gotCookie := ac.cookieHeader
		ac.mu.RUnlock()
		if gotAT != "at_from_u3" || gotAU != 3 || !strings.Contains(gotCookie, "ts2") {
			t.Fatalf("unexpected final state: at=%q authUser=%d cookie=%q urls=%v", gotAT, gotAU, gotCookie, urlsCopy)
		}
	})

	t.Run("same_authUser_mid_redirect_reload_retries_clean_app", func(t *testing.T) {
		credPath := filepath.Join(t.TempDir(), "acct_u0.json")
		writeCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts1", "", 0, 0)

		hop0Started := make(chan struct{})
		releaseHop0 := make(chan struct{})
		var mu sync.Mutex
		var sawStaleRedir bool
		var cleanTs2Fetches int

		doer := &stubGeminiDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				u := req.URL.String()
				cookie := fhttpHeader(req, "cookie")
				switch u {
				case "https://gemini.google.com/app":
					if strings.Contains(cookie, "ts1") {
						close(hop0Started)
						<-releaseHop0
						return &fhttp.Response{
							StatusCode: 302,
							Header:     fhttp.Header{"Location": []string{"https://gemini.google.com/app?stale_ts1_redir=1"}},
							Body:       io.NopCloser(strings.NewReader("")),
						}, nil
					}
					if strings.Contains(cookie, "ts2") {
						mu.Lock()
						cleanTs2Fetches++
						mu.Unlock()
						return &fhttp.Response{
							StatusCode: 200,
							Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_clean_ts2","cfb2h":"boq_ts2","FdrFJe":"444"}`)),
						}, nil
					}
				case "https://gemini.google.com/app?stale_ts1_redir=1":
					mu.Lock()
					sawStaleRedir = true
					mu.Unlock()
					return &fhttp.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader("<html>no SNlM0e here</html>")),
					}, nil
				}
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			},
		}

		pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
		defer pool.Reset()
		ac, err := LoadAccountFromFile(credPath, pool, cookies.NewRotator())
		if err != nil {
			t.Fatalf("LoadAccountFromFile: %v", err)
		}

		bootCh := make(chan error, 1)
		go func() {
			bootCh <- ac.Bootstrap(context.Background())
		}()

		<-hop0Started
		writeCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts2", "", 0, 2*time.Second)
		if !ac.ReloadFromDiskIfNewer() {
			t.Fatal("expected ReloadFromDiskIfNewer to succeed")
		}
		close(releaseHop0)

		if err := <-bootCh; err != nil {
			t.Fatalf("Bootstrap failed: %v", err)
		}
		mu.Lock()
		staleHit := sawStaleRedir
		cleanHits := cleanTs2Fetches
		mu.Unlock()
		if staleHit {
			t.Fatal("stale redirect /app?stale_ts1_redir=1 must not be followed after ReloadFromDiskIfNewer")
		}
		if cleanHits != 1 {
			t.Fatalf("expected 1 clean /app fetch with ts2, got %d", cleanHits)
		}
		ac.mu.RLock()
		gotAT := ac.at
		ac.mu.RUnlock()
		if gotAT != "at_clean_ts2" {
			t.Fatalf("expected at_clean_ts2, got %q", gotAT)
		}
	})

	t.Run("double_race_mid_redirect_on_retry_returns_503_not_stale_302", func(t *testing.T) {
		credPath := filepath.Join(t.TempDir(), "acct_dbl_redir.json")
		writeCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts1", "", 2, 0)

		u2Started := make(chan struct{})
		releaseU2 := make(chan struct{})
		u3Started := make(chan struct{})
		releaseU3 := make(chan struct{})

		doer := &stubGeminiDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				switch req.URL.String() {
				case "https://gemini.google.com/u/2/app":
					close(u2Started)
					<-releaseU2
					return &fhttp.Response{
						StatusCode: 302,
						Header:     fhttp.Header{"Location": []string{"https://gemini.google.com/u/2/app?hl=en"}},
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				case "https://gemini.google.com/u/3/app":
					close(u3Started)
					<-releaseU3
					return &fhttp.Response{
						StatusCode: 302,
						Header:     fhttp.Header{"Location": []string{"https://gemini.google.com/u/3/app?hl=en"}},
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				default:
					return &fhttp.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("unexpected redirect hop"))}, nil
				}
			},
		}

		pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
		defer pool.Reset()
		ac, err := LoadAccountFromFile(credPath, pool, cookies.NewRotator())
		if err != nil {
			t.Fatalf("LoadAccountFromFile: %v", err)
		}

		bootCh := make(chan error, 1)
		go func() {
			bootCh <- ac.Bootstrap(context.Background())
		}()

		<-u2Started
		writeCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts2", "", 3, 2*time.Second)
		if !ac.ReloadFromDiskIfNewer() {
			t.Fatal("expected first ReloadFromDiskIfNewer to succeed")
		}
		close(releaseU2)

		<-u3Started
		writeCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts3", "", 4, 4*time.Second)
		if !ac.ReloadFromDiskIfNewer() {
			t.Fatal("expected second ReloadFromDiskIfNewer to succeed")
		}
		close(releaseU3)

		err = <-bootCh
		var ue *transport.UpstreamError
		if !errors.As(err, &ue) || ue.StatusCode != 503 || ue.Code != "upstream_transient" {
			t.Fatalf("expected 503 upstream_transient (not stale 302), got %v", err)
		}
	})
}

func TestBootstrapPostRotationRetryErrorPropagated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var appCalls int
	var rotated bool
	doer := &stubGeminiDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.Contains(u, "RotateCookies"):
				rotated = true
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{"Set-Cookie": []string{"__Secure-1PSIDTS=rot_fresh; Path=/; Secure"}},
					Body:       io.NopCloser(strings.NewReader("[]")),
				}, nil
			case strings.HasSuffix(u, "/app"):
				appCalls++
				if appCalls == 1 {
					return &fhttp.Response{
						StatusCode: 302,
						Header:     fhttp.Header{"Location": []string{"https://accounts.google.com/ServiceLogin?continue=https://gemini.google.com/app"}},
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				}
				cancel()
				return nil, transport.NewUpstreamError("request cancelled", 499, "client_cancelled", nil, context.Canceled)
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}

	pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
	defer pool.Reset()
	ac := NewAccountClient("acct_post_rot_err", "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=old", "", pool, cookies.NewRotator())

	err := ac.Bootstrap(ctx)
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 499 || ue.Code != "client_cancelled" {
		t.Fatalf("expected 499 client_cancelled (never 401 auth_redirect), got %v", err)
	}
	if !rotated || appCalls != 2 {
		t.Fatalf("expected rotation + 2 /app attempts, got rotated=%v appCalls=%d", rotated, appCalls)
	}
}

func TestBootstrapDoubleRaceWithCookieOnlyDiskReload(t *testing.T) {
	writeCookieOnlyCred := func(t *testing.T, path, cookie string, bump time.Duration) {
		t.Helper()
		if _, err := cookies.SaveCredentialFile(path, cookies.CredentialFile{
			Kind:    "gemini-web",
			Name:    "acct_cookie_only",
			Cookies: cookie,
			AT:      "",
			BL:      "boq_disk",
			FSid:    "333",
		}); err != nil {
			t.Fatalf("SaveCredentialFile: %v", err)
		}
		if bump > 0 {
			future := time.Now().Add(bump)
			if err := os.Chtimes(path, future, future); err != nil {
				t.Fatalf("Chtimes: %v", err)
			}
		}
	}

	t.Run("empty_AT_double_race_returns_503_upstream_transient", func(t *testing.T) {
		credPath := filepath.Join(t.TempDir(), "acct_empty_at.json")
		writeCookieOnlyCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts_old", 0)

		firstStarted := make(chan struct{})
		releaseFirst := make(chan struct{})
		secondStarted := make(chan struct{})
		releaseSecond := make(chan struct{})

		doer := &stubGeminiDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				cookie := fhttpHeader(req, "cookie")
				switch {
				case strings.Contains(cookie, "ts_old"):
					close(firstStarted)
					<-releaseFirst
					return &fhttp.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_from_old","cfb2h":"boq_old","FdrFJe":"111"}`)),
					}, nil
				case strings.Contains(cookie, "ts_disk_1"):
					close(secondStarted)
					<-releaseSecond
					return &fhttp.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_from_disk1","cfb2h":"boq_1","FdrFJe":"111"}`)),
					}, nil
				default:
					return &fhttp.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("unexpected"))}, nil
				}
			},
		}

		pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
		defer pool.Reset()
		ac, err := LoadAccountFromFile(credPath, pool, cookies.NewRotator())
		if err != nil {
			t.Fatalf("LoadAccountFromFile: %v", err)
		}

		bootCh := make(chan error, 1)
		go func() {
			bootCh <- ac.Bootstrap(context.Background())
		}()

		<-firstStarted
		writeCookieOnlyCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts_disk_1", 2*time.Second)
		if !ac.ReloadFromDiskIfNewer() {
			t.Fatal("expected first ReloadFromDiskIfNewer to succeed")
		}
		close(releaseFirst)

		<-secondStarted
		writeCookieOnlyCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts_disk_2", 4*time.Second)
		if !ac.ReloadFromDiskIfNewer() {
			t.Fatal("expected second ReloadFromDiskIfNewer to succeed")
		}
		close(releaseSecond)

		err = <-bootCh
		var ue *transport.UpstreamError
		if !errors.As(err, &ue) || ue.StatusCode != 503 || ue.Code != "upstream_transient" {
			t.Fatalf("expected 503 upstream_transient (never nil with empty AT), got %v", err)
		}

		ac.mu.RLock()
		gotAT := ac.at
		gotBootAt := ac.bootstrappedAt
		gotCookie := ac.cookieHeader
		ac.mu.RUnlock()
		if gotAT != "" || !gotBootAt.IsZero() || !strings.Contains(gotCookie, "ts_disk_2") {
			t.Fatalf("unexpected state after double race: at=%q bootAt=%v cookie=%q", gotAT, gotBootAt, gotCookie)
		}
	})

	t.Run("stale_prior_AT_cleared_and_StreamGenerateLive_aborts_before_dispatch", func(t *testing.T) {
		credPath := filepath.Join(t.TempDir(), "acct_stale_at.json")
		writeCookieOnlyCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts_old", 0)

		firstStarted := make(chan struct{})
		releaseFirst := make(chan struct{})
		secondStarted := make(chan struct{})
		releaseSecond := make(chan struct{})
		var mu sync.Mutex
		var streamDispatched bool

		doer := &stubGeminiDoer{
			fn: func(req *fhttp.Request) (*fhttp.Response, error) {
				u := req.URL.String()
				if strings.Contains(u, "StreamGenerate") {
					mu.Lock()
					streamDispatched = true
					mu.Unlock()
					return &fhttp.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n"))}, nil
				}
				cookie := fhttpHeader(req, "cookie")
				switch {
				case strings.Contains(cookie, "ts_old"):
					close(firstStarted)
					<-releaseFirst
					return &fhttp.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_from_old","cfb2h":"boq_old","FdrFJe":"111"}`)),
					}, nil
				case strings.Contains(cookie, "ts_disk_1"):
					close(secondStarted)
					<-releaseSecond
					return &fhttp.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_from_disk1","cfb2h":"boq_1","FdrFJe":"111"}`)),
					}, nil
				default:
					return &fhttp.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("unexpected"))}, nil
				}
			},
		}

		pool := transport.NewPool(transport.PoolOptions{ClientConfig: transport.ClientConfig{CustomDoer: doer}, MaxSessions: 2})
		defer pool.Reset()
		ac, err := LoadAccountFromFile(credPath, pool, cookies.NewRotator())
		if err != nil {
			t.Fatalf("LoadAccountFromFile: %v", err)
		}
		// Simulate an earlier in-memory stale AT (even with fromDiskAT=true and empty BL so ensureBootstrapped calls bootstrapOnClient).
		ac.mu.Lock()
		ac.at = "at_very_old"
		ac.fromDiskAT = true
		ac.bl = ""
		ac.fSid = ""
		ac.mu.Unlock()

		streamCh := make(chan error, 1)
		go func() {
			_, sErr := ac.StreamGenerateLive(context.Background(), "hello", StreamOptions{}, nil)
			streamCh <- sErr
		}()

		<-firstStarted
		writeCookieOnlyCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts_disk_1", 2*time.Second)
		if !ac.ReloadFromDiskIfNewer() {
			t.Fatal("expected first ReloadFromDiskIfNewer to succeed")
		}
		ac.mu.RLock()
		midAT := ac.at
		midFromDisk := ac.fromDiskAT
		ac.mu.RUnlock()
		if midAT != "" || midFromDisk {
			t.Fatalf("expected ReloadFromDiskIfNewer with cookiesChanged && AT==\"\" to clear stale AT, got at=%q fromDiskAT=%v", midAT, midFromDisk)
		}
		close(releaseFirst)

		<-secondStarted
		writeCookieOnlyCred(t, credPath, "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts_disk_2", 4*time.Second)
		if !ac.ReloadFromDiskIfNewer() {
			t.Fatal("expected second ReloadFromDiskIfNewer to succeed")
		}
		close(releaseSecond)

		sErr := <-streamCh
		var ue *transport.UpstreamError
		if !errors.As(sErr, &ue) || ue.StatusCode != 503 || ue.Code != "upstream_transient" {
			t.Fatalf("expected StreamGenerateLive to return 503 upstream_transient, got %v", sErr)
		}
		mu.Lock()
		dispatched := streamDispatched
		mu.Unlock()
		if dispatched {
			t.Fatal("StreamGenerate must not be dispatched with empty/stale AT after double-race")
		}
	})
}
