package toolbridge

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

var callSeq atomic.Uint64

func nextToolCallID() string {
	seq := callSeq.Add(1) % 1_000_000
	ts := strconv.FormatInt(time.Now().UnixMilli(), 36)
	seqStr := strconv.FormatUint(seq, 36)
	for len(seqStr) < 3 {
		seqStr = "0" + seqStr
	}
	return "call_" + ts + seqStr
}

// ToOpenAIToolCalls formats normalized Calls into OpenAI tool_calls objects.
func ToOpenAIToolCalls(calls []Call) []OpenAIToolCall {
	out := make([]OpenAIToolCall, 0, len(calls))
	for _, c := range calls {
		argsMap := c.Arguments
		if argsMap == nil {
			argsMap = map[string]any{}
		}
		b, _ := json.Marshal(argsMap)
		out = append(out, OpenAIToolCall{
			ID:   nextToolCallID(),
			Type: "function",
			Function: OpenAIFunctionCall{
				Name:      c.Name,
				Arguments: string(b),
			},
		})
	}
	return out
}

// StreamOpenAIToolCallChunks emits the 2-step OpenAI streaming tool_calls delta sequence
// for pre-constructed OpenAIToolCall items so IDs remain identical across cache and SSE.
func StreamOpenAIToolCallChunks(emit func(delta map[string]any), oa []OpenAIToolCall) {
	for idx, call := range oa {
		emit(map[string]any{
			"tool_calls": []map[string]any{
				{
					"index": idx,
					"id":    call.ID,
					"type":  "function",
					"function": map[string]any{
						"name":      call.Function.Name,
						"arguments": "",
					},
				},
			},
		})
		emitArgumentStringChunks(emit, idx, call.Function.Arguments)
	}
}

func emitArgumentStringChunks(emit func(delta map[string]any), idx int, args string) {
	const maxChunk = 4096
	if len(args) <= maxChunk {
		emit(map[string]any{
			"tool_calls": []map[string]any{
				{
					"index": idx,
					"function": map[string]any{
						"arguments": args,
					},
				},
			},
		})
		return
	}
	for start := 0; start < len(args); {
		end := start + maxChunk
		if end >= len(args) {
			end = len(args)
		} else {
			for end > start && !utf8.RuneStart(args[end]) {
				end--
			}
		}
		emit(map[string]any{
			"tool_calls": []map[string]any{
				{
					"index": idx,
					"function": map[string]any{
						"arguments": args[start:end],
					},
				},
			},
		})
		start = end
	}
}

type streamedCallState struct {
	id         string
	name       string
	headerSent bool
	sentArgs   string
	finalArgs  string
	doneArgs   map[string]any
	done       bool
	invalid    bool
}

func (st *streamedCallState) emittedAny() bool {
	return st.headerSent || st.sentArgs != "" || st.finalArgs != ""
}

// LiveToolCallStreamer incrementally emits OpenAI tool_calls delta chunks as Gemini Web
// generates a {"tool_calls":[...]} JSON envelope, while guaranteeing schema validity.
type LiveToolCallStreamer struct {
	contract *Contract
	allow    map[string]map[string]any
	emit     func(delta map[string]any)
	states   []streamedCallState
	aborted  bool
	err      error
}

// NewLiveToolCallStreamer constructs an incremental tool_calls streamer for one attempt.
func NewLiveToolCallStreamer(tools any, toolChoice any, emit func(delta map[string]any)) *LiveToolCallStreamer {
	contract, err := ValidateToolRequest(tools, toolChoice)
	if err != nil || len(contract.Tools) == 0 || contract.ChoiceMode == "none" {
		return &LiveToolCallStreamer{aborted: true, emit: emit}
	}
	allow := make(map[string]map[string]any, len(contract.Tools))
	for _, t := range contract.Tools {
		allow[t.Name] = t.Parameters
	}
	return &LiveToolCallStreamer{
		contract: contract,
		allow:    allow,
		emit:     emit,
	}
}

// SentAny reports whether at least one tool_calls chunk has been emitted.
func (s *LiveToolCallStreamer) SentAny() bool {
	for _, st := range s.states {
		if st.headerSent || st.sentArgs != "" || st.finalArgs != "" {
			return true
		}
	}
	return false
}

