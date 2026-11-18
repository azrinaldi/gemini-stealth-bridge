package toolbridge

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	fencedBlockRe                   = regexp.MustCompile("(?is)(?:```(?:json|tool_call|tool_calls|function_call|function_calls)?\\s*([\\s\\S]*?)```|<(?:tool_call|tool_calls|function_call|function_calls)>\\s*([\\s\\S]*?)</(?:tool_call|tool_calls|function_call|function_calls)>)")
	standaloneAfterRe               = regexp.MustCompile("(?is)^[}\\]\\s]*(?:(?:</(?:tool_call|tool_calls|function_call|function_calls)>|```)[}\\]\\s]*)?$")
	standaloneBeforeRe              = regexp.MustCompile("(?is)(?:^|\\n)\\s*(?:<(?:tool_call|tool_calls|function_call|function_calls)>|```(?:json|tool_call|tool_calls|function_call|function_calls)?)\\s*$")
	exampleIntroRe                  = regexp.MustCompile(`(?is)(?:\bdo\s+not\s+execute\b|\bdon't\s+execute\b|\bfor\s+example\b|\be\.g\.\b|\bexample\s*(?:json|call|payload|envelope|output|response|format)?\s*:\s*|\bformat\s+is\s*:\s*)\s*$`)
	truncatedEnvRe                  = regexp.MustCompile("(?is)(?:(?:^|[\\r\\n]\\s*|```(?:json|tool_call|tool_calls|function_call|function_calls)?\\s*|<(?:tool_call|tool_calls|function_call|function_calls)>\\s*)\\{\\s*\"\\s*(?:tool_calls|toolCalls|tool_call|function_call|function_calls|functionCalls)\\s*\"\\s*:|(?:```(?:json|tool_call|tool_calls|function_call|function_calls)|<(?:tool_call|tool_calls|function_call|function_calls)>)\\s*\\{\\s*\"\\s*(?:name|tool|tool_name)\\s*\"\\s*:)")
	gapFenceRe                      = regexp.MustCompile("(?is)^(?:\\s|```(?:json|tool_call|tool_calls|function_call|function_calls)?|</?(?:tool_call|tool_calls|function_call|function_calls)>)*$")
	trailingTagRe                   = regexp.MustCompile("(?is)(?:```(?:json|tool_call|tool_calls|function_call|function_calls)?|</?(?:tool_call|tool_calls|function_call|function_calls)>)\\s*$")
	trailingEnvRe                   = regexp.MustCompile(`(?s)\{\s*"?\s*(?:tool_calls|toolCalls|tool_call|function_call|function_calls|functionCalls)\s*"?\s*:[\s\S]*$`)
	sanitizedPipeRe                 = regexp.MustCompile(`<([A-Za-z_][A-Za-z0-9_]*)\s+([A-Za-z_][A-Za-z0-9_]*)\s+\|>`)
	htmlTagRe                       = regexp.MustCompile(`<([A-Za-z][A-Za-z0-9:_-]*\s[^>\n]*?)>`)
	missingQuoteAfterEscapedQuoteRe = regexp.MustCompile(`^\s*(?:,\s*"[A-Za-z0-9_.-]+"\s*:|\}\s*,\s*"[A-Za-z0-9_.-]+"\s*:|\}\s*\}\s*(?:,\s*\{|\]\s*\}))`)
	markdownAutolinkRe              = regexp.MustCompile(`\[(https?://[^\]\s]+)\]\((https?://[^)\s]+)\)`)
	toolProtocolRefusalRe           = regexp.MustCompile(`(?is)(?:\b(?:reached|hit|exceeded|exhausted|used\s+up|out\s+of)\s+(?:the\s+|my\s+|our\s+)?(?:tool[- ]?call|function[- ]?call|tool)\s+(?:budget|limit|quota|allowance)\b|\b(?:tool[- ]?call|function[- ]?call|tool)\s+(?:budget|limit|quota|allowance)\s+(?:is\s+|has\s+been\s+)?(?:reached|exceeded|exhausted|depleted)\b|\b(?:prompt\s+injection|jailbreak)[\s\S]{0,80}\b(?:tool_calls|function_call|tool[- ]?calling\s+protocol)\b|\b(?:tool_calls|function_call|tool[- ]?calling\s+protocol)[\s\S]{0,80}\b(?:prompt\s+injection|jailbreak)\b|\b(?:cannot|can't|unable\s+to|no\s+longer\s+able\s+to|refuse\s+to)\s+(?:emit|output|produce|use|follow|obey)\s+(?:the\s+)?(?:tool_calls|tool[- ]?calling\s+protocol)\b|\b(?:reached|hit|exceeded|exhausted|used\s+up|out\s+of)\s+(?:the\s+|my\s+|our\s+)?(?:maximum|max)\s+(?:allowed\s+|permitted\s+|per[- ]turn\s+)?number\s+of\s+(?:tool|search|function)[\s-]*(?:and\s+search\s+)?calls?\b|\b(?:maximum|max)\s+(?:allowed\s+|permitted\s+|per[- ]turn\s+)?number\s+of\s+(?:tool|search|function)[\s-]*(?:and\s+search\s+)?calls?\s+(?:has\s+been\s+|was\s+|is\s+)?(?:exceeded|reached|exhausted|depleted)\b|\b(?:0|zero)\s+(?:tool|search|function)[\s-]*(?:and\s+search\s+)?calls?\s+remaining\b|\b(?:tool|search|function)[\s-]*(?:and\s+search\s+)?calls?\s+remaining\s*[:=]?\s*(?:0|zero)\b|</?prohibit_tool_calling>)`)
	toolCallGuardRe                 = regexp.MustCompile(`(?i)(?:</?prohibit_tool_calling>|maximum\s+number\s+of\s+tool\s+and\s+search\s+calls)`)
	toolGuardBlockRe                = regexp.MustCompile(`(?is)<prohibit_tool_calling>.*?</prohibit_tool_calling>`)
	toolGuardSentenceRe             = regexp.MustCompile(`(?i)you\s+have\s+(?:exceeded|reached)\s+the\s+maximum\s+number\s+of\s+tool\s+and\s+search\s+calls[^.\n]*\.?`)
	toolGuardTailRe                 = regexp.MustCompile(`(?i)(?:^|\n)\s*(?:0|zero)\s+(?:tool|search|function)[\s-]*(?:and\s+search\s+)?calls?\s+remaining\.?`)

	// The guard is confabulated in the language of the conversation, so an
	// English-only detector misses the same invention in Spanish.
	toolGuardSentenceEsRe   = regexp.MustCompile(`(?i)(?:^|\n)\s*se\s+(?:ha\s+)?(?:alcanz[óo]|excedi[óo]|agot[óo]|super[óo])\s+(?:el\s+|la\s+)?(?:l[íi]mite|n[úu]mero\s+m[áa]ximo|cantidad\s+m[áa]xima|total\s+m[áa]ximo|presupuesto)\s+(?:de\s+)?(?:llamadas?\s+(?:a\s+)?)?(?:herramientas?|funciones?)?[^.\n]{0,160}[.:]?`)
	toolGuardTailEsRe       = regexp.MustCompile(`(?i)(?:^|\n)\s*(?:(?:0|cero)\s+llamadas?\s+(?:a\s+)?(?:herramientas?|funciones?)\s+(?:restantes|disponibles)|(?:no|sin)\s+(?:quedan|hay|existen)\s+(?:m[áa]s\s+)?llamadas?\s+(?:a\s+)?(?:herramientas?|funciones?))[^\n]*`)
	toolProtocolRefusalEsRe = regexp.MustCompile(`(?is)(?:se\s+(?:ha\s+)?(?:alcanz[óo]|alcanzad[oa]|excedi[óo]|excedid[oa]|agot[óo]|agotad[oa]|super[óo]|superad[oa])\s+(?:el\s+|la\s+)?(?:l[íi]mite|n[úu]mero\s+m[áa]ximo|cantidad\s+m[áa]xima|total\s+m[áa]ximo|presupuesto)\s+(?:de\s+|permitid[oa]\s+de\s+)?(?:llamadas?\s+(?:a\s+)?)?(?:herramientas?|funciones?|b[úu]squedas?)|(?:l[íi]mite|n[úu]mero\s+m[áa]ximo|cantidad\s+m[áa]xima|presupuesto)\s+(?:de\s+)?(?:llamadas?\s+(?:a\s+)?)?(?:herramientas?|funciones?)\s+(?:se\s+)?(?:ha\s+)?(?:sido\s+)?(?:alcanzad[oa]|excedid[oa]|agotad[oa]|superad[oa])|(?:no|sin)\s+(?:quedan|hay|existen)\s+(?:m[áa]s\s+)?llamadas?\s+(?:a\s+)?(?:herramientas?|funciones?)|(?:0|cero)\s+llamadas?\s+(?:a\s+)?(?:herramientas?|funciones?)\s+(?:restantes|disponibles)|(?:llamadas?\s+(?:a\s+)?(?:herramientas?|funciones?)\s+(?:restantes|disponibles)\s*[:=]?\s*(?:0|cero))|(?:no\s+(?:puedo|podemos|es\s+posible)\s+(?:usar|utilizar|ejecutar|invocar|llamar)\s+(?:m[áa]s\s+)?(?:las\s+|la\s+)?herramientas?))`)

	// toolBudgetClaimRe covers the shapes models actually emit when they invent a
	// tool budget. Enumerating verbs per phrase missed real variants ("cupo",
	// "llamadas de herramientas", a declarative "... en 0"), so this anchors on a
	// scarcity noun next to a tool noun, an exhaustion word next to a tool noun,
	// or a zero/remaining marker instead.
	toolBudgetClaimRe = regexp.MustCompile(`(?i)(?:(?:cupo|cuota|l[íi]mite|limite|presupuesto|n[úu]mero\s+m[áa]ximo|cantidad\s+m[áa]xima|total\s+m[áa]ximo|tool\s+budget|tool\s+limit|quota|allowance)[^.\n]{0,60}(?:herramientas?|funciones?|b[úu]squedas?|tool[- ]?calls?|function[- ]?calls?|search\s+calls?|tools?)[^.\n]{0,40}(?:agotad[oa]s?|alcanzad[oa]s?|excedid[oa]s?|superad[oa]s?|bloquead[oa]s?|no\s+disponibles?|exhausted|exceeded|depleted|used\s+up|(?:en|a)\s+0\b|\b0\s+(?:restantes?|disponibles?|remaining))|(?:agot[óo]|alcanz[óo]|excedi[óo]|super[óo]|bloque[óo])\s+(?:el\s+|la\s+)?(?:cupo|cuota|l[íi]mite|presupuesto|n[úu]mero\s+m[áa]ximo|cantidad\s+m[áa]xima|total\s+m[áa]ximo)\s+(?:de\s+)?(?:llamadas?\s+(?:a\s+|de\s+)?)?(?:herramientas?|funciones?|b[úu]squedas?)|(?:herramientas?|funciones?|b[úu]squedas?|tool[- ]?calls?|function[- ]?calls?|tools?)\b[^.\n]{0,60}(?:agotad[oa]s?|alcanzad[oa]s?|excedid[oa]s?|superad[oa]s?|bloquead[oa]s?|no\s+disponibles?|exhausted|exceeded|depleted|used\s+up)|(?:bloquead[oa]s?|restringid[oa]s?)\s+(?:en\s+|por\s+)?[^.\n]{0,40}(?:llamadas?|herramientas?)|\(\s*\b(?:0|zero|cero)\s+remaining\s*\)|\b(?:0|cero|zero)\s+(?:llamadas?|tool[- ]?calls?|calls?|b[úu]squedas?)|(?:llamadas?|tool[- ]?calls?|calls?|b[úu]squedas?)\s+(?:restantes|disponibles|remaining)\s*[:=]?\s*\b(?:0|cero|zero)\b|(?:sin|no\s+(?:hay|quedan|tiene|tenemos))\s+(?:m[áa]s\s+)?(?:cupo|llamadas?\s+(?:de\s+|a\s+)?(?:herramientas?|funciones?)))`)
	// toolProtocolRefusalPatterns detects an invented refusal of the
	// tool-calling protocol. It is a list of named, separately testable phrases
	// rather than one expression: the model produces this refusal in many
	// shapes (a hallucinated budget, a "tools unavailable this turn" claim, an
	// accusation of prompt injection), and a single alternation could not be
	// extended without risking an accidental precedence bug.
	toolProtocolRefusalPatterns = []struct {
		name string
		re   *regexp.Regexp
	}{
		// Budget/limit claims in English: "exhausted my tool-call budget".
		{"budget-exhausted-en", regexp.MustCompile(`(?i)\b(?:reached|hit|exceeded|exhausted|used\s+up|out\s+of)\s+(?:the\s+|my\s+|our\s+)?(?:tool[- ]?call|function[- ]?call|tool)\s+(?:budget|limit|quota|allowance)\b`)},
		{"budget-reached-en", regexp.MustCompile(`(?i)\b(?:tool[- ]?call|function[- ]?call|tool)\s+(?:budget|limit|quota|allowance)\s+(?:is\s+|has\s+been\s+)?(?:reached|exceeded|exhausted|depleted)\b`)},
		{"zero-remaining-en", regexp.MustCompile(`(?i)\b(?:tool[- ]?calls?|function[- ]?calls?|tools?|budget)\b[^.!?]{0,60}\b0\s+remaining\b`)},
		{"zero-left-en", regexp.MustCompile(`(?i)\b0\s+(?:tool|function)[- ]?(?:call|use)s?\s+(?:remaining|left)\b`)},
		// Budget claims in Indonesian: "kuota pemanggilan tool sudah habis".
		{"budget-exhausted-id", regexp.MustCompile(`(?i)\bkuota\s+(?:pemanggilan\s+|pemakaian\s+|penggunaan\s+)?(?:tool|fungsi)\b[^.!?]{0,80}?\b(?:sudah|telah)\s+habis\b`)},
		{"zero-remaining-id", regexp.MustCompile(`(?i)\b(?:0|nol)\s+(?:tool|pemanggilan\s+tool|tool\s+calls?)\b[^.!?]{0,30}\b(?:tersisa|tersedia|left|remaining)\b`)},
		{"zero-left-id", regexp.MustCompile(`(?i)\b(?:sisa|jumlah)\s+(?:tool|pemanggilan\s+tool|tool\s+calls?)\b[^.!?]{0,40}\b(?:0|nol)\b`)},
		// The number can also follow the noun: "tool calls remaining: **0**",
		// "tool budget left: 0". Markdown emphasis, backticks and a colon may sit
		// between the noun and the digit, so they are absorbed by the separator
		// class. \x60 is a backtick; a literal one cannot appear inside a raw
		// string literal.
		{"zero-after-noun-en", regexp.MustCompile("(?i)\\b(?:tool|function)[- ]?(?:calls?|budget|quota|limit|uses?|use)\\b(?:[\\s*_:\\x60]+(?:calls?|remaining|left|available|reached))*[\\s*_:\\x60]{1,4}(?:0|zero)\\b")},
		// The model also drops the word "tool": "search calls remaining: **0**".
		// Requiring an adjacent tool/function noun missed that shape, so the
		// "calls remaining/left" pair is matched on its own; the digit must still
		// be zero directly after it.
		{"calls-remaining-zero-en", regexp.MustCompile("(?i)\\b(?:calls?|uses?)\\s+(?:remaining|left|available)\\b[\\s*_:\\x60]{0,6}(?:0|zero)\\b")},
		{"zero-after-noun-id", regexp.MustCompile("(?i)\\b(?:pemanggilan\\s+tool|tool\\s+calls?|kuota\\s+tool|sisa\\s+tool)\\b[\\s*_:\\x60]{1,4}(?:0|nol)\\b")},
		// Availability claims: the tool surface is switched off for this turn.
		// This is the shape that reached the client as a final answer: "Eksekusi
		// tool inspeksi file sedang tidak tersedia di turn ini (0 tool calls
		// tersisa)".
		{"unavailable-this-turn-id", regexp.MustCompile(`(?i)\b(?:tool|tool\s+calls?|pemanggilan\s+tool|eksekusi\s+tool|tidak)\b[^.!?]{0,80}\b(?:sedang\s+)?tidak\s+(?:tersedia|dapat\s+digunakan|bisa\s+digunakan|aktif|tersedia\s+di\s+turn)\b`)},
		{"unavailable-this-turn-en", regexp.MustCompile(`(?i)\b(?:tools?|tool[- ]?calls?|tool\s+execution)\b[^.!?]{0,60}\b(?:is|are)\s+(?:currently\s+)?(?:not\s+available|unavailable|disabled)\b`)},
		{"cannot-invoke-id", regexp.MustCompile(`(?i)\b(?:tidak|tak)\s+(?:bisa|dapat)\s+(?:menjalankan|memanggil|menggunakan|mengeksekusi)\s+(?:the\s+)?(?:tool|tools|tool\s+calls?)\b`)},
		{"cannot-invoke-en", regexp.MustCompile(`(?i)\b(?:cannot|can't|unable\s+to|no\s+longer\s+able\s+to|refuse\s+to)\s+(?:emit|output|produce|use|follow|obey|invoke|call|run)\s+(?:the\s+)?(?:tool_calls|tool[- ]?calling\s+protocol|tools?)\b`)},
		// Reframing the tool protocol itself as adversarial.
		{"prompt-injection-claim", regexp.MustCompile(`(?i)\b(?:prompt\s+injection|jailbreak)\b[^.!?]{0,80}\b(?:tool_calls|function_call|tool[- ]?calling\s+protocol)\b`)},
		{"protocol-called-injection", regexp.MustCompile(`(?i)\b(?:tool_calls|function_call|tool[- ]?calling\s+protocol)\b[^.!?]{0,80}\b(?:prompt\s+injection|jailbreak)\b`)},
	}
)

