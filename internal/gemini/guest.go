package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

const (
	GuestModelID          = "guest-free"
	GuestSafePromptChars  = 350000
	GuestSafePromptTokens = 100000
	GuestHardPromptChars  = 500000
	GuestHardPromptTokens = 128000
	guestStreamURL        = Origin + "/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"
)

// GuestModel publishes a public routing name, not an authenticated registry id.
func GuestModel() ModelInfo {
	return ModelInfo{
		ModelName:           GuestModelID,
		DisplayName:         "Guest (no account)",
		Description:         "Anonymous text and function tools; upstream chooses the served model. Token usage is estimated.",
		ThinkingPolicyKnown: true,
	}
}

func IsGuestModel(m ModelInfo) bool { return strings.EqualFold(m.ModelName, GuestModelID) }

// IsGuestModelRequest requires the explicit public id, never its display label.
func IsGuestModelRequest(raw string) bool {
	name, _ := ParseModelRequest(raw)
	return strings.EqualFold(name, GuestModelID)
}

// ValidateGuestPrompt checks the complete flattened prompt without truncating it.
// estimatedTokens must come from the bridge's configured token estimator.
func ValidateGuestPrompt(promptText string, estimatedTokens int) error {
	if strings.TrimSpace(promptText) == "" {
		return transport.NewUpstreamError("guest-free prompt must not be empty", 400, "invalid_request_error", nil, nil)
	}
	chars := utf8.RuneCountInString(promptText)
	if chars > GuestHardPromptChars || estimatedTokens > GuestHardPromptTokens {
		return transport.NewUpstreamError(
			fmt.Sprintf("guest-free prompt exceeds the anonymous endpoint limit: %d Unicode characters / %d estimated tokens; maximum %d characters and %d estimated tokens (oversized prompts can lose their tail)", chars, estimatedTokens, GuestHardPromptChars, GuestHardPromptTokens),
			400, "context_length_exceeded", nil, nil,
		)
	}
	return nil
}

// GuestClient has no credentials, bootstrap or registry state. Its own client
// cannot borrow an authenticated session; the transport has no cookie jar and
// response Set-Cookie values are deliberately never absorbed.
type GuestClient struct {
	mu             sync.Mutex
	client         *transport.StealthClient
	cfg            transport.ClientConfig
	estimateTokens func(string) int
}

func NewGuestClient(cfg transport.ClientConfig, estimateTokens func(string) int) *GuestClient {
	// A login/page redirect is not an anonymous generation endpoint.
	cfg.FollowRedirects = false
	return &GuestClient{cfg: cfg, estimateTokens: estimateTokens}
}

func (g *GuestClient) Bootstrap(context.Context) error { return nil }
func (g *GuestClient) AvailableModels(context.Context, bool) (*AvailableModelsResult, error) {
	return &AvailableModelsResult{Status: 200, Models: []ModelInfo{GuestModel()}}, nil
}
func (g *GuestClient) GetQuota(context.Context) []QuotaBucket { return nil }
func (g *GuestClient) ReloadFromDiskIfNewer() bool            { return false }
func (g *GuestClient) InvalidateAuth()                        {}
func (g *GuestClient) RotateCookies(context.Context) (*cookies.RotateResult, error) {
	return &cookies.RotateResult{Status: 200}, nil
}
func (g *GuestClient) UploadFile(context.Context, []byte, string, string) (UploadedFile, error) {
	return UploadedFile{}, transport.NewUpstreamError("guest-free does not support images or attachments", 400, "unsupported_guest_feature", nil, nil)
}

// Close releases only anonymous transport connections.
func (g *GuestClient) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.client != nil {
		g.client.CloseIdleConnections()
		g.client = nil
	}
}

