package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/toolbridge"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

type requestBaseURLKey struct{}

func withRequestBaseURL(ctx context.Context, r *http.Request, defaultPort int) context.Context {
	if r == nil {
		return ctx
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		if defaultPort <= 0 {
			defaultPort = 20133
		}
		host = fmt.Sprintf("127.0.0.1:%d", defaultPort)
	}
	return context.WithValue(ctx, requestBaseURLKey{}, scheme+"://"+host)
}

func (s *Server) requestBaseURL(ctx context.Context) string {
	if v, ok := ctx.Value(requestBaseURLKey{}).(string); ok && v != "" {
		return v
	}
	port := s.Cfg.Port
	if port <= 0 {
		port = 20133
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

type mediaEntry struct {
	ID          string
	AccountID   string
	UpstreamURL string
	FallbackURL string
	MimeType    string
	Ext         string
	Title       string
	CreatedAt   time.Time

	fetchMu sync.Mutex
	Data    []byte
}

type mediaDownloader interface {
	DownloadMedia(ctx context.Context, rawURL, fallbackURL string) ([]byte, string, error)
}

const maxCachedMediaEntries = 96

func (s *Server) registerMedia(ctx context.Context, accountID, upstreamURL, fallbackURL, mimeType, ext, title string) (string, *mediaEntry) {
	if upstreamURL == "" {
		return "", nil
	}
	if ext == "" {
		ext = "bin"
	}
	sum := sha256.Sum256([]byte(accountID + "|" + upstreamURL))
	id := hex.EncodeToString(sum[:12]) + "." + ext
	proxyURL := s.requestBaseURL(ctx) + "/v1/media/" + id

	s.mediaMu.Lock()
	if s.mediaItems == nil {
		s.mediaItems = make(map[string]*mediaEntry)
	}
	entry, exists := s.mediaItems[id]
	if !exists {
		entry = &mediaEntry{
			ID:          id,
			AccountID:   accountID,
			UpstreamURL: upstreamURL,
			FallbackURL: fallbackURL,
			MimeType:    mimeType,
			Ext:         ext,
			Title:       title,
			CreatedAt:   s.now(),
		}
		s.mediaItems[id] = entry
		s.mediaOrder = append(s.mediaOrder, id)
		for len(s.mediaOrder) > maxCachedMediaEntries {
			oldest := s.mediaOrder[0]
			s.mediaOrder = s.mediaOrder[1:]
			delete(s.mediaItems, oldest)
		}
	}
	s.mediaMu.Unlock()

	return proxyURL, entry
}

func (s *Server) ensureMediaBytes(ctx context.Context, entry *mediaEntry) ([]byte, string, error) {
	if entry == nil {
		return nil, "", fmt.Errorf("media entry not found")
	}
	entry.fetchMu.Lock()
	defer entry.fetchMu.Unlock()

	if len(entry.Data) > 0 {
		return entry.Data, entry.MimeType, nil
	}

	acct := s.findAccountByID(entry.AccountID)
	if acct == nil {
		accts := s.Accounts()
		if len(accts) > 0 {
			acct = accts[0]
		}
	}
	if acct == nil {
		return nil, "", fmt.Errorf("account %s not available to fetch media", entry.AccountID)
	}

	client := s.clientFor(acct)
	dl, ok := client.(mediaDownloader)
	if !ok {
		return nil, "", fmt.Errorf("upstream client does not support media download")
	}

	data, ct, err := dl.DownloadMedia(ctx, entry.UpstreamURL, entry.FallbackURL)
	if err != nil {
		return nil, "", err
	}
	if ct != "" && ct != "application/octet-stream" {
		entry.MimeType = ct
	}
	entry.Data = data
	return entry.Data, entry.MimeType, nil
}

// HandleGetMedia serves a cached or proxied generated image/video at GET /v1/media/<id>.
func (s *Server) HandleGetMedia(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/media/")
	id = strings.TrimPrefix(id, "/media/")
	id = strings.TrimSpace(id)
	if id == "" {
		sendJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "media id required", "type": "not_found_error"}}, nil)
		return
	}

	s.mediaMu.RLock()
	var entry *mediaEntry
	if s.mediaItems != nil {
		entry = s.mediaItems[id]
	}
	s.mediaMu.RUnlock()

	if entry == nil {
		sendJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "media not found or expired: " + id, "type": "not_found_error"}}, nil)
		return
	}

	data, mimeType, err := s.ensureMediaBytes(r.Context(), entry)
	if err != nil {
		sendJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}}, nil)
		return
	}
	if mimeType == "" {
		if strings.HasSuffix(id, ".mp4") {
			mimeType = "video/mp4"
		} else {
			mimeType = "image/jpeg"
		}
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeContent(w, r, entry.ID, entry.CreatedAt, bytes.NewReader(data))
}

