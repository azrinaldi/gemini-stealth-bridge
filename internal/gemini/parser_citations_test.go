package gemini

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeCandidateCitationArtifacts(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"single", "The answer is 42 [cite: 1].", "The answer is 42."},
		{"multi_ids", "One [cite:1, 2,\t3 ] two.", "One two."},
		{"boundaries", "[cite_start]One[cite_end] two.", "One two."},
		{"adjacent", "One [cite:1][cite_start][cite:2][cite_end].", "One."},
		{"adjacent_markdown", "**One [cite:1]**[cite:2].", "**One**."},
		{"spaced_markers", "One  [cite:1] \t[cite_end] [cite:2]  , two.", "One, two."},
		{"between_words", "One [cite:1]two[cite_end] three.", "One two three."},
		{"no_inserted_space", "one[cite:1]two", "onetwo"},
		{"bold_close", "**One [cite:1]**.", "**One**."},
		{"bold_open", "One [cite:1]**important** point.", "One **important** point."},
		{"markdown_closers", "_One [cite:1]_ and ~~two [cite:2]~~.", "_One_ and ~~two~~."},
		{"quote_close", "\"One [cite:1]\".", "\"One\"."},
		{"unicode_closers", "\u7b54\u6848 [cite:1]\u3002\u300d\uff01", "\u7b54\u6848\u3002\u300d\uff01"},
		{"line_break", "One [cite:1] \n  Two.", "One\n  Two."},
		{"ordinary_spaces", "One  two.\n\n  three.  ", "One  two.\n\n  three.  "},
		{"inline_literal", "The literal `[cite: 1]` is not a citation.", "The literal `[cite: 1]` is not a citation."},
		{"fenced_code", "```go\ns := \"[cite: 1]\"\na := []int{1, 2}\n```\nOne [cite:2].", "```go\ns := \"[cite: 1]\"\na := []int{1, 2}\n```\nOne."},
		{"fence_literal_backticks", "```go\ns := \"``` [cite:1]\"\n```\nOne [cite:2].", "```go\ns := \"``` [cite:1]\"\n```\nOne."},
		{"tilde_fence", "~~~text\n[cite_start]literal[cite_end]\n~~~~\nOne [cite:1].", "~~~text\n[cite_start]literal[cite_end]\n~~~~\nOne."},
		{"escaped_literal", `The literal \[cite: 1] stays.`, `The literal \[cite: 1] stays.`},
		{"code_followed_by_marker", "`value` [cite:1].", "`value`."},
		{"bare_internal", "One https://googleusercontent.com/lmdx_content/123.", "One."},
		{"bare_internal_unicode", "\u7b54\u6848 https://googleusercontent.com/lmdx_content/123\u2026\u300d", "\u7b54\u6848\u2026\u300d"},
		{"http_internal", "One http://googleusercontent.com/lmdx_content/a/b?x=1#part, two.", "One, two."},
		{"case_insensitive_host", "One HTTPS://GOOGLEUSERCONTENT.COM/lmdx_content/id.", "One."},
		{"internal_autolink", "One <https://googleusercontent.com/lmdx_content/id>.", "One."},
		{"internal_parentheses", "One (https://googleusercontent.com/lmdx_content/id).", "One."},
		{"numeric_link", "One [1](https://googleusercontent.com/lmdx_content/id).", "One."},
		{"marker_link", "One [cite:1](https://googleusercontent.com/lmdx_content/id).", "One."},
		{"meaningful_link", "Read [the report](https://googleusercontent.com/lmdx_content/id).", "Read the report."},
		{"link_title", "Read [the report](<https://googleusercontent.com/lmdx_content/id> \"title\").", "Read the report."},
		{"link_label_markers", "Read [[cite_start]the report[cite_end]](https://googleusercontent.com/lmdx_content/id).", "Read the report."},
		{"url_before_marker", "One https://googleusercontent.com/lmdx_content/id[cite:1].", "One."},
		{"marker_before_url", "One[cite:1]https://googleusercontent.com/lmdx_content/id.", "One."},
		{"url_markdown_close", "**One https://googleusercontent.com/lmdx_content/id**.", "**One**."},
		{"literal_url_code", "`https://googleusercontent.com/lmdx_content/id`", "`https://googleusercontent.com/lmdx_content/id`"},
		{"external_urls", "[link](https://example.com/a_(b)?x=1) <https://lh3.googleusercontent.com/lmdx_content/id>", "[link](https://example.com/a_(b)?x=1) <https://lh3.googleusercontent.com/lmdx_content/id>"},
		{"embedded_internal_url", "https://example.com/?url=https://googleusercontent.com/lmdx_content/id", "https://example.com/?url=https://googleusercontent.com/lmdx_content/id"},
		{"embedded_parenthesized_url", "https://example.com/a_(https://googleusercontent.com/lmdx_content/id)", "https://example.com/a_(https://googleusercontent.com/lmdx_content/id)"},
		{"other_external_scheme", "ftp://example.com/(https://googleusercontent.com/lmdx_content/id)", "ftp://example.com/(https://googleusercontent.com/lmdx_content/id)"},
		{"external_url_then_marker", "https://example.com/a[cite:1].", "https://example.com/a."},
		{"lookalike_host", "https://googleusercontent.com.example.com/lmdx_content/id", "https://googleusercontent.com.example.com/lmdx_content/id"},
		{"lookalike_path", "https://googleusercontent.com/lmdx_contentious/id", "https://googleusercontent.com/lmdx_contentious/id"},
		{"ordinary_brackets", "Use [citation needed], [cite], [cite: x], and [cite_startled].", "Use [citation needed], [cite], [cite: x], and [cite_startled]."},
		{"unfinished_marker", "One [cite: 1, 2", "One"},
		{"unfinished_boundary", "One [cite_en", "One"},
		{"unfinished_distinctive_prefix", "One [cite", "One"},
		{"unfinished_link", "Read [the report](https://googleusercontent.com/lmdx_content/id", "Read the report"},
		{"ambiguous_final_literals", "Use [ci and h", "Use [ci and h"},
		{"ambiguous_final_url", "Use https://googleusercontent.com/lmdx", "Use https://googleusercontent.com/lmdx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, thoughts := SanitizeCandidateTextAndThoughts(tc.raw, "native [cite:9]", true)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if thoughts != "native [cite:9]" {
				t.Fatalf("native reasoning changed: %q", thoughts)
			}
			sent := ""
			for i := 1; i <= len(tc.raw); i++ {
				if !utf8.ValidString(tc.raw[:i]) {
					continue
				}
				text, _ := SanitizeCandidateTextAndThoughts(tc.raw[:i], "native [cite:9]", false)
				if !strings.HasPrefix(text, sent) || !strings.HasPrefix(tc.want, text) {
					t.Fatalf("prefix %d (%q): snapshot %q does not extend %q toward %q", i, tc.raw[:i], text, sent, tc.want)
				}
				sent = text
			}
		})
	}
	res, err := ParseFrameLine(groundingFrameLine(t, groundingCandidate("rc_source", "One [cite:1].", [][]any{
		{"https://example.com/report", "The report", "favicon", "snippet"},
	})))
	if err != nil || res == nil || len(res.Frames) != 1 || res.Frames[0].Text != "One." || len(res.GroundingSources) != 1 || len(res.Frames[0].GroundingSources) != 1 {
		t.Fatalf("citation cleanup affected grounding: res=%+v err=%v", res, err)
	}
	if source := res.GroundingSources[0]; source.URL != "https://example.com/report" || source.Title != "The report" || source.Snippet != "snippet" {
		t.Fatalf("grounding source changed: %+v", source)
	}
}

