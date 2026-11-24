package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"math"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/toolbridge"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

type requestBaseURLKey struct{}

func withRequestBaseURL(ctx context.Context, r *http.Request, defaultPort int) context.Context {
	if r == nil {
		return ctx
	}
	host := r.Host
	if defaultPort <= 0 {
		defaultPort = 20133
	}
	if _, _, ok := splitRequestAuthority(host); !ok {
		host = fmt.Sprintf("127.0.0.1:%d", defaultPort)
	}
	return context.WithValue(ctx, requestBaseURLKey{}, requestScheme(r)+"://"+host)
}

func (s *Server) requestBaseURL(ctx context.Context) string {
	if v, ok := ctx.Value(requestBaseURLKey{}).(string); ok && v != "" {
		return v
	}
	port := s.cfgSnapshot().Port
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

	fetchMu sync.Mutex // Allows only one upstream download per entry at a time.
	// Guarded by Server.mediaMu, including byte accounting on eviction.
	data        []byte
	contentType string
}

type mediaDownloader interface {
	DownloadMedia(ctx context.Context, rawURL, fallbackURL string) ([]byte, string, error)
}

const (
	maxCachedMediaEntries = 96
	maxCachedMediaBytes   = 128 << 20
	mediaTTL              = 24 * time.Hour
)

var errMediaExpired = errors.New("media not found or expired")

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func safeMediaFilename(raw, fallback string) string {
	clean := func(name string) string {
		name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
		return strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
				return -1
			}
			return r
		}, name))
	}
	name := clean(raw)
	if name == "" || name == "." || name == ".." || name == "/" || name == "-" || len(name) > 240 {
		name = clean(fallback)
	}
	if name == "" || name == "." || name == ".." || name == "/" || name == "-" || len(name) > 240 {
		return "generated-media"
	}
	return name
}

// mediaDownloadCommand allows an HTTP(S) media URL and a single local filename.
// Ignore curl config, disable globbing, and never follow redirects.
func mediaDownloadCommand(rawURL, filename string) string {
	if strings.ContainsFunc(rawURL, unicode.IsControl) {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return ""
	}
	if _, _, ok := splitRequestAuthority(u.Host); !ok {
		return ""
	}
	path, ok := strings.CutPrefix(u.Path, "/v1/media/")
	if !ok || path == "" || path == "." || path == ".." || strings.ContainsAny(path, "/\\") {
		return ""
	}
	return "curl -q -fsS --globoff --proto '=http,https' --no-clobber -o " + shellSingleQuote(safeMediaFilename(filename, "generated-media")) + " -- " + shellSingleQuote(u.String())
}

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
	now := s.now()
	s.trimMediaCacheLocked(now, 0, "")
	entry := s.mediaItems[id]
	if entry == nil {
		entry = &mediaEntry{
			ID:          id,
			AccountID:   accountID,
			UpstreamURL: upstreamURL,
			FallbackURL: fallbackURL,
			MimeType:    mimeType,
			Ext:         ext,
			Title:       title,
			CreatedAt:   now,
		}
		s.mediaItems[id] = entry
		s.mediaOrder = append(s.mediaOrder, id)
		s.trimMediaCacheLocked(now, 0, id)
	}
	s.mediaMu.Unlock()

	return proxyURL, entry
}

// trimMediaCacheLocked removes expired entries, then evicts oldest entries
// until both limits have room. Keep the entry whose bytes are being added.
func (s *Server) trimMediaCacheLocked(now time.Time, incoming int, keep string) {
	for i := 0; i < len(s.mediaOrder); {
		id := s.mediaOrder[i]
		entry := s.mediaItems[id]
		if !now.Before(entry.CreatedAt.Add(mediaTTL)) || id != keep && (len(s.mediaOrder) > maxCachedMediaEntries || s.mediaBytes+incoming > maxCachedMediaBytes) {
			s.mediaBytes -= len(entry.data)
			entry.data = nil
			delete(s.mediaItems, id)
			s.mediaOrder = slices.Delete(s.mediaOrder, i, i+1)
			continue
		}
		i++
	}
}

