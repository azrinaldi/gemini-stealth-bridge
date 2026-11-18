package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/prompt"
)

func TestParseAnswerLimits(t *testing.T) {
	lim := parseAnswerLimits(map[string]any{
		"max_tokens": float64(12),
		"stop":       []any{"END", ""},
		"response_format": map[string]any{
			"type": "json_object",
		},
	})
	if lim.maxTokens != 12 {
		t.Errorf("maxTokens = %d, want 12", lim.maxTokens)
	}
	if len(lim.stopSeqs) != 1 || lim.stopSeqs[0] != "END" {
		t.Errorf("stopSeqs = %v, want [END]", lim.stopSeqs)
	}
	if !lim.wantJSON {
		t.Error("wantJSON = false, want true")
	}

	// max_completion_tokens is the newer spelling and must be accepted.
	alt := parseAnswerLimits(map[string]any{"max_completion_tokens": float64(7)})
	if alt.maxTokens != 7 {
		t.Errorf("max_completion_tokens: maxTokens = %d, want 7", alt.maxTokens)
	}
	// A single string stop is valid OpenAI input.
	single := parseAnswerLimits(map[string]any{"stop": "HALT"})
	if len(single.stopSeqs) != 1 || single.stopSeqs[0] != "HALT" {
		t.Errorf("string stop = %v, want [HALT]", single.stopSeqs)
	}
	// No controls.
	none := parseAnswerLimits(map[string]any{})
	if none.enabled() {
		t.Error("empty request should not enable limits")
	}
}

// A stop sequence must cut the answer and keep finish_reason "stop", which is
// what a native API returns. Silently returning the full text is the bug this
// pins.
func TestAnswerLimitsStopSequence(t *testing.T) {
	lim := parseAnswerLimits(map[string]any{"stop": []any{"STOPHERE"}})
	got, finish := lim.apply("alpha STOPHERE beta gamma")
	if strings.Contains(got, "STOPHERE") {
		t.Errorf("stop sequence leaked into answer: %q", got)
	}
	if got != "alpha " {
		t.Errorf("answer = %q, want %q", got, "alpha ")
	}
	if finish != "stop" {
		t.Errorf("finish = %q, want stop", finish)
	}

	// The earliest of several sequences wins regardless of request order.
	multi := parseAnswerLimits(map[string]any{"stop": []any{"BB", "AA"}})
	if got, _ := multi.apply("xx AA yy BB"); got != "xx " {
		t.Errorf("earliest stop not applied: %q", got)
	}
	// An absent sequence leaves the answer alone.
	if got, finish := lim.apply("nothing to cut"); got != "nothing to cut" || finish != "" {
		t.Errorf("absent stop changed answer: %q finish=%q", got, finish)
	}
}

// max_tokens must cap the answer and report finish_reason "length". The usage
// report and the cut use the same estimator, so the returned length must never
// exceed the budget.
func TestAnswerLimitsMaxTokens(t *testing.T) {
	long := strings.Repeat("alpha beta gamma delta ", 40)

	lim := parseAnswerLimits(map[string]any{"max_tokens": float64(5)})
	got, finish := lim.apply(long)
	if finish != "length" {
		t.Errorf("finish = %q, want length", finish)
	}
	if n := prompt.EstimateTokens(got); n > 5 {
		t.Errorf("returned %d tokens, budget was 5 (text=%q)", n, got)
	}
	if len(got) >= len(long) {
		t.Error("long answer was not truncated")
	}

	// A short answer inside the budget must pass through untouched.
	short := "one two"
	if got, finish := lim.apply(short); got != short || finish != "" {
		t.Errorf("answer inside budget changed: %q finish=%q", got, finish)
	}

	// Truncation must not end mid-word when a boundary exists.
	if strings.HasSuffix(strings.TrimRight(got, " "), "gam") {
		t.Errorf("truncation split a word: %q", got)
	}
}