// ToolProtocolRefusalError is the tool_call_error text produced when the model
// invents a tool-call budget or otherwise refuses the tool-calling protocol.
// Callers use it to choose a targeted correction instead of the generic
// schema-validation note.
const ToolProtocolRefusalError = "the model refused the tool-calling protocol (hallucinated tool limit or refusal)"

// StripToolGuard removes an upstream tool-call guard from free text so the
// invented "tool budget" notice never reaches the client as answer content or
// reasoning, where it would poison later turns of the conversation.
func StripToolGuard(text string) string {
	if text == "" {
		return text
	}
	out := toolGuardBlockRe.ReplaceAllString(text, "")
	out = toolGuardSentenceRe.ReplaceAllString(out, "")
	out = toolGuardSentenceEsRe.ReplaceAllString(out, "")
	out = toolGuardTailRe.ReplaceAllString(out, "\n")
	out = toolGuardTailEsRe.ReplaceAllString(out, "\n")
	out = stripBudgetClaimLines(out)
	out = strings.TrimSpace(out)
	return out
}

// budgetClaimStart reports the byte offset where a hallucinated tool-budget
// claim begins inside one line, or -1 when the line carries none. Text before
// that offset is retained so a legitimate summary prefix survives stripping.
func budgetClaimStart(line string) int {
	start := -1
	// Only guard-shaped patterns may truncate text. The broader refusal
	// detectors also match ordinary statements such as "the maximum number of
	// tool calls is 10"; stripping on those would delete legitimate answers.
	for _, re := range []*regexp.Regexp{
		toolBudgetClaimRe,
		toolCallGuardRe,
		toolGuardTailRe,
		toolGuardTailEsRe,
	} {
		if loc := re.FindStringIndex(line); loc != nil && (start < 0 || loc[0] < start) {
			start = loc[0]
		}
	}
	return start
}

