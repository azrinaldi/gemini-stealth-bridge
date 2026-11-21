package gemini

import (
	"encoding/json"
	"fmt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// GeneratedImage represents a generated image returned inside a StreamGenerate frame.
type GeneratedImage struct {
	URL           string `json:"url"`
	DownloadURL   string `json:"downloadUrl,omitempty"`
	Title         string `json:"title,omitempty"`
	Alt           string `json:"alt,omitempty"`
	RevisedPrompt string `json:"revisedPrompt,omitempty"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	IsGIF         bool   `json:"isGif,omitempty"`
}

// GeneratedVideo represents a generated video returned inside a StreamGenerate or hNvQHb frame.
type GeneratedVideo struct {
	URL          string `json:"url"`
	StreamURL    string `json:"streamUrl,omitempty"`
	ThumbnailURL string `json:"thumbnailUrl,omitempty"`
	Title        string `json:"title,omitempty"`
	MimeType     string `json:"mimeType,omitempty"`
}

// Frame is a single parsed candidate frame from a wrb.fr envelope.
type Frame struct {
	RCID               string            `json:"rcid"`
	Text               string            `json:"text"`
	Thoughts           string            `json:"thoughts"`
	Done               bool              `json:"done"`
	AnsweredModelID    string            `json:"answeredModelId,omitempty"`
	AnsweredModelLabel string            `json:"answeredModelLabel,omitempty"`
	GeneratedImages    []GeneratedImage  `json:"generatedImages,omitempty"`
	GeneratedVideos    []GeneratedVideo  `json:"generatedVideos,omitempty"`
	GroundingSources   []GroundingSource `json:"groundingSources,omitempty"`
}

// LineParseResult holds the parsed contents of one StreamGenerate line.
type LineParseResult struct {
	CID                string
	RID                string
	ContextToken       string
	AnsweredModelID    string
	AnsweredModelLabel string
	AsyncVideoCID      string
	AsyncVideoStatus   string
	VideoToolActive    bool
	// Activity is the upstream's own display text for the current tool phase
	// (pj[6][0], e.g. "Searching the web"). It is forwarded verbatim; no label is
	// synthesized when the field is absent.
	Activity string
	// AsyncVideoRefCID is the conversation id named by an explicit reference
	// field (pj[45]), when upstream sent one. It is reported separately so the
	// stream can prefer it even when the reference and the video-tool signal
	// arrive in different envelopes, in either order.
	AsyncVideoRefCID string
	MediaToolActive  bool
	StreamEnd        bool
	Frames           []Frame
	GroundingSources []GroundingSource
}

var (
	// Google's canned apology sentences returned with HTTP 200 when a request is rejected mid-flight.
	geminiErrorReplies = []string{
		"sorry, something went wrong. please try your request again.",
		"sorry, something went wrong. please try again.",
		"something went wrong. please try your request again.",
		"an error occurred. please try again.",
		"i encountered an error doing what you asked.",
		"i encountered an error while doing what you asked.",
		"i ran into an error while doing what you asked.",
		"i'm a language model, and i'm not designed to do that.",
		"i am a language model, and i am not designed to do that.",
		"i'm just a language model, so i can't help with that.",
		"i am just a language model, so i cannot help with that.",
		"i'm not programmed to do that.",
		"i am not programmed to do that.",
		"i'm not able to help with that, as i'm only a language model.",
		"i am not able to help with that, as i am only a language model.",
		"i'm unable to help with that, as i'm only a language model and don't have the necessary information or abilities.",
		"i'm unable to help you with that, as i'm only a language model and don't have the necessary information or abilities.",
		"i can't help with that because i'm only a language model and don't have the necessary information or abilities.",
		"i cannot help with that because i am only a language model and do not have the necessary information or abilities.",
		"i can't help you with that because i'm only a language model and don't have the ability to understand and respond.",
		"i cannot help you with that because i am only a language model and do not have the ability to understand and respond.",
		"i'm still learning how to answer this question.",
		"saya adalah model bahasa, dan saya tidak dirancang untuk itu.",
		"saya hanya model bahasa, jadi saya tidak bisa membantu terkait hal itu.",
		"saya hanya model bahasa dan tidak bisa membantu anda untuk itu.",
		"saya hanya model bahasa dan tidak dapat membantu anda untuk itu.",
		"saya tidak bisa membantu untuk itu karena saya hanya model bahasa dan tidak punya informasi atau kemampuan yang diperlukan.",
		"saya tidak bisa membantu anda karena saya hanya model bahasa dan tidak mampu memahami dan merespons hal tersebut.",
		"saya tidak bisa membantu terkait hal itu karena saya hanya model bahasa.",
		"saya tidak dapat membantu untuk itu karena saya hanya model bahasa dan tidak punya informasi atau kemampuan yang diperlukan.",
		"saya tidak dapat membantu anda karena saya hanya model bahasa dan tidak mampu memahami dan merespons hal tersebut.",
		"saya tidak dapat membantu terkait hal itu karena saya hanya model bahasa.",
		"saya tidak diprogram untuk melakukan hal itu.",
		"saya tidak diprogram untuk membantu terkait hal itu.",
		"saya masih belajar untuk menjawab pertanyaan ini.",
		"saya masih belajar, jadi saat ini saya belum bisa membantu terkait hal itu.",
		"maaf, saya belum bisa membantu terkait hal itu.",
		"maaf, saya tidak bisa membantu terkait hal itu.",
		"maaf, ada yang salah. silakan coba lagi.",
		"maaf, terjadi kesalahan. silakan coba lagi.",
	}
	geminiErrorReplyAnywhere = []string{
		"i encountered an error doing what you asked",
		"i encountered an error while doing what you asked",
		"i ran into an error while doing what you asked",
		"i'm having a hard time fulfilling your request",
		"i am having a hard time fulfilling your request",
		"i seem to be encountering an error",
		"i seem to be having trouble",
		"saya adalah model bahasa, dan saya tidak dirancang untuk itu",
		"saya hanya model bahasa, jadi saya tidak bisa membantu",
		"saya hanya model bahasa dan tidak bisa membantu",
		"saya hanya model bahasa dan tidak dapat membantu",
		"karena saya hanya model bahasa dan tidak punya informasi atau kemampuan yang diperlukan",
		"karena saya hanya model bahasa dan tidak mampu memahami dan merespons hal tersebut",
		"saya tidak diprogram untuk melakukan hal itu",
		"i'm not programmed to do that",
		"only a language model and don't have the necessary information or abilities",
		"only a language model and don't have the ability to understand and respond",
	}
	geminiCannedRefusalRe = regexp.MustCompile(
		`(?i)^(?:maaf,\s*)?(?:` +
			`saya\s+(?:tidak\s+(?:bisa|dapat|mampu)\s+membantu[^\n]*karena\s+saya\s+(?:hanya|hanyalah|adalah)\s+model\s+bahasa|tidak\s+diprogram\s+untuk\s+(?:melakukan|membantu)\s+hal\s+(?:itu|tersebut)|(?:hanya|hanyalah|adalah)\s+model\s+bahasa[^\n]*tidak\s+(?:bisa|dapat|dirancang|mampu))|` +
			`(?:i'm|i\s+am)\s+(?:not\s+programmed\s+to\s+do\s+that|(?:unable|not\s+able)\s+to\s+help[^\n]*(?:as|because)\s+(?:i'm|i\s+am)\s+(?:only|just|a)\s+(?:a\s+)?language\s+model)|` +
			`i\s+(?:can't|cannot)\s+help[^\n]*because\s+(?:i'm|i\s+am)\s+(?:only|just|a)\s+(?:a\s+)?language\s+model` +
			`)[.!]?$`,
	)
	hex16Re             = regexp.MustCompile(`^[0-9a-f]{14,16}$`)
	googleUserContentRe = regexp.MustCompile(`^https://lh3\.googleusercontent\.com/`)
	imageSizeSuffixRe   = regexp.MustCompile(`=s\d+(-[a-z0-9]+)*$`)
	// Placeholder links Gemini leaves in reply text where a rendered attachment
	// belongs. The path can carry several segments - a web image search returns
	// `image_collection/image_retrieval/<id>` - so a single-segment pattern left
	// the placeholder visible in the answer. The real media hosts are
	// `lh3.googleusercontent.com` (handled separately), so a path on the bare
	// host is always a placeholder.
	cardContentRe                 = regexp.MustCompile(`(?i)^https?://googleusercontent\.com/card_content/\d+$`)
	googleGenContentPlaceholderRe = regexp.MustCompile(`(?i)https?://googleusercontent\.com/(?:[\w-]+/)+[\w-]+`)
	closedThinkBlockRe            = regexp.MustCompile("(?is)(?:<(think|thinking|thought)(?:\\s[^>]*)?>\\s*([\\s\\S]*?)</(?:think|thinking|thought)\\s*>|```(?:thinking|thought)\\b[^\\n]*\\s*([\\s\\S]*?)```)\\s*")
	codeEventBlockRe              = regexp.MustCompile("(?is)```[a-z0-9_+-]*\\?code_(?:reference|stdout|stderr)&code_event_index=\\d+\\s*([\\s\\S]*?)```\\s*")
	openThinkStartRe              = regexp.MustCompile("(?is)^\\s*(?:<(?:think|thinking|thought)(?:\\s[^>]*)?>|```(?:thinking|thought)\\b|```[a-z0-9_+-]*\\?code_(?:reference|stdout|stderr)&code_event_index=\\d+)[\\s\\S]*$")
	openThinkTagOnlyRe            = regexp.MustCompile("(?is)^\\s*(?:<(?:think|thinking|thought)(?:\\s[^>]*)?>|```(?:thinking|thought)\\b[^\\n]*|```[a-z0-9_+-]*\\?code_(?:reference|stdout|stderr)&code_event_index=\\d+)\\s*")
	trailingOpenThinkRe           = regexp.MustCompile("(?is)(?:<(?:think|thinking|thought)(?:\\s[^>]*)?>|```(?:thinking|thought)\\b[^\\n]*|```[a-z0-9_+-]*\\?code_(?:reference|stdout|stderr)&code_event_index=\\d+)([\\s\\S]*)$")
	thinkingProcessHeaderRe       = regexp.MustCompile(
		"(?is)^\\s*(?:#{1,6}\\s*)?(?:(?:💭|🧠)\\s*)?(?:\\*\\*|__)?" +
			"(?:thinking\\s+process|thought\\s+process|internal\\s+reasoning|thought|" +
			"思考过程|思考過程|思维链|思維鏈|推理过程|推理過程|内部推理|內部推理|思考|" +
			"思考プロセス|推論プロセス|" +
			"생각\\s*과정|추론\\s*과정|생각이|생각|" +
			"quá\\s+trình\\s+suy\\s+nghĩ)" +
			"(?:(?:\\*\\*|__)?\\s*[:：](?:\\*\\*|__)?[ \\t]*(?:\\r?\\n+)?|(?:\\*\\*|__)?[ \\t]*\\r?\\n+)",
	)
	thoughtBubbleHeaderRe         = regexp.MustCompile("(?s)^\\s*(?:💭(?:[ \\t]*\\r?\\n+|[ \\t]+)|🧠[ \\t]*\\r?\\n+)")
	jspbArraySplitRe              = regexp.MustCompile(`[ \t]*(?:'\s*\]\s*/\s*\[\s*'|"\s*\]\s*/\s*\[\s*")[ \t]*\r?\n*`)
	thoughtBubbleMetaSplitRe      = regexp.MustCompile("(?im)(?:^|\\n)\\s*(?:let's|we\\s+can|we\\s+should|now\\s+let's|now\\s+i\\s+can|i\\s+will|i'll)\\s+(?:write|present|provide|formulate|draft|give|output|send|reply|answer|respond|summarize|deliver|produce|keep|make|explain|break\\s+down|structure|lay\\s+out)\\b[^\\n]*?[.:][ \\t]*\\r?\\n*")
	trailingThoughtCloseBracketRe = regexp.MustCompile(`[ \t]*(?:\)\]|'\s*\]\s*/\s*\[\s*'|"\s*\]\s*/\s*\[\s*")[ \t]*$`)
	trailingPartialSeamRe         = regexp.MustCompile(`(?:[ \t]*['"\)]+[\]/\['"\)]*|[.!?*)][#*\s'"\)\]/\[]+)$`)
	fencedToolStartRe             = regexp.MustCompile("(?is)```(?:json|tool_call|tool_calls|function_call|function_calls)?\\s*(?:\\{\\s*\"\\s*(?:tool_calls|toolcalls|tool_call|function_call|function_calls|functioncalls|name|tool|tool_name|function)\\s*\"\\s*:|\\[\\s*\\{\\s*\"\\s*(?:name|tool|tool_name|function)\\s*\"\\s*:)")
	xmlToolStartRe                = regexp.MustCompile("(?is)<(?:tool_call|tool_calls|function_call|function_calls)\\b")
	bareToolEnvLineRe             = regexp.MustCompile("(?is)(?:^|\\r?\\n)[ \\t]*(\\{\\s*\"\\s*(?:tool_calls|toolcalls|tool_call|function_call|function_calls|functioncalls)\\s*\"\\s*:)")
	finalAnswerMarkerRe           = regexp.MustCompile(
		"(?im)^\\s*(?:#{1,6}\\s*)?(?:\\*\\*|__)?" +
			"(?:final\\s+answer|answer|response|最终答案|最終答案|最终回答|最終回答|回答|答案|최종\\s*답변|답변)" +
			"(?:\\*\\*|__)?\\s*[:：]\\s*",
	)
	harnessReminderRe = regexp.MustCompile(`(?is)^\s*\[(?:Harness|Tool) reminder:(?:[\s\S]*?block\.\]|[^\]\n]*\])\s*`)
	assistantPrefixRe = regexp.MustCompile(`(?i)^\s*Assistant:\s*`)
)