func (s *Server) rewriteFrameMedia(ctx context.Context, accountID string, frame *gemini.Frame) {
	if frame == nil || (len(frame.GeneratedImages) == 0 && len(frame.GeneratedVideos) == 0) {
		return
	}
	for i := range frame.GeneratedImages {
		rawURL := frame.GeneratedImages[i].URL
		if rawURL == "" || strings.Contains(rawURL, "/v1/media/") {
			continue
		}
		proxyURL, _ := s.registerMedia(ctx, accountID, rawURL, "", "image/jpeg", "jpg", frame.GeneratedImages[i].Title)
		if proxyURL != "" {
			frame.Text = strings.ReplaceAll(frame.Text, rawURL, proxyURL)
			frame.GeneratedImages[i].URL = proxyURL
		}
	}
	for i := range frame.GeneratedVideos {
		rawURL := frame.GeneratedVideos[i].URL
		if rawURL != "" && !strings.Contains(rawURL, "/v1/media/") {
			proxyURL, _ := s.registerMedia(ctx, accountID, rawURL, frame.GeneratedVideos[i].StreamURL, "video/mp4", "mp4", frame.GeneratedVideos[i].Title)
			if proxyURL != "" {
				frame.Text = strings.ReplaceAll(frame.Text, rawURL, proxyURL)
				frame.GeneratedVideos[i].URL = proxyURL
			}
		}
		thumbURL := frame.GeneratedVideos[i].ThumbnailURL
		if thumbURL != "" && !strings.Contains(thumbURL, "/v1/media/") {
			proxyThumb, _ := s.registerMedia(ctx, accountID, thumbURL, "", "image/jpeg", "jpg", frame.GeneratedVideos[i].Title)
			if proxyThumb != "" {
				frame.Text = strings.ReplaceAll(frame.Text, thumbURL, proxyThumb)
				frame.GeneratedVideos[i].ThumbnailURL = proxyThumb
			}
		}
	}
}

func hasServerSideMediaCalls(calls []toolbridge.Call) bool {
	for _, c := range calls {
		if c.Name == toolbridge.BuiltinMediaToolName {
			return true
		}
	}
	return false
}

type generatedMediaResult struct {
	MediaType     string      `json:"media_type"`
	URL           string      `json:"url"`
	ThumbnailURL  string      `json:"thumbnail_url,omitempty"`
	MimeType      string      `json:"mime_type"`
	RevisedPrompt string      `json:"revised_prompt,omitempty"`
	AspectRatio   string      `json:"aspect_ratio,omitempty"`
	Markdown      string      `json:"markdown"`
	Entry         *mediaEntry `json:"-"`
}

type mediaGenParams struct {
	MediaType   string
	Prompt      string
	AspectRatio string
	Format      string
	DurationSec int
	OutputPath  string
}

// supportedAspectRatios are the ratios the Gemini Web image generator accepts.
// They are passed to the model as prompt qualifiers, not as a structured API
// field, so anything outside this set has to be snapped to the nearest entry.
var supportedAspectRatios = []struct {
	label string
	value float64
}{
	{"1:1", 1.0},
	{"4:3", 4.0 / 3.0},
	{"3:4", 3.0 / 4.0},
	{"16:9", 16.0 / 9.0},
	{"9:16", 9.0 / 16.0},
}

