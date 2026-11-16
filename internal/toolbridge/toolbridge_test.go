package toolbridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

func sampleTools() []any {
	return []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "read_file",
				"description": "Read a file",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":  map[string]any{"type": "string"},
						"lines": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
					},
					"required":             []any{"path"},
					"additionalProperties": false,
				},
			},
		},
	}
}

func TestToolInstructionsDoNotImposeArbitraryBatchLimits(t *testing.T) {
	builders := []struct {
		name  string
		build func(any, any) (string, error)
	}{
		{"instructions", BuildToolInstructions},
		{"reminder", BuildToolReminder},
		{"continuation", BuildContinuationToolReminder},
	}
	for _, b := range builders {
		t.Run(b.name, func(t *testing.T) {
			got, err := b.build(sampleTools(), "auto")
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"1–2", "2–4", "3+"} {
				if strings.Contains(got, forbidden) {
					t.Errorf("unexpected batch restriction %q", forbidden)
				}
			}
		})
	}
}

func TestValidateToolRequestAndInstructions(t *testing.T) {
	instr, err := BuildToolInstructions(sampleTools(), "none")
	if err != nil || instr != "" {
		t.Fatalf("expected empty instructions for tool_choice=none, got %q, err=%v", instr, err)
	}

	_, err = ValidateToolRequest("not-an-array", "auto")
	if err == nil {
		t.Fatal("expected error for non-array tools")
	}

	instrReq, err := BuildToolInstructions(sampleTools(), "required")
	if err != nil || !strings.Contains(instrReq, "You MUST call at least one tool") {
		t.Fatalf("expected required tool rule in instructions, got %q, err=%v", instrReq, err)
	}
}

func TestInterpretReplyVariantsAndSchemaValidation(t *testing.T) {
	tools := sampleTools()

	// 1. Fenced bare call
	fencedBare := "Let me check that file:\n```json\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/a\"}}\n```"
	res1, err := InterpretReply(fencedBare, tools, "auto")
	if err != nil || res1 == nil || len(res1.Calls) != 1 || res1.Calls[0].Name != "read_file" {
		t.Fatalf("unexpected fenced bare result: %+v, err=%v", res1, err)
	}
	if res1.Text != "Let me check that file:" {
		t.Fatalf("expected preamble preserved in InterpretResult.Text, got %q", res1.Text)
	}

	// 2. XML-wrapped bare call
	xmlBare := "Checking now:\n<tool_call>\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/b\"}}\n</tool_call>"
	res2, err := InterpretReply(xmlBare, tools, "auto")
	if err != nil || res2 == nil || len(res2.Calls) != 1 || res2.Calls[0].Arguments["path"] != "/tmp/b" {
		t.Fatalf("unexpected xml bare result: %+v, err=%v", res2, err)
	}

	// 3. Sanitized HTML pipe & unescaped quotes in tool_calls JSON
	sanitized := "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"<Foo Bar |>\"}}]}\n```"
	res3, err := InterpretReply(sanitized, tools, "auto")
	if err != nil || res3 == nil || res3.Calls[0].Arguments["path"] != "<Foo | Bar>" {
		t.Fatalf("expected <Foo | Bar> repair, got %+v, err=%v", res3, err)
	}

	// 4. Unknown tool rejected with 502 tool_call_error
	_, err = InterpretReply(`{"tool_calls":[{"name":"erase_all","arguments":{}}]}`, tools, "auto")
	if err == nil || !strings.Contains(err.Error(), `not among the tools`) {
		t.Fatalf("expected unknown tool error, got %v", err)
	}

	// 5. Schema violation (missing required path or out of bounds integer)
	_, err = InterpretReply(`{"tool_calls":[{"name":"read_file","arguments":{"path":"/tmp/x","lines":500}}]}`, tools, "auto")
	if err == nil || !strings.Contains(err.Error(), "violate the tool schema") {
		t.Fatalf("expected schema violation error, got %v", err)
	}

	// 6. Required tool missing -> 502 tool_call_error
	_, err = InterpretReply("Plain text answer without calling any tool.", tools, "required")
	if err == nil || !strings.Contains(err.Error(), "did not return the required tool call") {
		t.Fatalf("expected required tool call error, got %v", err)
	}
}