func (s *Server) ensureMediaBytes(ctx context.Context, entry *mediaEntry) ([]byte, string, error) {
	if entry == nil {
		return nil, "", fmt.Errorf("media entry not found")
	}
	entry.fetchMu.Lock()
	defer entry.fetchMu.Unlock()

	s.mediaMu.RLock()
	data, ct := entry.data, entry.contentType
	registered := s.mediaItems[entry.ID] == entry
	s.mediaMu.RUnlock()
	if !registered || !s.now().Before(entry.CreatedAt.Add(mediaTTL)) {
		return nil, "", errMediaExpired
	}
	if len(data) > 0 {
		return data, ct, nil
	}

	acct := s.findAccountByID(entry.AccountID)
	if acct == nil {
		return nil, "", fmt.Errorf("account %s not available to fetch media", entry.AccountID)
	}

	if err := accountBlockedError(acct); err != nil {
		return nil, "", err
	}
	client := s.clientFor(acct)
	dl, ok := client.(mediaDownloader)
	if !ok {
		return nil, "", fmt.Errorf("upstream client does not support media download")
	}

	var err error
	data, ct, err = dl.DownloadMedia(ctx, entry.UpstreamURL, entry.FallbackURL)
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("media download returned an empty body")
	}
	ct = safeMediaContentType(data, ct)
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	now := s.now()
	if !now.Before(entry.CreatedAt.Add(mediaTTL)) {
		s.trimMediaCacheLocked(now, 0, "")
		return nil, "", errMediaExpired
	}
	if len(data) <= maxCachedMediaBytes && s.mediaItems[entry.ID] == entry {
		s.trimMediaCacheLocked(now, len(data), entry.ID)
		entry.data, entry.contentType = data, ct
		s.mediaBytes += len(data)
	}
	return data, ct, nil
}

// HandleGetMedia serves a cached or proxied generated image/video at GET /v1/media/<id>.
func (s *Server) HandleGetMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/media/")
	id = strings.TrimPrefix(id, "/media/")
	id = strings.TrimSpace(id)
	if id == "" {
		sendJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "media id required", "type": "not_found_error"}}, nil)
		return
	}

	s.mediaMu.Lock()
	s.trimMediaCacheLocked(s.now(), 0, "")
	entry := s.mediaItems[id]
	s.mediaMu.Unlock()

	if entry == nil {
		sendJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "media not found or expired: " + id, "type": "not_found_error"}}, nil)
		return
	}

	data, mimeType, err := s.ensureMediaBytes(r.Context(), entry)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errMediaExpired) {
			status = http.StatusNotFound
		}
		if !errors.Is(err, errMediaExpired) {
			host := ""
			if u, perr := url.Parse(entry.UpstreamURL); perr == nil {
				host = u.Hostname()
			}
			log.Printf("[media] fetch failed id=%s account=%s upstream_host=%s: %v", entry.ID, entry.AccountID, host, err)
		}
		sendJSON(w, status, map[string]any{"error": map[string]any{"message": http.StatusText(status)}}, nil)
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if mimeType == "application/octet-stream" {
		w.Header().Set("Content-Disposition", "attachment")
	}
	http.ServeContent(w, r, entry.ID, entry.CreatedAt, bytes.NewReader(data))
}