func TestTruncateToTokensBoundaries(t *testing.T) {
	if got, cut := truncateToTokens("", 10); got != "" || cut {
		t.Errorf("empty text: got %q cut=%v", got, cut)
	}
	if got, cut := truncateToTokens("hello", 0); got != "hello" || cut {
		t.Errorf("zero budget must not cut: got %q cut=%v", got, cut)
	}
	// A budget larger than the text is a no-op.
	if got, cut := truncateToTokens("hello world", 9999); got != "hello world" || cut {
		t.Errorf("generous budget cut text: got %q cut=%v", got, cut)
	}
}

// Gemini wraps JSON in a fence. A client that asked for json_object cannot
// parse that, so the object must be returned bare.
func TestJSONObjectText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"fenced", "```json\n{\"a\": 1, \"b\": 2}\n```", `{"a": 1, "b": 2}`},
		{"prose_around", "Here is the result:\n{\"a\": 1}\nHope it helps.", `{"a": 1}`},
		{"bare", `{"a": 1}`, `{"a": 1}`},
		{"nested", "```json\n{\"a\": {\"b\": [1,2]}}\n```", `{"a": {"b": [1,2]}}`},
		// Braces inside string literals must not close the object early.
		{"brace_in_string", `{"a": "value } with brace"}`, `{"a": "value } with brace"}`},
		{"escaped_quote", `{"a": "say \"hi\" }"}`, `{"a": "say \"hi\" }"}`},
	}
	for _, tc := range cases {
		if got := jsonObjectText(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}

	// No object at all: return the input unchanged rather than inventing "{}".
	for _, in := range []string{"", "no json here", "{not valid json"} {
		if got := jsonObjectText(in); got != in {
			t.Errorf("non-JSON input %q changed to %q", in, got)
		}
	}
}

// End-to-end through the real handler: a client that budgets max_tokens must
// receive at most that many tokens and must be told why the answer stopped.
// Without the enforcement the handler returns the whole answer and reports
// "stop", which is what a native API never does.
func TestChatHandlerEnforcesMaxTokens(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	long := strings.Repeat("alpha beta gamma delta ", 40)
	client := clientFromFrames([]gemini.Frame{
		makeFrame(long, "", false, testModel.ModelID),
		makeFrame(long, "", true, testModel.ModelID),
	})
	rec := runChat(t, srv, acct, client, map[string]any{"max_tokens": float64(5)})
	body := rec.Body.String()

	// Without stream=true the handler answers with one JSON object; with it the
	// answer arrives as SSE chunks. Both must carry the same verdict.
	var finish any
	if strings.Contains(body, `"object":"chat.completion.chunk"`) {
		for _, ev := range parseSSEEvents(body) {
			choices, _ := ev["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			if ch, _ := choices[0].(map[string]any); ch != nil && ch["finish_reason"] != nil {
				finish = ch["finish_reason"]
			}
		}
	} else {
		var payload map[string]any
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("response is not JSON: %v\n%s", err, body)
		}
		choices, _ := payload["choices"].([]any)
		if len(choices) == 0 {
			t.Fatalf("no choices in response: %s", body)
		}
		ch, _ := choices[0].(map[string]any)
		finish = ch["finish_reason"]
		if content, _ := ch["message"].(map[string]any)["content"].(string); prompt.EstimateTokens(content) > 5 {
			t.Errorf("content is %d tokens, budget was 5", prompt.EstimateTokens(content))
		}
	}
	if finish != "length" {
		t.Errorf("finish_reason = %v, want length", finish)
	}
}

// A stop sequence must end the streamed answer at the marker.
func TestChatHandlerEnforcesStop(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	client := clientFromFrames([]gemini.Frame{
		makeFrame("alpha STOPHERE beta gamma", "", true, testModel.ModelID),
	})
	rec := runChat(t, srv, acct, client, map[string]any{"stop": []any{"STOPHERE"}})
	if body := rec.Body.String(); strings.Contains(body, "beta gamma") {
		t.Errorf("text after stop sequence was streamed:\n%s", body)
	}
}