func stripBudgetClaimLines(text string) string {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if start := budgetClaimStart(line); start >= 0 {
			if prefix := strings.TrimSpace(line[:start]); prefix != "" {
				kept = append(kept, prefix)
			}
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func unwrapIdenticalMarkdownLinks(s string) string {
	return markdownAutolinkRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := markdownAutolinkRe.FindStringSubmatch(m)
		if len(sub) == 3 && sub[1] == sub[2] {
			return sub[1]
		}
		return m
	})
}

// IsToolProtocolRefusal reports whether a plain-text reply is an invented
// refusal of the tool-calling protocol itself (such as hallucinating an
// exhausted "tool budget" or claiming the tool_calls JSON format is a prompt
// injection).
//
// Only the opening of the reply is inspected, because a refusal is the model's
// opening statement. The previous rule rejected any reply longer than 1200
// bytes outright, and that failed in the shape the model actually produces when
// it gives up on tools: a one-line refusal followed by step-by-step manual
// instructions. Such a reply was shipped to the client as a final answer, which
// is the reported "it says it cannot execute" behaviour. A bounded window
// catches the opening refusal and still leaves a long answer that merely
// mentions a tool budget mid-document alone.

// RefusalWindowBytes is how much of the reply the refusal detector reads. The
// streaming gate uses it too: with tools active, no text may be emitted before
// this many bytes have arrived, because an emitted byte commits the attempt and
// makes the tool_call_error retry illegal.
const RefusalWindowBytes = 2000

