package server

import (
	"strings"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/toolbridge"
)

// The retry for a hallucinated tool budget must name the invention instead of
// reusing the generic schema note, otherwise the retry is a coin flip.
func TestAppendToolErrorCorrectionTargetsInventedBudget(t *testing.T) {
	base := "HISTORIAL...\n\nAssistant:"

	refusal := appendToolErrorCorrection(base, "tool_call_error: "+toolbridge.ToolProtocolRefusalError)
	if !strings.Contains(refusal, "NO tool-call or search budget") {
		t.Fatalf("retry note must deny the invented budget, got %q", refusal)
	}
	if strings.Contains(refusal, "failed tool validation") {
		t.Fatal("refusal retry must not reuse the generic schema note")
	}
	if !strings.HasSuffix(refusal, "Assistant:") {
		t.Fatal("retry note must stay before the assistant turn marker")
	}

	schema := appendToolErrorCorrection(base, "tool_call_error: missing required property \"command\"")
	if !strings.Contains(schema, "failed tool validation") {
		t.Fatalf("schema failure must keep the generic note, got %q", schema)
	}
	if strings.Contains(schema, "NO tool-call or search budget") {
		t.Fatal("schema failure must not claim there is no tool budget")
	}

	plain := appendToolErrorCorrection("SIN MARCADOR", "tool_call_error: x")
	if !strings.HasSuffix(plain, "tool_calls\":[...]} ``` block with all required schema properties.]") {
		t.Fatalf("prompt without the assistant marker must append the note, got %q", plain)
	}
}

func TestRemainingStreamTailToleratesMidStreamCitationLinkRewrite(t *testing.T) {
	sent := "Aquí tienes la lista de repositorios verificados para seguimiento:\n\n" +
		"- golang/go — [[github.com/golang/go]]\n" +
		"- assafelovic/gpt-researcher — [[github.com/assafelovi"
	full := "Aquí tienes la lista de repositorios verificados para seguimiento:\n\n" +
		"- golang/go — [github.com/golang/go](https://github.com/golang/go)\n" +
		"- assafelovic/gpt-researcher — [github.com/assafelovic/gpt-researcher](https://github.com/assafelovic/gpt-researcher)\n" +
		"- prometheus/prometheus — [github.com/prometheus/prometheus](https://github.com/prometheus/prometheus)"

	if !hasCompatibleStreamPrefix(full, sent) {
		t.Fatal("expected mid-stream citation link rewrite to remain stream-compatible instead of triggering upstream_revision")
	}
	tail, ok := remainingStreamTail(full, sent)
	if !ok || !strings.Contains(tail, "prometheus/prometheus") {
		t.Fatalf("expected remaining tail to include trailing content, got ok=%v tail=%q", ok, tail)
	}
}