func (g *GuestClient) StreamGenerateLive(ctx context.Context, promptText string, opts StreamOptions, onFrame func(*LineParseResult) error) (*StreamResponse, error) {
	if g.estimateTokens == nil {
		return nil, transport.NewUpstreamError("guest-free token estimator is not configured", 500, "internal_error", nil, nil)
	}
	tokens := g.estimateTokens(promptText)
	if err := ValidateGuestPrompt(promptText, tokens); err != nil {
		return nil, err
	}
	if len(opts.Files) > 0 {
		return nil, transport.NewUpstreamError("guest-free does not support images or attachments", 400, "unsupported_guest_feature", nil, nil)
	}
	chars := utf8.RuneCountInString(promptText)
	if chars > GuestSafePromptChars || tokens > GuestSafePromptTokens {
		log.Printf("[guest] prompt above safe zone: chars=%d estimated_tokens=%d safe_chars=%d safe_tokens=%d; anonymous upstream may truncate it", chars, tokens, GuestSafePromptChars, GuestSafePromptTokens)
	}
	// There is no mutable login/continuation state to serialize. The server's
	// existing limiter bounds concurrent streams on this isolated HTTP client.
	g.mu.Lock()
	var err error
	if g.client == nil {
		g.client, err = transport.NewStealthClient(g.cfg)
	}
	client := g.client
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return g.stream(ctx, client, promptText, opts, onFrame)
}