func safeMediaContentType(data []byte, declared string) string {
	const binary = "application/octet-stream"
	ct, _, err := mime.ParseMediaType(declared)
	if declared == "" || ct == binary {
		ct = http.DetectContentType(data)
	} else if err != nil {
		return binary
	}
	switch ct {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/avif", "video/mp4", "video/webm":
		return ct
	default:
		return binary
	}
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
		upstreamURL := rawURL
		if frame.GeneratedImages[i].RawURL != "" {
			upstreamURL = frame.GeneratedImages[i].RawURL
		}
		proxyURL, _ := s.registerMedia(ctx, accountID, upstreamURL, "", "image/jpeg", "jpg", frame.GeneratedImages[i].Title)
		if proxyURL != "" {
			frame.Text = strings.ReplaceAll(frame.Text, rawURL, proxyURL)
			if frame.GeneratedImages[i].RawURL != "" {
				frame.Text = strings.ReplaceAll(frame.Text, frame.GeneratedImages[i].RawURL, proxyURL)
			}
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
	DownloadError error       `json:"-"`
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
	query := ""
	if idx := strings.IndexByte(base, '?'); idx >= 0 {
		base, query = base[:idx], base[idx:]
	}
	if idx := strings.LastIndex(base, "=s"); idx > strings.LastIndex(base, "/") {
		base = base[:idx]
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "png":
		return base + "=s0-rp" + query, "image/png", "png"
	case "gif":
		return base + query, "image/gif", "gif"
	default:
		return base + "=s1024-rw" + query, "image/jpeg", "jpg"
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

	cfg := s.cfgSnapshot()
	mediaModel := model
	opts := gemini.StreamOptions{
		Model:           mediaModel,
		ThinkingLevel:   1,
		Temporary:       true,
		Language:        cfg.Locale,
		DisableTools:    false,
		HasTools:        false,
		ProtocolMode:    "web",
		PersonalContext: "off",
		NoFrameTimeout:  cfg.NoFrameTimeout,
	}
	finish, err := s.beginAuxStream(account)
	if err != nil {
		return nil, "", err
	}
	defer finish()
	client := s.clientFor(account)
	var (
		lastFrame gemini.Frame
	)
	upstream, err := client.StreamGenerateLive(ctx, genPrompt, opts, func(update *gemini.LineParseResult) error {
		for _, f := range update.Frames {
			if fmtHint == "png" || fmtHint == "gif" {
				for i := range f.GeneratedImages {
					raw := f.GeneratedImages[i].URL
					if raw != "" && !strings.Contains(raw, "/v1/media/") {
						upstreamURL := raw
						if f.GeneratedImages[i].RawURL != "" {
							upstreamURL = f.GeneratedImages[i].RawURL
						}
						formattedURL, mime, ext := applyGoogleImageURLFormat(upstreamURL, fmtHint)
						proxyURL, _ := s.registerMedia(ctx, account.ID, formattedURL, upstreamURL, mime, ext, f.GeneratedImages[i].Title)
						if proxyURL != "" {
							f.Text = strings.ReplaceAll(f.Text, raw, proxyURL)
							if f.GeneratedImages[i].RawURL != "" {
								f.Text = strings.ReplaceAll(f.Text, f.GeneratedImages[i].RawURL, proxyURL)
							}
							f.GeneratedImages[i].URL = proxyURL
						}
					}
				}
			}
			s.rewriteFrameMedia(ctx, account.ID, &f)
			lastFrame = f
		}
		return nil
	})
	if err == nil && upstream != nil && upstream.Status != http.StatusOK {
		err = transport.NewUpstreamError(fmt.Sprintf("upstream HTTP %d during media generation", upstream.Status), 502, "upstream_http_error", nil, nil)
	}
	if err != nil {
		s.recordAuxStreamError(ctx, account, client, err)
		return nil, "", err
	}

	var results []generatedMediaResult
	for _, img := range lastFrame.GeneratedImages {
		id := strings.TrimPrefix(img.URL, s.requestBaseURL(ctx)+"/v1/media/")
		s.mediaMu.RLock()
		entry := s.mediaItems[id]
		s.mediaMu.RUnlock()
		mime := "image/jpeg"
		var downloadErr error
		if entry != nil {
			_, detectedMime, fetchErr := s.ensureMediaBytes(ctx, entry)
			downloadErr = fetchErr
			if fetchErr != nil {
				log.Printf("[media] image download failed account=%s: %v", account.ID, fetchErr)
			}
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
			DownloadError: downloadErr,
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
	if len(results) > 0 {
		s.recordAuxStreamSuccess(account, client)
	} else {
		s.recordAuxStreamError(ctx, account, client, errors.New("upstream did not return generated "+mType))
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
			localFile := safeMediaFilename(outPath, fmt.Sprintf("generated-%s-%s", items[0].MediaType, baseName))
			outObj = map[string]any{
				"status":             "ok",
				"media_type":         items[0].MediaType,
				"mime_type":          items[0].MimeType,
				"url":                items[0].URL,
				"urls":               urls,
				"revised_prompt":     items[0].RevisedPrompt,
				"markdown":           strings.Join(mds, "\n\n"),
				"suggested_filename": localFile,
			}
			if cmd := mediaDownloadCommand(items[0].URL, localFile); cmd != "" {
				outObj["local_download_cmd"] = cmd
				outObj["note"] = "The media is ready. If the user wants a local copy, use local_download_cmd to save it in the current working directory."
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
	localFile := safeMediaFilename("", fmt.Sprintf("generated-%s-%s", mediaType, baseName))
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
	}
	if cmd := mediaDownloadCommand(urls[0], localFile); cmd != "" {
		outObj["local_download_cmd"] = cmd
		outObj["note"] = "The media is already generated. If the user wants a local copy, use local_download_cmd; do not generate the media again."
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
		// The client named an image or video model this bridge does not serve, or
		// named none. Leave the model unset so prepareGeneration resolves it from
		// the account's live registry; runMediaGenerationOnAccount then picks the
		// entry that can actually produce media. Naming one here went stale on the
		// last upstream rename.
		delete(prepBody, "model")
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

	dataList := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if respFormat == "b64_json" && it.DownloadError != nil {
			sendChatError(w, false, transport.NewUpstreamError("generated media could not be downloaded for b64_json", 502, "media_download_failed", nil, nil))
			return
		}
		revPrompt := it.RevisedPrompt
		if revPrompt == "" {
			revPrompt = promptStr
		}
		itemMap := map[string]any{
			"url":            it.URL,
			"revised_prompt": revPrompt,
			"mime_type":      it.MimeType,
		}
		width, height, actualRatio := 0, 0, it.AspectRatio
		if it.DownloadError == nil {
			width, height, actualRatio = s.describeActualAspectRatio(ctx, it.Entry, it.AspectRatio)
		}
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
		if respFormat == "b64_json" {
			rawBytes, _, bErr := s.ensureMediaBytes(ctx, it.Entry)
			if bErr != nil || len(rawBytes) == 0 {
				log.Printf("[media] base64 download failed account=%s: %v", prep.account.ID, bErr)
				sendChatError(w, false, transport.NewUpstreamError("generated media could not be downloaded for b64_json", 502, "media_download_failed", nil, nil))
				return
			}
			itemMap["b64_json"] = base64.StdEncoding.EncodeToString(rawBytes)
		}
		dataList = append(dataList, itemMap)
	}

	sendJSON(w, http.StatusOK, map[string]any{
		"created": time.Now().Unix(),
		"data":    dataList,
	}, nil)
}