func TestAntiLeakToolCallShapesAndRepairs(t *testing.T) {
	tools := sampleTools()

	// 1. Top-level JSON array of calls: [{"name": "read_file", "arguments": {...}}]
	arrReply := "```json\n[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/arr\"}}]\n```"
	resArr, err := InterpretReply(arrReply, tools, "auto")
	if err != nil || resArr == nil || len(resArr.Calls) != 1 || resArr.Calls[0].Arguments["path"] != "/tmp/arr" {
		t.Fatalf("expected top-level array call to parse, got %+v err=%v", resArr, err)
	}

	// 2. Alternative envelope and argument keys ("tool_call", "tool", "input") + trailing comma + multiline string
	altReply := "I will read it now:\n```json\n{\"tool_call\":{\"tool\":\"read_file\",\"input\":{\"path\":\"/tmp/line1\\nline2\",},}}\n```\nDone."
	resAlt, err := InterpretReply(altReply, tools, "auto")
	if err != nil || resAlt == nil || len(resAlt.Calls) != 1 || resAlt.Calls[0].Name != "read_file" {
		t.Fatalf("expected alt envelope + trailing comma repair to parse, got %+v err=%v", resAlt, err)
	}

	// 3. Bare call surrounded by preamble and postamble without fences (recognized because tool name is in allow)
	bareSurrounded := "Let me check that file first.\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/bare\"}}\nI will inspect the output next."
	resBare, err := InterpretReply(bareSurrounded, tools, "auto")
	if err != nil || resBare == nil || len(resBare.Calls) != 1 || resBare.Calls[0].Arguments["path"] != "/tmp/bare" {
		t.Fatalf("expected bare call with postamble to be captured instead of leaking, got %+v err=%v", resBare, err)
	}

	// 4. SafeStreamablePrefix holds back short text and tool calls, but streams long plain prose
	if got := SafeStreamablePrefix(`{"tool_calls":[{"name":"read_file"}]}`); got != "" {
		t.Fatalf("expected empty prefix for JSON tool call, got %q", got)
	}
	longProse := strings.Repeat("This is a clean plain-text final summary sentence with no tool calls. ", 8)
	safe := SafeStreamablePrefix(longProse)
	if safe == "" || len(safe) >= len(longProse) || !strings.HasPrefix(longProse, safe) {
		t.Fatalf("expected non-empty safe prefix shorter than full text, got len=%d (full=%d)", len(safe), len(longProse))
	}
}

func TestProseExamplesSpacedStreamingAndSchemaRefEnforcement(t *testing.T) {
	tools := sampleTools()

	// 1. Inline prose examples must never be executed as tool calls
	for _, example := range []string{
		`Here is an example, do not execute: {"name":"read_file","arguments":{"path":"/tmp/example"}}`,
		`The envelope format is: {"tool_calls":[{"name":"read_file","arguments":{"path":"/tmp/example"}}]} — send it to the harness.`,
	} {
		res, err := InterpretReply(example, tools, "auto")
		if err != nil || res != nil {
			t.Fatalf("expected inline prose example not to trigger tool call, got res=%+v err=%v", res, err)
		}
	}

	// 2. Spaced JSON tool call after preamble must never leak through SafeStreamablePrefix
	spaced := `Preamble text before tool call: { "name": "read_file", "arguments": {"path": "` + strings.Repeat("x", 500) + `"}}`
	if got := SafeStreamablePrefix(spaced); got != "" {
		t.Fatalf("expected SafeStreamablePrefix to hold back spaced tool call JSON, got %q", got)
	}

	// 3. additionalProperties: false without properties and $ref validation
	noPropsSchema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
	}
	if msg := CheckSchema(noPropsSchema, map[string]any{"unexpected": true}, "arguments"); msg == "" {
		t.Fatal("expected additionalProperties:false without properties to reject unexpected key")
	}

	refSchema := map[string]any{
		"$ref": "#/$defs/allowed",
		"$defs": map[string]any{
			"allowed": map[string]any{
				"const": "safe",
			},
		},
	}
	if msg := CheckSchema(refSchema, "unsafe", "arguments"); msg == "" {
		t.Fatal("expected $ref schema constraint violation to be rejected")
	}
	if msg := CheckSchema(refSchema, "safe", "arguments"); msg != "" {
		t.Fatalf("expected valid $ref value to pass, got %q", msg)
	}
}

