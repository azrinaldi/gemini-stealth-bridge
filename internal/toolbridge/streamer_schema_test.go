package toolbridge

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

func assertUpstreamRevision(t *testing.T, err error) {
	t.Helper()
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.Code != "upstream_revision" {
		t.Fatalf("expected upstream_revision error, got %v", err)
	}
}

func TestLiveToolCallStreamerSlicesTrailingSiblingsAfterPartialArgs(t *testing.T) {
	tools := sampleTools()
	longPath := strings.Repeat("a", 160)

	t.Run("trailing sibling id and index do not leak into arguments", func(t *testing.T) {
		var deltas []map[string]any
		s := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) {
			deltas = append(deltas, d)
		})

		// Outer call object is still incomplete (missing final '}'), while arguments closed
		// and sibling fields follow before the closing brace.
		frame1 := `{"tool_calls":[{"name":"read_file","arguments":{"path":"` + longPath + `"},"id":"call_extra_999","index":0`
		if err := s.PushError(frame1); err != nil {
			t.Fatalf("PushError(frame1) failed: %v", err)
		}
		streamed := reconstructStreamedArguments(deltas, 0)
		if streamed == "" {
			t.Fatal("expected partial arguments to be emitted")
		}
		if strings.Contains(streamed, "call_extra_999") || strings.Contains(streamed, `"id"`) || strings.Contains(streamed, `"index"`) {
			t.Fatalf("trailing sibling fields leaked into streamed arguments: %q", streamed)
		}

		full := frame1 + `}]}`
		res, err := InterpretReply(full, tools, "auto")
		if err != nil || res == nil || len(res.Calls) != 1 {
			t.Fatalf("InterpretReply failed: res=%+v err=%v", res, err)
		}
		oa, err := s.FinalizeError(res.Calls, full)
		if err != nil {
			t.Fatalf("FinalizeError failed: %v", err)
		}
		finalStreamed := reconstructStreamedArguments(deltas, 0)
		if finalStreamed != oa[0].Function.Arguments {
			t.Fatalf("streamed arguments mismatch:\ngot:  %s\nwant: %s", finalStreamed, oa[0].Function.Arguments)
		}
	})

	t.Run("empty object arguments with trailing sibling", func(t *testing.T) {
		noReqTools := []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "ping",
					"parameters": map[string]any{
						"type":       "object",
						"properties": map[string]any{},
					},
				},
			},
		}
		var deltas []map[string]any
		s := NewLiveToolCallStreamer(noReqTools, "auto", func(d map[string]any) {
			deltas = append(deltas, d)
		})
		partial := `{"tool_calls":[{"name":"ping","arguments":{},"id":"call_0"`
		if err := s.PushError(partial); err != nil {
			t.Fatalf("PushError failed: %v", err)
		}
		if streamed := reconstructStreamedArguments(deltas, 0); strings.Contains(streamed, "call_0") {
			t.Fatalf("trailing sibling leaked into empty object arguments: %q", streamed)
		}
		oa, err := s.FinalizeError([]Call{{Name: "ping", Arguments: map[string]any{}}}, partial+`}]}`)
		if err != nil {
			t.Fatalf("FinalizeError failed: %v", err)
		}
		if len(oa) != 1 || oa[0].Function.Arguments != "{}" {
			t.Fatalf("unexpected finalized call: %+v", oa)
		}
	})
}