func TestSanitizeCandidateCitationReasoningInteraction(t *testing.T) {
	for _, tc := range []struct {
		name, raw, wantText, wantThoughts string
	}{
		{"closed_think", "<think>reason [cite:9]</think>Assistant: One [cite:1].", "One.", "native\n\nreason [cite:9]"},
		{"open_think", "One [cite:1].<think>reason [cite:9]", "One.", "native\n\nreason [cite:9]"},
		{"literal_think_early_return", "<think>literal\nReal answer [cite:1].", "<think>literal\nReal answer.", "native"},
		{"control_token_policy_unchanged", "<ctrl94>thought [cite:9]<ctrl95>One [cite:1].", "<ctrl94>thought <ctrl95>One.", "native"},
		{"sandbox_trace", "```python?code_reference&code_event_index=1\nprint('[cite:9]')\n```\nOne [cite:1].", "One.", "native\n\nprint('[cite:9]')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, thoughts := SanitizeCandidateTextAndThoughts(tc.raw, "native", true)
			if text != tc.wantText || thoughts != tc.wantThoughts {
				t.Fatalf("got text=%q thoughts=%q; want text=%q thoughts=%q", text, thoughts, tc.wantText, tc.wantThoughts)
			}
		})
	}
}

func TestParseFrameLineCitationFixtures(t *testing.T) {
	for _, tc := range []struct {
		name, line, want string
	}{
		{
			"dense",
			`[["wrb.fr",null,"[null,[\"c_123\",\"r_456\"],null,null,[[\"rc_789\",[\"The answer is 42 [cite: 1, 2].\"],null,null,null,null,null,null,[2],{\"38\":[[\"native [cite:9]\"]]}]],{\"26\":\"ctx_tok_1\",\"40\":\"954c0abcba24364\",\"43\":\"4 Argon\"}]"]]`,
			"The answer is 42.",
		},
		{
			"sparse_candidate",
			`[["wrb.fr",null,"[null,[\"c_123\",\"r_456\"],null,null,[[\"rc_789\",[\"[cite_start]Read [the report](https://googleusercontent.com/lmdx_content/42).[cite_end]\"],null,null,null,{\"9\":[2],\"38\":[[\"native [cite:9]\"]]}]],{\"26\":\"ctx_tok_1\",\"40\":\"954c0abcba24364\",\"43\":\"4 Argon\"}]"]]`,
			"Read the report.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ParseFrameLine(tc.line)
			if err != nil || res == nil || len(res.Frames) != 1 {
				t.Fatalf("ParseFrameLine: res=%+v err=%v", res, err)
			}
			f := res.Frames[0]
			if f.Text != tc.want || f.Thoughts != "native [cite:9]" || !f.Done || f.RCID != "rc_789" {
				t.Fatalf("unexpected sanitized frame: %+v", f)
			}
			if res.CID != "c_123" || res.RID != "r_456" || res.ContextToken != "ctx_tok_1" || res.AnsweredModelID != "954c0abcba24364" || res.AnsweredModelLabel != "4 Argon" {
				t.Fatalf("parser metadata changed: %+v", res)
			}
		})
	}
}