func TestLeadingSpaceKeysAndSchemaInferredToolName(t *testing.T) {
	tools := []any{
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
				"name": "write",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"content": map[string]any{"type": "string"},
						"i":       map[string]any{"type": "string"},
					},
					"required": []any{"path", "i"},
				},
			},
		},
	}

	// Exact SentencePiece leading-space key reproduction: {" name":"bash", ...}
	raw := `{"tool_calls":[{" name":"bash","arguments":{"command":"npm install","i":"Installing npm dependencies"}},{"name":"write","arguments":{"content":"<!doctype html>\n<html></html>\n","i":"Writing index.html","path":"index.html"}}]}`
	res, err := InterpretReply(raw, tools, "auto")
	if err != nil || res == nil || len(res.Calls) != 2 {
		t.Fatalf("expected 2 parsed tool calls from leading-space key payload, got res=%+v err=%v", res, err)
	}
	if res.Calls[0].Name != "bash" || res.Calls[0].Arguments["command"] != "npm install" {
		t.Fatalf("unexpected first call: %+v", res.Calls[0])
	}
	if res.Calls[1].Name != "write" || res.Calls[1].Arguments["path"] != "index.html" {
		t.Fatalf("unexpected second call: %+v", res.Calls[1])
	}

	// Missing "name" on 2nd call inferred from schema + flat arguments on 1st call
	inferredRaw := `{"tool_calls":[{"name":"bash","command":"npm test","i":"Running tests"},{"path":"src/app.ts","content":"console.log(1);","i":"Writing app.ts"}]}`
	resInf, err := InterpretReply(inferredRaw, tools, "auto")
	if err != nil || resInf == nil || len(resInf.Calls) != 2 {
		t.Fatalf("expected 2 parsed tool calls from flat + schema-inferred payload, got res=%+v err=%v", resInf, err)
	}
	if resInf.Calls[0].Name != "bash" || resInf.Calls[1].Name != "write" {
		t.Fatalf("expected bash and write, got %+v", resInf.Calls)
	}
}

func TestLiveToolCallStreamerIncrementalAndMultiCall(t *testing.T) {
	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "write_file",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"content": map[string]any{"type": "string"},
					},
					"required": []any{"path", "content"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
					"required": []any{"path"},
				},
			},
		},
	}

	var deltas []map[string]any
	streamer := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) {
		deltas = append(deltas, d)
	})

	frame1 := "```json\n{\"tool_calls\":[{\"name\":\"write_file\",\"arguments\":{\"path\":\"src/scene.ts\",\"content\":\"import * as THREE from 'three';\\nexport class SceneManager {\\n  private scene: THREE.Scene;\\n  constructor() {\\n    this.scene = new THREE.Scene();"
	frame2 := frame1 + "\\n    console.log('initialized');\\n  }\\n}\"}},{\"name\":\"read_file\",\"arguments\":{\"path\":\"package.json\"}}"
	frame3 := frame2 + "]}\n```"

	streamer.Push(frame1)
	if len(deltas) < 2 {
		t.Fatalf("expected header + live partial arguments on frame1, got %d deltas: %+v", len(deltas), deltas)
	}
	deltasAfterFrame1 := len(deltas)

	streamer.Push(frame2)
	if len(deltas) <= deltasAfterFrame1 {
		t.Fatalf("expected additional deltas on frame2 as call 0 finishes and call 1 completes, got %d", len(deltas))
	}

	res, err := InterpretReply(frame3, tools, "auto")
	if err != nil {
		t.Fatalf("InterpretReply failed: %v", err)
	}
	oa := streamer.Finalize(res.Calls, frame3)
	if len(oa) != 2 {
		t.Fatalf("expected 2 finalized OpenAI tool calls, got %d", len(oa))
	}

	// Reconstruct arguments per tool index from streamed deltas
	reconstructed := map[int]string{}
	names := map[int]string{}
	for _, d := range deltas {
		tcs, _ := d["tool_calls"].([]map[string]any)
		for _, tc := range tcs {
			idx := tc["index"].(int)
			fn := tc["function"].(map[string]any)
			if n, ok := fn["name"].(string); ok && n != "" {
				names[idx] = n
			}
			if a, ok := fn["arguments"].(string); ok {
				reconstructed[idx] += a
			}
		}
	}
	if names[0] != "write_file" || names[1] != "read_file" {
		t.Fatalf("unexpected streamed tool names: %+v", names)
	}
	if reconstructed[0] != oa[0].Function.Arguments {
		t.Fatalf("call 0 streamed args mismatch:\ngot:  %s\nwant: %s", reconstructed[0], oa[0].Function.Arguments)
	}
	if reconstructed[1] != oa[1].Function.Arguments {
		t.Fatalf("call 1 streamed args mismatch:\ngot:  %s\nwant: %s", reconstructed[1], oa[1].Function.Arguments)
	}
}