const (
	geminiErrorReplyMaxChars         = 200
	geminiErrorReplyAnywhereMaxChars = 400
)

// GeminiErrorReply returns the matched canned error sentence if text is one of
// Gemini Web's silent HTTP-200 failure replies, or "" if text is a genuine answer.
func GeminiErrorReply(text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return ""
	}
	lower := strings.ToLower(t)
	if len(t) <= geminiErrorReplyAnywhereMaxChars {
		for _, s := range geminiErrorReplyAnywhere {
			if strings.Contains(lower, s) {
				return s
			}
		}
	}
	if len(t) > geminiErrorReplyMaxChars {
		return ""
	}
	for _, s := range geminiErrorReplies {
		bare := strings.TrimSuffix(s, ".")
		if lower == s || lower == bare || strings.HasPrefix(lower, bare) {
			return s
		}
	}
	if geminiCannedRefusalRe.MatchString(t) {
		return lower
	}
	return ""
}

func asciiHasPrefixFold(s, lowerPrefix string) bool {
	if len(s) < len(lowerPrefix) {
		return false
	}
	for i := 0; i < len(lowerPrefix); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lowerPrefix[i] {
			return false
		}
	}
	return true
}

func mayContainInlineThinkTag(s string) bool {
	for off := 0; off < len(s); {
		idxLt := strings.IndexByte(s[off:], '<')
		idxBt := strings.Index(s[off:], "```")
		next := -1
		isFence := false
		if idxLt >= 0 && (idxBt < 0 || idxLt <= idxBt) {
			next = off + idxLt
		} else if idxBt >= 0 {
			next = off + idxBt
			isFence = true
		}
		if next < 0 {
			return false
		}
		rest := s[next+1:]
		if isFence {
			rest = s[next+3:]
		}
		if asciiHasPrefixFold(rest, "think") || asciiHasPrefixFold(rest, "thought") {
			return true
		}
		if isFence {
			off = next + 3
		} else {
			off = next + 1
		}
	}
	return false
}

