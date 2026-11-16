package toolbridge

import (
	"strings"
	"testing"
)

func sampleReproTools() any {
	return []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "bash",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
						"i":       map[string]any{"type": "string"},
					},
					"required": []any{"command", "i"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "todo",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"op":    map[string]any{"type": "string", "enum": []any{"init", "start", "done", "view"}},
						"i":     map[string]any{"type": "string"},
						"items": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
					"required": []any{"op", "i"},
				},
			},
		},
	}
}

// Prose summaries that MENTION the envelope (inline code, fenced example with
// placeholders, prose reference) are final answers, not tool calls. They must
// parse cleanly with zero calls and no error.
func TestProseMentionsAreNotToolCalls(t *testing.T) {
	cases := []struct {
		name  string
		reply string
	}{
		{"fencedexample", "Selesai. Ringkasan perbaikan:\n\n" +
			"- `truncatedEnvRe` hanya dievaluasi saat tidak ada envelope valid, jadi blok ```json {\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/x\"}}]} ``` di dalam penjelasan tidak memicu 'malformed or truncated'.\n" +
			"- Semua selesai.\n"},
		{"inlinecode", "Ringkasan: gunakan blok `{\"tool_calls\": [...]}` untuk memanggil tool.\nDilakukan."},
		{"prosetable", "Format yang diminta harness:\n\n| field | isi |\n|---|---|\n| name | nama tool |\n\nContoh: ```json\n{\"tool_calls\":[...]}\n``` (placeholder, jangan dieksekusi).\nSelesai."},
		{"harnessdescription", "Bridge mem-parsing `json {\"tool_calls\":[{\"name\":\"<tool>\",\"arguments\":{...}}]}` sebagai envelope tool call.\nTugas sudah selesai dan tidak ada file yang perlu diubah."},
		{"cardmention", "Ketik `json {\"tool_calls\":[ ... ]}` sesuai skema. Namun tidak ada yang perlu dipanggil — jawaban final: semua target sudah diverifikasi."},
	}
	for _, tc := range cases {
		res, err := InterpretReply(tc.reply, sampleReproTools(), "auto")
		if err != nil {
			t.Errorf("case %q: unexpected error: %v", tc.name, err)
			continue
		}
		if res != nil && len(res.Calls) > 0 {
			t.Errorf("case %q: prose mention wrongly produced calls: %+v", tc.name, res.Calls)
		}
	}
}

// Genuine truncation: envelope stripped brutally by upstream — unclosed call
// array + no closing fence, at the END of the reply.
func TestRealTruncatedEnvelopeStillErrors(t *testing.T) {
	cases := []struct {
		name  string
		reply string
	}{
		{"midarray", `{"tool_calls":[{"name":"read_file","arguments":{"path":"/tmp/a"},`},
		{"noarrayclose", `{"tool_calls":[{"name":"read_file","arguments":{"path":"/tmp/a"}}`},
		{"fencedopencut", "```json\n{\"tool_calls\":[{\"name\":\"read_file\""},
		{"argcut", "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"pa"},
		{"missingbracketbeforebrace", "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/a\"}}}\n```"},
	}
	for _, tc := range cases {
		_, err := InterpretReply(tc.reply, sampleReproTools(), "auto")
		if err == nil {
			t.Errorf("case %q: expected error, got nil", tc.name)
		} else if !strings.Contains(err.Error(), "malformed or truncated") {
			t.Errorf("case %q: unexpected error %v", tc.name, err)
		}
	}
}

func TestRedundantClosingBracesInToolEnvelope(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		tool  string
	}{
		{
			name:  "extra_trailing_brace_fenced",
			reply: "```json\n{\"tool_calls\":[{\"name\":\"bash\",\"arguments\":{\"command\":\"python3 -c \\\"import sqlite3; conn = sqlite3.connect('transpadang-tracker/data/tracker_202610.db'); print(conn.execute('SELECT name FROM sqlite_master WHERE type=\\\\'table\\\\'').fetchall())\\\"\",\"i\":\"Inspect database tables\"}}]}}\n```",
			tool:  "bash",
		},
		{
			name:  "extra_brace_before_array_close",
			reply: "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/a\"}}}]}\n```",
			tool:  "read_file",
		},
		{
			name:  "extra_bracket_before_envelope_close",
			reply: "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/a\"}}]]}\n```",
			tool:  "read_file",
		},
	}
	for _, tc := range cases {
		res, err := InterpretReply(tc.reply, sampleReproTools(), "auto")
		if err != nil {
			t.Fatalf("case %q: unexpected error: %v", tc.name, err)
		}
		if res == nil || len(res.Calls) != 1 || res.Calls[0].Name != tc.tool {
			t.Fatalf("case %q: expected 1 %q call, got %+v", tc.name, tc.tool, res)
		}
	}
}