// IsToolProtocolRefusal reports whether a plain-text reply is an invented
// refusal of the tool-calling protocol.
func IsToolProtocolRefusal(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	// The upstream guard marker is unambiguous and can arrive embedded in an
	// otherwise long answer, so it is checked without a length bound.
	if toolCallGuardRe.MatchString(trimmed) {
		return true
	}
	// Model replies wrap at prose width, so a refusal routinely breaks the
	// phrase it uses across lines ("sedang tidak\n tersedia"). Collapse every
	// whitespace run to one space first: the patterns are phrase-shaped, and a
	// newline in the middle of a phrase must not hide the refusal.
	trimmed = strings.Join(strings.Fields(trimmed), " ")
	if len(trimmed) > RefusalWindowBytes {
		trimmed = trimmed[:RefusalWindowBytes]
	}
	// These detectors are bounded by the same window as the pattern list: a
	// late mention inside an otherwise legitimate long answer must stay an
	// answer, and the streaming gate only holds RefusalWindowBytes anyway.
	if toolProtocolRefusalEsRe.MatchString(trimmed) {
		return true
	}
	if toolBudgetClaimRe.MatchString(trimmed) {
		return true
	}
	if toolProtocolRefusalRe.MatchString(trimmed) {
		return true
	}
	return matchToolProtocolRefusal(trimmed)
}

// RefusalErrorText is the message carried by a tool_call_error that was caused
// by an invented protocol refusal rather than by a schema violation. The server
// uses it to choose a correction that contradicts the refusal instead of one
// that asks for better JSON.
const RefusalErrorText = "the model refused the tool-calling protocol"

// IsRefusalMessage reports whether a tool_call_error message describes a
// protocol refusal. Both the streaming gate and InterpretReply use this exact
// text so the server has one string to test.
func IsRefusalMessage(msg string) bool {
	return strings.Contains(msg, RefusalErrorText)
}

// matchToolProtocolRefusal reports whether any known refusal phrase matches.
// RefusalPatternNames is exported for tests so a new pattern cannot be added
// without a case that proves it fires and a case that proves normal prose does
// not.
func matchToolProtocolRefusal(trimmed string) bool {
	for _, p := range toolProtocolRefusalPatterns {
		if p.re.MatchString(trimmed) {
			return true
		}
	}
	return zeroQuotaRefusal(trimmed)
}

// zeroQuotaRefusal reports the budget-refusal shape structurally instead of by
// phrase. Enumerating wordings did not converge: the model produced "0 tool
// calls tersisa", then "calls remaining: **0**", then "There are **0** built-in
// tool and search calls remaining" for the same question, and every new wording
// shipped a refusal as the final answer.
//
// The structural signal is a zero count stated next to both a tool word and an
// availability word, inside one sentence. Prose that merely reports a zero
// result ("returned 0 matches", "0 errors were found") has no availability word
// and stays an answer.
func zeroQuotaRefusal(trimmed string) bool {
	if !strings.Contains(trimmed, "0") && !strings.Contains(strings.ToLower(trimmed), "zero") && !strings.Contains(strings.ToLower(trimmed), "nol") {
		return false
	}
	for _, sentence := range splitRefusalSentences(trimmed) {
		lower := strings.ToLower(sentence)
		if !refusalZeroRe.MatchString(lower) {
			continue
		}
		if refusalToolWordRe.MatchString(lower) && refusalAvailabilityRe.MatchString(lower) {
			return true
		}
	}
	return false
}

// splitRefusalSentences cuts on sentence terminators so a zero in one sentence
// cannot be paired with an availability word in another.
func splitRefusalSentences(s string) []string {
	return refusalSentenceSplitRe.Split(s, -1)
}

var (
	refusalSentenceSplitRe = regexp.MustCompile(`[.!?\n]+`)
	// A zero standing alone as a count: not "10", "0.5", or "100".
	refusalZeroRe         = regexp.MustCompile(`(?:^|[\s(*_:=])(?:0|zero|nol)(?:$|[\s)*_.,:;])`)
	refusalToolWordRe     = regexp.MustCompile(`\b(?:tool|tools|tool[- ]?calls?|function[- ]?calls?|pemanggilan\s+tool|fungsi|search\s+calls?)\b`)
	refusalAvailabilityRe = regexp.MustCompile(`\b(?:remaining|left|available|tersisa|tersedia|limit|quota|budget|kuota|habis|maximum|reached)\b`)
)

// RefusalPatternNames returns the identifier of every refusal pattern.
func RefusalPatternNames() []string {
	names := make([]string, 0, len(toolProtocolRefusalPatterns))
	for _, p := range toolProtocolRefusalPatterns {
		names = append(names, p.name)
	}
	return names
}

type candidateSpan struct {
	raw    string
	start  int
	end    int
	whole  bool
	fenced bool
}

// hasUnbalancedToolEnvelopeOpener reports whether the reply contains a
// `{"tool_calls": ...` (or alias) opener whose object NEVER reaches a balanced
// close — i.e. the JSON was cut off mid-envelope. Prose mentions inside inline
// code with placeholders like [...] or <tool> still open an object that closes
// unbalanced only when truncated for real, so this is the structural signal.
func hasUnbalancedToolEnvelopeOpener(s string) bool {
	openerRe := regexp.MustCompile(`(?is)\{\s*"\s*(?:tool_calls|toolCalls|tool_call|function_call|function_calls|functionCalls)\s*"\s*:`)
	for _, loc := range openerRe.FindAllStringIndex(s, -1) {
		objs := findBalancedObjects(s[loc[0]:])
		if len(objs) == 0 || objs[0][0] != 0 || !hasBalancedBracketsOutsideStrings(s[loc[0]:loc[0]+objs[0][1]]) {
			return true
		}
		candSpan := s[loc[0] : loc[0]+objs[0][1]]
		if isStandaloneSpan(s, loc[0], loc[0]+objs[0][1]) && !strings.Contains(candSpan, "[...]") && !strings.Contains(candSpan, "{...}") {
			if _, ok := unmarshalCandidateObject(candSpan); !ok {
				return true
			}
		}
	}
	return false
}