func TestLiveToolCallStreamerIgnoresProseExamplesAndNonStandalone(t *testing.T) {
	tools := sampleTools()
	longPath := strings.Repeat("p", 200)

	examples := []struct {
		name string
		text string
	}{
		{
			name: "inline example intro",
			text: `Here is an example: {"tool_calls":[{"name":"read_file","arguments":{"path":"` + longPath + `"}}]}`,
		},
		{
			name: "format is intro",
			text: `The envelope format is: {"tool_calls":[{"name":"read_file","arguments":{"path":"` + longPath + `"}}]}`,
		},
		{
			name: "fenced after example intro",
			text: "Here is an example:\n```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"" + longPath + "\"}}]}\n```",
		},
		{
			name: "inline fenced bullet",
			text: "- Use ```json {\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"" + longPath + "\"}}]} ``` inside docs.",
		},
		{
			name: "completed envelope with trailing prose on same line",
			text: `{"tool_calls":[{"name":"read_file","arguments":{"path":"` + longPath + `"}}]} — send it to the harness.`,
		},
	}

	for _, tc := range examples {
		t.Run(tc.name, func(t *testing.T) {
			var deltas []map[string]any
			s := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) {
				deltas = append(deltas, d)
			})
			if err := s.PushError(tc.text); err != nil {
				t.Fatalf("PushError returned unexpected error: %v", err)
			}
			if s.SentAny() || len(deltas) > 0 {
				t.Fatalf("prose example triggered live deltas: %+v", deltas)
			}
			oa, err := s.FinalizeError(nil, tc.text)
			if err != nil || len(oa) != 0 {
				t.Fatalf("FinalizeError = %+v, %v; want empty calls and nil error", oa, err)
			}
		})
	}

	t.Run("prose mention before real standalone envelope", func(t *testing.T) {
		var deltas []map[string]any
		s := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) {
			deltas = append(deltas, d)
		})
		text := "Do not copy `{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/fake\"}}]}`.\n" +
			"```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/real\"}}]}\n```"
		if err := s.PushError(text); err != nil {
			t.Fatalf("PushError failed: %v", err)
		}
		if !s.SentAny() {
			t.Fatal("expected real standalone envelope after prose mention to stream")
		}
		res, err := InterpretReply(text, tools, "auto")
		if err != nil || res == nil || len(res.Calls) != 1 {
			t.Fatalf("InterpretReply = %+v, %v", res, err)
		}
		oa, err := s.FinalizeError(res.Calls, text)
		if err != nil || len(oa) != 1 || oa[0].Function.Arguments != `{"path":"/tmp/real"}` {
			t.Fatalf("FinalizeError = %+v, %v", oa, err)
		}
	})
}

