package gemini

import (
	"context"
	"errors"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
	fhttp "github.com/bogdanfinn/fhttp"
	"io"
	"strings"
	"testing"
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
	doer := &stubGeminiDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.HasSuffix(u, "/app"):
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader(`<html>no token</html>`)),
				}, nil
			case strings.Contains(u, "RotateCookies"):
				return &fhttp.Response{
					StatusCode: 429,
					Header:     fhttp.Header{"Retry-After": []string{"60"}},
					Body:       io.NopCloser(strings.NewReader(`rate limited`)),
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
	ac := NewAccountClient("acct_429", "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=ts1", "", pool, cookies.NewRotator())
	err := ac.Bootstrap(context.Background())
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 429 || ue.Code != "rate_limit_error" {
		t.Fatalf("expected 429 rate_limit_error on rotation 429 during bootstrap, got %+v (%v)", ue, err)
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