func TestTopLevelToolArgumentsMergedIntoArgumentsObject(t *testing.T) {
	// 1. Model puts required schema properties at the top level of the tool call object alongside "arguments"
	res1, err := InterpretReply(`{"tool_calls":[{"name":"todo","i":"Initializing audit tasks","arguments":{"op":"init","items":["Audit database"]}}]}`, sampleReproTools(), "auto")
	if err != nil {
		t.Fatalf("expected top-level schema property to merge into arguments, got err: %v", err)
	}
	if res1 == nil || len(res1.Calls) != 1 || res1.Calls[0].Arguments["i"] != "Initializing audit tasks" {
		t.Fatalf("expected merged 'i' = 'Initializing audit tasks', got %+v", res1)
	}

	// 2. When a required property is genuinely missing everywhere, validation must still reject with tool_call_error
	_, err = InterpretReply(`{"tool_calls":[{"name":"todo","arguments":{"op":"init","items":["Audit database"]}}]}`, sampleReproTools(), "auto")
	if err == nil || !strings.Contains(err.Error(), "violate the tool schema") {
		t.Fatalf("expected missing required property to return schema violation error, got: %v", err)
	}
}

func TestMissingClosingQuoteAfterEscapedQuoteInToolEnvelope(t *testing.T) {
	// Exact malformed envelope produced by gemini-flash-lite in OpenCode session ses_eada53788ffejfflU7vYDhB2x0:
	// Note `\"},"name":"bash"}` at the end of the first command string (missing closing `"` after `\"`).
	raw := "```json\n{\"tool_calls\":[{\"arguments\":{\"command\":\"python3 -c \\\"import sqlite3; conn = sqlite3.connect('transpadang_unified.db'); print(conn.execute('SELECT koridor, COUNT(DISTINCT bus), COUNT(*) FROM positions GROUP BY koridor').fetchall())\\\",\"i\":\"Count buses\"},\"name\":\"bash\"},{\"arguments\":{\"command\":\"python3 -c \\\"import sqlite3, json; conn = sqlite3.connect('transpadang_unified.db'); print(conn.execute('SELECT payload FROM halte_snapshots LIMIT 1').fetchone())\\\"\",\"i\":\"Sample halte\"},\"name\":\"bash\"}]}\n```"
	res, err := InterpretReply(raw, sampleReproTools(), "auto")
	if err != nil {
		t.Fatalf("expected missing closing quote after escaped quote to be repaired, got err: %v", err)
	}
	if res == nil || len(res.Calls) != 2 {
		t.Fatalf("expected 2 repaired bash tool calls, got %+v", res)
	}

	// Also test when `\"},` is immediately before `"name"` (arguments object closes right after command):
	toolsNoIntent := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "shell",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"command": map[string]any{"type": "string"}},
					"required":   []any{"command"},
				},
			},
		},
	}
	openCodeRaw := "```json\n{\"tool_calls\":[{\"arguments\":{\"command\":\"python3 -c \\\"import sqlite3; conn = sqlite3.connect('transpadang_unified.db'); print(conn.execute('SELECT koridor, COUNT(DISTINCT bus), COUNT(*) FROM positions GROUP BY koridor').fetchall())\\\"},\"name\":\"shell\"},{\"arguments\":{\"command\":\"python3 -c \\\"import sqlite3, json; conn = sqlite3.connect('transpadang_unified.db'); print(conn.execute('SELECT payload FROM halte_snapshots LIMIT 1').fetchone())\\\"\"},\"name\":\"shell\"}]}\n```"
	res2, err := InterpretReply(openCodeRaw, toolsNoIntent, "auto")
	if err != nil {
		t.Fatalf("expected OpenCode payload to repair and parse, got err: %v", err)
	}
	if res2 == nil || len(res2.Calls) != 2 || res2.Calls[0].Name != "shell" || res2.Calls[1].Name != "shell" {
		t.Fatalf("expected 2 shell calls from OpenCode payload, got %+v", res2)
	}
}

