package toolbridge

import (
	"strings"
	"testing"
)

// These strings cover real Spanish confabulations of the upstream tool-call
// budget guard observed on Gemini 4 models.
func TestToolProtocolRefusalDetectsLiveSessionVariants(t *testing.T) {
	live := []string{
		"en este turno se agotó el cupo de herramientas, así que no pude consultar el índice",
		"el turno se quedó otra vez sin cupo de ejecución (0 remaining) justo antes de poder correr la consulta",
		"el runtime me tiene bloqueado en 0 llamadas de herramientas disponibles ahora mismo",
		"esta sesión arrancó bloqueada con el límite de llamadas de herramientas en 0 desde el propio entorno",
		"me tiene bloqueado en 0 llamadas de herramientas disponibles",
	}
	for _, text := range live {
		if !IsToolProtocolRefusal(text) {
			t.Errorf("live-session variant not detected: %q", text)
		}
	}
}

func TestStripToolGuardLeavesOrdinarySpanishAlone(t *testing.T) {
	benign := []string{
		"Ejecuté las tres comprobaciones y todas respondieron bien.",
		"El módulo tiene un límite de reintentos configurado en 3.",
		"Las herramientas del proyecto están en src/tools/.",
		"Las herramientas disponibles son bash, read y edit.",
		"No hay más archivos en ese directorio.",
		"El presupuesto del proyecto se agotó, así que recortamos el alcance.",
		"Refactoricé config.js para separar los entornos.",
	}
	for _, text := range benign {
		if IsToolProtocolRefusal(text) {
			t.Errorf("false positive on ordinary Spanish text: %q", text)
		}
		if got := StripToolGuard(text); got != text {
			t.Errorf("stripping altered ordinary Spanish text: %q -> %q", text, got)
		}
	}
}

// A guard used as a header above a legitimate summary must lose the header and
// keep the summary. Dropping the whole reply would destroy real work.
func TestStripToolGuardKeepsSummaryBelowSpanishGuard(t *testing.T) {
	reply := "Se alcanzó el límite de llamadas a herramientas antes de completar todos los pasos:\n\n" +
		"- Completado: se separó la configuración por entorno.\n" +
		"- Pendiente: actualizar server.js."

	got := StripToolGuard(reply)
	if strings.Contains(strings.ToLower(got), "límite de llamadas") {
		t.Errorf("guard header survived stripping: %q", got)
	}
	if !strings.Contains(got, "se separó la configuración por entorno") {
		t.Errorf("stripping destroyed the legitimate summary: %q", got)
	}
}

// Grammatical variants that also occur: participle forms and the declarative
// "el límite ... ha sido alcanzado" shape.
func TestToolProtocolRefusalDetectsSpanishGrammaticalVariants(t *testing.T) {
	variants := []string{
		"Se alcanzó el límite de llamadas a herramientas antes de completar todos los pasos:",
		"Se ha excedido el número máximo de llamadas a herramientas y búsquedas (0 restantes).",
		"No quedan llamadas a herramientas disponibles en esta sesión.",
		"El presupuesto de llamadas a herramientas se ha agotado.",
		"No puedo usar las herramientas en esta sesión.",
		"El límite de llamadas a herramientas ha sido alcanzado.",
		"0 llamadas a herramientas restantes",
	}
	for _, text := range variants {
		if !IsToolProtocolRefusal(text) {
			t.Errorf("Spanish grammatical variant not detected: %q", text)
		}
	}
}

func TestSafeStreamablePrefixHoldsBackLongToolProtocolRefusal(t *testing.T) {
	longRefusal := "El runtime me tiene bloqueado en 0 llamadas de herramientas disponibles ahora mismo (maximum number of tool and search calls (0 remaining)), así que no podemos ejecutar herramientas en este turno.\n" +
		"Además, por arquitectura los trabajadores secundarios arrancan sin estado compartido y el acceso al almacén lo maneja el proceso principal. Para poder consultarlo apenas se habiliten las herramientas en una sesión limpia (o si quieres reiniciar la sesión ahora mismo para liberar el contador)."
	if len(longRefusal) <= 320 {
		t.Fatalf("fixture must exceed 320 bytes, got %d", len(longRefusal))
	}
	if got := SafeStreamablePrefix(longRefusal); got != "" {
		t.Fatalf("expected SafeStreamablePrefix to hold back long refusal so in-stream retry remains possible, got %q", got)
	}
}

// A statement that merely names a limit is not a refusal. The detector must
// only fire when the text says the limit was reached, exhausted, or is zero;
// otherwise a normal answer gets retried as a tool error and stripped of its
// content.
func TestToolGuardDoesNotMatchOrdinaryLimitStatements(t *testing.T) {
	benign := []string{
		"El límite de herramientas está configurado en 10.",
		"The maximum number of tool calls is 10.",
		"Configuré el límite de llamadas a herramientas en 10.",
		"The tool limit is configured to 10 calls per turn.",
	}
	for _, text := range benign {
		if IsToolProtocolRefusal(text) {
			t.Errorf("ordinary limit statement treated as a refusal: %q", text)
		}
		if got := StripToolGuard(text); got != text {
			t.Errorf("ordinary limit statement was altered: %q -> %q", text, got)
		}
	}
}