func hasBareToolEnvelopeKey(s string) bool {
	for i := 0; i+8 <= len(s); i++ {
		c := s[i]
		if c == 't' || c == 'T' {
			if asciiHasPrefixFold(s[i:], "tool_call") || asciiHasPrefixFold(s[i:], "toolcall") {
				return true
			}
		} else if c == 'f' || c == 'F' {
			if asciiHasPrefixFold(s[i:], "function_call") || asciiHasPrefixFold(s[i:], "functioncall") {
				return true
			}
		}
	}
	return false
}

func utf8SafeSlice(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

func utf8SafeLowerHead(s string, maxRunes int) string {
	count := 0
	for idx := range s {
		if count >= maxRunes {
			return strings.ToLower(s[:idx])
		}
		count++
	}
	return strings.ToLower(s)
}

// SanitizeCandidateTextAndThoughts strips any inline <think>/<thinking>/<thought>
// blocks, inline thought-bubble monologues (💭), or echoed prompt markers from
// candidate text and merges extracted inline thoughts into the thoughts channel
// so reasoning never leaks into or duplicates chat content.
func SanitizeCandidateTextAndThoughts(text, thoughts string, done bool) (string, string) {
	return sanitizeCandidateTextAndThoughts(text, thoughts, done)
}

func sanitizeCandidateTextAndThoughts(text, thoughts string, done bool) (string, string) {
	if text == "" {
		return "", thoughts
	}

	var extraThoughts []string
	appendThought := func(t string) {
		t = strings.TrimSpace(t)
		if t == "" {
			return
		}
		if thoughts != "" && strings.Contains(thoughts, t) {
			return
		}
		extraThoughts = append(extraThoughts, t)
	}

	hasThinkTag := mayContainInlineThinkTag(text)
	hasCodeTag := strings.Contains(text, "?code_")

	// 1. Extract and strip any closed <think>...</think> / <thinking>...</thinking> / ```thinking...``` blocks
	//    as well as Gemini Web's internal ```python?code_reference&code_event_index=N``` sandbox trace blocks.
	if hasThinkTag && closedThinkBlockRe.MatchString(text) {
		matches := closedThinkBlockRe.FindAllStringSubmatch(text, -1)
		for _, m := range matches {
			if len(m) > 2 && m[2] != "" {
				appendThought(m[2])
			} else if len(m) > 3 && m[3] != "" {
				appendThought(m[3])
			}
		}
		text = closedThinkBlockRe.ReplaceAllString(text, "")
		hasThinkTag = mayContainInlineThinkTag(text)
	}
	if hasCodeTag && codeEventBlockRe.MatchString(text) {
		matches := codeEventBlockRe.FindAllStringSubmatch(text, -1)
		for _, m := range matches {
			if len(m) > 1 && m[1] != "" {
				appendThought(m[1])
			}
		}
		text = codeEventBlockRe.ReplaceAllString(text, "")
		hasCodeTag = strings.Contains(text, "?code_")
	}

	trimmed := strings.TrimLeft(text, " \t\r\n")
	lowerTrimmed := utf8SafeLowerHead(trimmed, 48)

	// 2. If streaming (!done), hold back partial opening prefixes of <think>, <thinking>, <thought>, ```python?code_, [Harness reminder:, Assistant:, or multilingual thinking headers
	if !done && trimmed != "" && utf8.RuneCountInString(trimmed) < 32 && utf8.ValidString(lowerTrimmed) {
		for _, prefix := range []string{
			"<think>", "<thinking>", "<thought>",
			"```thinking", "```thought", "```python?code_", "```text?code_",
			"thinking process:", "**thinking process", "### thinking process",
			"thought process:", "**thought process", "### thought process",
			"internal reasoning:", "**internal reasoning",
			"思考过程：", "思考过程:", "**思考过程", "### 思考过程",
			"思考過程：", "思维链：", "推理过程：", "思考：",
			"思考プロセス：", "생각 과정:", "생각이:", "생각:", "💭", "🧠",
			"[harness reminder:", "[tool reminder:", "assistant:",
		} {
			if len(lowerTrimmed) < len(prefix) && strings.HasPrefix(prefix, lowerTrimmed) {
				return "", joinThoughts(thoughts, extraThoughts)
			}
		}
	}

	// 3. Handle echoed [Harness reminder: ... at start
	if strings.HasPrefix(lowerTrimmed, "[harness reminder:") || strings.HasPrefix(lowerTrimmed, "[tool reminder:") {
		if harnessReminderRe.MatchString(text) {
			text = harnessReminderRe.ReplaceAllString(text, "")
			trimmed = strings.TrimLeft(text, " \t\r\n")
			lowerTrimmed = utf8SafeLowerHead(trimmed, 48)
		} else if !done {
			return "", joinThoughts(thoughts, extraThoughts)
		} else {
			// Strip unclosed/truncated reminder line on final frame so scaffolding never leaks.
			if nl := strings.IndexByte(trimmed, '\n'); nl >= 0 {
				text = strings.TrimLeft(trimmed[nl+1:], " \t\r\n")
			} else {
				text = ""
			}
			trimmed = strings.TrimLeft(text, " \t\r\n")
			lowerTrimmed = utf8SafeLowerHead(trimmed, 48)
		}
	}

	// 4. Strip echoed leading "Assistant:" prefix
	if strings.HasPrefix(lowerTrimmed, "assistant:") {
		text = assistantPrefixRe.ReplaceAllString(text, "")
	}

	// 5. Handle unclosed <think> / <thinking> / <thought> block at start or tail (even if done=true)
	if hasThinkTag || hasCodeTag {
		if openThinkStartRe.MatchString(text) {
			// An OPEN think marker at the very start of the text. If the whole
			// remainder looks like think content (or the stream is mid-flight),
			// convert it to thoughts. But a literal marker embedded in prose
			// followed by real answer sentences is NOT a think block: keep it
			// intact on a final frame.
			inner := openThinkTagOnlyRe.ReplaceAllString(text, "")
			if done && inner != "" {
				// Heuristic: on a final frame the model usually answers in
				// paragraphs. A genuine unclosed think block that swallowed the
				// whole answer is rare; treat the text as content if it has
				// sentence-like prose after the marker's first line.
				if firstNL := strings.IndexByte(inner, '\n'); firstNL >= 0 && strings.TrimSpace(inner[firstNL+1:]) != "" {
					return text, joinThoughts(thoughts, extraThoughts)
				}
			}
			appendThought(inner)
			return "", joinThoughts(thoughts, extraThoughts)
		}
		if loc := trailingOpenThinkRe.FindStringSubmatchIndex(text); len(loc) >= 4 {
			// A literal </think>/<thinking>/<thought> embedded mid-sentence in prose (a line
			// ABOUT think tags, a mermaid label, etc.) must NOT swallow the rest
			// of the answer. A genuine unclosed think block sits at the very tail
			// of the reply with no further prose AFTER the tag's line.
			rem := text[loc[0]:loc[3]]
			nextNL := strings.IndexByte(rem, '\n')
			proseAfterTagLine := nextNL >= 0 && strings.TrimSpace(rem[nextNL+1:]) != ""
			if !done || !proseAfterTagLine {
				inner := text[loc[2]:loc[3]]
				appendThought(inner)
				text = strings.TrimRight(text[:loc[0]], " \t\r\n")
			}
		}
	}

	// 6. Handle inline "Thinking Process:" / "Thought Process:" blocks emitted in text instead of c[37].
	if restText, extractedThought, ok := extractThinkingProcessBlock(text, done); ok {
		appendThought(extractedThought)
		text = restText
	}

	return text, joinThoughts(thoughts, extraThoughts)
}

func findInlineToolBlockStart(s string) int {
	best := -1
	if strings.Contains(s, "```") {
		if loc := fencedToolStartRe.FindStringIndex(s); len(loc) == 2 {
			best = loc[0]
		}
	}
	if strings.Contains(s, "<") {
		if loc := xmlToolStartRe.FindStringIndex(s); len(loc) == 2 && (best < 0 || loc[0] < best) {
			best = loc[0]
		}
	}
	if strings.Contains(s, "{") && hasBareToolEnvelopeKey(s) {
		if loc := bareToolEnvLineRe.FindStringSubmatchIndex(s); len(loc) >= 4 && loc[2] >= 0 && (best < 0 || loc[2] < best) {
			best = loc[2]
		}
	}
	return best
}

func cleanInlineThoughtText(s string) string {
	s = strings.TrimSpace(s)
	if loc := thoughtBubbleHeaderRe.FindStringIndex(s); len(loc) == 2 && loc[0] == 0 {
		s = strings.TrimSpace(s[loc[1]:])
	} else {
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "💭"), "🧠"))
	}
	s = strings.TrimSpace(trailingThoughtCloseBracketRe.ReplaceAllString(s, ""))
	return s
}

