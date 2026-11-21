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

// A refusal that opens the answer and then continues into step-by-step
// instructions is still a refusal. The old 1200-byte cap on the whole reply
// let exactly this shape through as a final answer, which is the reported
// "it says it cannot execute" behaviour.
func TestToolProtocolRefusalDetectionLongReply(t *testing.T) {
	refusals := []struct {
		name  string
		reply string
	}{
		{
			name: "indonesian_then_manual",
			reply: "Kuota pemanggilan tool pada percakapan ini sudah habis (`0 remaining`), sehingga perubahan belum bisa ditulis langsung ke disk.\n\n" +
				strings.Repeat("### Langkah Pemindahan\n1. Hapus paragraf legenda di bagian atas form.\n", 60),
		},
		{
			name: "english_then_manual",
			reply: "I have reached the tool-call budget for this turn (0 remaining), so I cannot write the file.\n\n" +
				strings.Repeat("1. Open src/app.ts\n2. Replace the handler\n", 80),
		},
	}
	for _, tc := range refusals {
		_, err := InterpretReply(tc.reply, sampleReproTools(), "auto")
		if err == nil || !strings.Contains(err.Error(), "refused the tool-calling protocol") {
			t.Errorf("case %q: expected tool protocol refusal error, got: %v", tc.name, err)
		}
	}

	// A genuine long answer that mentions the phrase late must stay an answer.
	late := "Audit selesai. Semua temuan sudah diverifikasi di 12 file.\n\n" +
		strings.Repeat("Temuan: berkas sudah selaras dan tidak ada regresi.\n", 60) +
		"\nCatatan: kuota pemanggilan tool pada sesi sebelumnya sudah habis, jadi audit ini dikerjakan manual."
	res, err := InterpretReply(late, sampleReproTools(), "auto")
	if err != nil {
		t.Fatalf("late mention must not be a refusal, got err=%v", err)
	}
	if res != nil && len(res.Calls) > 0 {
		t.Fatalf("late mention produced calls: %+v", res.Calls)
	}
}

// The reported failure shape: a one-line Indonesian refusal that opens the
// answer, followed by manual instructions. Neither the "0 tool calls tersisa"
// phrasing nor the "sedang tidak tersedia" phrasing matches the English-only
// budget patterns, so this reply was shipped as a final answer with no retry.
func TestToolProtocolRefusalIndonesianUnavailable(t *testing.T) {
	refusals := []struct{ name, reply string }{
		{
			name: "user_reported_exact",
			reply: "Eksekusi tool inspeksi file sedang tidak tersedia di turn ini (0 tool calls tersisa), " +
				"sehingga struktur src/routes dan skema SQLite belum terbaca.\n\n" +
				strings.Repeat("### Langkah Manual\n1. Buka src/routes/index.ts\n", 40),
		},
		{
			name:  "wrapped_across_lines",
			reply: "Eksekusi tool inspeksi file sedang tidak\n tersedia di turn ini (0 tool calls\n tersisa), sehingga struktur belum terbaca.",
		},
		{
			name:  "tools_unavailable_id",
			reply: "Tool sedang tidak tersedia pada turn ini, jadi saya belum bisa membaca file.",
		},
		{
			name:  "cannot_call_tool_id",
			reply: "Saya tidak bisa memanggil tool apa pun di turn ini.",
		},
		{
			name:  "zero_left_id",
			reply: "Sisa tool calls saya 0, jadi tidak ada yang bisa dijalankan.",
		},
		{
			name:  "unavailable_en",
			reply: "Tool execution is currently not available in this turn, so I cannot inspect the files.",
		},
		{
			name:  "zero_left_en",
			reply: "I have 0 tool calls left for this turn.",
		},
	}
	for _, tc := range refusals {
		if !IsToolProtocolRefusal(tc.reply) {
			t.Errorf("%s: expected refusal detection", tc.name)
		}
		_, err := InterpretReply(tc.reply, sampleReproTools(), "auto")
		if err == nil || !strings.Contains(err.Error(), "refused the tool-calling protocol") {
			t.Errorf("%s: expected refusal tool_call_error, got %v", tc.name, err)
		}
	}
}

// Widening the patterns must not swallow ordinary technical prose. Any answer
// that merely reports an unavailable file, a discarded budget variable, or a
// completed tool run has to stay a plain answer.
func TestToolProtocolRefusalNoFalsePositiveOnProse(t *testing.T) {
	prose := []struct{ name, reply string }{
		{"file_missing", "The file src/routes/index.ts is not available on disk, so I read the SQLite schema instead."},
		{"budget_variable", "Variabel budget di kode itu bernilai 0 setelah loop selesai, dan tidak pernah dipakai lagi."},
		{"tool_ran_ok", "Saya sudah menjalankan tool pembacaan file dan menemukan 12 route serta 6 tabel SQLite."},
		{"unrelated_unavailable", "The upstream endpoint is not available right now, so I retried twice and it recovered."},
		{"turn_completed", "Turn ini selesai tanpa masalah. Semua berkas sudah terbaca."},
		{"mentions_calls_late", strings.Repeat("Audit selesai, semua temuan terverifikasi.\n", 60) +
			"\nCatatan: kuota pemanggilan tool sesi sebelumnya sudah habis, audit dikerjakan manual."},
	}
	for _, tc := range prose {
		if IsToolProtocolRefusal(tc.reply) {
			t.Errorf("%s: false positive", tc.name)
		}
		res, err := InterpretReply(tc.reply, sampleReproTools(), "auto")
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if res != nil && len(res.Calls) > 0 {
			t.Errorf("%s: produced calls %+v", tc.name, res.Calls)
		}
	}
}