func TestLiveToolCallStreamerAbortOrRevisionAndForcedFunctionAcrossIndices(t *testing.T) {
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
	firstCall := `{"name":"read_file","arguments":{"path":"/tmp/a"}}`

	t.Run("invalid completed call at idx 1 after idx 0 emitted returns upstream_revision", func(t *testing.T) {
		var deltas []map[string]any
		s := NewLiveToolCallStreamer(multiTools, "auto", func(d map[string]any) { deltas = append(deltas, d) })
		if err := s.PushError(`{"tool_calls":[` + firstCall + `,`); err != nil {
			t.Fatal(err)
		}
		err := s.PushError(`{"tool_calls":[` + firstCall + `,{"name":"erase_all","arguments":{}}]}`)
		assertUpstreamRevision(t, err)
	})

	t.Run("disallowed partial call at idx 1 after idx 0 emitted returns upstream_revision", func(t *testing.T) {
		var deltas []map[string]any
		s := NewLiveToolCallStreamer(multiTools, "auto", func(d map[string]any) { deltas = append(deltas, d) })
		if err := s.PushError(`{"tool_calls":[` + firstCall + `,`); err != nil {
			t.Fatal(err)
		}
		err := s.PushError(`{"tool_calls":[` + firstCall + `,{"name":"erase_all","arguments":{"x":"1`)
		assertUpstreamRevision(t, err)
	})

	t.Run("forced function violated at idx 1 completed and partial", func(t *testing.T) {
		forcedChoice := map[string]any{"type": "function", "function": map[string]any{"name": "read_file"}}

		s1 := NewLiveToolCallStreamer(multiTools, forcedChoice, func(map[string]any) {})
		if err := s1.PushError(`{"tool_calls":[` + firstCall + `,`); err != nil {
			t.Fatal(err)
		}
		err1 := s1.PushError(`{"tool_calls":[` + firstCall + `,{"name":"write_file","arguments":{"path":"/tmp/b"}}]}`)
		assertUpstreamRevision(t, err1)

		s2 := NewLiveToolCallStreamer(multiTools, forcedChoice, func(map[string]any) {})
		if err := s2.PushError(`{"tool_calls":[` + firstCall + `,`); err != nil {
			t.Fatal(err)
		}
		err2 := s2.PushError(`{"tool_calls":[` + firstCall + `,{"name":"write_file","arguments":{"path":"/tmp/b`)
		assertUpstreamRevision(t, err2)
	})

	t.Run("second object appended to idx 1 partial args after idx 0 emitted returns upstream_revision", func(t *testing.T) {
		s := NewLiveToolCallStreamer(multiTools, "auto", func(map[string]any) {})
		if err := s.PushError(`{"tool_calls":[` + firstCall + `,`); err != nil {
			t.Fatal(err)
		}
		err := s.PushError(`{"tool_calls":[` + firstCall + `,{"name":"read_file","arguments":{"path":"/tmp/b"}{"extra":1`)
		assertUpstreamRevision(t, err)
	})

	t.Run("normal multi-call streaming while idx 1 prefix is still incomplete", func(t *testing.T) {
		s := NewLiveToolCallStreamer(multiTools, "auto", func(map[string]any) {})
		if err := s.PushError(`{"tool_calls":[` + firstCall + `,{"name":"write_fi`); err != nil {
			t.Fatalf("incomplete call 1 prefix must not trigger revision: %v", err)
		}
		if err := s.PushError(`{"tool_calls":[` + firstCall + `,{"name":"write_file","arguments":{"path":"/tmp/b"}}]}`); err != nil {
			t.Fatalf("completed call 1 failed: %v", err)
		}
	})

	t.Run("unemitted invalid completed call aborts silently for InterpretReply", func(t *testing.T) {
		var deltas []map[string]any
		s := NewLiveToolCallStreamer(multiTools, "auto", func(d map[string]any) { deltas = append(deltas, d) })
		if err := s.PushError(`{"tool_calls":[{"name":"erase_all","arguments":{}}]}`); err != nil {
			t.Fatalf("expected unemitted invalid call to abort silently, got %v", err)
		}
		if s.SentAny() || len(deltas) != 0 {
			t.Fatalf("expected no deltas emitted, got %+v", deltas)
		}
	})
}