func isAnswerStartEmoji(r rune) bool {
	if r == '💭' || r == '🧠' {
		return false
	}
	return (r >= 0x1F300 && r <= 0x1FAFF) || (r >= 0x2600 && r <= 0x27BF)
}

// findGluedThoughtCloseSeam detects the zero-whitespace seam left when Gemini Web's
// detokenizer strips <thought_close> between the end of an inline thought sentence
// (ending with ., !, ?, *, ), or ]) and the first token of the answer (a markdown
// heading like "### 1." or a leading reaction emoji like "📊", "👍", "🧭", "🎯", "✨", "🔧", "✅").
func findGluedThoughtCloseSeam(body string) int {
	inBackticks := false
	for i := range len(body) {
		if body[i] == '`' {
			inBackticks = !inBackticks
			continue
		}
		if inBackticks || i < 12 {
			continue
		}
		ch := body[i]
		if ch != '.' && ch != '!' && ch != '?' && ch != '*' && ch != ')' && ch != ']' {
			continue
		}
		rest := body[i+1:]
		if rest == "" {
			break
		}
		if rest[0] == '#' {
			hashes := 0
			for hashes < len(rest) && hashes < 6 && rest[hashes] == '#' {
				hashes++
			}
			if hashes > 0 && hashes < len(rest) && (rest[hashes] == ' ' || rest[hashes] == '\t') {
				afterSpace := strings.TrimLeft(rest[hashes:], " \t")
				if afterSpace != "" && afterSpace[0] != '\n' && afterSpace[0] != '\r' {
					return i + 1
				}
			}
			continue
		}
		if rest[0] >= 0x80 {
			r, width := utf8.DecodeRuneInString(rest)
			if r != utf8.RuneError && isAnswerStartEmoji(r) && len(strings.TrimSpace(rest[width:])) > 0 {
				return i + 1
			}
		}
	}
	return -1
}