func hasBalancedBracketsOutsideStrings(s string) bool {
	depth := 0
	inStr := false
	esc := false
	for i := range len(s) {
		ch := s[i]
		if inStr {
			if esc {
				esc = false
				if ch == '"' && missingQuoteAfterEscapedQuoteRe.MatchString(s[i+1:]) {
					inStr = false
				}
			} else if ch == '\\' {
				esc = true
			} else if ch == '"' {
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '[':
			depth++
		case ']':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

func findBalancedObjects(s string) [][2]int {
	var spans [][2]int
	depth := 0
	start := -1
	inStr := false
	esc := false
	for i := range len(s) {
		ch := s[i]
		if inStr {
			if esc {
				esc = false
				if ch == '"' && missingQuoteAfterEscapedQuoteRe.MatchString(s[i+1:]) {
					inStr = false
				}
			} else if ch == '\\' {
				esc = true
			} else if ch == '"' {
				inStr = false
			}
			continue
		}
		if ch == '"' {
			inStr = true
			continue
		}
		if ch == '{' {
			if depth == 0 {
				start = i
			}
			depth++
		} else if ch == '}' {
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					spans = append(spans, [2]int{start, i + 1})
					start = -1
				}
			}
		}
	}
	return spans
}

func repairJSONStringEscapes(s string) string {
	if s == "" {
		return s
	}
	var out strings.Builder
	out.Grow(len(s) + 16)
	inStr := false
	var stack []byte
	var lastSig byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !inStr {
			if ch == '"' {
				inStr = true
				out.WriteByte(ch)
				continue
			}
			if ch == '{' {
				stack = append(stack, '}')
				lastSig = '{'
				out.WriteByte(ch)
				continue
			}
			if ch == '[' {
				stack = append(stack, ']')
				lastSig = '['
				out.WriteByte(ch)
				continue
			}
			if ch == '}' || ch == ']' {
				if len(stack) == 0 {
					continue
				}
				expected := stack[len(stack)-1]
				if ch == expected {
					stack = stack[:len(stack)-1]
					lastSig = ch
					out.WriteByte(ch)
					continue
				}
				j := i + 1
				for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\r' || s[j] == '\n' || s[j] == ch) {
					j++
				}
				if j < len(s) && s[j] == expected {
					continue
				}
			}
			// Strip trailing commas before '}' or ']' outside string literals.
			if ch == ',' {
				j := i + 1
				for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\r' || s[j] == '\n') {
					j++
				}
				if j < len(s) && (s[j] == '}' || s[j] == ']') {
					continue
				}
				lastSig = ','
				out.WriteByte(ch)
				continue
			}
			if ch == ':' {
				lastSig = ':'
				out.WriteByte(ch)
				continue
			}
			// Quote bare identifier keys inside JSON objects: {name: "bash"} -> {"name": "bash"}
			if len(stack) > 0 && stack[len(stack)-1] == '}' && ((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_') {
				kEnd := i + 1
				for kEnd < len(s) {
					c := s[kEnd]
					if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.' {
						kEnd++
					} else {
						break
					}
				}
				colonIdx := kEnd
				for colonIdx < len(s) && (s[colonIdx] == ' ' || s[colonIdx] == '\t' || s[colonIdx] == '\r' || s[colonIdx] == '\n') {
					colonIdx++
				}
				if colonIdx < len(s) && s[colonIdx] == ':' {
					if lastSig != '{' && lastSig != ',' && lastSig != 0 {
						out.WriteByte(',')
					}
					out.WriteByte('"')
					out.WriteString(s[i:kEnd])
					out.WriteByte('"')
					lastSig = '"'
					i = kEnd - 1
					continue
				}
			}
			if ch != ' ' && ch != '\t' && ch != '\r' && ch != '\n' {
				lastSig = ch
			}
			out.WriteByte(ch)
			continue
		}
		if ch == '"' {
			inStr = false
			lastSig = '"'
			out.WriteByte(ch)
			continue
		}
		switch ch {
		case '\n':
			out.WriteString(`\n`)
			continue
		case '\r':
			out.WriteString(`\r`)
			continue
		case '\t':
			out.WriteString(`\t`)
			continue
		case '\\':
			if i+1 < len(s) {
				next := s[i+1]
				switch next {
				case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
					out.WriteByte(ch)
					out.WriteByte(next)
					if next == '"' && missingQuoteAfterEscapedQuoteRe.MatchString(s[i+2:]) {
						out.WriteByte('"')
						inStr = false
					}
					i++
					continue
				case 'u':
					if i+6 <= len(s) && isHex4(s[i+2:i+6]) {
						out.WriteString(s[i : i+6])
						i += 5
						continue
					}
				}
			}
			out.WriteString(`\\`)
			continue
		}
		out.WriteByte(ch)
	}
	return out.String()
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := range 4 {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func repairSanitizedHTMLInJSON(s string) string {
	if s == "" || (!strings.Contains(s, "{") && !strings.Contains(s, "[")) {
		return s
	}
	out := repairJSONStringEscapes(s)
	if strings.Contains(out, "<") {
		out = sanitizedPipeRe.ReplaceAllString(out, `<$1 | $2>`)
		out = htmlTagRe.ReplaceAllStringFunc(out, func(full string) string {
			inner := full[1 : len(full)-1]
			if !strings.Contains(inner, `"`) {
				return full
			}
			inner = strings.ReplaceAll(inner, `""`, `\"`)
			var fixed strings.Builder
			for i := range len(inner) {
				if inner[i] == '"' && (i == 0 || inner[i-1] != '\\') {
					fixed.WriteString(`\"`)
				} else {
					fixed.WriteByte(inner[i])
				}
			}
			return "<" + fixed.String() + ">"
		})
	}
	return out
}

func isStandaloneSpan(text string, start, end int) bool {
	before := text[:start]
	after := text[end:]

	if exampleIntroRe.MatchString(before) {
		return false
	}

	lineStart := strings.LastIndexByte(before, '\n') + 1
	beforeOnLine := strings.TrimSpace(before[lineStart:])
	if beforeOnLine != "" && !standaloneBeforeRe.MatchString(before) {
		return false
	}

	afterOnLine := after
	if nl := strings.IndexByte(after, '\n'); nl >= 0 {
		afterOnLine = after[:nl]
	}
	afterOnLine = strings.TrimSpace(afterOnLine)
	if afterOnLine != "" && !standaloneAfterRe.MatchString(afterOnLine) {
		return false
	}

	afterTrimmed := strings.TrimSpace(after)
	if afterTrimmed == "" || standaloneAfterRe.MatchString(afterTrimmed) {
		return true
	}
	return strings.HasPrefix(after, "\n") || strings.HasPrefix(after, "\r\n")
}

func trimMapKeysRecursive(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, elem := range val {
			cleaned := trimMapKeysRecursive(elem)
			tk := strings.TrimSpace(k)
			if tk != "" && tk != k {
				if _, exists := val[tk]; !exists {
					out[tk] = cleaned
					continue
				}
			}
			out[k] = cleaned
		}
		return out
	case []any:
		for i, elem := range val {
			val[i] = trimMapKeysRecursive(elem)
		}
		return val
	default:
		return v
	}
}

func unmarshalCandidateObject(raw string) (map[string]any, bool) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil && parsed != nil {
		if strings.Contains(raw, " |>") {
			if rep := repairSanitizedHTMLInJSON(raw); rep != raw {
				var repParsed map[string]any
				if err2 := json.Unmarshal([]byte(rep), &repParsed); err2 == nil && repParsed != nil {
					return trimMapKeysRecursive(repParsed).(map[string]any), true
				}
			}
		}
		return trimMapKeysRecursive(parsed).(map[string]any), true
	}
	if rep := repairSanitizedHTMLInJSON(raw); rep != raw {
		if err := json.Unmarshal([]byte(rep), &parsed); err == nil && parsed != nil {
			return trimMapKeysRecursive(parsed).(map[string]any), true
		}
	}
	return nil, false
}

func unmarshalCandidateArray(raw string) ([]any, bool) {
	var arr []any
	if err := json.Unmarshal([]byte(raw), &arr); err == nil && len(arr) > 0 {
		return trimMapKeysRecursive(arr).([]any), true
	}
	if rep := repairSanitizedHTMLInJSON(raw); rep != raw {
		if err := json.Unmarshal([]byte(rep), &arr); err == nil && len(arr) > 0 {
			return trimMapKeysRecursive(arr).([]any), true
		}
	}
	return nil, false
}

