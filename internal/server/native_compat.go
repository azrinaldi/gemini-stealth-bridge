package server

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/prompt"
)

// OpenAI-compatible request controls that Gemini Web has no upstream equivalent
// for. They are enforced here, on the assembled answer, because a client that
// sends max_tokens or stop trusts the bridge to honour it: ignoring them
// silently returns a longer answer than the caller budgeted for.
//
// Token and stop limits are enforced incrementally during streaming via
// visible()/visibleAfter() and on the finished answer via apply(). response_format
// json_object is unwrapped once the full object has arrived.

// answerLimits holds the sampling-free controls from an OpenAI request that
// gsb enforces itself.
type answerLimits struct {
	// maxTokens caps the completion length in estimated tokens. Zero disables.
	maxTokens int
	// stopSeqs are the sequences that end the answer, in request order.
	stopSeqs []string
	// wantJSON asks for a bare JSON object instead of prose.
	wantJSON bool
}

// parseAnswerLimits reads the request controls gsb enforces locally.
func parseAnswerLimits(body map[string]any) answerLimits {
	var lim answerLimits
	for _, k := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		if v, ok := body[k].(float64); ok && v > 0 {
			lim.maxTokens = int(v)
			break
		}
	}
	switch v := body["stop"].(type) {
	case string:
		if v != "" {
			lim.stopSeqs = []string{v}
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				lim.stopSeqs = append(lim.stopSeqs, s)
			}
		}
	}
	if text, ok := body["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			typ, _ := format["type"].(string)
			lim.wantJSON = strings.EqualFold(typ, "json_object") || strings.EqualFold(typ, "json_schema")
		}
	}
	if rf, ok := body["response_format"].(map[string]any); ok {
		typ, _ := rf["type"].(string)
		if strings.EqualFold(typ, "json_object") || strings.EqualFold(typ, "json_schema") {
			lim.wantJSON = true
		}
	}
	return lim
}

// enabled reports whether any control shapes the answer.
func (l answerLimits) enabled() bool {
	return l.maxTokens > 0 || len(l.stopSeqs) > 0 || l.wantJSON
}

// streamable reports whether the answer may be streamed incrementally.
//
// A token budget and a stop sequence can be honoured while streaming: the
// emitter asks visible() for the prefix that is still inside the limit, so no
// bytes reach the client that the final answer would not contain. A bare JSON
// object cannot be unwrapped before it is complete, so json_object falls back
// to emitting the answer once.
func (l answerLimits) streamable() bool {
	return !l.wantJSON
}

// visible returns the prefix of text that is safe to emit, and whether a limit
// has been reached. When a limit is reached the caller must stop emitting and
// finish with the returned verdict.
func (l answerLimits) visible(text string) (string, string) {
	return l.visibleAfter(text, 0)
}

// visibleAfter returns the prefix of text that is safe to emit without stepping
// back below minCut (already-streamed byte length).
func (l answerLimits) visibleAfter(text string, minCut int) (string, string) {
	if !l.enabled() {
		return text, ""
	}
	visible, finish := l.applyWithFloor(text, minCut)
	if finish != "" {
		return visible, finish
	}
	// A later frame can complete a stop sequence that starts in this frame.
	// Retain the longest suffix that could be its prefix until then.
	hold := 0
	for _, stop := range l.stopSeqs {
		for n := min(len(stop)-1, len(visible)); n > hold; n-- {
			if utf8.RuneStart(visible[len(visible)-n]) && strings.HasSuffix(visible, stop[:n]) {
				hold = n
				break
			}
		}
	}
	cut := len(visible) - hold
	if cut < minCut && minCut <= len(visible) {
		cut = minCut
	}
	return visible[:cut], ""
}

// apply cuts the answer at the first stop sequence and at the token budget.
// It reports which limit fired so the caller can set finish_reason accordingly.
func (l answerLimits) apply(text string) (string, string) {
	return l.applyWithFloor(text, 0)
}

func (l answerLimits) applyWithFloor(text string, minCut int) (string, string) {
	finish := ""
	if idx := l.stopIndex(text); idx >= 0 {
		text = text[:idx]
		finish = "stop"
	}
	if l.maxTokens > 0 {
		if cut, truncated := truncateToTokensWithFloor(text, l.maxTokens, minCut); truncated {
			text = cut
			finish = "length"
		}
	}
	return text, finish
}

// stopIndex returns the earliest offset of any stop sequence, or -1.
func (l answerLimits) stopIndex(text string) int {
	best := -1
	for _, s := range l.stopSeqs {
		if i := strings.Index(text, s); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}

// truncateToTokens cuts text so that it estimates no more than budget tokens,
// using the same estimator as the usage report so the two cannot disagree.
// It prefers a word boundary when one exists near the cut.
func truncateToTokens(text string, budget int) (string, bool) {
	return truncateToTokensWithFloor(text, budget, 0)
}

func truncateToTokensWithFloor(text string, budget int, minCut int) (string, bool) {
	if text == "" || budget <= 0 {
		return text, false
	}
	if prompt.EstimateTokens(text) <= budget {
		return text, false
	}
	// Grow a prefix until it reaches the budget. The estimator is not linear in
	// bytes, so the cut point is found by measuring instead of scaling.
	lo, hi := 0, len(text)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if prompt.EstimateTokens(text[:mid]) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	cut := lo
	for cut > 0 && cut < len(text) && !utf8.RuneStart(text[cut]) {
		cut--
	}
	if cut > 0 && cut < len(text) {
		// Do not end mid-word: step back to the last space in the final 20% of
		// the kept prefix, but never past the budget or below already-emitted bytes.
		back := cut - cut/5
		if back < 0 {
			back = 0
		}
		if back < minCut {
			back = minCut
		}
		if back < cut {
			if i := strings.LastIndexByte(text[back:cut], ' '); i >= 0 {
				cut = back + i
			}
		}
	}
	if cut < minCut && minCut <= len(text) {
		cut = minCut
	}
	return text[:cut], true
}

// jsonObjectText extracts a bare JSON object from a model answer.
//
// Gemini Web wraps JSON in a ```json fence and often adds prose around it.
// A client that asked for response_format=json_object cannot parse that, so the
// object is unwrapped here. When no object is present the original text is
// returned unchanged: inventing an empty object would hide the failure.
func jsonObjectText(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return text
	}
	if start, end := jsonObjectSpan(trimmed); start >= 0 {
		return trimmed[start : end+1]
	}
	return text
}

// jsonObjectSpan returns the offset range of the first valid balanced JSON
// object in s, or (-1, -1). String literals are skipped so braces inside them
// do not affect the depth count; if an earlier brace pair in prose is not valid
// JSON, scanning continues from the next opening brace.
func jsonObjectSpan(s string) (int, int) {
	for searchFrom := 0; searchFrom < len(s); {
		rel := strings.IndexByte(s[searchFrom:], '{')
		if rel < 0 {
			return -1, -1
		}
		start := searchFrom + rel
		depth := 0
		inString := false
		escaped := false
		closed := false
		for i := start; i < len(s); i++ {
			c := s[i]
			if inString {
				switch {
				case escaped:
					escaped = false
				case c == '\\':
					escaped = true
				case c == '"':
					inString = false
				}
				continue
			}
			switch c {
			case '"':
				inString = true
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					if json.Valid([]byte(s[start : i+1])) {
						return start, i
					}
					closed = true
				}
			}
			if closed {
				break
			}
		}
		searchFrom = start + 1
	}
	return -1, -1
}