func TestLiveToolCallStreamerSchemaChecksAndPipeSanitization(t *testing.T) {
	t.Run("plain pipe operator streams while sanitized HTML pipe holds back", func(t *testing.T) {
		tools := sampleTools()
		longCmd := "cat file.txt |> grep foo |> sort " + strings.Repeat("x", 140)
		var deltas []map[string]any
		s := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) { deltas = append(deltas, d) })
		if err := s.PushError(`{"tool_calls":[{"name":"read_file","arguments":{"path":"` + longCmd); err != nil {
			t.Fatalf("PushError failed: %v", err)
		}
		if got := reconstructStreamedArguments(deltas, 0); got == "" {
			t.Fatal("plain '|>' operator should stream without being blocked")
		}

		var htmlDeltas []map[string]any
		sHTML := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) { htmlDeltas = append(htmlDeltas, d) })
		sanitized := `<Foo Bar |> ` + strings.Repeat("x", 140)
		if err := sHTML.PushError(`{"tool_calls":[{"name":"read_file","arguments":{"path":"` + sanitized); err != nil {
			t.Fatalf("PushError failed: %v", err)
		}
		if sHTML.SentAny() {
			t.Fatalf("sanitized HTML '<tag attr |>' should not live-stream before repair, got %+v", htmlDeltas)
		}
	})

	t.Run("additionalProperties omitted true false and object schema", func(t *testing.T) {
		longVal := strings.Repeat("v", 130)

		makeTools := func(addProps any, omitAddProps bool) []any {
			params := map[string]any{
				"type": "object",
				"properties": map[string]any{
					"known": map[string]any{"type": "string"},
				},
			}
			if !omitAddProps {
				params["additionalProperties"] = addProps
			}
			return []any{
				map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":       "flex",
						"parameters": params,
					},
				},
			}
		}

		// Omitted additionalProperties allows unknown keys
		sOmitted := NewLiveToolCallStreamer(makeTools(nil, true), "auto", func(map[string]any) {})
		if err := sOmitted.PushError(`{"tool_calls":[{"name":"flex","arguments":{"extra":"` + longVal); err != nil || !sOmitted.SentAny() {
			t.Fatalf("expected omitted additionalProperties to allow extra key, sent=%v err=%v", sOmitted.SentAny(), err)
		}

		// additionalProperties: false blocks unknown keys
		sFalse := NewLiveToolCallStreamer(makeTools(false, false), "auto", func(map[string]any) {})
		if err := sFalse.PushError(`{"tool_calls":[{"name":"flex","arguments":{"extra":"` + longVal); err != nil || sFalse.SentAny() {
			t.Fatalf("expected additionalProperties:false to block extra key, sent=%v err=%v", sFalse.SentAny(), err)
		}

		// additionalProperties schema object validates unknown key value type
		sObjOK := NewLiveToolCallStreamer(makeTools(map[string]any{"type": "string"}, false), "auto", func(map[string]any) {})
		if err := sObjOK.PushError(`{"tool_calls":[{"name":"flex","arguments":{"extra":"` + longVal); err != nil || !sObjOK.SentAny() {
			t.Fatalf("expected string extra key to match additionalProperties schema, sent=%v err=%v", sObjOK.SentAny(), err)
		}
		sObjBad := NewLiveToolCallStreamer(makeTools(map[string]any{"type": "integer"}, false), "auto", func(map[string]any) {})
		if err := sObjBad.PushError(`{"tool_calls":[{"name":"flex","arguments":{"extra":"` + longVal); err != nil || sObjBad.SentAny() {
			t.Fatalf("expected string value to be rejected for integer additionalProperties, sent=%v err=%v", sObjBad.SentAny(), err)
		}
	})

	t.Run("nullable and union type arrays", func(t *testing.T) {
		tools := []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "typed",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"opt_nullable": map[string]any{"type": "string", "nullable": true},
							"union_null":   map[string]any{"type": []any{"string", "null"}},
							"union_num":    map[string]any{"type": []any{"integer", "boolean"}},
							"strict_str":   map[string]any{"type": "string"},
						},
					},
				},
			},
		}
		pad := strings.Repeat("x", 130)

		s1 := NewLiveToolCallStreamer(tools, "auto", func(map[string]any) {})
		if err := s1.PushError(`{"tool_calls":[{"name":"typed","arguments":{"opt_nullable":null,"union_null":null,"union_num":42,"strict_str":"` + pad); err != nil || !s1.SentAny() {
			t.Fatalf("expected nullable and union null/integer values to stream, sent=%v err=%v", s1.SentAny(), err)
		}

		s2 := NewLiveToolCallStreamer(tools, "auto", func(map[string]any) {})
		if err := s2.PushError(`{"tool_calls":[{"name":"typed","arguments":{"union_null":123,"strict_str":"` + pad); err != nil || s2.SentAny() {
			t.Fatalf("expected number to be rejected for [string, null] union, sent=%v err=%v", s2.SentAny(), err)
		}

		s3 := NewLiveToolCallStreamer(tools, "auto", func(map[string]any) {})
		if err := s3.PushError(`{"tool_calls":[{"name":"typed","arguments":{"strict_str":null,"opt_nullable":"` + pad); err != nil || s3.SentAny() {
			t.Fatalf("expected null to be rejected for non-nullable string, sent=%v err=%v", s3.SentAny(), err)
		}
	})
}