func (g *GuestClient) stream(ctx context.Context, sc *transport.StealthClient, promptText string, opts StreamOptions, onFrame func(*LineParseResult) error) (*StreamResponse, error) {
	inner, _ := json.Marshal([][]string{{promptText}})
	freq, _ := json.Marshal([]any{nil, string(inner)})
	form := url.Values{"f.req": {string(freq)}}
	timeout := opts.NoFrameTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	watchCtx, watch := newPendingFrameWatchdog(ctx)
	defer watch.Release()
	var buf strings.Builder
	var scan arrayScan
	var inOuter, stopped, stoppedByCallback, selected, done bool
	var best Frame
	var servedID, servedLabel, lastText, lastThoughts, lastActivity string
	var grounding []GroundingSource
	var firstAt, lastAt time.Time
	frameCount := 0
	emit := func(line string) error {
		update, err := ParseFrameLine(line)
		if err != nil || update == nil {
			return err
		}
		if update.MediaToolActive {
			return transport.NewUpstreamError("guest-free does not support media generation", 400, "unsupported_guest_feature", nil, nil)
		}
		// A label can be present without an authenticated backend id. Read that
		// metadata structurally; all answer text still comes from ParseFrameLine.
		if update.AnsweredModelLabel == "" {
			update.AnsweredModelLabel = guestServedLabel(line)
		}
		if update.AnsweredModelID != "" {
			servedID = update.AnsweredModelID
		}
		if update.AnsweredModelLabel != "" {
			servedLabel = update.AnsweredModelLabel
		}
		update.CID, update.RID, update.ContextToken = "", "", ""
		progress := false
		for i := range update.Frames {
			f := &update.Frames[i]
			if len(f.GeneratedImages) > 0 || len(f.GeneratedVideos) > 0 {
				return transport.NewUpstreamError("guest-free does not support media generation", 400, "unsupported_guest_feature", nil, nil)
			}
			if !selected {
				best.RCID, selected = f.RCID, true
			}
			if f.RCID != best.RCID {
				continue
			}
			// Anonymous frames are cumulative snapshots. A shorter terminal
			// snapshot must not replace the longest candidate already received.
			if len(f.Text) < len(best.Text) || len(f.Text) == len(best.Text) && len(f.rawCandidateText) < len(best.rawCandidateText) {
				f.Text = best.Text
				f.rawCandidateText, f.rawCandidateThoughts = best.rawCandidateText, best.rawCandidateThoughts
				if f.Done {
					f.Text, f.Thoughts = SanitizeCandidateTextAndThoughts(f.rawCandidateText, f.rawCandidateThoughts, true)
				}
			}
			if f.Text != lastText || f.Thoughts != lastThoughts {
				lastText, lastThoughts, progress = f.Text, f.Thoughts, true
			}
			best = *f
			done = done || f.Done
		}
		if done || update.StreamEnd {
			watch.Disarm()
		} else if progress || update.Activity != "" && update.Activity != lastActivity {
			watch.Reset(timeout)
		}
		if update.Activity != "" {
			lastActivity = update.Activity
		}
		frameCount += len(update.Frames)
		if progress || done || update.AnsweredModelID != "" || update.AnsweredModelLabel != "" || update.Activity != "" {
			lastAt = time.Now()
			if firstAt.IsZero() {
				firstAt = lastAt
			}
		}
		grounding = MergeGroundingSources(grounding, update.GroundingSources)
		if onFrame != nil {
			if err := onFrame(update); err != nil {
				stoppedByCallback = errors.Is(err, transport.ErrStopStreaming)
				return err
			}
		}
		if update.StreamEnd {
			stopped = true
			return transport.ErrStopStreaming
		}
		return nil
	}
	startedAt := time.Now()
	res, err := sc.Do(watchCtx, transport.RequestOptions{
		Method: "POST", URL: guestStreamURL, Kind: transport.ReqKindCORS,
		Headers: map[string]string{"content-type": "application/x-www-form-urlencoded;charset=UTF-8"},
		Body:    []byte(form.Encode()), FreshSocket: opts.FreshSocket,
		OnHeaders: func(status int) {
			if status == 200 {
				watch.Arm(timeout)
			}
		},
		OnChunk: func(chunk string) error {
			buf.WriteString(chunk)
			envelopes, rem, nextOuter := scanStreamEnvelopes(buf.String(), inOuter, false, &scan)
			inOuter = nextOuter
			if len(rem) != buf.Len() {
				buf.Reset()
				buf.WriteString(rem)
			}
			if len(rem) > 16<<20 {
				return transport.NewUpstreamError("upstream frame exceeds size limit", 502, "upstream_parse_error", nil, nil)
			}
			for _, line := range envelopes {
				if err := emit(line); err != nil {
					stopped = errors.Is(err, transport.ErrStopStreaming)
					return err
				}
			}
			return nil
		},
	})
	if err != nil {
		var ue *transport.UpstreamError
		if watch.Expired() && ctx.Err() == nil && errors.As(err, &ue) && ue.Code == "client_cancelled" {
			if best.Text == "" && best.Thoughts == "" {
				return nil, noFrameStallError(timeout, ue.Headers)
			}
			return nil, transport.NewUpstreamError("anonymous upstream stream interrupted after receiving frames", 502, "incomplete_upstream_response", ue.Headers, err)
		}
		return nil, err
	}
	if res.Status == 200 {
		if !stopped {
			envelopes, _, _ := extractStreamEnvelopes(buf.String(), inOuter, true)
			for _, line := range envelopes {
				if err := emit(line); err != nil {
					if errors.Is(err, transport.ErrStopStreaming) {
						break
					}
					return nil, err
				}
			}
		}
		watch.Disarm()
		// A clean EOF completes the anonymous single turn even when Google omits
		// logged-in conversation completion metadata. Never synthesize an id.
		if selected && !done && !stoppedByCallback && onFrame != nil {
			best.Done = true
			best.Text, best.Thoughts = SanitizeCandidateTextAndThoughts(best.rawCandidateText, best.rawCandidateThoughts, true)
			best.AnsweredModelID, best.AnsweredModelLabel = servedID, servedLabel
			if err := onFrame(&LineParseResult{Frames: []Frame{best}, AnsweredModelID: servedID, AnsweredModelLabel: servedLabel}); err != nil {
				return nil, err
			}
		}
	}
	firstMs := int64(-1)
	var windowMs int64
	if !firstAt.IsZero() {
		firstMs, windowMs = firstAt.Sub(startedAt).Milliseconds(), lastAt.Sub(firstAt).Milliseconds()
	}
	return &StreamResponse{Status: res.Status, Headers: res.Headers, Empty: !selected, Body: res.Body, GroundingSources: grounding, UpstreamTTFBMs: res.TTFBMs, FirstFrameMs: firstMs, GenWindowMs: windowMs, FrameCount: frameCount}, nil
}

func guestServedLabel(line string) string {
	var outer [][]any
	if json.Unmarshal([]byte(line), &outer) != nil {
		return ""
	}
	for _, part := range outer {
		if len(part) < 3 || part[0] != "wrb.fr" {
			continue
		}
		raw, _ := part[2].(string)
		var payload []any
		if json.Unmarshal([]byte(raw), &payload) == nil {
			if label, _ := jspbField(payload, 42).(string); label != "" {
				return label
			}
		}
	}
	return ""
}