// response_format=json_object must strip Gemini's code fence so the client can
// parse the payload directly.
func TestChatHandlerStripsJSONFence(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	client := clientFromFrames([]gemini.Frame{
		makeFrame("```json\n{\"a\": 1}\n```", "", true, testModel.ModelID),
	})
	rec := runChat(t, srv, acct, client, map[string]any{"response_format": map[string]any{"type": "json_object"}})
	body := rec.Body.String()
	if strings.Contains(body, "```") {
		t.Errorf("code fence survived json_object request:\n%s", body)
	}
	if !strings.Contains(body, `{\"a\": 1}`) && !strings.Contains(body, `{"a": 1}`) {
		t.Errorf("JSON object missing from response:\n%s", body)
	}
}

// A streaming request with max_tokens must still stream, but must never put
// more than the budget on the wire and must end with finish_reason "length".
// Buffering the whole answer would be a regression: clients set max_tokens on
// almost every request, so it must not cost incremental delivery.
func TestChatHandlerStreamsWithinMaxTokens(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	long := strings.Repeat("alpha beta gamma delta ", 40)
	client := clientFromFrames([]gemini.Frame{
		makeFrame(long[:200], "", false, testModel.ModelID),
		makeFrame(long, "", false, testModel.ModelID),
		makeFrame(long, "", true, testModel.ModelID),
	})
	rec := runChat(t, srv, acct, client, map[string]any{
		"stream":     true,
		"max_tokens": float64(6),
	})
	body := rec.Body.String()

	var streamed strings.Builder
	var finish any
	for _, ev := range parseSSEEvents(body) {
		choices, _ := ev["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		ch, _ := choices[0].(map[string]any)
		if ch == nil {
			continue
		}
		if d, ok := ch["delta"].(map[string]any); ok {
			if c, ok := d["content"].(string); ok {
				streamed.WriteString(c)
			}
		}
		if ch["finish_reason"] != nil {
			finish = ch["finish_reason"]
		}
	}
	if finish != "length" {
		t.Errorf("finish_reason = %v, want length (body=%s)", finish, body)
	}
	if n := prompt.EstimateTokens(streamed.String()); n > 6 {
		t.Errorf("streamed %d tokens, budget was 6: %q", n, streamed.String())
	}
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Error("max_tokens must not force a non-streaming response")
	}
}