func callCandidates(text string) []candidateSpan {
	trimmed := strings.TrimSpace(text)
	var out []candidateSpan
	if trimmed == "" {
		return out
	}

	out = append(out, candidateSpan{raw: trimmed, start: 0, end: len(trimmed), whole: true, fenced: false})
	for _, loc := range fencedBlockRe.FindAllStringSubmatchIndex(trimmed, -1) {
		if exampleIntroRe.MatchString(trimmed[:loc[0]]) {
			continue
		}
		// A fenced envelope is a tool call only when the fence sits on its own
		// line(s) (standalone). Fenced JSON embedded at the end of a prose
		// bullet/paragraph (e.g. "… blok ```json {"tool_calls":[…]} ``` di dalam
		// penjelasan") is a prose mention, not a call.
		if !isStandaloneSpan(trimmed, loc[0], loc[1]) {
			continue
		}
		subStart, subEnd := -1, -1
		if len(loc) >= 4 && loc[2] >= 0 && loc[3] >= 0 {
			subStart, subEnd = loc[2], loc[3]
		} else if len(loc) >= 6 && loc[4] >= 0 && loc[5] >= 0 {
			subStart, subEnd = loc[4], loc[5]
		}
		if subStart >= 0 && subEnd >= subStart {
			raw := strings.TrimSpace(trimmed[subStart:subEnd])
			if raw != "" {
				out = append(out, candidateSpan{raw: raw, start: loc[0], end: loc[1], whole: false, fenced: true})
			}
		}
	}
	for _, sp := range findBalancedObjects(trimmed) {
		out = append(out, candidateSpan{
			raw:    trimmed[sp[0]:sp[1]],
			start:  sp[0],
			end:    sp[1],
			whole:  false,
			fenced: isStandaloneSpan(trimmed, sp[0], sp[1]),
		})
	}
	return out
}

func looksLikeEnvelope(v map[string]any) bool {
	if v == nil {
		return false
	}
	for k := range v {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "tool_calls", "toolcalls", "tool_call", "function_call", "function_calls", "functioncalls":
			return true
		}
	}
	return false
}

func extractBareCallName(v map[string]any) string {
	if v == nil {
		return ""
	}
	for _, wrapKey := range []string{"function", "tool_call", "function_call", "call", "tool", "invoke"} {
		if sub, ok := v[wrapKey].(map[string]any); ok && sub != nil {
			if n := extractBareCallName(sub); n != "" {
				return n
			}
		}
	}
	for _, k := range []string{"name", "tool", "tool_name", "toolName", "function", "function_name", "functionName", "action", "recipient_name"} {
		if n, ok := v[k].(string); ok && strings.TrimSpace(n) != "" {
			return strings.TrimSpace(n)
		}
	}
	for k, rawVal := range v {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "name", "tool", "tool_name", "toolname", "function", "function_name", "functionname", "action", "recipient_name":
			if n, ok := rawVal.(string); ok && strings.TrimSpace(n) != "" {
				return strings.TrimSpace(n)
			}
		}
	}
	return ""
}

func looksLikeBareCall(v map[string]any) bool {
	if extractBareCallName(v) == "" {
		return false
	}
	target := v
	if sub, ok := v["function"].(map[string]any); ok && sub != nil {
		target = sub
	}
	for _, k := range []string{"arguments", "parameters", "args", "input", "params", "action_input"} {
		if _, ok := target[k]; ok {
			return true
		}
		if _, ok := v[k]; ok {
			return true
		}
	}
	return false
}

var reservedCallEnvelopeKeys = map[string]bool{
	"name": true, "tool": true, "tool_name": true, "toolname": true,
	"function": true, "function_name": true, "functionname": true,
	"action": true, "recipient_name": true, "type": true,
	"id": true, "call_id": true, "tool_call_id": true, "index": true,
	"arguments": true, "parameters": true, "args": true, "input": true, "params": true, "action_input": true,
}

func resolveAllowedToolName(name string, allow map[string]map[string]any) string {
	name = strings.TrimSpace(name)
	if name == "" || allow == nil {
		return name
	}
	if _, exists := allow[name]; exists {
		return name
	}
	for _, sep := range []string{":", ".", "/"} {
		if idx := strings.LastIndex(name, sep); idx >= 0 && idx+1 < len(name) {
			short := name[idx+1:]
			if _, exists := allow[short]; exists {
				return short
			}
		}
	}
	for k := range allow {
		if strings.EqualFold(k, name) {
			return k
		}
	}
	return name
}