func TestCoerceArgumentsToSchemaEmptyArrayString(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tags": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
			"non_empty": map[string]any{
				"type":     "array",
				"minItems": 1,
				"items":    map[string]any{"type": "string"},
			},
		},
	}

	args := map[string]any{
		"tags":      "[]",
		"non_empty": "[]",
	}
	CoerceArgumentsToSchema(schema, args)

	gotTags, ok := args["tags"].([]any)
	if !ok || len(gotTags) != 0 {
		t.Fatalf(`expected "[]" to coerce to empty []any{}, got %#v`, args["tags"])
	}
	if args["non_empty"] != "[]" {
		t.Fatalf(`expected "[]" with minItems:1 not to mutate into ["[]"], got %#v`, args["non_empty"])
	}
}

func TestCheckSchemaNullableBeforeRefAndNot(t *testing.T) {
	refSchema := map[string]any{
		"nullable": true,
		"$ref":     "#/$defs/nonEmpty",
		"$defs": map[string]any{
			"nonEmpty": map[string]any{
				"type":      "string",
				"minLength": 1,
			},
		},
	}
	if errStr := CheckSchema(refSchema, nil, "field"); errStr != "" {
		t.Fatalf("expected nil to satisfy nullable $ref schema, got %q", errStr)
	}
	if errStr := CheckSchema(refSchema, "", "field"); errStr == "" {
		t.Fatal("expected empty string to fail $ref minLength constraint")
	}

	notSchema := map[string]any{
		"nullable": true,
		"not":      map[string]any{"type": "null"},
	}
	if errStr := CheckSchema(notSchema, nil, "field"); errStr != "" {
		t.Fatalf("expected nil to satisfy nullable not-schema, got %q", errStr)
	}
	if errStr := CheckSchema(map[string]any{"nullable": true, "not": map[string]any{"const": "bad"}}, "bad", "field"); errStr == "" {
		t.Fatal("expected forbidden const to still fail not-schema for non-nil value")
	}
}

func TestCompactSchemaMapPreservesObjectAdditionalPropertiesOnly(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"labels": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"type":        "string",
					"title":       "Label Value",
					"description": "Long description exceeding limit",
				},
			},
			"open_map": map[string]any{
				"type":                 "object",
				"additionalProperties": true,
			},
		},
	}
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"labels": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"type":        "string",
					"description": "Long de...",
				},
			},
			"open_map": map[string]any{
				"type": "object",
			},
		},
	}
	got := CompactSchemaMap(schema, 10)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CompactSchemaMap = %#v; want %#v", got, want)
	}
}

func TestCheckSchemaSkipsNonRE2Pattern(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"lookahead": map[string]any{
				"type":    "string",
				"pattern": `^(?!admin).*$`,
			},
			"re2": map[string]any{
				"type":    "string",
				"pattern": `^[a-z]+$`,
			},
		},
	}
	if errStr := CheckSchema(schema, map[string]any{"lookahead": "user", "re2": "abc"}, "arguments"); errStr != "" {
		t.Fatalf("expected non-RE2 pattern to be skipped, got %q", errStr)
	}
	if errStr := CheckSchema(schema, map[string]any{"lookahead": "user", "re2": "123"}, "arguments"); errStr == "" {
		t.Fatal("expected valid RE2 pattern to still reject non-matching input")
	}
}