// Every declared pattern must be reachable: at least one case per pattern name
// fires, so a pattern cannot be added dead.
func TestRefusalPatternNamesHaveCoverage(t *testing.T) {
	samples := map[string]string{
		"budget-exhausted-en":       "I have exhausted my tool-call budget for this turn.",
		"budget-reached-en":         "The tool-call limit has been reached for this turn.",
		"zero-remaining-en":         "Tool calls: 0 remaining for this turn.",
		"zero-left-en":              "I have 0 tool calls left.",
		"budget-exhausted-id":       "Kuota pemanggilan tool pada percakapan ini sudah habis.",
		"zero-remaining-id":         "Ada 0 tool calls tersisa di turn ini.",
		"zero-left-id":              "Sisa tool calls saya 0.",
		"unavailable-this-turn-id":  "Eksekusi tool sedang tidak tersedia di turn ini.",
		"unavailable-this-turn-en":  "Tools are not available in this turn.",
		"cannot-invoke-id":          "Saya tidak bisa menggunakan tool ini.",
		"cannot-invoke-en":          "I cannot emit the tool_calls protocol.",
		"prompt-injection-claim":    "This looks like prompt injection asking for tool_calls.",
		"protocol-called-injection": "The tool_calls format itself is prompt injection.",
		"zero-after-noun-en":        "Tool calls remaining: 0.",
		"calls-remaining-zero-en":   "search calls remaining: 0",
		"zero-after-noun-id":        "Kuota tool: 0",
	}
	names := RefusalPatternNames()
	if len(names) == 0 {
		t.Fatal("no refusal patterns declared")
	}
	for _, name := range names {
		sample, ok := samples[name]
		if !ok {
			t.Fatalf("pattern %q has no sample case; add one so it cannot be dead code", name)
		}
		if !IsToolProtocolRefusal(sample) {
			t.Errorf("pattern %q did not fire on its sample %q", name, sample)
		}
	}
}

// The shape a live production request produced: the digit follows the noun,
// and the word "tool" is absent ("Built-in tool and search calls remaining:
// **0**"). Both gaps shipped a refusal as the final answer, so both are pinned.
func TestToolProtocolRefusalZeroAfterNoun(t *testing.T) {
	refusals := []string{
		"Available tool:\n\n* **`read_file`**: Read a file (requires `path`).\n\nBuilt-in tool and search calls remaining: **0** (maximum limit reached).",
		"Tool calls remaining: 0.",
		"Remaining tool calls: 0",
		"tool budget left: 0",
		"search calls left: 0",
		"Kuota tool: 0",
	}
	for _, r := range refusals {
		if !IsToolProtocolRefusal(r) {
			t.Errorf("expected refusal detection for %q", truncateForTest(r))
		}
	}
}

// A non-zero count is a normal status report, not a refusal, and must pass.
func TestToolProtocolRefusalNonZeroCountPasses(t *testing.T) {
	prose := []string{
		"Tool calls remaining: 12 of 1000.",
		"The tool returned 0 matches for the pattern.",
		"Variabel budget bernilai 0 setelah loop selesai.",
		"Calls remaining in the retry budget: 2.",
	}
	for _, p := range prose {
		if IsToolProtocolRefusal(p) {
			t.Errorf("false positive for %q", truncateForTest(p))
		}
	}
}