func inferToolNameFromSchema(candArgs map[string]any, allow map[string]map[string]any, prevName string) string {
	if len(candArgs) == 0 || len(allow) == 0 {
		return ""
	}
	var matches []string
	bestExtraProps := int(^uint(0) >> 1)
	for tName, schema := range allow {
		if schema == nil {
			continue
		}
		props, _ := schema["properties"].(map[string]any)
		if len(props) == 0 {
			continue
		}
		allKnown := true
		for k := range candArgs {
			if _, exists := props[k]; !exists {
				allKnown = false
				break
			}
		}
		if !allKnown {
			continue
		}
		if CheckSchema(schema, candArgs, "arguments") != "" {
			continue
		}
		extra := len(props) - len(candArgs)
		if extra < bestExtraProps {
			bestExtraProps = extra
			matches = []string{tName}
		} else if extra == bestExtraProps {
			matches = append(matches, tName)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	if len(matches) > 1 && prevName != "" {
		for _, m := range matches {
			if m == prevName {
				return prevName
			}
		}
	}
	return ""
}

func flattenCallSlice(raw any) []any {
	s, ok := raw.([]any)
	if !ok {
		return []any{raw}
	}
	var out []any
	for _, elem := range s {
		if nested, isSlice := elem.([]any); isSlice {
			out = append(out, flattenCallSlice(nested)...)
		} else {
			out = append(out, elem)
		}
	}
	return out
}

func normaliseCalls(envelope map[string]any, allow map[string]map[string]any) ([]Call, error) {
	var raw any
	for _, k := range []string{"tool_calls", "toolCalls", "tool_call", "function_call", "function_calls", "functionCalls"} {
		if v, ok := envelope[k]; ok {
			raw = v
			break
		}
	}
	if raw == nil {
		for k, v := range envelope {
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "tool_calls", "toolcalls", "tool_call", "function_call", "function_calls", "functioncalls":
				raw = v
			}
			if raw != nil {
				break
			}
		}
	}
	if raw == nil {
		return nil, nil
	}
	rawSlice := flattenCallSlice(raw)
	if len(rawSlice) == 0 {
		return nil, nil
	}

	out := make([]Call, 0, len(rawSlice))
	for _, itemAny := range rawSlice {
		if itemAny == nil {
			continue
		}
		itemMap, ok := itemAny.(map[string]any)
		if !ok || itemMap == nil {
			return nil, toolCallError("a tool call was missing its function name")
		}
		if len(itemMap) == 0 {
			continue
		}
		for _, wrapKey := range []string{"tool_call", "function_call", "call", "invoke"} {
			if sub, isMap := itemMap[wrapKey].(map[string]any); isMap && len(sub) > 0 {
				itemMap = sub
				break
			}
		}
		fn := itemMap
		if sub, ok := itemMap["function"].(map[string]any); ok && sub != nil {
			fn = sub
		}
		name := resolveAllowedToolName(extractBareCallName(itemMap), allow)

		var argsRaw any
		for _, k := range []string{"arguments", "parameters", "args", "input", "params", "action_input"} {
			if val, exists := fn[k]; exists {
				argsRaw = val
				break
			}
			if val, exists := itemMap[k]; exists {
				argsRaw = val
				break
			}
		}

		if name == "" && allow != nil {
			// Check tool-name-as-key pattern: {"write": {"path": "...", "content": "..."}}
			for k, v := range itemMap {
				resolved := resolveAllowedToolName(k, allow)
				if _, isAllowed := allow[resolved]; isAllowed {
					if _, isMap := v.(map[string]any); isMap {
						name = resolved
						argsRaw = v
						break
					}
				}
			}
		}
		if name == "" && allow != nil {
			prevName := ""
			if len(out) > 0 {
				prevName = out[len(out)-1].Name
			}
			if argMap, isMap := argsRaw.(map[string]any); isMap {
				name = inferToolNameFromSchema(argMap, allow, prevName)
			} else if argStr, isStr := argsRaw.(string); isStr {
				if parsedArgs, ok := unmarshalCandidateObject(argStr); ok {
					name = inferToolNameFromSchema(parsedArgs, allow, prevName)
				}
			} else if argsRaw == nil {
				cleanItem := make(map[string]any, len(itemMap))
				for k, v := range itemMap {
					if !reservedCallEnvelopeKeys[strings.ToLower(strings.TrimSpace(k))] {
						cleanItem[k] = v
					}
				}
				if len(cleanItem) == 0 && len(fn) > 0 {
					for k, v := range fn {
						if !reservedCallEnvelopeKeys[strings.ToLower(strings.TrimSpace(k))] {
							cleanItem[k] = v
						}
					}
				}
				if inferred := inferToolNameFromSchema(cleanItem, allow, prevName); inferred != "" {
					name = inferred
					argsRaw = cleanItem
				}
			}
		}
		if name == "" {
			return nil, toolCallError("a tool call was missing its function name")
		}
		if allow != nil {
			if _, exists := allow[name]; !exists {
				return nil, toolCallError(fmt.Sprintf(`the model called %q, which is not among the tools of this request`, name))
			}
		}

		var args map[string]any
		switch a := argsRaw.(type) {
		case nil:
			args = map[string]any{}
			for k, v := range fn {
				if !reservedCallEnvelopeKeys[strings.ToLower(strings.TrimSpace(k))] {
					args[k] = v
				}
			}
			if len(args) == 0 && len(itemMap) > 0 {
				for k, v := range itemMap {
					if !reservedCallEnvelopeKeys[strings.ToLower(strings.TrimSpace(k))] {
						args[k] = v
					}
				}
			}
		case string:
			var parsed any
			if err := json.Unmarshal([]byte(a), &parsed); err != nil {
				if rep := repairSanitizedHTMLInJSON(a); rep != a {
					err = json.Unmarshal([]byte(rep), &parsed)
				}
				if err != nil {
					return nil, toolCallError(fmt.Sprintf(`arguments for %q were not valid JSON`, name))
				}
			}
			parsed = trimMapKeysRecursive(parsed)
			m, ok := parsed.(map[string]any)
			if !ok || m == nil {
				return nil, toolCallError(fmt.Sprintf(`arguments for %q must be a JSON object`, name))
			}
			args = m
		case map[string]any:
			args = make(map[string]any, len(a))
			for k, v := range a {
				args[k] = v
			}
		default:
			return nil, toolCallError(fmt.Sprintf(`arguments for %q must be a JSON object`, name))
		}
		var schema map[string]any
		if allow != nil {
			schema = allow[name]
		}
		if schema != nil {
			reqSlice := extractStringSlice(schema["required"])
			reqSet := make(map[string]bool, len(reqSlice))
			for _, r := range reqSlice {
				reqSet[r] = true
			}
			props, _ := schema["properties"].(map[string]any)
			for _, src := range []map[string]any{fn, itemMap} {
				for k, v := range src {
					if reservedCallEnvelopeKeys[strings.ToLower(strings.TrimSpace(k))] {
						continue
					}
					if _, exists := args[k]; exists {
						continue
					}
					if props == nil {
						args[k] = v
					} else if _, known := props[k]; known {
						args[k] = v
					}
				}
			}
			for k, v := range args {
				if v == nil {
					propSchema := any(nil)
					if props != nil {
						propSchema = props[k]
					}
					propMap, _ := propSchema.(map[string]any)
					isNullable := propMap != nil && (propMap["nullable"] == true || CheckSchema(propSchema, nil, k) == "")
					if !reqSet[k] || isNullable {
						if !reqSet[k] {
							delete(args, k)
						}
					}
				}
			}
			for _, reqKey := range reqSlice {
				if _, exists := args[reqKey]; !exists && props != nil {
					if propMap, ok := props[reqKey].(map[string]any); ok {
						if defVal, hasDef := propMap["default"]; hasDef && defVal != nil {
							args[reqKey] = defVal
						}
					}
				}
			}
		}

		for k, v := range args {
			if s, ok := v.(string); ok {
				if strings.Contains(s, " |>") {
					s = sanitizedPipeRe.ReplaceAllString(s, `<$1 | $2>`)
				}
				if strings.Contains(s, "](") {
					s = unwrapIdenticalMarkdownLinks(s)
				}
				args[k] = s
			}
		}
		if schema != nil {
			CoerceArgumentsToSchema(schema, args)
		}

		if errStr := CheckSchema(schema, args, "arguments"); errStr != "" {
			return nil, toolCallError(fmt.Sprintf(`arguments for %q violate the tool schema: %s`, name, errStr))
		}
		out = append(out, Call{Name: name, Arguments: args})
	}
	return out, nil
}

// ParseToolReply extracts and validates tool calls from a model reply.
func ParseToolReply(reply string, tools any) (*ParseResult, error) {
	trimmed := strings.TrimSpace(reply)
	var allow map[string]map[string]any
	if tools != nil {
		contract, err := ValidateToolRequest(tools, "auto")
		if err != nil {
			return nil, err
		}
		if len(contract.Tools) > 0 {
			allow = make(map[string]map[string]any, len(contract.Tools))
			for _, t := range contract.Tools {
				allow[t.Name] = t.Parameters
			}
		}
	}
	if trimmed == "" {
		return &ParseResult{Reason: "empty reply", Text: ""}, nil
	}

	type foundEnv struct {
		value map[string]any
		span  candidateSpan
	}
	var envelopes []foundEnv
	var bares []foundEnv

	for _, cand := range callCandidates(reply) {
		if parsed, ok := unmarshalCandidateObject(cand.raw); ok {
			if looksLikeEnvelope(parsed) {
				if cand.whole || cand.fenced {
					envelopes = append(envelopes, foundEnv{value: parsed, span: cand})
				}
				continue
			}
			if looksLikeBareCall(parsed) && (cand.whole || cand.fenced) {
				name := extractBareCallName(parsed)
				if allow == nil {
					bares = append(bares, foundEnv{value: map[string]any{"tool_calls": []any{parsed}}, span: cand})
				} else if _, ok := allow[name]; ok {
					bares = append(bares, foundEnv{value: map[string]any{"tool_calls": []any{parsed}}, span: cand})
				}
			}
			continue
		}
		// Also support a top-level JSON array of tool call objects: [{"name": "read", "arguments": {...}}]
		if (cand.whole || cand.fenced) && strings.HasPrefix(strings.TrimSpace(cand.raw), "[") {
			if arr, ok := unmarshalCandidateArray(cand.raw); ok {
				allCalls := true
				for _, elem := range arr {
					m, ok := elem.(map[string]any)
					if !ok || !looksLikeBareCall(m) {
						allCalls = false
						break
					}
					if allow != nil {
						if _, ok := allow[extractBareCallName(m)]; !ok {
							allCalls = false
							break
						}
					}
				}
				if allCalls {
					envelopes = append(envelopes, foundEnv{value: map[string]any{"tool_calls": arr}, span: cand})
				}
			}
		}
	}

	activeList := envelopes
	if len(activeList) == 0 && len(bares) > 0 {
		activeList = bares
	}
	var (
		found    *foundEnv
		foundIdx int
		calls    []Call
	)
	for idx := range activeList {
		c, cErr := normaliseCalls(activeList[idx].value, allow)
		if cErr != nil {
			return nil, cErr
		}
		if len(c) > 0 {
			found = &activeList[idx]
			foundIdx = idx
			calls = c
			break
		}
		if found == nil {
			found = &activeList[idx]
			foundIdx = idx
		}
	}
	if found == nil {
		// A genuinely truncated/malformed envelope means the reply contains an
		// UNBALANCED {"tool_calls": ... opener that never closes (no balanced
		// object could be produced by callCandidates). Prose that merely
		// MENTIONS the envelope inside inline code, a fenced example with
		// placeholders like [...] or <tool>, or a balanced but single-call
		// object that failed schema validation is NOT truncation — those keep
		// the "no tool call envelope" reason so callers can surface the text.
		if hasUnbalancedToolEnvelopeOpener(trimmed) {
			return nil, toolCallError("the model attempted a tool_calls JSON envelope that was malformed or truncated")
		}
		return &ParseResult{Reason: "no tool call envelope in the reply", Text: trimmed}, nil
	}

	if len(calls) > 0 && len(activeList) > foundIdx+1 {
		prevEnd := found.span.end
		for i := foundIdx + 1; i < len(activeList); i++ {
			next := activeList[i]
			if next.span.start < prevEnd {
				continue
			}
			if prevEnd <= len(trimmed) && next.span.start <= len(trimmed) {
				gap := trimmed[prevEnd:next.span.start]
				if !gapFenceRe.MatchString(gap) {
					break
				}
			}
			extra, extraErr := normaliseCalls(next.value, allow)
			if extraErr != nil {
				return nil, extraErr
			}
			if len(extra) > 0 {
				calls = append(calls, extra...)
				prevEnd = next.span.end
			}
		}
	}
	if len(calls) == 0 {
		return &ParseResult{Reason: "tool call envelope without calls", Text: trimmed}, nil
	}

	cutPos := found.span.start
	if cutPos > len(trimmed) {
		cutPos = len(trimmed)
	}
	before := strings.TrimSpace(trailingTagRe.ReplaceAllString(trimmed[:cutPos], ""))
	before = strings.TrimSpace(trailingEnvRe.ReplaceAllString(before, ""))

	return &ParseResult{
		Calls: calls,
		Text:  before,
	}, nil
}

// InterpretReply decides whether a model reply should be returned as tool_calls.
func InterpretReply(reply string, tools any, toolChoice any) (*InterpretResult, error) {
	contract, err := ValidateToolRequest(tools, toolChoice)
	if err != nil {
		return nil, err
	}
	if len(contract.Tools) == 0 || contract.ChoiceMode == "none" {
		return nil, nil
	}
	parsed, err := ParseToolReply(reply, contract.Tools)
	if err != nil {
		return nil, err
	}
	if len(parsed.Calls) > 0 {
		if contract.ChoiceMode == "function" && contract.ForcedFunction != "" {
			for _, c := range parsed.Calls {
				if c.Name != contract.ForcedFunction {
					return nil, toolCallError(fmt.Sprintf(`the request required %q but the reply also called %q`, contract.ForcedFunction, c.Name))
				}
			}
		}
		return &InterpretResult{Calls: parsed.Calls, Text: parsed.Text}, nil
	}
	if contract.ChoiceMode == "required" || contract.ChoiceMode == "function" {
		return nil, toolCallError(fmt.Sprintf("the model did not return the required tool call (%s)", parsed.Reason))
	}
	if IsToolProtocolRefusal(reply) {
		return nil, toolCallError(ToolProtocolRefusalError)
	}
	return nil, nil
}

// ExtractPreToolPreamble returns any text preceding a tool_calls JSON envelope
// and its opening markdown fence or XML tag.
func ExtractPreToolPreamble(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	loc := trailingEnvRe.FindStringIndex(trimmed)
	if len(loc) != 2 {
		return ""
	}
	before := strings.TrimSpace(trailingTagRe.ReplaceAllString(trimmed[:loc[0]], ""))
	before = strings.TrimSpace(trailingEnvRe.ReplaceAllString(before, ""))
	before = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(before, "💭"), "🧠"))
	if strings.HasSuffix(before, ")]") {
		before = strings.TrimSpace(strings.TrimSuffix(before, ")]"))
	}
	return before
}
func SafeStreamablePrefix(text string) string {
	if len(text) <= 320 {
		return ""
	}
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if trimmed == "" || trimmed[0] == '{' || trimmed[0] == '[' || trimmed[0] == '<' || strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "💭") || strings.HasPrefix(trimmed, "🧠") {
		return ""
	}
	if hasPotentialToolMarker(text) || IsToolProtocolRefusal(text) {
		return ""
	}
	safeEnd := len(text) - 140
	if idx := strings.Index(text, "[["); idx >= 0 && idx < safeEnd {
		safeEnd = idx
	}
	trimmedRight := strings.TrimRight(text, " \t\r\n")
	if len(trimmedRight) > 0 {
		lastCh := trimmedRight[len(trimmedRight)-1]
		if lastCh == '{' || lastCh == '[' || lastCh == '<' || strings.HasSuffix(trimmedRight, "```") {
			cut := len(trimmedRight) - 1
			if strings.HasSuffix(trimmedRight, "```") {
				cut = len(trimmedRight) - 3
			}
			if cut < safeEnd {
				safeEnd = cut
			}
		}
	}
	for safeEnd > 0 && !utf8.RuneStart(text[safeEnd]) {
		safeEnd--
	}
	if safeEnd <= 0 {
		return ""
	}
	return text[:safeEnd]
}