func TestLiveToolCallStreamerCompletedCallRevision(t *testing.T) {
	tools := sampleTools()
	complete := `{"tool_calls":[{"name":"read_file","arguments":{"path":"/tmp/a"}}]}`

	assertRevision := func(t *testing.T, err error) {
		t.Helper()
		var ue *transport.UpstreamError
		if !errors.As(err, &ue) || ue.Code != "upstream_revision" {
			t.Fatalf("expected upstream_revision error, got %v", err)
		}
	}

	t.Run("name change after completion", func(t *testing.T) {
		multiTools := append(sampleTools(), map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "write_file",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
					"required": []any{"path"},
				},
			},
		})
		var deltas []map[string]any
		streamer := NewLiveToolCallStreamer(multiTools, "auto", func(d map[string]any) {
			deltas = append(deltas, d)
		})
		if err := streamer.PushError(complete); err != nil {
			t.Fatalf("initial push failed: %v", err)
		}
		changedName := `{"tool_calls":[{"name":"write_file","arguments":{"path":"/tmp/a"}}]}`
		if err := streamer.PushError(changedName); err == nil {
			t.Fatal("expected revision error for changed tool name")
		} else {
			assertRevision(t, err)
		}
		oa, err := streamer.FinalizeError([]Call{{Name: "write_file", Arguments: map[string]any{"path": "/tmp/a"}}}, "")
		if err == nil || oa != nil {
			t.Fatalf("expected finalize to reject renamed call, got oa=%+v err=%v", oa, err)
		}
		assertRevision(t, err)
	})

	t.Run("argument change after completion", func(t *testing.T) {
		var deltas []map[string]any
		streamer := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) {
			deltas = append(deltas, d)
		})
		if err := streamer.PushError(complete); err != nil {
			t.Fatalf("initial push failed: %v", err)
		}
		changed := `{"tool_calls":[{"name":"read_file","arguments":{"path":"/tmp/b"}}]}`
		err := streamer.PushError(changed)
		assertRevision(t, err)

		// The authoritative completion must also refuse to return the stale streamed args.
		oa, err := streamer.FinalizeError([]Call{{Name: "read_file", Arguments: map[string]any{"path": "/tmp/b"}}}, "")
		if err == nil || oa != nil {
			t.Fatalf("expected finalize to reject revised call, got oa=%+v err=%v", oa, err)
		}
		assertRevision(t, err)

		// The legacy signature cannot report the revision, but it must still return the
		// authoritative arguments rather than the obsolete streamed bytes.
		compat := streamer.Finalize([]Call{{Name: "read_file", Arguments: map[string]any{"path": "/tmp/b"}}}, "")
		if len(compat) != 1 || compat[0].Function.Arguments != `{"path":"/tmp/b"}` {
			t.Fatalf("compatibility Finalize returned stale arguments: %+v", compat)
		}
	})

	t.Run("revision before any emission is not an error", func(t *testing.T) {
		var deltas []map[string]any
		streamer := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) {
			deltas = append(deltas, d)
		})
		// Only a prefix that cannot emit a header yet.
		if err := streamer.PushError(`{"tool_calls":[{"name":"read_file","argum`); err != nil {
			t.Fatalf("expected incomplete prefix to be ignored, got %v", err)
		}
		if len(deltas) != 0 {
			t.Fatalf("expected no deltas before schema-safe validation, got %+v", deltas)
		}
		if err := streamer.PushError(complete); err != nil {
			t.Fatalf("expected completed call to be accepted after unemitted prefix, got %v", err)
		}
	})
}