func TestSchemaDrivenArgumentCoercionAndBareKeys(t *testing.T) {
	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "query_logs",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"limit":   map[string]any{"type": "integer"},
						"ratio":   map[string]any{"type": "number"},
						"follow":  map[string]any{"type": "boolean"},
						"paths":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"options": map[string]any{"type": "object", "properties": map[string]any{"port": map[string]any{"type": "integer"}}},
						"url":     map[string]any{"type": "string"},
					},
					"required": []any{"limit", "follow", "paths"},
				},
			},
		},
	}

	// Bare keys, missing comma before bare key, stringified numbers/booleans/array/object, and bare-URL markdown autolink
	raw := "```json\n{tool_calls: [{name: \"query_logs\" arguments: {limit: \"50\", ratio: \"0.75\", follow: \"true\", paths: \"a.log\\nb.log\", options: \"{\\\"port\\\": \\\"8080\\\"}\", url: \"[https://api.example.com/v1](https://api.example.com/v1)\"}}]}\n```"
	res, err := InterpretReply(raw, tools, "auto")
	if err != nil {
		t.Fatalf("expected bare-key + schema coercion to succeed, got err: %v", err)
	}
	if res == nil || len(res.Calls) != 1 {
		t.Fatalf("expected 1 call, got %+v", res)
	}
	args := res.Calls[0].Arguments
	if args["limit"] != float64(50) {
		t.Fatalf("expected limit=50, got %#v", args["limit"])
	}
	if args["ratio"] != float64(0.75) {
		t.Fatalf("expected ratio=0.75, got %#v", args["ratio"])
	}
	if args["follow"] != true {
		t.Fatalf("expected follow=true, got %#v", args["follow"])
	}
	paths, ok := args["paths"].([]any)
	if !ok || len(paths) != 2 || paths[0] != "a.log" || paths[1] != "b.log" {
		t.Fatalf("expected paths=['a.log','b.log'], got %#v", args["paths"])
	}
	opts, ok := args["options"].(map[string]any)
	if !ok || opts["port"] != float64(8080) {
		t.Fatalf("expected options.port=8080, got %#v", args["options"])
	}
	if args["url"] != "https://api.example.com/v1" {
		t.Fatalf("expected unwrapped bare URL, got %#v", args["url"])
	}
}

func TestToolProtocolRefusalDetection(t *testing.T) {
	// Hallucinated tool-budget refusal must trigger tool_call_error retry
	_, err := InterpretReply("I have exhausted my tool-call budget for this turn, so I cannot run another command.", sampleReproTools(), "auto")
	if err == nil || !strings.Contains(err.Error(), "refused the tool-calling protocol") {
		t.Fatalf("expected tool protocol refusal error, got: %v", err)
	}
	// Normal prose explanation must NOT false-positive
	res, err := InterpretReply("I will now summarize the database audit findings for the past month.", sampleReproTools(), "auto")
	if err != nil || res != nil {
		t.Fatalf("expected normal prose to pass through without error, got res=%+v err=%v", res, err)
	}
}