// Once the budget fires, later upstream frames must not be compared against
// what was streamed, and must not surface a revision error. Gemini keeps
// growing the answer after the client's budget is spent, which is what produced
// "upstream revised an already streamed answer" in a live run.
func TestChatHandlerIgnoresFramesAfterBudget(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	head := strings.Repeat("alpha beta gamma delta ", 40)
	// A shorter first frame, then a longer one that does not extend what the
	// client already received, plus a final revision.
	client := clientFromFrames([]gemini.Frame{
		makeFrame(head[:120], "", false, testModel.ModelID),
		makeFrame(head[:400], "", false, testModel.ModelID),
		makeFrame("completely different text", "", true, testModel.ModelID),
	})
	rec := runChat(t, srv, acct, client, map[string]any{
		"stream":     true,
		"max_tokens": float64(6),
	})
	body := rec.Body.String()
	if strings.Contains(body, "upstream_revision") {
		t.Errorf("budget exhaustion surfaced a revision error:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"length"`) {
		t.Errorf("missing length verdict:\n%s", body)
	}
	// The answer must be the text frozen when the budget fired, never the later
	// upstream revision the client was never shown.
	if strings.Contains(body, "completely different") {
		t.Errorf("answer was replaced by a post-budget upstream revision:\n%s", body)
	}
}

// RejectLabelMismatch must end the stream on the first frame that reveals the
// relabel, not at the end of generation. Waiting costs the whole upstream
// latency and, on the eager path, the wrong answer is already on the wire.
func TestShouldAbortForModelRejection(t *testing.T) {
	cfg := config.Load()
	cfg.RejectDowngrade = false
	cfg.RejectLabelMismatch = true

	// id matches the requested slot, label names another tier.
	relabel := testModel
	relabel.ModelName = "gemini-4-carbon"
	relabel.ModelID = "95a4c0abcba24364"

	if !shouldAbortForModelRejection(cfg, false, relabel, "95a4c0abcba24364", "4 Argon") {
		t.Error("label mismatch must abort the stream")
	}
	// A matching label must not abort.
	if shouldAbortForModelRejection(cfg, false, relabel, "95a4c0abcba24364", "4 Carbon") {
		t.Error("matching label must not abort")
	}
	// No identity yet: nothing to judge.
	if shouldAbortForModelRejection(cfg, false, relabel, "", "") {
		t.Error("empty identity must not abort")
	}
	// Media tool replies are authored by the bridge, not the tier.
	if shouldAbortForModelRejection(cfg, true, relabel, "95a4c0abcba24364", "4 Argon") {
		t.Error("media tool answer must not abort")
	}
	// Opt-in flag off: report the relabel, do not fail the request.
	cfg.RejectLabelMismatch = false
	if shouldAbortForModelRejection(cfg, false, relabel, "95a4c0abcba24364", "4 Argon") {
		t.Error("must not abort when RejectLabelMismatch is off")
	}
	// A different backend id is a downgrade, still caught by RejectDowngrade.
	cfg.RejectDowngrade = true
	if !shouldAbortForModelRejection(cfg, false, relabel, "someOtherId", "") {
		t.Error("downgrade must still abort")
	}
}

// A pool request that hits a label mismatch must fail over to another account
// instead of returning 502 to the caller. The relabel is detected on the first
// frame, so the retry happens before any answer is sent.
func TestChatHandlerFailsOverOnLabelMismatch(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.AccountTokens = nil
	cfg.MaxConcurrentPerAccount = 0
	cfg.MaxRPMPerAccount = 0
	cfg.MaxAttempts = 3
	cfg.MaxUpstreamAttempts = 3
	cfg.RejectDowngrade = false
	cfg.RejectLabelMismatch = true
	srv := New(cfg)

	m := testModel
	m.ModelName = "gemini-4-carbon"
	m.ModelID = "95a4c0abcba24364"

	relabelled := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			if err := onFrame(&gemini.LineParseResult{
				CID: "c1", RID: "r1",
				AnsweredModelID:    "95a4c0abcba24364",
				AnsweredModelLabel: "4 Argon",
				Frames:             []gemini.Frame{makeFrame("wrong tier", "", true, "95a4c0abcba24364")},
			}); err != nil {
				return nil, err
			}
			return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1"}, nil
		},
	}
	correct := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(*gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			if err := onFrame(&gemini.LineParseResult{
				CID: "c2", RID: "r2",
				AnsweredModelID:    "95a4c0abcba24364",
				AnsweredModelLabel: "4 Carbon",
				Frames:             []gemini.Frame{makeFrame("CORRECT", "", true, "95a4c0abcba24364")},
			}); err != nil {
				return nil, err
			}
			return &gemini.StreamResponse{Status: 200, CID: "c2", RID: "r2"}, nil
		},
	}

	a1 := &AccountState{ID: "acct1", Email: "a1@example.com", Models: []gemini.ModelInfo{m}, Capacity: 2, ModelsAt: time.Now(), Client: relabelled}
	a2 := &AccountState{ID: "acct2", Email: "a2@example.com", Models: []gemini.ModelInfo{m}, Capacity: 2, ModelsAt: time.Now(), Client: correct}
	srv.SetAccounts([]*AccountState{a1, a2})

	body := map[string]any{
		"model":    m.ModelName,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, body, AuthScope{OK: true})

	if rec.Code != 200 {
		t.Fatalf("label mismatch did not fail over, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "CORRECT") {
		t.Errorf("expected the correct-tier answer, got %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "wrong tier") {
		t.Errorf("relabelled answer reached the client: %s", rec.Body.String())
	}
}