// Err reports a terminal revision or validation error detected while streaming.
func (s *LiveToolCallStreamer) Err() error {
	if s == nil {
		return nil
	}
	return s.err
}

func (s *LiveToolCallStreamer) ensureState(idx int) *streamedCallState {
	for len(s.states) <= idx {
		s.states = append(s.states, streamedCallState{
			id: nextToolCallID(),
		})
	}
	return &s.states[idx]
}

func (s *LiveToolCallStreamer) resetState(idx int, name string) {
	id := ""
	if idx >= 0 && idx < len(s.states) {
		id = s.states[idx].id
	}
	if id == "" {
		id = nextToolCallID()
	}
	s.states[idx] = streamedCallState{id: id, name: name}
}

func upstreamRevisionError(detail string) error {
	return transport.NewUpstreamError("upstream_revision: "+detail, 502, "upstream_revision", nil, nil)
}

func (s *LiveToolCallStreamer) revision(idx int, st *streamedCallState, detail string) error {
	if st != nil {
		st.invalid = true
	}
	err := upstreamRevisionError("tool call " + strconv.Itoa(idx) + " " + detail)
	s.err = err
	return err
}

func (s *LiveToolCallStreamer) emitHeader(idx int, st *streamedCallState, name string) error {
	if st.headerSent {
		if st.name != name {
			return s.revision(idx, st, "changed name after its header was emitted")
		}
		return nil
	}
	st.name = name
	st.headerSent = true
	emitToolCallHeader(s.emit, idx, st.id, name)
	return nil
}

func emitToolCallHeader(emit func(delta map[string]any), idx int, id, name string) {
	emit(map[string]any{
		"tool_calls": []map[string]any{
			{
				"index": idx,
				"id":    id,
				"type":  "function",
				"function": map[string]any{
					"name":      name,
					"arguments": "",
				},
			},
		},
	})
}