func TestCitationSanitizerStreamingSnapshots(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
	}{
		{"One ", "One"},
		{"One [", "One"},
		{"One [ci", "One"},
		{"One [cite: 1, ", "One"},
		{"One [cite_sta", "One"},
		{"One [cite:1] ", "One"},
		{"One [cite:1]**", "One"},
		{"One [cite:1]**bold", "One **bold"},
		{"One h", "One"},
		{"One https://googleusercontent.com/lmdx_cont", "One"},
		{"One https://googleusercontent.com/lmdx_content/42.", "One"},
		{"One [label", "One"},
		{"One [label](htt", "One"},
		{"One [label](https://googleusercontent.com/lmdx_content/42", "One label"},
	} {
		got, _ := SanitizeCandidateTextAndThoughts(tc.raw, "", false)
		if got != tc.want {
			t.Errorf("snapshot %q: got %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestParseFrameLineCitationCumulativePrefixes(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"punctuation", "One [cite:1]. Two [cite:2,3] , three.", "One. Two, three."},
		{"mixed_markers", "[cite_start]One [cite:1][cite_end] [cite:2].", "One."},
		{"markdown_close", "**One [cite:1]**. _Two [cite:2]_.", "**One**. _Two_."},
		{"markdown_adjacent", "**One [cite:1]**[cite:2].", "**One**."},
		{"markdown_open", "One [cite:1]**bold** and [cite_start]two[cite_end].", "One **bold** and two."},
		{"unicode", "\u7b54\u6848 [cite:1]\u3002\u300d Next.", "\u7b54\u6848\u3002\u300d Next."},
		{"internal_url", "One https://googleusercontent.com/lmdx_content/42. Next.", "One. Next."},
		{"autolink", "One <http://googleusercontent.com/lmdx_content/42>. Next.", "One. Next."},
		{"numeric_link", "One [1](https://googleusercontent.com/lmdx_content/42). Next.", "One. Next."},
		{"label_link", "Read [the report](https://googleusercontent.com/lmdx_content/42). Next.", "Read the report. Next."},
		{"title_link", "Read [the report](<https://googleusercontent.com/lmdx_content/42> \"title\"). Next.", "Read the report. Next."},
		{"external_link", "Read [the report](https://example.com/a_(b)?x=1).", "Read [the report](https://example.com/a_(b)?x=1)."},
		{"external_query", "https://example.com/?url=https://googleusercontent.com/lmdx_content/id", "https://example.com/?url=https://googleusercontent.com/lmdx_content/id"},
		{"external_parentheses", "https://example.com/a_(https://googleusercontent.com/lmdx_content/id)", "https://example.com/a_(https://googleusercontent.com/lmdx_content/id)"},
		{"ordinary_brackets", "Use [citation needed], [cite], [cite: x], and [cite_startled].", "Use [citation needed], [cite], [cite: x], and [cite_startled]."},
		{"code", "```go\na := []string{\"[cite:1]\"}\n```\nOne [cite:2].", "```go\na := []string{\"[cite:1]\"}\n```\nOne."},
		{"final_partial", "One [cite_end", "One"},
		{"final_ambiguous", "Use [ci", "Use [ci"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent := ""
			for i := 1; i <= len(tc.raw); i++ {
				if !utf8.ValidString(tc.raw[:i]) {
					continue
				}
				res, err := ParseFrameLine(citationFrameLine(t, tc.raw[:i], false))
				if err != nil || res == nil || len(res.Frames) != 1 {
					t.Fatalf("prefix %d: ParseFrameLine res=%+v err=%v", i, res, err)
				}
				text := res.Frames[0].Text
				if !strings.HasPrefix(text, sent) || !strings.HasPrefix(tc.want, text) {
					t.Fatalf("prefix %d (%q): snapshot %q does not extend %q toward %q", i, tc.raw[:i], text, sent, tc.want)
				}
				// This is the delta slicing used by streaming consumers of Frame.Text.
				sent += text[len(sent):]
			}
			res, err := ParseFrameLine(citationFrameLine(t, tc.raw, true))
			if err != nil || res == nil || len(res.Frames) != 1 {
				t.Fatalf("final ParseFrameLine: res=%+v err=%v", res, err)
			}
			final := res.Frames[0].Text
			if !strings.HasPrefix(final, sent) {
				t.Fatalf("final %q retracts streamed %q", final, sent)
			}
			sent += final[len(sent):]
			if sent != tc.want || final != tc.want {
				t.Fatalf("streamed %q final %q, want %q", sent, final, tc.want)
			}
		})
	}
}