// aspectRatioAliases maps loose client spellings onto a canonical ratio.
var aspectRatioAliases = map[string]string{
	"landscape": "16:9", "widescreen": "16:9", "horizontal": "16:9",
	"portrait": "9:16", "vertical": "9:16",
	"square": "1:1",
	"4:3":    "4:3", "3:4": "3:4",
	"16:9": "16:9", "9:16": "9:16", "1:1": "1:1",
}

// snapAspectRatio returns the supported ratio closest to the requested value.
func snapAspectRatio(value float64) string {
	best := supportedAspectRatios[0]
	bestDelta := math.Abs(value - best.value)
	for _, candidate := range supportedAspectRatios[1:] {
		if delta := math.Abs(value - candidate.value); delta < bestDelta {
			best, bestDelta = candidate, delta
		}
	}
	return best.label
}

// parseAspectRatioLabel canonicalises a "W:H" / alias string.
func parseAspectRatioLabel(raw string) (string, bool) {
	key := strings.TrimSpace(strings.ToLower(raw))
	if key == "" {
		return "", false
	}
	if canonical, ok := aspectRatioAliases[key]; ok {
		return canonical, true
	}
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 {
		return "", false
	}
	w, errW := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	h, errH := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return "", false
	}
	return snapAspectRatio(w / h), true
}