func TestLiveToolCallStreamerWhitespaceAndKeyOrderEquivalence(t *testing.T) {
	tools := sampleTools()
	var deltas []map[string]any
	streamer := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) {
		deltas = append(deltas, d)
	})

	first := `{"tool_calls":[{"name":"read_file","arguments":{"path":"/tmp/a","lines":2}}]}`
	// Same parsed value, different whitespace and key order. This must not be a revision.
	reordered := "{\n  \"tool_calls\" : [ {\n    \"arguments\" : { \"lines\" : 2, \"path\" : \"/tmp/a\" },\n    \"name\" : \"read_file\"\n  } ]\n}"
	if err := streamer.PushError(first); err != nil {
		t.Fatalf("initial push failed: %v", err)
	}
	if err := streamer.PushError(reordered); err != nil {
		t.Fatalf("whitespace/key-order equivalent call was treated as a revision: %v", err)
	}

	oa, err := streamer.FinalizeError([]Call{{Name: "read_file", Arguments: map[string]any{"path": "/tmp/a", "lines": 2}}}, reordered)
	if err != nil {
		t.Fatalf("finalize rejected equivalent parsed arguments: %v", err)
	}
	if len(oa) != 1 || oa[0].Function.Arguments != `{"lines":2,"path":"/tmp/a"}` {
		t.Fatalf("unexpected canonical finalization: %+v", oa)
	}
	if !json.Valid([]byte(oa[0].Function.Arguments)) {
		t.Fatalf("final arguments are not valid JSON: %s", oa[0].Function.Arguments)
	}

	reconstructed := reconstructStreamedArguments(deltas, 0)
	if reconstructed != oa[0].Function.Arguments {
		t.Fatalf("streamed bytes diverged from canonical finalization:\nstreamed: %s\nfinal:    %s", reconstructed, oa[0].Function.Arguments)
	}
}

func TestLiveToolCallStreamerPartialPrefixMismatch(t *testing.T) {
	tools := sampleTools()

	t.Run("emitted byte changes", func(t *testing.T) {
		longPath := strings.Repeat("a", 160)
		frame1 := `{"tool_calls":[{"name":"read_file","arguments":{"path":"` + longPath
		frame2 := `{"tool_calls":[{"name":"read_file","arguments":{"path":"` + "b" + longPath[1:]

		var deltas []map[string]any
		streamer := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) {
			deltas = append(deltas, d)
		})
		if err := streamer.PushError(frame1); err != nil {
			t.Fatalf("initial partial push failed: %v", err)
		}
		reconstructed := reconstructStreamedArguments(deltas, 0)
		if reconstructed == "" {
			t.Fatal("expected schema-safe partial arguments to be emitted")
		}

		err := streamer.PushError(frame2)
		var ue *transport.UpstreamError
		if !errors.As(err, &ue) || ue.Code != "upstream_revision" {
			t.Fatalf("expected upstream_revision for non-monotonic partial prefix, got %v", err)
		}

		// No second JSON object may be appended to the already-emitted partial object.
		reconstructed = reconstructStreamedArguments(deltas, 0)
		if strings.Count(reconstructed, "{") != 1 {
			t.Fatalf("expected exactly one partial JSON object, got %q", reconstructed)
		}
		if strings.Contains(reconstructed, "b"+longPath[1:]) {
			t.Fatalf("revised partial bytes leaked into the stream: %q", reconstructed)
		}

		oa, err := streamer.FinalizeError([]Call{{Name: "read_file", Arguments: map[string]any{"path": "b" + longPath[1:]}}}, "")
		if err == nil || oa != nil {
			t.Fatalf("expected finalize to fail closed after prefix mismatch, got oa=%+v err=%v", oa, err)
		}
	})
}