func TestParseFrameLineCitationEOFFinalization(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
	}{
		{"One [cite:1]. Finish", "One. Finish"},
		{"Literal [ci", "Literal [ci"},
		{"Literal https://googleusercontent.com/lmdx", "Literal https://googleusercontent.com/lmdx"},
		{"Literal [label]", "Literal [label]"},
		{"One [cite:1].  ", "One.  "},
		{"One [cite:1, 2", "One"},
		{"One [cite_end", "One"},
	} {
		res, err := ParseFrameLine(citationFrameLine(t, tc.raw, false))
		if err != nil || res == nil || len(res.Frames) != 1 {
			t.Fatalf("ParseFrameLine: res=%+v err=%v", res, err)
		}
		f := res.Frames[0]
		final, thoughts := SanitizeCandidateTextAndThoughts(f.rawCandidateText, f.rawCandidateThoughts, true)
		if final != tc.want || !strings.HasPrefix(final, f.Text) || thoughts != "" {
			t.Fatalf("raw %q: streamed %q final %q thoughts %q, want %q", tc.raw, f.Text, final, thoughts, tc.want)
		}
		encoded, err := json.Marshal(f)
		if err != nil || strings.Contains(string(encoded), "rawCandidate") || strings.Contains(string(encoded), "[cite:") {
			t.Fatalf("raw candidate leaked into frame JSON: %s err=%v", encoded, err)
		}
	}
}

func citationFrameLine(t *testing.T, text string, done bool) string {
	t.Helper()
	status := 1
	if done {
		status = 2
	}
	inner := []any{nil, []any{"c_cite", "r_cite"}, nil, nil, []any{
		[]any{"rc_cite", []any{text}, nil, nil, nil, nil, nil, nil, []any{status}},
	}}
	payload, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal([]any{[]any{"wrb.fr", nil, string(payload)}})
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}