func canonicalArgumentString(args map[string]any) string {
	if args == nil {
		args = map[string]any{}
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func callArgsEqual(a, b map[string]any) bool {
	if a == nil {
		a = map[string]any{}
	}
	if b == nil {
		b = map[string]any{}
	}
	return jsonEqual(a, b)
}

// completeStreamedArgs resolves the final argument bytes for a call whose partial
// prefix has already been emitted. It only succeeds when the emitted prefix remains a
// byte prefix of a valid JSON object whose parsed value matches the authoritative call,
// so the concatenated stream can never carry stale or repaired-away values.
func completeStreamedArgs(st *streamedCallState, rawArgs, canonical string, authoritative map[string]any) (string, bool) {
	prefix := st.sentArgs
	if prefix == "" {
		return canonical, true
	}
	if strings.HasPrefix(canonical, prefix) {
		return canonical, true
	}
	if !strings.HasPrefix(rawArgs, prefix) {
		return "", false
	}
	candidates := []string{rawArgs}
	if repaired := repairSanitizedHTMLInJSON(rawArgs); repaired != rawArgs {
		candidates = append(candidates, repaired)
	}
	for _, candidate := range candidates {
		if !strings.HasPrefix(candidate, prefix) || !json.Valid([]byte(candidate)) {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(candidate), &parsed); err != nil || parsed == nil {
			continue
		}
		if callArgsEqual(parsed, authoritative) {
			return candidate, true
		}
	}
	return "", false
}

func (s *LiveToolCallStreamer) firstEmittedIndex() int {
	for i := range s.states {
		if s.states[i].emittedAny() {
			return i
		}
	}
	return -1
}

// abortOrRevision fails closed with upstream_revision when any streamed state would be
// left stale, and otherwise aborts silently like the historical best-effort streamer.
func (s *LiveToolCallStreamer) abortOrRevision(idx int, detail string) error {
	if idx < 0 || idx >= len(s.states) || !s.states[idx].emittedAny() {
		idx = s.firstEmittedIndex()
	}
	if idx >= 0 && idx < len(s.states) {
		return s.revision(idx, &s.states[idx], detail)
	}
	s.aborted = true
	return nil
}

// Push inspects the cumulative candidate text and emits any newly available tool call
// headers or verified argument slices. It preserves the historical signature and
// ignores the revision error; callers that need to fail closed should use PushError.
func (s *LiveToolCallStreamer) Push(text string) {
	_ = s.PushError(text)
}

// PushError is Push with terminal revision and validation errors reported to the caller.
func (s *LiveToolCallStreamer) PushError(text string) error {
	if s == nil {
		return nil
	}
	if s.err != nil {
		return s.err
	}
	if s.aborted || s.emit == nil {
		return nil
	}
	arrStart := findToolCallsArrayStart(text)
	if arrStart < 0 {
		if idx := s.firstEmittedIndex(); idx >= 0 {
			return s.revision(idx, &s.states[idx], "disappeared from the candidate text")
		}
		return nil
	}
	pos := arrStart
	visibleIdx := 0
	for {
		idx := visibleIdx
		pos = skipJSONWSAndCommas(text, pos)
		if pos >= len(text) || text[pos] == ']' {
			return s.checkRemovedCalls(idx)
		}
		if text[pos] != '{' {
			return s.abortOrRevision(idx, "regressed to a malformed tool-call object")
		}
		objEnd := findSingleBalancedObjectEnd(text, pos)
		if objEnd > pos {
			rawObj := text[pos:objEnd]
			parsed, ok := unmarshalCandidateObject(rawObj)
			if !ok {
				return s.abortOrRevision(idx, "regressed to a malformed completed call")
			}
			normCalls, err := normaliseCalls(map[string]any{"tool_calls": []any{parsed}}, s.allow)
			if err != nil {
				if idx < len(s.states) && s.states[idx].emittedAny() {
					return s.revision(idx, &s.states[idx], "changed to an invalid completed call")
				}
				s.aborted = true
				s.err = err
				return err
			}
			if len(normCalls) != 1 {
				return s.abortOrRevision(idx, "regressed to an empty completed call")
			}
			call := normCalls[0]
			if call.Name == BuiltinMediaToolName {
				// Intercepted media does not occupy a client call slot.
				pos = objEnd
				continue
			}
			visibleIdx++
			if s.contract != nil && s.contract.ChoiceMode == "function" && s.contract.ForcedFunction != "" && idx == 0 && call.Name != s.contract.ForcedFunction {
				return s.abortOrRevision(idx, "changed to a function that is not forced")
			}
			st := s.ensureState(idx)
			if st.invalid {
				return s.revision(idx, st, "is invalid")
			}
			if st.done {
				if st.name == call.Name && callArgsEqual(st.doneArgs, call.Arguments) {
					// Whitespace and key-order changes are not revisions.
					pos = objEnd
					continue
				}
				if st.emittedAny() {
					return s.revision(idx, st, "changed after it was completed")
				}
				s.resetState(idx, call.Name)
				st = &s.states[idx]
			}
			if st.headerSent && st.name != call.Name {
				return s.revision(idx, st, "changed name after its header was emitted")
			}
			if st.name == "" {
				st.name = call.Name
			}
			canonical := canonicalArgumentString(call.Arguments)
			if st.sentArgs != "" {
				completed, ok := completeStreamedArgs(st, extractRawArgumentsObject(rawObj), canonical, call.Arguments)
				if !ok {
					return s.revision(idx, st, "partial arguments are no longer a monotonic prefix")
				}
				if err := s.emitHeader(idx, st, call.Name); err != nil {
					return err
				}
				if tail := completed[len(st.sentArgs):]; tail != "" {
					emitArgumentStringChunks(s.emit, idx, tail)
				}
				st.sentArgs = completed
				st.finalArgs = completed
				st.doneArgs = call.Arguments
				st.done = true
				pos = objEnd
				continue
			}
			if err := s.emitHeader(idx, st, call.Name); err != nil {
				return err
			}
			emitArgumentStringChunks(s.emit, idx, canonical)
			st.sentArgs = canonical
			st.finalArgs = canonical
			st.doneArgs = call.Arguments
			st.done = true
			pos = objEnd
			continue
		}

		// Incomplete object at end of text: check if we can emit its header and verified partial arguments
		partialObj := text[pos:]
		name, argsOffset, ok := parsePartialCallPrefix(partialObj)
		if !ok {
			if idx < len(s.states) && s.states[idx].emittedAny() {
				return s.revision(idx, &s.states[idx], "regressed to an incomplete object")
			}
			return nil
		}
		if name == BuiltinMediaToolName {
			return s.checkRemovedCalls(idx)
		}
		if _, allowed := s.allow[name]; !allowed {
			if idx < len(s.states) && s.states[idx].emittedAny() {
				return s.revision(idx, &s.states[idx], "changed to a tool that is not allowed")
			}
			return nil
		}
		if s.contract != nil && s.contract.ChoiceMode == "function" && s.contract.ForcedFunction != "" && idx == 0 && name != s.contract.ForcedFunction {
			if idx < len(s.states) && s.states[idx].emittedAny() {
				return s.revision(idx, &s.states[idx], "changed to a function that is not forced")
			}
			return nil
		}
		partialArgs := partialObj[argsOffset:]
		if argsEnd := findSingleBalancedObjectEnd(partialArgs, 0); argsEnd >= 0 && strings.Contains(partialArgs[argsEnd:], "{") {
			if idx < len(s.states) && s.states[idx].emittedAny() {
				return s.revision(idx, &s.states[idx], "appended a second JSON object to partial arguments")
			}
			return nil
		}
		st := s.ensureState(idx)
		if st.invalid {
			return s.revision(idx, st, "is invalid")
		}
		if st.done {
			if st.emittedAny() {
				return s.revision(idx, st, "regressed from a completed call to a partial object")
			}
			s.resetState(idx, name)
			st = &s.states[idx]
		}
		if st.headerSent && st.name != name {
			return s.revision(idx, st, "changed name while streaming partial arguments")
		}
		if st.name == "" {
			st.name = name
		}
		prefix := st.sentArgs
		if prefix != "" && (len(partialArgs) < len(prefix) || !strings.HasPrefix(partialArgs, prefix)) {
			return s.revision(idx, st, "partial arguments are no longer a monotonic prefix")
		}
		headerOK, safeLen := verifyPartialArgsForLiveStream(partialArgs, s.allow[name])
		if !headerOK {
			if prefix != "" {
				return s.revision(idx, st, "partial arguments are no longer schema-safe")
			}
			return nil
		}
		if err := s.emitHeader(idx, st, name); err != nil {
			return err
		}
		if safeLen > len(st.sentArgs) {
			if safeLen > len(partialArgs) {
				safeLen = len(partialArgs)
			}
			if !strings.HasPrefix(partialArgs[:safeLen], st.sentArgs) {
				return s.revision(idx, st, "partial arguments are no longer a monotonic prefix")
			}
			delta := partialArgs[len(st.sentArgs):safeLen]
			st.sentArgs = partialArgs[:safeLen]
			emitArgumentStringChunks(s.emit, idx, delta)
		}
		return nil
	}
}

func (s *LiveToolCallStreamer) checkRemovedCalls(firstMissing int) error {
	for idx := firstMissing; idx < len(s.states); idx++ {
		if st := &s.states[idx]; st.emittedAny() {
			return s.revision(idx, st, "was removed after it was streamed")
		}
	}
	return nil
}

func (s *LiveToolCallStreamer) authoritativeToolCalls(calls []Call) []OpenAIToolCall {
	out := make([]OpenAIToolCall, 0, len(calls))
	for idx, c := range calls {
		callID := ""
		if s != nil && idx < len(s.states) {
			callID = s.states[idx].id
		}
		if callID == "" {
			callID = nextToolCallID()
		}
		out = append(out, OpenAIToolCall{
			ID:   callID,
			Type: "function",
			Function: OpenAIFunctionCall{
				Name:      c.Name,
				Arguments: canonicalArgumentString(c.Arguments),
			},
		})
	}
	return out
}

// Finalize completes emission of all validated tool calls at the end of a turn and returns
// the canonical OpenAIToolCall slice with matching IDs and argument strings. It preserves
// the historical signature; callers that must fail closed on upstream revisions should
// use FinalizeError. If a revision invalidates streamed state, the returned calls still
// carry authoritative arguments and are never stale.
func (s *LiveToolCallStreamer) Finalize(calls []Call, fullText string) []OpenAIToolCall {
	out, err := s.FinalizeError(calls, fullText)
	if err != nil {
		return s.authoritativeToolCalls(calls)
	}
	return out
}

// FinalizeError is Finalize with terminal revision errors reported to the caller.
// The caller supplies validated calls in client-visible order, excluding intercepted
// server-side calls. Changes to an emitted call yield an upstream_revision error
// instead of returning stale argument bytes.
func (s *LiveToolCallStreamer) FinalizeError(calls []Call, fullText string) ([]OpenAIToolCall, error) {
	if s != nil {
		if fullText != "" {
			if err := s.PushError(fullText); err != nil {
				return nil, err
			}
		}
		if s.err != nil {
			return nil, s.err
		}
	}
	out := make([]OpenAIToolCall, 0, len(calls))
	for idx, c := range calls {
		argsStr := canonicalArgumentString(c.Arguments)
		callID := ""
		if s != nil && idx < len(s.states) {
			st := &s.states[idx]
			if st.invalid {
				return nil, s.revision(idx, st, "is invalid")
			}
			callID = st.id
			if st.done && st.finalArgs != "" {
				if st.name != c.Name || !callArgsEqual(st.doneArgs, c.Arguments) {
					if st.emittedAny() {
						return nil, s.revision(idx, st, "changed after it was completed")
					}
					s.resetState(idx, c.Name)
					st = &s.states[idx]
				} else {
					if !json.Valid([]byte(st.finalArgs)) {
						return nil, s.revision(idx, st, "produced malformed final arguments")
					}
					parsed, ok := unmarshalCandidateObject(st.finalArgs)
					if !ok || !callArgsEqual(parsed, c.Arguments) {
						return nil, s.revision(idx, st, "returned stale final arguments")
					}
					out = append(out, OpenAIToolCall{
						ID:   callID,
						Type: "function",
						Function: OpenAIFunctionCall{
							Name:      c.Name,
							Arguments: st.finalArgs,
						},
					})
					continue
				}
			}
			if st.headerSent && st.name != c.Name {
				if st.emittedAny() {
					return nil, s.revision(idx, st, "changed name after its header was emitted")
				}
				s.resetState(idx, c.Name)
				st = &s.states[idx]
			}
			if s.emit != nil {
				if !st.headerSent {
					st.name = c.Name
					st.headerSent = true
					emitToolCallHeader(s.emit, idx, callID, c.Name)
				}
				// PushError already finished every complete raw call. Any pending
				// prefix must match the authoritative encoding before we append it.
				if !strings.HasPrefix(argsStr, st.sentArgs) {
					return nil, s.revision(idx, st, "partial arguments are no longer a monotonic prefix")
				}
				if tail := argsStr[len(st.sentArgs):]; tail != "" {
					emitArgumentStringChunks(s.emit, idx, tail)
				}
				st.sentArgs = argsStr
				st.finalArgs = argsStr
				st.doneArgs = c.Arguments
				st.done = true
			}
		}
		if callID == "" {
			callID = nextToolCallID()
			if s != nil && s.emit != nil {
				emitToolCallHeader(s.emit, idx, callID, c.Name)
				emitArgumentStringChunks(s.emit, idx, argsStr)
			}
		}
		out = append(out, OpenAIToolCall{
			ID:   callID,
			Type: "function",
			Function: OpenAIFunctionCall{
				Name:      c.Name,
				Arguments: argsStr,
			},
		})
	}
	if s != nil {
		for idx := len(calls); idx < len(s.states); idx++ {
			st := &s.states[idx]
			if st.emittedAny() {
				return nil, s.revision(idx, st, "was removed from the completed calls")
			}
		}
	}
	return out, nil
}

func findToolCallsArrayStart(s string) int {
	idx := strings.Index(s, `"tool_calls"`)
	if idx < 0 {
		return -1
	}
	prev := idx - 1
	for prev >= 0 && (s[prev] == ' ' || s[prev] == '\t' || s[prev] == '\r' || s[prev] == '\n') {
		prev--
	}
	if prev < 0 || (s[prev] != '{' && s[prev] != ',') {
		return -1
	}
	pos := idx + len(`"tool_calls"`)
	pos = skipJSONWS(s, pos)
	if pos >= len(s) || s[pos] != ':' {
		return -1
	}
	pos = skipJSONWS(s, pos+1)
	if pos >= len(s) || s[pos] != '[' {
		return -1
	}
	return pos + 1
}

func skipJSONWS(s string, pos int) int {
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\r' || s[pos] == '\n') {
		pos++
	}
	return pos
}

func skipJSONWSAndCommas(s string, pos int) int {
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\r' || s[pos] == '\n' || s[pos] == ',') {
		pos++
	}
	return pos
}