func startsLikeThoughtContinuation(s string) bool {
	lower := strings.ToLower(strings.TrimLeft(s, " \t\r\n"))
	for _, prefix := range []string{
		"wait—", "wait -", "wait,", "wait:", "wait ",
		"let's ", "let me ", "actually,", "actually ",
		"hmm,", "hmm ", "hold on", "first, let's", "but wait",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func splitInlineThoughtAnswer(body string) (thoughtPart string, answerPart string, ok bool) {
	if ansLoc := finalAnswerMarkerRe.FindStringIndex(body); len(ansLoc) == 2 {
		ans := strings.TrimSpace(body[ansLoc[1]:])
		if ans != "" {
			return cleanInlineThoughtText(body[:ansLoc[0]]), ans, true
		}
	}
	if jspbLoc := jspbArraySplitRe.FindStringIndex(body); len(jspbLoc) == 2 {
		ans := strings.TrimSpace(body[jspbLoc[1]:])
		if ans != "" && !strings.HasPrefix(ans, "```") && !strings.HasPrefix(ans, "{") {
			return cleanInlineThoughtText(body[:jspbLoc[0]]), ans, true
		}
	}
	if idx := strings.Index(body, ")]"); idx >= 0 {
		after := strings.TrimSpace(body[idx+2:])
		if after != "" && !strings.HasPrefix(after, "```") && !strings.HasPrefix(after, "{") {
			return cleanInlineThoughtText(body[:idx]), after, true
		}
	}
	if seam := findGluedThoughtCloseSeam(body); seam > 0 {
		ans := strings.TrimSpace(body[seam:])
		if ans != "" {
			return cleanInlineThoughtText(body[:seam]), ans, true
		}
	}
	if locs := thoughtBubbleMetaSplitRe.FindAllStringIndex(body, -1); len(locs) > 0 {
		for i := len(locs) - 1; i >= 0; i-- {
			cand := locs[i]
			matchedSpan := strings.TrimRight(body[cand[0]:cand[1]], " \t\r\n")
			// A meta-sentence ending with a colon (e.g. "Let's structure the answer:")
			// introduces an outline inside the thought block unless the answer is glued
			// on the very same line.
			if strings.HasSuffix(matchedSpan, ":") && strings.ContainsAny(body[cand[0]:cand[1]], "\r\n") {
				continue
			}
			ans := strings.TrimSpace(body[cand[1]:])
			if ans != "" && !startsLikeThoughtContinuation(ans) {
				return cleanInlineThoughtText(body[:cand[1]]), ans, true
			}
		}
	}
	return "", "", false
}

func holdBackTrailingToolOpener(s string) string {
	if strings.Count(s, "```")%2 == 1 {
		if lastFence := strings.LastIndex(s, "```"); lastFence >= 0 && len(s)-lastFence <= 256 {
			cut := lastFence
			for cut > 0 && (s[cut-1] == ' ' || s[cut-1] == '\t') {
				cut--
			}
			return strings.TrimRight(s[:cut], " \t\r\n")
		}
	}
	trimmedRight := strings.TrimRight(s, " \t\r\n")
	lastNL := strings.LastIndexByte(trimmedRight, '\n')
	lineStart := 0
	if lastNL >= 0 {
		lineStart = lastNL + 1
	}
	tailLine := strings.TrimLeft(trimmedRight[lineStart:], " \t\r")
	if tailLine != "" && (tailLine[0] == '`' || tailLine[0] == '<' || tailLine[0] == '{' ||
		strings.HasPrefix(strings.ToLower(tailLine), "let's ") ||
		strings.HasPrefix(strings.ToLower(tailLine), "we can ") ||
		strings.HasPrefix(strings.ToLower(tailLine), "we should ") ||
		strings.HasPrefix(strings.ToLower(tailLine), "now let's ") ||
		strings.HasPrefix(strings.ToLower(tailLine), "now i can ") ||
		strings.HasPrefix(strings.ToLower(tailLine), "i will ") ||
		strings.HasPrefix(strings.ToLower(tailLine), "i'll ")) {
		return strings.TrimRight(trimmedRight[:lineStart], " \t\r\n")
	}
	if loc := trailingPartialSeamRe.FindStringIndex(trimmedRight); len(loc) == 2 {
		if trimmedRight[loc[0]] == '.' || trimmedRight[loc[0]] == '!' || trimmedRight[loc[0]] == '?' || trimmedRight[loc[0]] == '*' || trimmedRight[loc[0]] == ')' || trimmedRight[loc[0]] == ']' {
			if loc[0] > 0 && trimmedRight[loc[0]-1] != '(' && trimmedRight[loc[0]-1] != '\'' && trimmedRight[loc[0]-1] != '"' {
				return strings.TrimRight(trimmedRight[:loc[0]+1], " \t\r\n")
			}
		}
		return strings.TrimRight(trimmedRight[:loc[0]], " \t\r\n")
	}
	return trimmedRight
}

func mayStartWithThinkingHeader(trimmed string) bool {
	r, _ := utf8.DecodeRuneInString(trimmed)
	if r == utf8.RuneError {
		return false
	}
	switch r {
	case '#', '*', '_', 't', 'T', 'i', 'I', 'q', 'Q', '💭', '🧠', '思', '推', '内', '內', '생', '추':
		return true
	default:
		return false
	}
}

func extractThinkingProcessBlock(text string, done bool) (restText string, thought string, ok bool) {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if trimmed == "" {
		return text, "", false
	}
	if !mayStartWithThinkingHeader(trimmed) {
		if toolStart := findInlineToolBlockStart(text); toolStart > 0 {
			before := strings.TrimSpace(text[:toolStart])
			if strings.HasSuffix(before, ")]") {
				return strings.TrimLeft(text[toolStart:], " \t\r\n"), cleanInlineThoughtText(before), true
			}
		}
		return text, "", false
	}
	wsPrefixLen := len(text) - len(trimmed)
	headSlice := utf8SafeSlice(text, wsPrefixLen+240)
	if hdrLoc := thinkingProcessHeaderRe.FindStringIndex(headSlice); len(hdrLoc) == 2 {
		body := text[hdrLoc[1]:]
		if toolStart := findInlineToolBlockStart(body); toolStart >= 0 {
			return strings.TrimLeft(body[toolStart:], " \t\r\n"), cleanInlineThoughtText(body[:toolStart]), true
		}
		if thoughtPart, ansPart, hasSplit := splitInlineThoughtAnswer(body); hasSplit {
			return ansPart, thoughtPart, true
		}
		if !done {
			safe := holdBackTrailingToolOpener(body)
			return "", cleanInlineThoughtText(safe), true
		}
		return "", cleanInlineThoughtText(body), true
	}
	if hdrLoc := thoughtBubbleHeaderRe.FindStringIndex(headSlice); len(hdrLoc) == 2 {
		body := text[hdrLoc[1]:]
		if toolStart := findInlineToolBlockStart(body); toolStart >= 0 {
			return strings.TrimLeft(body[toolStart:], " \t\r\n"), cleanInlineThoughtText(body[:toolStart]), true
		}
		if thoughtPart, ansPart, hasSplit := splitInlineThoughtAnswer(body); hasSplit {
			return ansPart, thoughtPart, true
		}
		if !done {
			safe := holdBackTrailingToolOpener(body)
			return "", cleanInlineThoughtText(safe), true
		}
		return "", cleanInlineThoughtText(body), true
	}
	return text, "", false
}
func joinThoughts(base string, extras []string) string {
	if len(extras) == 0 {
		return base
	}
	ext := strings.Join(extras, "\n\n")
	if strings.TrimSpace(base) == "" {
		return ext
	}
	if strings.HasSuffix(base, "\n\n") {
		return base + ext
	}
	if strings.HasSuffix(base, "\n") {
		return base + "\n" + ext
	}
	return base + "\n\n" + ext
}

// jspbField reads a 0-based index from a JSPB array, supporting both dense
// positional slots and the trailing sparse dictionary object at arr[len-1]
// (where keys are 1-based field numbers).
func jspbField(arr []any, index int) any {
	if len(arr) == 0 {
		return nil
	}
	if index >= 0 && index < len(arr) && arr[index] != nil {
		val := arr[index]
		if index < len(arr)-1 {
			return val
		}
		if _, isMap := val.(map[string]any); !isMap {
			return val
		}
	}
	if lastMap, ok := arr[len(arr)-1].(map[string]any); ok {
		return lastMap[strconv.Itoa(index+1)]
	}
	return nil
}

// ParseFrameLine parses a single line from a StreamGenerate response into
// {CID, RID, ContextToken, AnsweredModelID, AnsweredModelLabel, Frames}.
func ParseFrameLine(line string) (*LineParseResult, error) {
	s := strings.TrimSpace(line)
	if s == "" || s == ")]}'" || s == ")]}" || isDigits(s) {
		return nil, nil
	}
	var outer []any
	if err := json.Unmarshal([]byte(s), &outer); err != nil {
		return nil, transport.NewUpstreamError("malformed upstream stream frame", 502, "upstream_parse_error", nil, err)
	}

	res := &LineParseResult{}
	for _, partAny := range outer {
		part, ok := partAny.([]any)
		if !ok || len(part) == 0 {
			continue
		}
		tag, _ := part[0].(string)
		if tag == "er" {
			code := extractErrCode(part)
			return nil, classifyRPCErrorCode(code, part)
		}
		if tag == "e" || tag == "di" || tag == "af.httprm" {
			res.StreamEnd = true
			continue
		}
		if tag != "wrb.fr" {
			continue
		}

		if err := checkErrorExtension(part); err != nil {
			return nil, err
		}

		if len(part) <= 2 || part[2] == nil {
			continue
		}
		innerStr, ok := part[2].(string)
		if !ok {
			return nil, transport.NewUpstreamError("invalid upstream frame payload", 502, "upstream_parse_error", nil, nil)
		}

		var pj []any
		if err := json.Unmarshal([]byte(innerStr), &pj); err != nil {
			return nil, transport.NewUpstreamError("malformed upstream frame payload", 502, "upstream_parse_error", nil, err)
		}

		if len(pj) > 1 {
			if ids, ok := pj[1].([]any); ok {
				if len(ids) > 0 {
					if cid, ok := ids[0].(string); ok && cid != "" {
						res.CID = cid
					}
				}
				if len(ids) > 1 {
					if rid, ok := ids[1].(string); ok && rid != "" {
						res.RID = rid
					}
				}
			}
		}

		if rawCtx, ok := jspbField(pj, 25).(string); ok && rawCtx != "" {
			res.ContextToken = rawCtx
		}

		var answeredID, answeredLabel string
		if rawID, ok := jspbField(pj, 39).(string); ok && hex16Re.MatchString(rawID) {
			answeredID = rawID
			if rawLabel, ok := jspbField(pj, 42).(string); ok && rawLabel != "" {
				answeredLabel = rawLabel
			}
			res.AnsweredModelID = answeredID
			res.AnsweredModelLabel = answeredLabel
		}

		// pj[6] ("7" in the 1-based sparse dictionary) carries the current tool
		// phase: [displayText, [toolName, [.., .., label]]]. The display text is
		// upstream's own wording for what it is doing, so it is surfaced verbatim
		// and never synthesized locally.
		if toolStatus, ok := jspbField(pj, 6).([]any); ok && len(toolStatus) > 0 {
			if activity, _ := toolStatus[0].(string); activity != "" {
				res.Activity = activity
			}
			if len(toolStatus) >= 2 {
				if toolMeta, ok := toolStatus[1].([]any); ok && len(toolMeta) > 0 {
					toolName, _ := toolMeta[0].(string)
					if toolName == "video_generation" {
						res.VideoToolActive = true
						res.MediaToolActive = true
						if res.CID != "" {
							res.AsyncVideoCID = res.CID
						}
						if msg, _ := toolStatus[0].(string); msg != "" {
							res.AsyncVideoStatus = msg
						}
					} else if len(toolMeta) > 1 {
						if labelArr, ok := toolMeta[1].([]any); ok && len(labelArr) > 2 {
							if lbl, _ := labelArr[2].(string); strings.Contains(strings.ToLower(lbl), "image") {
								res.MediaToolActive = true
							}
						}
					}
				}
			}
		}
		if res.MediaToolActive {
			answeredID = ""
			answeredLabel = ""
			res.AnsweredModelID = ""
			res.AnsweredModelLabel = ""
		}
		// pj[45] is an explicit reference to the conversation that owns the async
		// media. It is reported even when the video-tool signal arrived in another
		// envelope, so the stream can prefer this CID over the ambient one.
		if asyncRef, ok := jspbField(pj, 45).([]any); ok && len(asyncRef) > 0 {
			if refCID, _ := asyncRef[0].(string); strings.HasPrefix(refCID, "c_") {
				res.AsyncVideoRefCID = refCID
				if res.AsyncVideoCID != "" {
					res.AsyncVideoCID = refCID
				}
			}
		}

		if len(pj) > 4 {
			if cands, ok := pj[4].([]any); ok {
				for _, cAny := range cands {
					c, ok := cAny.([]any)
					if !ok {
						continue
					}
					if frame, ok := parseCandidateSlice(c, answeredID, answeredLabel); ok {
						if len(frame.GeneratedImages) > 0 || len(frame.GeneratedVideos) > 0 {
							res.MediaToolActive = true
							res.AnsweredModelID = ""
							res.AnsweredModelLabel = ""
						}
						res.GroundingSources = MergeGroundingSources(res.GroundingSources, frame.GroundingSources)
						res.Frames = append(res.Frames, frame)
					}
				}
			}
		}
	}
	return res, nil
}

// BuildContinuationMetadata constructs the 10-element Gemini Web session continuation array.
func BuildContinuationMetadata(cid, rid, rcid, contextToken string) []any {
	if cid == "" {
		return nil
	}
	return []any{cid, rid, rcid, nil, nil, nil, nil, nil, nil, contextToken}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func jspbSubField(parent any, index int) any {
	if parent == nil {
		return nil
	}
	if arr, ok := parent.([]any); ok {
		return jspbField(arr, index)
	}
	if m, ok := parent.(map[string]any); ok {
		return m[strconv.Itoa(index+1)]
	}
	return nil
}

func parseCandidateSlice(c []any, answeredID, answeredLabel string) (Frame, bool) {
	var rcid string
	if len(c) > 0 {
		rcid, _ = c[0].(string)
	}
	var text string
	if len(c) > 1 {
		if tArr, ok := c[1].([]any); ok && len(tArr) > 0 {
			text, _ = tArr[0].(string)
		}
	}
	// A card reply puts nothing but a card_content link in the text slot; the
	// actual answer sits in field 22. Stripping the link alone would deliver an
	// empty answer, so the card body replaces it. When field 22 is empty the link
	// is kept: a visible link the client can follow beats a silently empty reply,
	// and the caller already treats an empty candidate as a retryable failure.
	keepCardLink := false
	if cardContentRe.MatchString(strings.TrimSpace(text)) {
		if alt, ok := jspbField(c, 22).([]any); ok && len(alt) > 0 {
			if s, _ := alt[0].(string); strings.TrimSpace(s) != "" {
				text = s
			} else {
				keepCardLink = true
			}
		} else {
			keepCardLink = true
		}
	}
	if !keepCardLink && googleGenContentPlaceholderRe.MatchString(text) {
		text = strings.TrimSpace(googleGenContentPlaceholderRe.ReplaceAllString(text, ""))
	}

	var thoughts string
	if tf, ok := jspbField(c, 37).([]any); ok && len(tf) > 0 {
		if tf0, ok := tf[0].([]any); ok && len(tf0) > 0 {
			thoughts, _ = tf0[0].(string)
		}
	}

	done := false
	if len(c) > 8 {
		if stArr, ok := c[8].([]any); ok && len(stArr) > 0 {
			if f, ok := stArr[0].(float64); ok && int(f) == 2 {
				done = true
			}
		}
	}

	images := extractGeneratedImages(c)
	videos := extractGeneratedVideos(c)
	if len(images) > 0 || len(videos) > 0 {
		answeredID = ""
		answeredLabel = ""
	}
	text, thoughts = sanitizeCandidateTextAndThoughts(text, thoughts, done)

	grounding := extractGroundingSources(c)

	var mdParts []string
	for _, img := range images {
		if strings.Contains(text, img.URL) {
			continue
		}
		alt := img.Title
		if alt == "" {
			alt = img.Alt
		}
		if alt == "" {
			alt = "Generated Image"
		}
		mdParts = append(mdParts, fmt.Sprintf("![%s](%s)\n\n[Download Image (%s)](%s)", alt, img.URL, alt, img.URL))
	}
	for _, vid := range videos {
		if strings.Contains(text, vid.URL) {
			continue
		}
		title := vid.Title
		if title == "" {
			title = "Generated Video"
		}
		if vid.ThumbnailURL != "" {
			mdParts = append(mdParts, fmt.Sprintf("[![%s](%s)](%s)\n\n[Download Video (%s)](%s)", title, vid.ThumbnailURL, vid.URL, title, vid.URL))
		} else {
			mdParts = append(mdParts, fmt.Sprintf("[Download Video (%s)](%s)", title, vid.URL))
		}
	}
	if len(mdParts) > 0 {
		joined := strings.Join(mdParts, "\n\n")
		if strings.TrimSpace(text) == "" {
			text = joined
		} else {
			text = strings.TrimSpace(text) + "\n\n" + joined
		}
	}

	return Frame{
		RCID:               rcid,
		Text:               text,
		Thoughts:           thoughts,
		Done:               done,
		AnsweredModelID:    answeredID,
		AnsweredModelLabel: answeredLabel,
		GeneratedImages:    images,
		GeneratedVideos:    videos,
		GroundingSources:   grounding,
	}, true
}

// ParseConversationPollResponse parses an hNvQHb batchexecute response payload
// when polling for an asynchronous video generation turn.
func ParseConversationPollResponse(body string) (*LineParseResult, error) {
	for _, rawLine := range strings.Split(body, "\n") {
		s := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(s, "[[") || !strings.Contains(s, "hNvQHb") {
			continue
		}
		var outer []any
		if err := json.Unmarshal([]byte(s), &outer); err != nil {
			continue
		}
		for _, partAny := range outer {
			part, ok := partAny.([]any)
			if !ok || len(part) < 3 {
				continue
			}
			tag, _ := part[0].(string)
			rpcID, _ := part[1].(string)
			if tag != "wrb.fr" || rpcID != "hNvQHb" {
				continue
			}
			innerStr, ok := part[2].(string)
			if !ok || innerStr == "" {
				return nil, fmt.Errorf("conversation not found or empty in hNvQHb")
			}
			var inner []any
			if err := json.Unmarshal([]byte(innerStr), &inner); err != nil || len(inner) == 0 {
				continue
			}
			turns, ok := inner[0].([]any)
			if !ok || len(turns) == 0 {
				continue
			}
			turn, ok := turns[0].([]any)
			if !ok || len(turn) <= 3 {
				continue
			}
			res := &LineParseResult{}
			if ids, ok := turn[0].([]any); ok {
				if len(ids) > 0 {
					res.CID, _ = ids[0].(string)
				}
				if len(ids) > 1 {
					res.RID, _ = ids[1].(string)
				}
			}
			if candWrap, ok := turn[3].([]any); ok && len(candWrap) > 0 {
				if cands, ok := candWrap[0].([]any); ok {
					for _, cAny := range cands {
						if c, ok := cAny.([]any); ok {
							if frame, ok := parseCandidateSlice(c, "", ""); ok {
								res.Frames = append(res.Frames, frame)
							}
						}
					}
				}
			}
			if len(res.Frames) > 0 {
				return res, nil
			}
		}
	}
	return nil, nil
}

func extractGeneratedImages(c []any) []GeneratedImage {
	c12 := jspbField(c, 12)
	if c12 == nil {
		return nil
	}
	c12_7, ok := jspbSubField(c12, 7).([]any)
	if !ok || len(c12_7) == 0 {
		return nil
	}
	items, ok := c12_7[0].([]any)
	if !ok {
		return nil
	}
	var images []GeneratedImage
	for _, itemAny := range items {
		item, ok := itemAny.([]any)
		if !ok || len(item) == 0 {
			continue
		}
		sub0, ok := item[0].([]any)
		if !ok || len(sub0) <= 3 {
			continue
		}
		sub0_3, ok := sub0[3].([]any)
		if !ok || len(sub0_3) <= 3 {
			continue
		}
		rawURL, _ := sub0_3[3].(string)
		if !googleUserContentRe.MatchString(rawURL) {
			continue
		}
		baseURL := imageSizeSuffixRe.ReplaceAllString(rawURL, "")
		fullURL := baseURL + "=s0-d"
		var title, alt string
		var width, height int
		if len(sub0_3) > 2 {
			if w, ok := sub0_3[1].(float64); ok && w > 0 {
				width = int(w)
			}
			if h, ok := sub0_3[2].(float64); ok && h > 0 {
				height = int(h)
			}
		}
		if len(item) > 3 {
			if meta, ok := item[3].([]any); ok {
				if len(meta) > 6 {
					title, _ = meta[6].(string)
				}
				if len(meta) > 5 {
					if altArr, ok := meta[5].([]any); ok && len(altArr) > 0 {
						alt, _ = altArr[0].(string)
					}
				}
			}
		}
		images = append(images, GeneratedImage{
			URL:           fullURL,
			DownloadURL:   baseURL + "=s0-d",
			Title:         title,
			Alt:           alt,
			RevisedPrompt: alt,
			Width:         width,
			Height:        height,
			IsGIF:         strings.HasSuffix(strings.ToLower(baseURL), ".gif"),
		})
	}
	return images
}

func extractGeneratedVideos(c []any) []GeneratedVideo {
	c12 := jspbField(c, 12)
	if c12 == nil {
		return nil
	}
	v60 := jspbSubField(c12, 59)
	if v60 == nil {
		return nil
	}
	var videos []GeneratedVideo
	seen := make(map[string]bool)
	var walk func(node any)
	walk = func(node any) {
		arr, ok := node.([]any)
		if !ok {
			return
		}
		if len(arr) > 7 {
			fname, _ := arr[2].(string)
			mime := ""
			if len(arr) > 11 {
				mime, _ = arr[11].(string)
			}
			if urls, ok := arr[7].([]any); ok && len(urls) >= 2 && (strings.HasSuffix(fname, ".mp4") || strings.Contains(mime, "video/")) {
				thumbURL, _ := urls[0].(string)
				downloadURL, _ := urls[1].(string)
				streamURL := ""
				if len(urls) > 2 {
					streamURL, _ = urls[2].(string)
				}
				if _, ok := mediaURL(downloadURL, nil); !ok {
					downloadURL = ""
				}
				if _, ok := mediaURL(streamURL, nil); !ok {
					streamURL = ""
				}
				if _, ok := mediaURL(thumbURL, nil); !ok {
					thumbURL = ""
				}
				primary := downloadURL
				if primary == "" {
					primary = streamURL
				}
				if primary != "" && !seen[primary] {
					seen[primary] = true
					if mime == "" {
						mime = "video/mp4"
					}
					if thumbURL != "" && googleUserContentRe.MatchString(thumbURL) {
						thumbURL = imageSizeSuffixRe.ReplaceAllString(thumbURL, "") + "=s1024"
					}
					videos = append(videos, GeneratedVideo{
						URL:          primary,
						StreamURL:    streamURL,
						ThumbnailURL: thumbURL,
						Title:        fname,
						MimeType:     mime,
					})
					return
				}
			}
		}
		for _, child := range arr {
			walk(child)
		}
	}
	walk(v60)
	return videos
}

func extractErrCode(entry []any) int {
	for i := len(entry) - 1; i >= 1; i-- {
		if f, ok := entry[i].(float64); ok && f > 0 {
			return int(f)
		}
	}
	return 502
}

func checkErrorExtension(entry []any) error {
	if len(entry) <= 5 {
		return nil
	}
	ext5, ok := entry[5].([]any)
	if !ok {
		return nil
	}
	// Pattern: part[5][2][0][1][0]
	if len(ext5) > 2 {
		if list, ok := ext5[2].([]any); ok {
			for _, itemAny := range list {
				item, ok := itemAny.([]any)
				if !ok || len(item) < 2 {
					continue
				}
				if sub, ok := item[1].([]any); ok && len(sub) > 0 {
					if f, ok := sub[0].(float64); ok && f > 0 {
						return classifyRPCErrorCode(int(f), entry)
					}
					if codeArr, ok := sub[0].([]any); ok && len(codeArr) > 0 {
						if f, ok := codeArr[0].(float64); ok && f > 0 {
							return classifyRPCErrorCode(int(f), entry)
						}
					}
				}
			}
		}
	}
	return nil
}

func classifyRPCErrorCode(code int, raw any) error {
	rawBytes, _ := json.Marshal(raw)
	snippet := string(rawBytes)
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	switch code {
	case 1014:
		return transport.NewUpstreamError(
			"upstream error code 1014: access is temporarily restricted; retrying later usually succeeds",
			503,
			"upstream_degraded",
			nil,
			nil,
		)
	case 1016:
		return transport.NewUpstreamError(
			"upstream error code 1016: session is unauthenticated or the saved cookies have expired (code 1016) — run `gsb --login <name>` to sign in again",
			401,
			"upstream_auth_error",
			nil,
			nil,
		)
	case 1037:
		return transport.NewUpstreamError(
			"upstream error code 1037",
			429,
			"rate_limit_error",
			nil,
			nil,
		)
	case 1060:
		return transport.NewUpstreamError(
			"upstream error code 1060",
			429,
			"rate_limit_error",
			nil,
			nil,
		)
	case 1185:
		// Observed on accounts with plenty of quota left, so it is not a quota
		// signal: Gemini Web fails the turn on its side. Report it as a retryable
		// upstream degradation so the pool can fail over to a healthier account
		// instead of surfacing a bare number to the caller.
		return transport.NewUpstreamError(
			fmt.Sprintf("upstream error code 1185: Gemini Web failed the turn upstream; retrying on another account usually succeeds: %s", snippet),
			502,
			"upstream_degraded",
			nil,
			nil,
		)
	case 1013:
		// Raised at random when generating with certain models and gone on the
		// next attempt. It is not a quota or auth signal, so it must be retried
		// rather than presented to the caller as a generic upstream failure.
		return transport.NewUpstreamError(
			"upstream error code 1013: Gemini reported a temporary model error; retrying usually succeeds",
			503,
			"upstream_transient",
			nil,
			nil,
		)
	case 1050, 1052:
		return transport.NewUpstreamError(
			fmt.Sprintf("upstream error code %d", code),
			400,
			"model_invalid",
			nil,
			nil,
		)
	case 400:
		return transport.NewUpstreamError(
			fmt.Sprintf("upstream error code 400: %s", snippet),
			400,
			"upstream_rpc_400",
			nil,
			nil,
		)
	case 401, 403:
		return transport.NewUpstreamError(
			fmt.Sprintf("upstream auth error %d: %s", code, snippet),
			code,
			"upstream_auth_error",
			nil,
			nil,
		)
	default:
		return transport.NewUpstreamError(
			fmt.Sprintf("upstream error code %d", code),
			502,
			"upstream_error",
			nil,
			nil,
		)
	}
}