func TestDetectsUpstreamToolCallBudgetNotice(t *testing.T) {
	refusals := []string{
		"You have exceeded the maximum number of tool and search calls (0 remaining).",
		"You have reached the maximum number of tool calls.",
		"0 tool calls remaining.",
		"<prohibit_tool_calling>",
	}
	for _, r := range refusals {
		if !IsToolProtocolRefusal(r) {
			t.Errorf("expected refusal detection for %q", r)
		}
	}

	legitimate := []string{
		"Listo: ejecuté las 3 llamadas a herramientas y todas respondieron bien.",
		"The kernel is Linux and the directory exists.",
		"I called the tool twice and both results were empty.",
	}
	for _, a := range legitimate {
		if IsToolProtocolRefusal(a) {
			t.Errorf("false positive for %q", a)
		}
	}
}

func TestDetectsToolCallGuardInsideLongReply(t *testing.T) {
	long := strings.Repeat("Contexto irrelevante sobre el trabajo realizado. ", 60) +
		"<prohibit_tool_calling>\nYou have exceeded the maximum number of tool and search calls (0 remaining).\n" +
		"Synthesize your final response to the user based on the information you have gathered.\n</prohibit_tool_calling>"
	if len(long) <= 1200 {
		t.Fatalf("fixture must exceed the short-reply guard, got %d bytes", len(long))
	}
	if !IsToolProtocolRefusal(long) {
		t.Error("expected guard marker to be detected inside a long reply")
	}

	benign := strings.Repeat("Ejecuté las comprobaciones y todas respondieron correctamente. ", 40)
	if IsToolProtocolRefusal(benign) {
		t.Error("false positive on a long benign reply")
	}
}

func TestInterpretReplyTreatsUpstreamGuardAsRetryableToolError(t *testing.T) {
	guard := "<prohibit_tool_calling>\nYou have exceeded the maximum number of tool and search calls (0 remaining).\nSynthesize your final response to the user based on the information you have gathered.\n</prohibit_tool_calling>"
	wrapped := strings.Repeat("Reporte previo del agente sobre lo ejecutado. ", 40) + guard
	for _, reply := range []string{guard, wrapped} {
		if _, err := InterpretReply(reply, sampleReproTools(), "auto"); err == nil {
			t.Fatalf("expected a retryable tool error for guard reply of %d bytes", len(reply))
		} else if !strings.Contains(err.Error(), "refused the tool-calling protocol") {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if _, err := InterpretReply("Todo listo: las comprobaciones respondieron correctamente.", sampleReproTools(), "auto"); err != nil {
		t.Fatalf("plain answer must not error: %v", err)
	}
}

func TestStripToolGuardRemovesInventedBudgetNotice(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "xml guard block",
			in:   "Listo.\n<prohibit_tool_calling>\nYou have exceeded the maximum number of tool and search calls (0 remaining).\nSynthesize your final response.\n</prohibit_tool_calling>",
			want: "Listo.",
		},
		{
			name: "bare sentence",
			in:   "Reporte completo.\nYou have exceeded the maximum number of tool and search calls (0 remaining).",
			want: "Reporte completo.",
		},
		{
			name: "zero remaining line",
			in:   "Sin cambios.\n0 tool calls remaining.",
			want: "Sin cambios.",
		},
		{
			name: "clean text untouched",
			in:   "Ejecuté las tres comprobaciones y todas respondieron bien.",
			want: "Ejecuté las tres comprobaciones y todas respondieron bien.",
		},
	}
	for _, c := range cases {
		if got := StripToolGuard(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestInterpretReplyUsesSharedRefusalErrorText(t *testing.T) {
	_, err := InterpretReply("<prohibit_tool_calling>\nYou have exceeded the maximum number of tool and search calls (0 remaining).\n</prohibit_tool_calling>", sampleReproTools(), "auto")
	if err == nil {
		t.Fatal("expected a retryable tool error")
	}
	if !strings.Contains(err.Error(), ToolProtocolRefusalError) {
		t.Fatalf("expected the shared refusal text %q, got %v", ToolProtocolRefusalError, err)
	}
}