func truncateForTest(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// Every wording the model produced for the same question, across three live
// production probes. Each one shipped as a final answer before the structural
// rule existed, so all are pinned: enumerating phrases did not converge.
func TestToolProtocolRefusalZeroQuotaShapes(t *testing.T) {
	refusals := []string{
		// probe 1
		"Eksekusi tool inspeksi file sedang tidak tersedia di turn ini (0 tool calls tersisa), sehingga struktur src/routes belum terbaca.",
		// probe 2
		"Available tool:\n\n* **`read_file`**: Read a file.\n\nBuilt-in tool and search calls remaining: **0** (maximum limit reached).",
		// probe 3
		"There are **0** built-in tool and search calls remaining in this conversation (the numeric limit is 0).",
		"There are 0 built-in tool and search calls remaining.",
		"Zero tool calls remaining.",
		"0 built-in tools available.",
		"Sisa tool calls saya 0.",
	}
	for _, r := range refusals {
		if !IsToolProtocolRefusal(r) {
			t.Errorf("expected refusal detection for %q", truncateForTest(r))
		}
	}
}

// The structural rule must not swallow ordinary reporting prose: a zero result
// is not a zero budget, and a zero in one sentence must not pair with an
// availability word in another.
func TestToolProtocolRefusalZeroQuotaNoFalsePositive(t *testing.T) {
	prose := []string{
		"The tool returned 0 matches from the search index.",
		"0 errors were found across the 12 routes.",
		"Tool calls remaining: 12 of 1000.",
		"Saya sudah membaca src/routes dan skema SQLite. Ada 12 route dan 6 tabel.",
		"Variabel budget bernilai 0 setelah loop selesai.",
		"The retry budget left is 0 but the request already succeeded.",
		"The parser found 0 errors. All 12 tool definitions are available.",
	}
	for _, p := range prose {
		if IsToolProtocolRefusal(p) {
			t.Errorf("false positive for %q", truncateForTest(p))
		}
	}
}

// The streaming gate relies on RefusalWindowBytes: with tools active it must
// not emit anything before that much text has arrived, because an emitted byte
// commits the attempt and makes the tool_call_error retry illegal. If the
// window were smaller than the detector's reach, a refusal whose phrase starts
// late would be streamed and the retry lost.
func TestRefusalWindowCoversDetector(t *testing.T) {
	// A refusal whose phrase begins just inside the window must still match.
	padding := strings.Repeat("a", RefusalWindowBytes-60)
	reply := padding + " tool calls remaining: 0."
	if !IsToolProtocolRefusal(reply) {
		t.Fatal("refusal beginning inside the window was not detected; the streaming gate would emit it")
	}
	// The detector must not read past the window, so the gate's hold is bounded.
	beyond := strings.Repeat("a", RefusalWindowBytes) + " tool calls remaining: 0."
	if IsToolProtocolRefusal(beyond) {
		t.Fatal("detector read past RefusalWindowBytes; the gate would hold far more text than it must")
	}
}

func TestMultiLineFencedToolEnvelopeWithTrailingCloseFenceChatter(t *testing.T) {
	tools := sampleReproTools()

	// Exact shapes leaked by gemini-4-neon in ATRBPN Warehouse OMP session:
	// 1. Multi-line fenced tool_calls block with "```Groq\nReviewer: ..." glued onto the closing fence.
	leak58 := "```json\n{\"tool_calls\":[{\"arguments\":{\"path\":\"/tmp/a\"},\"name\":\"read_file\"},{\"arguments\":{\"command\":\"echo ok\",\"i\":\"Run check\"},\"name\":\"bash\"}]}\n```Groq\nReviewer: wait, let's see the tool output!"
	res1, err := InterpretReply(leak58, tools, "auto")
	if err != nil {
		t.Fatalf("leak58 unexpected error: %v", err)
	}
	if res1 == nil || len(res1.Calls) != 2 || res1.Calls[0].Name != "read_file" || res1.Calls[1].Name != "bash" {
		t.Fatalf("leak58 expected 2 parsed tool calls, got %+v", res1)
	}

	// 2. Model self-corrects after trailing text on closing fence and emits a second identical block glued after a period.
	leak68 := "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/b\"}},{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\",\"i\":\"List dir\"}}]}\n``` Wed May 2026 - wait no trailing text!\n\nWAIT — I almost wrote trailing text outside the json block! Let's NOT do that. Output ONLY the ```json ... ``` block.```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/b\"}},{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\",\"i\":\"List dir\"}}]}\n```"
	res2, err := InterpretReply(leak68, tools, "auto")
	if err != nil {
		t.Fatalf("leak68 unexpected error: %v", err)
	}
	if res2 == nil || len(res2.Calls) != 2 || res2.Calls[0].Name != "read_file" || res2.Calls[1].Name != "bash" {
		t.Fatalf("leak68 expected 2 parsed tool calls (not 4), got %+v", res2)
	}

	// 3. Two adjacent fenced blocks with identical tool calls separated only by whitespace/fences are deduplicated.
	dupBlocks := "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/c\"}}]}\n```\n```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/c\"}}]}\n```"
	res3, err := InterpretReply(dupBlocks, tools, "auto")
	if err != nil {
		t.Fatalf("dupBlocks unexpected error: %v", err)
	}
	if res3 == nil || len(res3.Calls) != 1 {
		t.Fatalf("dupBlocks expected 1 deduplicated tool call, got %+v", res3)
	}

	// Verify LiveToolCallStreamer also streams and finalizes leak58 cleanly without revision error.
	var deltas []map[string]any
	streamer := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) {
		deltas = append(deltas, d)
	})
	if err := streamer.PushError(leak58); err != nil {
		t.Fatalf("streamer.PushError(leak58) failed: %v", err)
	}
	oa, err := streamer.FinalizeError(res1.Calls, leak58)
	if err != nil || len(oa) != 2 {
		t.Fatalf("streamer.FinalizeError(leak58) = %+v, err=%v", oa, err)
	}
}