func TestLiveToolCallStreamerCanonicalFinalization(t *testing.T) {
	tools := sampleTools()

	t.Run("header only then authoritative canonical arguments", func(t *testing.T) {
		var deltas []map[string]any
		streamer := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) {
			deltas = append(deltas, d)
		})

		// Header is safe, but the arguments prefix is too short to emit raw JSON.
		if err := streamer.PushError(`{"tool_calls":[{"name":"read_file","arguments":{"path":"/tmp/a`); err != nil {
			t.Fatalf("partial push failed: %v", err)
		}
		if got := reconstructStreamedArguments(deltas, 0); got != "" {
			t.Fatalf("expected no raw partial arguments before schema-safe validation, got %q", got)
		}

		authoritative := []Call{{Name: "read_file", Arguments: map[string]any{"path": "/tmp/a"}}}
		oa, err := streamer.FinalizeError(authoritative, "")
		if err != nil {
			t.Fatalf("finalize failed: %v", err)
		}
		if len(oa) != 1 {
			t.Fatalf("expected one finalized call, got %+v", oa)
		}
		want := `{"path":"/tmp/a"}`
		if oa[0].Function.Arguments != want {
			t.Fatalf("expected canonical authoritative arguments %s, got %s", want, oa[0].Function.Arguments)
		}
		reconstructed := reconstructStreamedArguments(deltas, 0)
		if reconstructed != want {
			t.Fatalf("expected finalize to emit canonical arguments, streamed %q want %q", reconstructed, want)
		}
		if !json.Valid([]byte(reconstructed)) {
			t.Fatalf("finalized stream is malformed JSON: %q", reconstructed)
		}
	})

	t.Run("emitted partial then authoritative canonical tail", func(t *testing.T) {
		longPath := strings.Repeat("a", 160)
		partial := `{"tool_calls":[{"name":"read_file","arguments":{"path":"` + longPath
		var deltas []map[string]any
		streamer := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) {
			deltas = append(deltas, d)
		})
		if err := streamer.PushError(partial); err != nil {
			t.Fatalf("partial push failed: %v", err)
		}
		if got := reconstructStreamedArguments(deltas, 0); got == "" {
			t.Fatal("expected schema-safe partial arguments to be emitted")
		}

		authoritative := []Call{{Name: "read_file", Arguments: map[string]any{"path": longPath}}}
		oa, err := streamer.FinalizeError(authoritative, "")
		if err != nil {
			t.Fatalf("finalize failed: %v", err)
		}
		want := `{"path":"` + longPath + `"}`
		if len(oa) != 1 || oa[0].Function.Arguments != want {
			t.Fatalf("expected canonical authoritative arguments, got %+v", oa)
		}
		reconstructed := reconstructStreamedArguments(deltas, 0)
		if reconstructed != want {
			t.Fatalf("finalize did not close the streamed prefix canonically:\nstreamed: %s\nwant:     %s", reconstructed, want)
		}
		if !json.Valid([]byte(reconstructed)) {
			t.Fatalf("finalized stream is malformed JSON: %q", reconstructed)
		}
	})

	t.Run("authoritative change after partial emission fails closed", func(t *testing.T) {
		longPath := strings.Repeat("a", 160)
		var deltas []map[string]any
		streamer := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) {
			deltas = append(deltas, d)
		})
		if err := streamer.PushError(`{"tool_calls":[{"name":"read_file","arguments":{"path":"` + longPath); err != nil {
			t.Fatalf("partial push failed: %v", err)
		}
		oa, err := streamer.FinalizeError([]Call{{Name: "read_file", Arguments: map[string]any{"path": "b" + longPath[1:]}}}, "")
		if err == nil || oa != nil {
			t.Fatalf("expected finalize to fail closed on revised authoritative arguments, got oa=%+v err=%v", oa, err)
		}
		var ue *transport.UpstreamError
		if !errors.As(err, &ue) || ue.Code != "upstream_revision" {
			t.Fatalf("expected upstream_revision error, got %v", err)
		}
	})
}