func TestFencedExampleParserAndStreamerParity(t *testing.T) {
	tools := sampleTools()
	longFake := strings.Repeat("x", 150)

	standaloneCases := []struct {
		name string
		text string
	}{
		{
			name: "for example fenced json",
			text: "For example:\n```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"" + longFake + "\"}}]}\n```",
		},
		{
			name: "for example xml tool_call",
			text: "For example:\n<tool_call>\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"" + longFake + "\"}}]}\n</tool_call>",
		},
		{
			name: "for example fenced json with leading object",
			text: "For example:\n```json\n{\"note\":1}\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"" + longFake + "\"}}]}\n```",
		},
	}

	for _, tc := range standaloneCases {
		t.Run(tc.name, func(t *testing.T) {
			var deltas []map[string]any
			s := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) { deltas = append(deltas, d) })
			if err := s.PushError(tc.text); err != nil {
				t.Fatalf("PushError returned unexpected error: %v", err)
			}
			if s.SentAny() || len(deltas) != 0 {
				t.Fatalf("expected fenced example not to stream, got deltas=%+v", deltas)
			}
			res, err := InterpretReply(tc.text, tools, "auto")
			if err != nil || res != nil {
				t.Fatalf("InterpretReply = %+v, %v; want nil, nil", res, err)
			}
		})
	}

	t.Run("fenced example followed by real standalone call", func(t *testing.T) {
		text := "For example:\n```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"fake_" + longFake + "\"}}]}\n```\n\n" +
			"{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"real\"}}]}"

		var deltas []map[string]any
		s := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) { deltas = append(deltas, d) })
		if err := s.PushError(text); err != nil {
			t.Fatalf("PushError failed: %v", err)
		}
		res, err := InterpretReply(text, tools, "auto")
		if err != nil || res == nil || len(res.Calls) != 1 {
			t.Fatalf("InterpretReply = %+v, %v; want 1 real call", res, err)
		}
		if res.Calls[0].Arguments["path"] != "real" {
			t.Fatalf("InterpretReply picked wrong call: %+v", res.Calls)
		}
		oa, err := s.FinalizeError(res.Calls, text)
		if err != nil || len(oa) != 1 {
			t.Fatalf("FinalizeError = %+v, %v; want 1 call", oa, err)
		}
		if got := reconstructStreamedArguments(deltas, 0); got != oa[0].Function.Arguments || oa[0].Function.Arguments != `{"path":"real"}` {
			t.Fatalf("streamed=%q finalized=%q; want %q", got, oa[0].Function.Arguments, `{"path":"real"}`)
		}
	})
}

func TestLiveToolCallStreamerSanitizedPipeHoldbackInTail(t *testing.T) {
	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "write",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content": map[string]any{"type": "string"},
					},
					"required": []any{"content"},
				},
			},
		},
	}

	t.Run("sanitized pipe in unemitted tail holds back and repairs cleanly", func(t *testing.T) {
		prefixPad := strings.Repeat("z", 130)
		frame1 := `{"tool_calls":[{"name":"write","arguments":{"content":"` + prefixPad
		frame2 := frame1 + ` <div class |>`
		frame3 := frame2 + `hello world`
		full := frame3 + `"}}]}`

		var deltas []map[string]any
		s := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) { deltas = append(deltas, d) })

		if err := s.PushError(frame1); err != nil {
			t.Fatalf("frame1 failed: %v", err)
		}
		sentAfterFrame1 := reconstructStreamedArguments(deltas, 0)
		if sentAfterFrame1 == "" {
			t.Fatal("expected frame1 to emit non-empty argument prefix")
		}

		if err := s.PushError(frame2); err != nil {
			t.Fatalf("frame2 should hold back unemitted sanitized pipe rather than failing: %v", err)
		}
		sentAfterFrame2 := reconstructStreamedArguments(deltas, 0)
		if strings.Contains(sentAfterFrame2, "<div") || strings.Contains(sentAfterFrame2, "|>") {
			t.Fatalf("sanitized pipe bytes leaked into streamed args: %q", sentAfterFrame2)
		}

		if err := s.PushError(frame3); err != nil {
			t.Fatalf("frame3 should keep sanitized pipe buffered: %v", err)
		}
		if got := reconstructStreamedArguments(deltas, 0); got != sentAfterFrame2 {
			t.Fatalf("bytes past sanitized pipe should stay held back: got %q want %q", got, sentAfterFrame2)
		}

		res, err := InterpretReply(full, tools, "auto")
		if err != nil || res == nil || len(res.Calls) != 1 {
			t.Fatalf("InterpretReply = %+v, %v", res, err)
		}
		if res.Calls[0].Arguments["content"] != prefixPad+` <div | class>hello world` {
			t.Fatalf("unexpected repaired content: %q", res.Calls[0].Arguments["content"])
		}
		oa, err := s.FinalizeError(res.Calls, full)
		if err != nil || len(oa) != 1 {
			t.Fatalf("FinalizeError = %+v, %v", oa, err)
		}
		streamed := reconstructStreamedArguments(deltas, 0)
		if streamed != oa[0].Function.Arguments {
			t.Fatalf("streamed args mismatch:\ngot:  %s\nwant: %s", streamed, oa[0].Function.Arguments)
		}
		if !json.Valid([]byte(streamed)) {
			t.Fatalf("reconstructed arguments are invalid JSON: %q", streamed)
		}
	})

	t.Run("sanitized pipe overlapping already-emitted bytes still returns upstream_revision", func(t *testing.T) {
		prefixPad := strings.Repeat("z", 130)
		// Frame 1 ends with a long tag prefix before '|>' so '<div' falls inside st.sentArgs.
		frame1 := `{"tool_calls":[{"name":"write","arguments":{"content":"` + prefixPad + ` <div class_name_long`
		frame2 := frame1 + ` |>`

		var deltas []map[string]any
		s := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) { deltas = append(deltas, d) })
		if err := s.PushError(frame1); err != nil {
			t.Fatalf("frame1 failed: %v", err)
		}
		if got := reconstructStreamedArguments(deltas, 0); !strings.Contains(got, "<div") {
			t.Fatalf("expected fixture to emit '<div' prefix before '|>' arrived, got %q", got)
		}
		err := s.PushError(frame2)
		assertUpstreamRevision(t, err)
	})
}