func findSingleBalancedObjectEnd(s string, start int) int {
	if start >= len(s) || s[start] != '{' {
		return -1
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inStr {
			if escaped {
				escaped = false
				if ch == '"' && missingQuoteAfterEscapedQuoteRe.MatchString(s[i+1:]) {
					inStr = false
				}
			} else if ch == '\\' {
				escaped = true
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
			depth++
		} else if ch == '}' {
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

func parseJSONStringLiteral(s string, start int) (val string, next int, ok bool) {
	if start >= len(s) || s[start] != '"' {
		return "", start, false
	}
	escaped := false
	for i := start + 1; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			var out string
			if err := json.Unmarshal([]byte(s[start:i+1]), &out); err != nil {
				return "", start, false
			}
			return out, i + 1, true
		}
	}
	return "", start, false
}

func parsePartialCallPrefix(partialObj string) (name string, argsBraceOffset int, ok bool) {
	pos := skipJSONWS(partialObj, 0)
	if pos >= len(partialObj) || partialObj[pos] != '{' {
		return "", 0, false
	}
	pos = skipJSONWS(partialObj, pos+1)
	k1, next, ok := parseJSONStringLiteral(partialObj, pos)
	if !ok || k1 != "name" {
		return "", 0, false
	}
	pos = skipJSONWS(partialObj, next)
	if pos >= len(partialObj) || partialObj[pos] != ':' {
		return "", 0, false
	}
	pos = skipJSONWS(partialObj, pos+1)
	toolName, next, ok := parseJSONStringLiteral(partialObj, pos)
	if !ok || toolName == "" {
		return "", 0, false
	}
	pos = skipJSONWS(partialObj, next)
	if pos >= len(partialObj) || partialObj[pos] != ',' {
		return "", 0, false
	}
	pos = skipJSONWS(partialObj, pos+1)
	k2, next, ok := parseJSONStringLiteral(partialObj, pos)
	if !ok || k2 != "arguments" {
		return "", 0, false
	}
	pos = skipJSONWS(partialObj, next)
	if pos >= len(partialObj) || partialObj[pos] != ':' {
		return "", 0, false
	}
	pos = skipJSONWS(partialObj, pos+1)
	if pos >= len(partialObj) || partialObj[pos] != '{' {
		return "", 0, false
	}
	return toolName, pos, true
}

func findJSONValueEnd(s string, start int) int {
	if start >= len(s) {
		return -1
	}
	switch s[start] {
	case '{':
		return findSingleBalancedObjectEnd(s, start)
	case '[':
		depth := 0
		inStr := false
		escaped := false
		for i := start; i < len(s); i++ {
			ch := s[i]
			if inStr {
				if escaped {
					escaped = false
				} else if ch == '\\' {
					escaped = true
				} else if ch == '"' {
					inStr = false
				}
				continue
			}
			if ch == '"' {
				inStr = true
				continue
			}
			if ch == '[' || ch == '{' {
				depth++
			} else if ch == ']' || ch == '}' {
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
		return -1
	case '"':
		_, next, ok := parseJSONStringLiteral(s, start)
		if !ok {
			return -1
		}
		return next
	default:
		i := start
		for i < len(s) && s[i] != ',' && s[i] != '}' && s[i] != ']' &&
			s[i] != ' ' && s[i] != '\t' && s[i] != '\r' && s[i] != '\n' {
			i++
		}
		if i == start {
			return -1
		}
		return i
	}
}

// extractRawArgumentsObject returns the raw JSON object used as arguments regardless of
// top-level key ordering, mirroring normaliseCalls' argument-key preference.
func extractRawArgumentsObject(rawObj string) string {
	pos := skipJSONWS(rawObj, 0)
	if pos >= len(rawObj) || rawObj[pos] != '{' {
		return ""
	}
	pos++
	keys := [...]string{"arguments", "parameters", "args", "input", "params", "action_input"}
	spans := make([][2]int, len(keys))
	for {
		pos = skipJSONWS(rawObj, pos)
		if pos >= len(rawObj) || rawObj[pos] == '}' {
			break
		}
		key, next, ok := parseJSONStringLiteral(rawObj, pos)
		if !ok {
			break
		}
		pos = skipJSONWS(rawObj, next)
		if pos >= len(rawObj) || rawObj[pos] != ':' {
			break
		}
		pos = skipJSONWS(rawObj, pos+1)
		valueStart := pos
		valueEnd := findJSONValueEnd(rawObj, valueStart)
		if valueEnd <= valueStart {
			break
		}
		key = strings.TrimSpace(key)
		for i, want := range keys {
			if key == want && spans[i][1] == 0 {
				spans[i] = [2]int{valueStart, valueEnd}
				break
			}
		}
		pos = skipJSONWS(rawObj, valueEnd)
		if pos < len(rawObj) && rawObj[pos] == ',' {
			pos++
			continue
		}
		break
	}
	for _, span := range spans {
		if span[1] > span[0] {
			if rawObj[span[0]] != '{' {
				return ""
			}
			return rawObj[span[0]:span[1]]
		}
	}
	return ""
}

func verifyPartialArgsForLiveStream(partialArgs string, schema map[string]any) (headerOK bool, safeLen int) {
	if len(partialArgs) < 2 || partialArgs[0] != '{' {
		return false, 0
	}
	if strings.Contains(partialArgs, " |>") {
		return false, 0
	}
	props, _ := schema["properties"].(map[string]any)
	reqList := extractStringSlice(schema["required"])

	seenKeys := make(map[string]bool)
	depth := 0
	inStr := false
	escaped := false
	expectingKey := false
	for i := 0; i < len(partialArgs); i++ {
		ch := partialArgs[i]
		if inStr {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inStr = false
			}
			continue
		}
		switch ch {
		case '{':
			depth++
			if depth == 1 {
				expectingKey = true
			}
		case '}':
			depth--
		case '[':
			depth++
		case ']':
			depth--
		case ',':
			if depth == 1 {
				expectingKey = true
			}
		case '"':
			if depth == 1 && expectingKey {
				key, next, ok := parseJSONStringLiteral(partialArgs, i)
				if !ok {
					// Key string literal still being generated
					i = len(partialArgs)
					break
				}
				afterKey := skipJSONWS(partialArgs, next)
				if afterKey >= len(partialArgs) {
					i = len(partialArgs)
					break
				}
				if partialArgs[afterKey] != ':' {
					return false, 0
				}
				valPos := skipJSONWS(partialArgs, afterKey+1)
				if valPos >= len(partialArgs) {
					i = len(partialArgs)
					break
				}
				if props != nil {
					propSchema, exists := props[key]
					if !exists {
						return false, 0
					}
					if !isValueTokenCompatibleWithSchema(partialArgs[valPos], propSchema) {
						return false, 0
					}
				}
				seenKeys[key] = true
				expectingKey = false
				i = valPos - 1
				continue
			}
			inStr = true
		}
	}
	if len(seenKeys) == 0 && len(reqList) > 0 {
		return false, 0
	}
	headerOK = true
	if len(partialArgs) < 120 {
		return true, 0
	}
	for _, req := range reqList {
		if !seenKeys[req] {
			if props != nil {
				if pm, ok := props[req].(map[string]any); ok && pm["default"] != nil {
					continue
				}
			}
			return true, 0
		}
	}
	cut := len(partialArgs) - 8
	for cut > 0 && !utf8.RuneStart(partialArgs[cut]) {
		cut--
	}
	for back := 1; back <= 6 && cut-back >= 0; back++ {
		if partialArgs[cut-back] == '\\' {
			cut -= back
			break
		}
	}
	if cut <= 1 {
		return true, 0
	}
	if repairJSONStringEscapes(partialArgs[:cut]) != partialArgs[:cut] {
		return true, 0
	}
	return true, cut
}

func isValueTokenCompatibleWithSchema(firstByte byte, propSchema any) bool {
	pm, ok := propSchema.(map[string]any)
	if !ok || pm == nil {
		return firstByte != 'n'
	}
	tStr, _ := pm["type"].(string)
	if tStr == "" {
		return firstByte != 'n'
	}
	switch tStr {
	case "string":
		return firstByte == '"'
	case "object":
		return firstByte == '{'
	case "array":
		return firstByte == '['
	case "boolean":
		return firstByte == 't' || firstByte == 'f'
	case "number", "integer":
		return firstByte == '-' || (firstByte >= '0' && firstByte <= '9')
	default:
		return true
	}
}