func reconstructStreamedArguments(deltas []map[string]any, target int) string {
	var out strings.Builder
	for _, d := range deltas {
		tcs, _ := d["tool_calls"].([]map[string]any)
		for _, tc := range tcs {
			idx, _ := tc["index"].(int)
			if idx != target {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			if args, ok := fn["arguments"].(string); ok {
				out.WriteString(args)
			}
		}
	}
	return out.String()
}

func TestToolSchemaCompactionParity(t *testing.T) {
	defer SetCompactOptions(true, 400)

	longDesc := strings.Repeat("Detailed tool parameter specification with UTF-8 文字 descriptions. ", 15)
	tools := make([]any, 0, 30)
	for i := range 30 {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        fmt.Sprintf("tool_%02d", i),
				"description": longDesc,
				"parameters": map[string]any{
					"$schema":              "https://json-schema.org/draft/2020-12/schema",
					"$id":                  fmt.Sprintf("https://example.com/schemas/tool_%02d.json", i),
					"title":                fmt.Sprintf("Tool %02d Parameters", i),
					"$comment":             "Internal schema comment that models do not need",
					"type":                 "object",
					"additionalProperties": false,
					"examples":             []any{map[string]any{"query": "example query"}},
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"title":       "Query String",
							"description": longDesc,
							"examples":    []any{"foo", "bar"},
						},
					},
					"required": []any{"query"},
				},
			},
		})
	}

	SetCompactOptions(false, 400)
	uncompactedInstr, err := BuildToolInstructions(tools, "auto")
	if err != nil {
		t.Fatalf("BuildToolInstructions uncompacted: %v", err)
	}
	uncompactedRem, _ := BuildToolReminder(tools, "auto")

	SetCompactOptions(true, 400)
	compactedInstr, err := BuildToolInstructions(tools, "auto")
	if err != nil {
		t.Fatalf("BuildToolInstructions compacted: %v", err)
	}
	compactedRem, _ := BuildToolReminder(tools, "auto")

	if len(compactedInstr) >= len(uncompactedInstr)*70/100 {
		t.Fatalf("expected significant reduction in compacted instructions size: compacted=%d vs uncompacted=%d", len(compactedInstr), len(uncompactedInstr))
	}
	if strings.Contains(compactedInstr, "$schema") || strings.Contains(compactedInstr, "additionalProperties") || strings.Contains(compactedInstr, "$comment") {
		t.Fatalf("expected non-functional schema keys stripped from compacted instructions")
	}
	if len(compactedRem) >= len(uncompactedRem) || strings.Contains(compactedRem, "(query)") {
		t.Fatalf("expected compacted reminder to list tool names without required arg lists, got: %s", compactedRem)
	}

	// Validation must still enforce additionalProperties:false from original schema
	_, err = InterpretReply(`{"tool_calls":[{"name":"tool_00","arguments":{"query":"ok","extra_field":1}}]}`, tools, "auto")
	if err == nil || !strings.Contains(err.Error(), "violate the tool schema") {
		t.Fatalf("expected original schema validation to reject extra_field despite prompt compaction, got err=%v", err)
	}
	res, err := InterpretReply(`{"tool_calls":[{"name":"tool_00","arguments":{"query":"ok"}}]}`, tools, "auto")
	if err != nil || res == nil || len(res.Calls) != 1 {
		t.Fatalf("expected valid call to pass validation, got res=%+v err=%v", res, err)
	}
}

func TestSafeStreamablePrefixAllowsMarkdownCodeBlocksAndLinks(t *testing.T) {
	mdReview := "Berikut adalah hasil investigasi alur penanganan error `HTTP 403` di [server.js](server.js#L192):\n\n```javascript\nfunction isFreeTierCliGateError(status, errType, errMsg) {\n  if (status < 400) return false;\n  return errType === \"FreeTierError\";\n}\n```\n\n" +
		strings.Repeat("Penjelasan tambahan mengenai arsitektur dan mitigasi blokir IP jaringan secara mendetail. ", 6)
	got := SafeStreamablePrefix(mdReview)
	if got == "" || !strings.Contains(got, "function isFreeTierCliGateError") {
		t.Fatalf("expected SafeStreamablePrefix to stream markdown with JS code blocks and braces, got len=%d: %q", len(got), got)
	}
}