// parseSizeDimensions reads a "WxH" (or "W*H") size string.
func parseSizeDimensions(raw string) (float64, float64, bool) {
	key := strings.TrimSpace(strings.ToLower(raw))
	if key == "" {
		return 0, 0, false
	}
	normalised := strings.ReplaceAll(key, "*", "x")
	normalised = strings.ReplaceAll(normalised, "×", "x")
	parts := strings.SplitN(normalised, "x", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, errW := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	h, errH := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// resolveAspectRatio reconciles the two shape hints a client may send.
//
// aspect_ratio is authoritative; size is accepted as a convenience and is
// translated from its pixel dimensions rather than from a fixed allow-list, so
// any real ratio (1280x720, 2560x1440, 1200x630, ...) resolves correctly. When
// both are supplied and disagree, the request is rejected instead of silently
// dropping one of them.
func resolveAspectRatio(ar, size string) (string, error) {
	// OpenAI's images API allows size "auto" (gpt-image-1), and pass-through
	// clients forward it. It carries no shape information, so treat it — in
	// either field — as absent rather than rejecting the request.
	if strings.EqualFold(strings.TrimSpace(ar), "auto") {
		ar = ""
	}
	if strings.EqualFold(strings.TrimSpace(size), "auto") {
		size = ""
	}
	fromAR, hasAR := parseAspectRatioLabel(ar)

	var fromSize string
	hasSize := false
	if w, h, ok := parseSizeDimensions(size); ok {
		fromSize = snapAspectRatio(w / h)
		hasSize = true
	} else if strings.TrimSpace(size) != "" {
		return "", fmt.Errorf("invalid size %q: expected pixel dimensions such as \"1024x1024\"", size)
	}

	if strings.TrimSpace(ar) != "" && !hasAR {
		return "", fmt.Errorf("invalid aspect_ratio %q: expected one of 1:1, 4:3, 3:4, 16:9, 9:16", ar)
	}

	switch {
	case hasAR && hasSize && fromAR != fromSize:
		return "", fmt.Errorf("conflicting shape hints: aspect_ratio %q (%s) contradicts size %q (%s); send only one of them", ar, fromAR, size, fromSize)
	case hasAR:
		return fromAR, nil
	case hasSize:
		return fromSize, nil
	default:
		return "", nil
	}
}

func applyGoogleImageURLFormat(rawURL, format string) (string, string, string) {
	base := rawURL
	if idx := strings.LastIndex(base, "=s"); idx > strings.LastIndex(base, "/") {
		base = base[:idx]
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "png":
		return base + "=s0-rp", "image/png", "png"
	case "gif":
		return base, "image/gif", "gif"
	default:
		return base + "=s0-d", "image/jpeg", "jpg"
	}
}

func (s *Server) runMediaGenerationOnAccount(
	ctx context.Context,
	account *AccountState,
	model *gemini.ModelInfo,
	mediaType string,
	userPrompt string,
	extra ...mediaGenParams,
) ([]generatedMediaResult, string, error) {
	var params mediaGenParams
	if len(extra) > 0 {
		params = extra[0]
	}
	mType := strings.ToLower(strings.TrimSpace(mediaType))
	if mType != "video" {
		mType = "image"
	}
	cleanPrompt := strings.TrimSpace(userPrompt)
	if cleanPrompt == "" {
		return nil, "", transport.NewUpstreamError("prompt is required for media generation", 400, "invalid_request_error", nil, nil)
	}

	ar := params.AspectRatio
	fmtHint := strings.ToLower(strings.TrimSpace(params.Format))
	var qualifiers []string
	if ar != "" {
		qualifiers = append(qualifiers, "aspect ratio "+ar)
	}
	if mType == "image" && fmtHint == "gif" {
		qualifiers = append(qualifiers, "animated GIF")
	}
	if mType == "video" && params.DurationSec > 0 {
		qualifiers = append(qualifiers, fmt.Sprintf("%d seconds", params.DurationSec))
	}
	qualSuffix := ""
	if len(qualifiers) > 0 {
		qualSuffix = " (" + strings.Join(qualifiers, ", ") + ")"
	}

	genPrompt := "Generate an image" + qualSuffix + ": " + cleanPrompt
	if mType == "video" {
		genPrompt = "Generate a video" + qualSuffix + ": " + cleanPrompt
	}

	mediaModel := model
	if flash := resolveModelOnAccount(account, "gemini-flash"); flash != nil {
		mediaModel = flash
	}
	opts := gemini.StreamOptions{
		Model:           mediaModel,
		ThinkingLevel:   1,
		Temporary:       true,
		Language:        s.Cfg.Locale,
		DisableTools:    false,
		HasTools:        false,
		ProtocolMode:    "web",
		PersonalContext: "off",
	}
	client := s.clientFor(account)
	var (
		lastFrame gemini.Frame
		hasFrame  bool
	)
	upstream, err := client.StreamGenerateLive(ctx, genPrompt, opts, func(update *gemini.LineParseResult) error {
		for _, f := range update.Frames {
			if fmtHint == "png" || fmtHint == "gif" {
				for i := range f.GeneratedImages {
					raw := f.GeneratedImages[i].URL
					if raw != "" && !strings.Contains(raw, "/v1/media/") {
						formattedURL, mime, ext := applyGoogleImageURLFormat(raw, fmtHint)
						proxyURL, _ := s.registerMedia(ctx, account.ID, formattedURL, raw, mime, ext, f.GeneratedImages[i].Title)
						if proxyURL != "" {
							f.Text = strings.ReplaceAll(f.Text, raw, proxyURL)
							f.GeneratedImages[i].URL = proxyURL
						}
					}
				}
			}
			s.rewriteFrameMedia(ctx, account.ID, &f)
			lastFrame = f
			hasFrame = true
		}
		return nil
	})
	if err != nil && !hasFrame {
		return nil, "", err
	}
	if upstream != nil && upstream.Status != 200 && !hasFrame {
		return nil, "", transport.NewUpstreamError(fmt.Sprintf("upstream HTTP %d during media generation", upstream.Status), 502, "upstream_http_error", nil, nil)
	}

	var results []generatedMediaResult
	for _, img := range lastFrame.GeneratedImages {
		id := strings.TrimPrefix(img.URL, s.requestBaseURL(ctx)+"/v1/media/")
		s.mediaMu.RLock()
		entry := s.mediaItems[id]
		s.mediaMu.RUnlock()
		mime := "image/jpeg"
		if entry != nil {
			_, detectedMime, _ := s.ensureMediaBytes(ctx, entry)
			if detectedMime != "" {
				mime = detectedMime
			} else if entry.MimeType != "" {
				mime = entry.MimeType
			}
		}
		alt := img.Title
		if alt == "" {
			alt = "Generated Image"
		}
		revPrompt := img.RevisedPrompt
		if revPrompt == "" {
			revPrompt = img.Alt
		}
		if revPrompt == "" {
			revPrompt = cleanPrompt
		}
		results = append(results, generatedMediaResult{
			MediaType:     "image",
			URL:           img.URL,
			MimeType:      mime,
			RevisedPrompt: revPrompt,
			AspectRatio:   ar,
			Markdown:      fmt.Sprintf("![%s](%s)", alt, img.URL),
			Entry:         entry,
		})
	}
	for _, vid := range lastFrame.GeneratedVideos {
		id := strings.TrimPrefix(vid.URL, s.requestBaseURL(ctx)+"/v1/media/")
		s.mediaMu.RLock()
		entry := s.mediaItems[id]
		s.mediaMu.RUnlock()
		if entry != nil {
			_, _, _ = s.ensureMediaBytes(ctx, entry)
		}
		title := vid.Title
		if title == "" {
			title = "Generated Video"
		}
		md := fmt.Sprintf("[Download Video (%s)](%s)", title, vid.URL)
		if vid.ThumbnailURL != "" {
			md = fmt.Sprintf("[![%s](%s)](%s)\n\n%s", title, vid.ThumbnailURL, vid.URL, md)
		}
		results = append(results, generatedMediaResult{
			MediaType:     "video",
			URL:           vid.URL,
			ThumbnailURL:  vid.ThumbnailURL,
			MimeType:      "video/mp4",
			RevisedPrompt: cleanPrompt,
			AspectRatio:   ar,
			Markdown:      md,
			Entry:         entry,
		})
	}
	return results, lastFrame.Text, nil
}

// executeServerSideMediaCalls executes any BuiltinMediaToolName ("generate_media")
// calls inside interpreted.Calls and returns the formatted tool result block to
// append to curPrompt, plus any remaining client-side tool calls.
func (s *Server) executeServerSideMediaCalls(
	ctx context.Context,
	accts []*AccountState,
	scope AuthScope,
	account *AccountState,
	model *gemini.ModelInfo,
	calls []toolbridge.Call,
) (promptAddition string, clientCalls []toolbridge.Call, err error) {
	var mediaCalls []toolbridge.Call
	for _, c := range calls {
		if c.Name == toolbridge.BuiltinMediaToolName {
			mediaCalls = append(mediaCalls, c)
		} else {
			clientCalls = append(clientCalls, c)
		}
	}
	if len(mediaCalls) == 0 {
		return "", calls, nil
	}

	var toolOutputs []string
	for _, mc := range mediaCalls {
		mType, _ := mc.Arguments["media_type"].(string)
		mPrompt, _ := mc.Arguments["prompt"].(string)
		ar, _ := mc.Arguments["aspect_ratio"].(string)
		formatStr, _ := mc.Arguments["format"].(string)
		outPath, _ := mc.Arguments["output_path"].(string)
		durSec := 0
		if d, ok := mc.Arguments["duration"].(float64); ok {
			durSec = int(d)
		}
		mParams := mediaGenParams{
			MediaType:   mType,
			Prompt:      mPrompt,
			AspectRatio: ar,
			Format:      formatStr,
			DurationSec: durSec,
			OutputPath:  outPath,
		}
		items, replyText, genErr := s.runMediaGenerationOnAccount(ctx, account, model, mType, mPrompt, mParams)
		if (genErr != nil || len(items) == 0) && scope.AccountID == "" {
			originGroup := account.AccountGroup
			tried := map[string]bool{account.ID: true}
			fallbackModelName := model.ModelName
			if strings.EqualFold(strings.TrimSpace(mType), "video") {
				fallbackModelName = "gemini-flash"
			}
			for range len(accts) {
				nextAcct, nextModel := s.pickFallbackAccount(accts, tried, originGroup, fallbackModelName, nil)
				if nextAcct == nil || nextModel == nil {
					break
				}
				tried[nextAcct.ID] = true
				items, replyText, genErr = s.runMediaGenerationOnAccount(ctx, nextAcct, nextModel, mType, mPrompt, mParams)
				if genErr == nil && len(items) > 0 {
					break
				}
			}
		}
		var outObj map[string]any
		if genErr != nil {
			outObj = map[string]any{
				"status": "error",
				"error":  genErr.Error(),
			}
		} else if len(items) == 0 {
			outObj = map[string]any{
				"status":  "no_media_returned",
				"message": replyText,
			}
		} else {
			urls := make([]string, 0, len(items))
			mds := make([]string, 0, len(items))
			for _, it := range items {
				urls = append(urls, it.URL)
				mds = append(mds, it.Markdown)
			}
			baseName := filepath.Base(items[0].URL)
			localFile := strings.TrimSpace(outPath)
			if localFile == "" {
				localFile = fmt.Sprintf("./generated-%s-%s", items[0].MediaType, baseName)
			}
			outObj = map[string]any{
				"status":             "ok",
				"media_type":         items[0].MediaType,
				"mime_type":          items[0].MimeType,
				"url":                items[0].URL,
				"urls":               urls,
				"revised_prompt":     items[0].RevisedPrompt,
				"markdown":           strings.Join(mds, "\n\n"),
				"suggested_filename": localFile,
				"local_download_cmd": fmt.Sprintf("curl -fsSL %q -o %q", items[0].URL, localFile),
				"note":               "The media is ready at the URL above. IMPORTANT: If you have a bash/shell tool available, run local_download_cmd now (or save to the filename requested by the user) so the file is saved directly into the user's current working directory on their laptop, then include the saved filename and markdown link in your final reply.",
			}
			if items[0].AspectRatio != "" {
				outObj["aspect_ratio"] = items[0].AspectRatio
			}
			if items[0].ThumbnailURL != "" {
				outObj["thumbnail_url"] = items[0].ThumbnailURL
			}
		}
		b, _ := json.Marshal(outObj)
		callBytes, _ := json.Marshal(map[string]any{
			"tool_calls": []any{mc},
		})
		toolOutputs = append(toolOutputs, fmt.Sprintf(" %s\n\nUser: [Harness tool result for %s]:\n%s", string(callBytes), toolbridge.BuiltinMediaToolName, string(b)))
	}
	return strings.Join(toolOutputs, "\n\n") + "\n\nAssistant:", clientCalls, nil
}

// formatDirectMediaToolContinuation formats a continuation prompt block when
// Gemini Web's built-in media generator produces GeneratedImages or
// GeneratedVideos directly on a tool-enabled turn without a JSON tool_calls
// envelope, allowing the coding agent to save the generated file to disk via
// bash/curl without re-triggering image generation on Round 2.
func formatDirectMediaToolContinuation(images []gemini.GeneratedImage, videos []gemini.GeneratedVideo) string {
	var urls []string
	mediaType := "image"
	revPrompt := "generated media"
	for _, img := range images {
		if img.URL != "" {
			urls = append(urls, img.URL)
		}
		if img.RevisedPrompt != "" {
			revPrompt = img.RevisedPrompt
		} else if img.Alt != "" {
			revPrompt = img.Alt
		}
	}
	for _, vid := range videos {
		if vid.URL != "" {
			urls = append(urls, vid.URL)
			mediaType = "video"
		}
		if vid.Title != "" {
			revPrompt = vid.Title
		}
	}
	if len(urls) == 0 {
		return ""
	}
	baseName := filepath.Base(urls[0])
	localFile := fmt.Sprintf("./generated-%s-%s", mediaType, baseName)
	synthCallBytes, _ := json.Marshal(map[string]any{
		"tool_calls": []any{
			map[string]any{
				"name": toolbridge.BuiltinMediaToolName,
				"arguments": map[string]any{
					"media_type": mediaType,
					"prompt":     revPrompt,
				},
			},
		},
	})
	outObj := map[string]any{
		"status":             "ok",
		"media_type":         mediaType,
		"url":                urls[0],
		"urls":               urls,
		"revised_prompt":     revPrompt,
		"suggested_filename": localFile,
		"local_download_cmd": fmt.Sprintf("curl -fsSL %q -o %q", urls[0], localFile),
		"note":               "The media has ALREADY been generated and is ready at the URL above (do NOT generate another image). IMPORTANT: If you have a bash/shell tool available, emit a JSON tool_calls block calling bash to run local_download_cmd now so the file is saved directly into the user's current working directory on their laptop, then report the saved filename and URL in your final reply.",
	}
	b, _ := json.Marshal(outObj)
	return fmt.Sprintf(" %s\n\nUser: [Harness tool result for %s]:\n%s\n\nAssistant:", string(synthCallBytes), toolbridge.BuiltinMediaToolName, string(b))
}

// describeActualAspectRatio inspects the generated bytes and returns the real
// pixel dimensions plus the ratio they actually resolve to. The generator
// decides the final size, so reporting the request back would be a lie: a
// request for 16:9 can legitimately come back as 1376x768.
func (s *Server) describeActualAspectRatio(ctx context.Context, entry *mediaEntry, declared string) (width, height int, ratio string) {
	ratio = declared
	if entry == nil {
		return 0, 0, ratio
	}
	raw, _, err := s.ensureMediaBytes(ctx, entry)
	if err != nil || len(raw) == 0 {
		return 0, 0, ratio
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, ratio
	}
	width, height = cfg.Width, cfg.Height
	if snapped := snapAspectRatio(float64(width) / float64(height)); snapped != "" {
		ratio = snapped
	}
	return width, height, ratio
}

// HandleImageGeneration implements POST /v1/images/generations (OpenAI Images API).
func (s *Server) HandleImageGeneration(ctx context.Context, w http.ResponseWriter, body map[string]any, scope AuthScope) {
	s.handleMediaGenerationEndpoint(ctx, w, body, scope, "image")
}

// HandleVideoGeneration implements POST /v1/videos/generations.
func (s *Server) HandleVideoGeneration(ctx context.Context, w http.ResponseWriter, body map[string]any, scope AuthScope) {
	s.handleMediaGenerationEndpoint(ctx, w, body, scope, "video")
}

func (s *Server) handleMediaGenerationEndpoint(ctx context.Context, w http.ResponseWriter, body map[string]any, scope AuthScope, mediaType string) {
	if body == nil {
		sendChatError(w, false, transport.NewUpstreamError("request body must be a JSON object", 400, "invalid_request_error", nil, nil))
		return
	}
	promptStr, _ := body["prompt"].(string)
	promptStr = strings.TrimSpace(promptStr)
	if promptStr == "" {
		sendChatError(w, false, transport.NewUpstreamError("prompt is required", 400, "invalid_request_error", nil, nil))
		return
	}
	requestedModel, _ := body["model"].(string)
	respFormat, _ := body["response_format"].(string)
	respFormat = strings.ToLower(strings.TrimSpace(respFormat))
	sizeStr, _ := body["size"].(string)
	arStr, _ := body["aspect_ratio"].(string)
	outFmt, _ := body["output_format"].(string)
	if outFmt == "" {
		outFmt, _ = body["format"].(string)
	}
	durSec := 0
	if d, ok := body["duration"].(float64); ok {
		durSec = int(d)
	}
	resolvedAR, arErr := resolveAspectRatio(arStr, sizeStr)
	if arErr != nil {
		sendChatError(w, false, transport.NewUpstreamError(arErr.Error(), 400, "invalid_request_error", nil, nil))
		return
	}
	mParams := mediaGenParams{
		MediaType:   mediaType,
		Prompt:      promptStr,
		AspectRatio: resolvedAR,
		Format:      outFmt,
		DurationSec: durSec,
	}

	msgs := []map[string]any{
		{"role": "user", "content": promptStr},
	}
	prepBody := map[string]any{
		"model":    requestedModel,
		"messages": []any{msgs[0]},
	}
	if requestedModel == "" || strings.HasPrefix(strings.ToLower(requestedModel), "dall-e") || strings.HasPrefix(strings.ToLower(requestedModel), "gpt-image") || strings.HasPrefix(strings.ToLower(requestedModel), "imagen") || strings.HasPrefix(strings.ToLower(requestedModel), "veo") {
		prepBody["model"] = "gemini-pro"
	}

	prep, prepErr := s.prepareGeneration(ctx, prepBody, scope, msgs, nil, nil)
	if prepErr != nil {
		sendChatError(w, false, prepErr)
		return
	}

	items, replyText, err := s.runMediaGenerationOnAccount(ctx, prep.account, prep.model, mediaType, promptStr, mParams)
	if (err != nil || len(items) == 0) && scope.AccountID == "" {
		originGroup := prep.account.AccountGroup
		tried := map[string]bool{prep.account.ID: true}
		fallbackModelName := prep.model.ModelName
		if strings.EqualFold(strings.TrimSpace(mediaType), "video") {
			fallbackModelName = "gemini-flash"
		}
		for range len(prep.accts) {
			nextAcct, nextModel := s.pickFallbackAccount(prep.accts, tried, originGroup, fallbackModelName, nil)
			if nextAcct == nil || nextModel == nil {
				break
			}
			tried[nextAcct.ID] = true
			items, replyText, err = s.runMediaGenerationOnAccount(ctx, nextAcct, nextModel, mediaType, promptStr, mParams)
			if err == nil && len(items) > 0 {
				prep.account = nextAcct
				prep.model = nextModel
				break
			}
		}
	}
	if err != nil {
		sendChatError(w, false, err)
		return
	}
	if len(items) == 0 {
		msg := "upstream did not return generated " + mediaType
		if strings.TrimSpace(replyText) != "" {
			msg = strings.TrimSpace(replyText)
		}
		sendChatError(w, false, transport.NewUpstreamError(msg, 502, "media_generation_failed", nil, nil))
		return
	}

	prep.account.mu.Lock()
	prep.account.Fails = 0
	prep.account.LastUsed = s.now()
	prep.account.mu.Unlock()
	s.refreshQuotaAsync(prep.account, s.clientFor(prep.account))

	dataList := make([]map[string]any, 0, len(items))
	for _, it := range items {
		revPrompt := it.RevisedPrompt
		if revPrompt == "" {
			revPrompt = promptStr
		}
		itemMap := map[string]any{
			"url":            it.URL,
			"revised_prompt": revPrompt,
			"mime_type":      it.MimeType,
		}
		width, height, actualRatio := s.describeActualAspectRatio(ctx, it.Entry, it.AspectRatio)
		if actualRatio != "" {
			itemMap["aspect_ratio"] = actualRatio
		}
		if width > 0 && height > 0 {
			itemMap["width"] = width
			itemMap["height"] = height
			itemMap["size"] = fmt.Sprintf("%dx%d", width, height)
		}
		if it.ThumbnailURL != "" {
			itemMap["thumbnail_url"] = it.ThumbnailURL
		}
		if respFormat == "b64_json" && it.Entry != nil {
			if rawBytes, _, bErr := s.ensureMediaBytes(ctx, it.Entry); bErr == nil && len(rawBytes) > 0 {
				itemMap["b64_json"] = base64.StdEncoding.EncodeToString(rawBytes)
			}
		}
		dataList = append(dataList, itemMap)
	}

	sendJSON(w, http.StatusOK, map[string]any{
		"created": time.Now().Unix(),
		"data":    dataList,
	}, nil)
}
