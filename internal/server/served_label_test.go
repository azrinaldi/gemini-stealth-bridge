package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

// carbonModel is the live frontier slot: one backend id, renamed in place.
func carbonModel() gemini.ModelInfo {
	return gemini.ModelInfo{
		ModelID:     "95a4c0abcba24364",
		ModelName:   "gemini-carbon",
		DisplayName: "Most complex work",
		Description: "4 Carbon",
		Capacity:    2,
		ModelNumber: 8,
	}
}

func runRelabelChat(t *testing.T, answeredLabel string) map[string]any {
	t.Helper()
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.AccountTokens = map[string]string{"audit": "acct"}
	cfg.RejectDowngrade = true
	cfg.MaxPromptChars = 850000
	srv := New(cfg)
	model := carbonModel()
	acct := &AccountState{
		ID:       "acct",
		Email:    "acct@example.com",
		Models:   []gemini.ModelInfo{model},
		Capacity: 2,
		ModelsAt: time.Now(),
	}
	srv.SetAccounts([]*AccountState{acct})
	acct.Client = &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			if err := onFrame(&gemini.LineParseResult{
				CID:                "c1",
				RID:                "r1",
				AnsweredModelID:    model.ModelID,
				AnsweredModelLabel: answeredLabel,
				Frames: []gemini.Frame{
					{RCID: "rc1", Text: "synthetic answer", Done: true, AnsweredModelID: model.ModelID},
				},
			}); err != nil {
				return nil, err
			}
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}
	body := map[string]any{
		"model":    model.ModelName,
		"messages": []any{map[string]any{"role": "user", "content": "synthetic"}},
	}
	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, body, AuthScope{OK: true, AccountID: "acct"})
	if rec.Code != 200 {
		t.Fatalf("expected 200: a relabel is not a substitution, got %d (%s)", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return payload
}

// Asking for the renamed tier and getting the old tier's label must be visible
// in the response, because the model id alone cannot tell the two apart.
func TestChatReportsRelabelInPlace(t *testing.T) {
	payload := runRelabelChat(t, "4 Argon")
	served, ok := payload["model_served"].(map[string]any)
	if !ok {
		t.Fatalf("model_served missing, payload = %v", payload)
	}
	if served["label"] != "4 Argon" {
		t.Fatalf("served label = %v, want 4 Argon", served["label"])
	}
	if served["label_mismatch"] != true {
		t.Fatalf("label_mismatch = %v, want true", served["label_mismatch"])
	}
}

// A label that agrees with the catalogue must not add noise to the payload.
func TestChatStaysQuietWhenLabelAgrees(t *testing.T) {
	payload := runRelabelChat(t, "4 Carbon")
	if _, exists := payload["model_served"]; exists {
		t.Fatalf("model_served must stay absent when the label agrees, payload = %v", payload)
	}
}