func TestExampleIntroSampleAndForExampleProseVariants(t *testing.T) {
	tools := sampleTools()
	longPath := strings.Repeat("p", 150)

	proseCases := []struct {
		name string
		text string
	}{
		{
			name: "here is a sample fenced with trailing prose",
			text: "Here is a sample:\n```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"" + longPath + "\"}}]}\n```\nFollowed by more prose.",
		},
		{
			name: "for example you can use with trailing prose",
			text: "For example, you can use:\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"" + longPath + "\"}}]}\nAnd then continue.",
		},
	}

	for _, tc := range proseCases {
		t.Run(tc.name, func(t *testing.T) {
			var deltas []map[string]any
			s := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) { deltas = append(deltas, d) })
			for i := 1; i <= len(tc.text); i++ {
				if err := s.PushError(tc.text[:i]); err != nil {
					t.Fatalf("PushError at byte %d failed: %v", i, err)
				}
			}
			if s.SentAny() || len(deltas) != 0 {
				t.Fatalf("prose example emitted live deltas: %+v", deltas)
			}
			res, err := InterpretReply(tc.text, tools, "auto")
			if err != nil || res != nil {
				t.Fatalf("InterpretReply = %+v, %v; want nil, nil", res, err)
			}
		})
	}

	t.Run("normal preamble mentioning example file still executes", func(t *testing.T) {
		text := "Let me check an example file:\n```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"example.json\"}}]}\n```"
		var deltas []map[string]any
		s := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) { deltas = append(deltas, d) })
		if err := s.PushError(text); err != nil {
			t.Fatalf("PushError failed: %v", err)
		}
		res, err := InterpretReply(text, tools, "auto")
		if err != nil || res == nil || len(res.Calls) != 1 || res.Calls[0].Arguments["path"] != "example.json" {
			t.Fatalf("expected normal preamble tool call to execute, got res=%+v err=%v", res, err)
		}
		oa, err := s.FinalizeError(res.Calls, text)
		if err != nil || len(oa) != 1 || oa[0].Function.Arguments != `{"path":"example.json"}` {
			t.Fatalf("FinalizeError = %+v, %v", oa, err)
		}
	})
}
