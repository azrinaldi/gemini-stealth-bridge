package gemini

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
	"github.com/google/uuid"
)

var videoPromptHintRe = regexp.MustCompile(`(?i)\b(video|videos|veo|mp4|clip|clips|animasi|animation|footage)\b`)

// PromptRequestsVideo reports whether prompt likely asks Gemini Web to generate a video.
func PromptRequestsVideo(prompt string) bool {
	return videoPromptHintRe.MatchString(prompt)
}

// UploadedFile represents the [[uploadPath], safeName] tuple expected in inner[0][3].
type UploadedFile struct {
	Path     string
	Filename string
}

// StreamOptions configures a StreamGenerate request.
type StreamOptions struct {
	Model           *ModelInfo
	ThinkingLevel   int
	Temporary       bool
	Language        string
	Metadata        []any // 10-element [cid, rid, rcid, null, null, null, null, null, null, contextToken]
	Files           []UploadedFile
	DisableTools    bool
	HasTools        bool
	ProtocolMode    string // "bridge" | "web"
	PersonalContext string // "auto" | "off" | "on"
	FreshSocket     bool
}

// StreamResponse captures the final HTTP status, headers, and conversation state from StreamGenerateLive.
type StreamResponse struct {
	Status           int
	Headers          map[string]string
	Empty            bool
	CID              string
	RID              string
	Body             string
	GroundingSources []GroundingSource
}

// UploadFile uploads a binary file or image to Google's content-push service
// (with fallback to 2-step Scotrup resumable upload) and returns an UploadedFile.
func (ac *AccountClient) UploadFile(ctx context.Context, data []byte, filename, contentType string) (UploadedFile, error) {
	if len(data) == 0 {
		return UploadedFile{}, transport.NewUpstreamError("uploadFile requires a non-empty Buffer", 400, "invalid_request_error", nil, nil)
	}
	sess, err := ac.acquireSession(ctx)
	if err != nil {
		return UploadedFile{}, err
	}
	var uploaded UploadedFile
	err = sess.Lease(ctx, func(leasedCookie string) error {
		if leasedCookie != "" {
			ac.mu.Lock()
			ac.cookieHeader = leasedCookie
			ac.mu.Unlock()
		}
		var uErr error
		uploaded, uErr = ac.uploadFileLocked(ctx, sess.Client, data, filename, contentType)
		return uErr
	})
	return uploaded, err
}

func (ac *AccountClient) uploadFileLocked(ctx context.Context, sc *transport.StealthClient, data []byte, filename, contentType string) (UploadedFile, error) {
	if filename == "" {
		filename = "image.png"
	}
	safeName := strings.Map(func(r rune) rune {
		if r == '"' || r == '\r' || r == '\n' {
			return '_'
		}
		return r
	}, filename)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	ac.mu.RLock()
	pushID := ac.pushID
	pctx := ac.pctx
	cookie := ac.cookieHeader
	ac.mu.RUnlock()
	if pushID == "" {
		pushID = defaultPushID
	}

	var randHex [8]byte
	_, _ = rand.Read(randHex[:])
	boundary := "----GeminiBridgeUpload" + hex.EncodeToString(randHex[:])

	var body bytes.Buffer
	fmt.Fprintf(&body, "--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\nContent-Type: %s\r\n\r\n", boundary, safeName, contentType)
	body.Write(data)
	fmt.Fprintf(&body, "\r\n--%s--\r\n", boundary)

	res, err := sc.Do(ctx, transport.RequestOptions{
		Method: "POST",
		URL:    "https://content-push.googleapis.com/upload",
		Kind:   transport.ReqKindCORS,
		Headers: map[string]string{
			"cookie":         cookie,
			"origin":         Origin,
			"referer":        Origin + "/",
			"sec-fetch-site": "cross-site",
			"x-tenant-id":    defaultTenant,
			"push-id":        pushID,
			"content-type":   "multipart/form-data; boundary=" + boundary,
		},
		Body:    body.Bytes(),
		Timeout: 60 * time.Second,
	})
	if err == nil {
		ac.absorbAndPersistCookies(res.SetCookies)
		trimmed := strings.TrimSpace(res.Body)
		if res.Status == 200 && strings.HasPrefix(trimmed, "/") {
			return UploadedFile{Path: trimmed, Filename: safeName}, nil
		}
	}

	// Fallback path: 2-step Scotrup resumable upload via push.clients6.google.com/upload/
	initHeaders := map[string]string{
		"cookie":                              cookie,
		"origin":                              Origin,
		"referer":                             Origin + "/",
		"sec-fetch-site":                      "same-site",
		"x-tenant-id":                         defaultTenant,
		"push-id":                             pushID,
		"x-goog-upload-command":               "start",
		"x-goog-upload-protocol":              "resumable",
		"x-goog-upload-header-content-length": strconv.Itoa(len(data)),
		"content-type":                        "application/x-www-form-urlencoded;charset=UTF-8",
	}
	if pctx != "" {
		initHeaders["x-client-pctx"] = pctx
	}
	initRes, initErr := sc.Do(ctx, transport.RequestOptions{
		Method:  "POST",
		URL:     "https://push.clients6.google.com/upload/",
		Kind:    transport.ReqKindCORS,
		Headers: initHeaders,
		Body:    []byte("File name: " + safeName),
		Timeout: 30 * time.Second,
	})
	if initErr == nil {
		ac.absorbAndPersistCookies(initRes.SetCookies)
		uploadURL := initRes.Headers["x-goog-upload-url"]
		if initRes.Status == 200 && uploadURL != "" {
			putHeaders := map[string]string{
				"cookie":                cookie,
				"origin":                Origin,
				"referer":               Origin + "/",
				"sec-fetch-site":        "same-site",
				"x-tenant-id":           defaultTenant,
				"push-id":               pushID,
				"x-goog-upload-command": "upload, finalize",
				"x-goog-upload-offset":  "0",
				"content-type":          "application/x-www-form-urlencoded;charset=UTF-8",
			}
			if pctx != "" {
				putHeaders["x-client-pctx"] = pctx
			}
			putRes, putErr := sc.Do(ctx, transport.RequestOptions{
				Method:  "POST",
				URL:     uploadURL,
				Kind:    transport.ReqKindCORS,
				Headers: putHeaders,
				Body:    data,
				Timeout: 60 * time.Second,
			})
			if putErr == nil {
				ac.absorbAndPersistCookies(putRes.SetCookies)
				trimmed := strings.TrimSpace(putRes.Body)
				if putRes.Status == 200 && strings.HasPrefix(trimmed, "/") {
					return UploadedFile{Path: trimmed, Filename: safeName}, nil
				}
			}
		}
	}

	if err != nil {
		return UploadedFile{}, err
	}
	status := 502
	code := "upstream_upload_error"
	if res.Status == 401 || res.Status == 403 {
		status = 401
		code = "upstream_auth_error"
	}
	return UploadedFile{}, transport.NewUpstreamError(
		fmt.Sprintf("file upload failed with HTTP %d", res.Status),
		status,
		code,
		res.Headers,
		nil,
	)
}

// BuildInnerRequest constructs the 81-slot StreamGenerate inner payload, uuid,
// and x-goog-ext-525001261-jspb header value matching gemini-web.js.
func (ac *AccountClient) BuildInnerRequest(prompt string, opts StreamOptions) (innerJSON string, uuidVal string, modelHeader string) {
	ac.mu.RLock()
	fSid := ac.fSid
	defLang := ac.locale
	defProto := ac.protocolMode
	ac.mu.RUnlock()

	lang := opts.Language
	if lang == "" {
		lang = defLang
	}
	if lang == "" {
		lang = "en"
	}
	protoMode := opts.ProtocolMode
	if protoMode == "" {
		protoMode = defProto
	}
	meta := opts.Metadata
	if len(meta) == 0 {
		meta = []any{"", "", "", nil, nil, nil, nil, nil, nil, ""}
	}

	var filesSlot any
	if len(opts.Files) > 0 {
		fArr := make([]any, 0, len(opts.Files))
		for _, f := range opts.Files {
			fArr = append(fArr, []any{[]string{f.Path}, f.Filename})
		}
		filesSlot = fArr
	}

	inner := make([]any, 81)
	inner[0] = []any{prompt, 0, nil, filesSlot, nil, nil, 0}
	inner[1] = []string{lang}
	inner[2] = meta
	inner[6] = []int{1}
	inner[7] = 1  // enable_token_streaming
	inner[10] = 1 // request_source
	isPureWeb := strings.EqualFold(protoMode, "web") && !opts.HasTools && !opts.DisableTools
	if !isPureWeb && (opts.DisableTools || opts.HasTools || !strings.EqualFold(protoMode, "web")) {
		if opts.DisableTools || opts.HasTools {
			inner[11] = 1 // disable_tools (strip consumer Workspace/YouTube/Maps extensions)
		} else {
			inner[11] = 0
		}
	} else {
		inner[11] = 0
	}
	inner[17] = [][]int{{0}}
	inner[18] = 0
	inner[27] = 1 // return_request_routing_early
	inner[30] = []int{4}
	inner[41] = []int{1}
	if opts.Temporary && (inner[11] != 0 || !PromptRequestsVideo(prompt)) {
		inner[45] = 1
	}
	inner[53] = 0
	inner[61] = []any{}
	switch strings.ToLower(strings.TrimSpace(opts.PersonalContext)) {
	case "on":
		inner[67] = 0
		inner[68] = 0
	case "off":
		inner[67] = 1
		inner[68] = 1
	default:
		if isPureWeb {
			inner[67] = 0
			inner[68] = 0
		} else {
			inner[67] = 1 // is_personal_context_disabled
			inner[68] = 1
		}
	}
	if len(meta) >= 10 {
		cid, _ := meta[0].(string)
		ctxTok, _ := meta[9].(string)
		if cid != "" && ctxTok != "" {
			inner[71] = []any{cid, nil, nil, nil, nil, nil, nil, ctxTok}
		}
	}
	inner[79] = 1
	thinkLvl := opts.ThinkingLevel
	if thinkLvl <= 0 {
		thinkLvl = 1
	}
	inner[80] = thinkLvl

	uuidVal = strings.ToUpper(uuid.NewString())
	inner[59] = uuidVal

	if opts.Model != nil {
		if opts.Model.ModelNumber > 0 {
			inner[79] = opts.Model.ModelNumber
		}
		modelHeader = BuildModelHeader(*opts.Model, thinkLvl, fSid)
	}

	b, _ := json.Marshal(inner)
	return string(b), uuidVal, modelHeader
}

// StreamGenerateLive executes a live StreamGenerate request over the account's
// multiplexed HTTP/2 session, invoking onFrame as lines arrive.
func (ac *AccountClient) StreamGenerateLive(
	ctx context.Context,
	prompt string,
	opts StreamOptions,
	onFrame func(update *LineParseResult) error,
) (*StreamResponse, error) {
	sess, err := ac.acquireSession(ctx)
	if err != nil {
		return nil, err
	}

	var result *StreamResponse
	err = sess.RunStream(ctx, func(leasedCookie string) error {
		var runErr error
		result, runErr = ac.streamGenerateLocked(ctx, sess.Client, leasedCookie, prompt, opts, onFrame)
		return runErr
	})
	return result, err
}

func (ac *AccountClient) streamGenerateLocked(
	ctx context.Context,
	sc *transport.StealthClient,
	leasedCookie string,
	prompt string,
	opts StreamOptions,
	onFrame func(update *LineParseResult) error,
) (*StreamResponse, error) {
	if err := ac.ensureBootstrapped(ctx, sc); err != nil {
		return nil, err
	}

	innerJSON, uuidVal, modelHeader := ac.BuildInnerRequest(prompt, opts)

	startGen := ac.cookieGen.Load()
	ac.mu.RLock()
	at := ac.at
	bl := ac.bl
	fSid := ac.fSid
	cookie := ac.cookieHeader
	defLang := ac.locale
	authUser := ac.authUser
	ac.mu.RUnlock()
	if leasedCookie != "" {
		cookie = leasedCookie
	}

	lang := opts.Language
	if lang == "" {
		lang = defLang
	}
	if lang == "" {
		lang = "en"
	}
	q := url.Values{}
	q.Set("hl", lang)
	q.Set("_reqid", strconv.FormatInt(ac.nextReqID(), 10))
	if authUser > 0 {
		q.Set("authuser", strconv.Itoa(authUser))
	}
	if bl != "" {
		q.Set("bl", bl)
	}
	if fSid != "" {
		q.Set("f.sid", fSid)
	}

	freqBytes, _ := json.Marshal([]any{nil, innerJSON})
	form := url.Values{}
	form.Set("at", at)
	form.Set("f.req", string(freqBytes))

	headers := map[string]string{
		"content-type":              "application/x-www-form-urlencoded;charset=utf-8",
		"x-same-domain":             "1",
		"x-goog-ext-525005358-jspb": fmt.Sprintf(`[%q,1]`, uuidVal),
		"x-goog-ext-73010989-jspb":  "[0]",
		"x-goog-ext-73010990-jspb":  "[0,0,0]",
		"cookie":                    cookie,
	}
	if modelHeader != "" {
		headers["x-goog-ext-525001261-jspb"] = modelHeader
	}
	var (
		lineBuf         strings.Builder
		inOuterArray    bool
		cid, rid        string
		asyncVideoCID   string
		frameCount      int
		sawDone         bool
		sawContextToken bool
		sawStreamEnd    bool
		grounding       []GroundingSource
	)

	emitLine := func(line string) error {
		parsed, err := ParseFrameLine(line)
		if err != nil {
			return err
		}
		if parsed == nil {
			return nil
		}
		if parsed.CID != "" {
			cid = parsed.CID
		} else {
			parsed.CID = cid
		}
		if parsed.RID != "" {
			rid = parsed.RID
		} else {
			parsed.RID = rid
		}
		if parsed.AsyncVideoCID != "" {
			asyncVideoCID = parsed.AsyncVideoCID
		}
		for _, f := range parsed.Frames {
			if f.Done {
				sawDone = true
			}
		}
		if sawDone && parsed.ContextToken != "" {
			sawContextToken = true
		}
		if parsed.StreamEnd {
			sawStreamEnd = true
		}
		frameCount += len(parsed.Frames)
		grounding = MergeGroundingSources(grounding, parsed.GroundingSources)
		if onFrame != nil && (len(parsed.Frames) > 0 || parsed.AnsweredModelID != "" || parsed.MediaToolActive || parsed.CID != "" || parsed.RID != "" || parsed.ContextToken != "") {
			return onFrame(parsed)
		}
		return nil
	}

	res, err := sc.Do(ctx, transport.RequestOptions{
		Method:      "POST",
		URL:         Origin + "/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?" + q.Encode(),
		Kind:        transport.ReqKindCORS,
		Headers:     headers,
		Body:        []byte(form.Encode()),
		FreshSocket: opts.FreshSocket,
		OnChunk: func(chunk string) error {
			lineBuf.WriteString(chunk)
			envelopes, rem, nextInOuter := extractStreamEnvelopes(lineBuf.String(), inOuterArray, false)
			inOuterArray = nextInOuter
			lineBuf.Reset()
			lineBuf.WriteString(rem)
			for _, env := range envelopes {
				if err := emitLine(env); err != nil {
					return err
				}
				if sawStreamEnd || (sawDone && sawContextToken) {
					return transport.ErrStopStreaming
				}
			}
			return nil
		},
	})
	if err != nil {
		var ue *transport.UpstreamError
		if errors.As(err, &ue) {
			return nil, ue
		}
		return nil, err
	}
	ac.absorbAndPersistCookiesWithGen(res.SetCookies, startGen, true)

	if res.Status != 200 {
		if res.Status == 403 && (strings.Contains(res.Body, "TOS_VIOLATION") || strings.Contains(res.Body, "PERMISSION_DENIED")) {
			return nil, classifyUpstreamAuthOrBan(res.Status, res.Headers, res.Body, "account_banned")
		}
		if res.Status == 302 || res.Status == 301 || res.Status == 303 || res.Status == 307 || res.Status == 308 {
			return nil, classifyUpstreamAuthOrBan(res.Status, res.Headers, res.Body, "auth_redirect")
		}
	}

	if res.Status == 200 {
		if tail := strings.TrimSpace(lineBuf.String()); tail != "" {
			envelopes, _, _ := extractStreamEnvelopes(tail, inOuterArray, true)
			for _, env := range envelopes {
				if err := emitLine(env); err != nil {
					var ue *transport.UpstreamError
					if errors.As(err, &ue) && ue.Headers == nil {
						ue.Headers = res.Headers
					}
					return nil, err
				}
			}
		}
		if frameCount == 0 && asyncVideoCID != "" {
			// If Temporary was enabled and inner[45] was 1, Google does not persist the conversation for hNvQHb polling.
			// Re-run once with Temporary=false and clean up the conversation afterward.
			usedTemp := opts.Temporary && !PromptRequestsVideo(prompt)
			if usedTemp {
				retryOpts := opts
				retryOpts.Temporary = false
				retryRes, retryErr := ac.streamGenerateLocked(ctx, sc, leasedCookie, prompt, retryOpts, onFrame)
				if retryRes != nil && retryRes.CID != "" {
					go ac.deleteConversationAsync(retryRes.CID)
				}
				return retryRes, retryErr
			}
			polled, pollErr := ac.pollAsyncVideoGenerationLocked(ctx, sc, cookie, at, bl, fSid, lang, asyncVideoCID)
			if pollErr != nil {
				if opts.Temporary {
					go ac.deleteConversationAsync(asyncVideoCID)
				}
				return nil, pollErr
			}
			if polled != nil && len(polled.Frames) > 0 {
				frameCount += len(polled.Frames)
				if onFrame != nil {
					if err := onFrame(polled); err != nil && !errors.Is(err, transport.ErrStopStreaming) {
						if opts.Temporary {
							go ac.deleteConversationAsync(asyncVideoCID)
						}
						return nil, err
					}
				}
			}
			if opts.Temporary {
				go ac.deleteConversationAsync(asyncVideoCID)
			}
		}
	}

	return &StreamResponse{
		Status:           res.Status,
		Headers:          res.Headers,
		Empty:            frameCount == 0,
		CID:              cid,
		RID:              rid,
		Body:             res.Body,
		GroundingSources: grounding,
	}, nil
}

// extractStreamEnvelopes incrementally extracts complete top-level ["wrb.fr",...],
// ["er",...], ["e",...], or ["di",...] JSON array elements from a StreamGenerate
// chunk buffer, supporting both single outer array streaming (with no newlines
// between elements) and newline/length-delimited outer arrays.
func extractStreamEnvelopes(buf string, inOuter bool, flushTail bool) (envelopes []string, rem string, nextInOuter bool) {
	pos := 0
	n := len(buf)
	for pos < n {
		if !inOuter {
			for pos < n && (buf[pos] == ' ' || buf[pos] == '\t' || buf[pos] == '\r' || buf[pos] == '\n') {
				pos++
			}
			if pos >= n {
				return envelopes, "", false
			}
			if strings.HasPrefix(buf[pos:], ")]}'") {
				pos += 4
				continue
			}
			if strings.HasPrefix(buf[pos:], ")]}") && (pos+3 < n || flushTail) {
				if pos+3 >= n || buf[pos+3] != '\'' {
					pos += 3
					continue
				}
			}
			if buf[pos] >= '0' && buf[pos] <= '9' {
				nl := strings.IndexByte(buf[pos:], '\n')
				if nl < 0 {
					if !flushTail {
						return envelopes, buf[pos:], false
					}
					if isDigits(strings.TrimSpace(buf[pos:])) {
						return envelopes, "", false
					}
				} else if isDigits(strings.TrimSpace(buf[pos : pos+nl])) {
					pos += nl + 1
					continue
				}
			}
			if buf[pos] == '[' {
				j := pos + 1
				for j < n && (buf[j] == ' ' || buf[j] == '\t' || buf[j] == '\r' || buf[j] == '\n') {
					j++
				}
				if j >= n {
					if !flushTail {
						return envelopes, buf[pos:], false
					}
					envelopes = append(envelopes, buf[pos:])
					return envelopes, "", false
				}
				if buf[j] == '[' {
					inOuter = true
					pos = j
					continue
				}
				if buf[j] == ']' {
					pos = j + 1
					continue
				}
			}
			nl := strings.IndexByte(buf[pos:], '\n')
			if nl >= 0 {
				envelopes = append(envelopes, buf[pos:pos+nl])
				pos += nl + 1
				continue
			}
			if flushTail {
				envelopes = append(envelopes, buf[pos:])
				return envelopes, "", false
			}
			return envelopes, buf[pos:], false
		}

		for pos < n && (buf[pos] == ' ' || buf[pos] == '\t' || buf[pos] == '\r' || buf[pos] == '\n' || buf[pos] == ',') {
			pos++
		}
		if pos >= n {
			return envelopes, "", true
		}
		if buf[pos] == ']' {
			inOuter = false
			pos++
			continue
		}
		if buf[pos] == '[' {
			end := findBalancedJSONArrayEnd(buf, pos)
			if end > pos {
				envelopes = append(envelopes, "["+buf[pos:end]+"]")
				pos = end
				continue
			}
			if flushTail {
				envelopes = append(envelopes, "["+buf[pos:])
				return envelopes, "", false
			}
			return envelopes, buf[pos:], true
		}
		nl := strings.IndexByte(buf[pos:], '\n')
		if nl >= 0 {
			envelopes = append(envelopes, buf[pos:pos+nl])
			pos += nl + 1
			inOuter = false
			continue
		}
		if flushTail {
			envelopes = append(envelopes, buf[pos:])
			return envelopes, "", false
		}
		return envelopes, buf[pos:], true
	}
	return envelopes, "", inOuter
}

func findBalancedJSONArrayEnd(s string, start int) int {
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inStr {
			if esc {
				esc = false
			} else if ch == '\\' {
				esc = true
			} else if ch == '"' {
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

func (ac *AccountClient) pollAsyncVideoGenerationLocked(
	ctx context.Context,
	sc *transport.StealthClient,
	cookie, at, bl, fSid, lang, cid string,
) (*LineParseResult, error) {
	pollCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	payloadBytes, _ := json.Marshal([]any{cid, 10, nil, 1, []int{1}, []int{4}, nil, 1})
	freqBytes, _ := json.Marshal([][][]any{{{"hNvQHb", string(payloadBytes), nil, "generic"}}})
	shortID := strings.TrimPrefix(cid, "c_")

	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-pollCtx.Done():
			return nil, pollCtx.Err()
		case <-ticker.C:
			q := url.Values{}
			q.Set("rpcids", "hNvQHb")
			q.Set("source-path", "/app/"+shortID)
			if bl != "" {
				q.Set("bl", bl)
			}
			if fSid != "" {
				q.Set("f.sid", fSid)
			}
			q.Set("hl", lang)
			q.Set("_reqid", strconv.FormatInt(ac.nextReqID(), 10))
			q.Set("rt", "c")

			form := url.Values{}
			form.Set("at", at)
			form.Set("f.req", string(freqBytes))

			res, err := sc.Do(pollCtx, transport.RequestOptions{
				Method: "POST",
				URL:    Origin + "/_/BardChatUi/data/batchexecute?" + q.Encode(),
				Kind:   transport.ReqKindCORS,
				Headers: map[string]string{
					"content-type":  "application/x-www-form-urlencoded;charset=utf-8",
					"x-same-domain": "1",
					"cookie":        cookie,
				},
				Body:    []byte(form.Encode()),
				Timeout: 20 * time.Second,
			})
			if err != nil || res.Status != 200 {
				continue
			}
			ac.absorbAndPersistCookies(res.SetCookies)
			parsed, parseErr := ParseConversationPollResponse(res.Body)
			if parseErr != nil {
				return nil, parseErr
			}
			if parsed != nil && len(parsed.Frames) > 0 {
				f := parsed.Frames[0]
				if len(f.GeneratedVideos) > 0 || (f.Done && strings.TrimSpace(f.Text) != "" && !strings.Contains(strings.ToLower(f.Text), "generating your video")) {
					f.Done = true
					parsed.Frames[0] = f
					return parsed, nil
				}
			}
		}
	}
}

func (ac *AccountClient) deleteConversationAsync(cid string) {
	if cid == "" {
		return
	}
	// Wait 90s before deleting temporary video conversations so any lazy /v1/media/
	// proxy download from contribution.usercontent.google.com completes before
	// Google invalidates the conversation's media attachments.
	time.Sleep(90 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess, err := ac.acquireSession(ctx)
	if err != nil {
		return
	}
	ac.mu.RLock()
	at := ac.at
	bl := ac.bl
	fSid := ac.fSid
	cookie := ac.cookieHeader
	lang := ac.locale
	authUser := ac.authUser
	ac.mu.RUnlock()
	if lang == "" {
		lang = "en"
	}
	payloadBytes, _ := json.Marshal([]string{cid})
	freqBytes, _ := json.Marshal([][][]any{{{"GzXR5e", string(payloadBytes), nil, "generic"}}})
	q := url.Values{}
	q.Set("rpcids", "GzXR5e")
	sourcePath := "/app"
	if authUser > 0 {
		sourcePath = "/u/" + strconv.Itoa(authUser) + "/app"
	}
	q.Set("source-path", sourcePath)
	if authUser > 0 {
		q.Set("authuser", strconv.Itoa(authUser))
	}
	if bl != "" {
		q.Set("bl", bl)
	}
	if fSid != "" {
		q.Set("f.sid", fSid)
	}
	q.Set("hl", lang)
	q.Set("_reqid", strconv.FormatInt(ac.nextReqID(), 10))
	q.Set("rt", "c")
	form := url.Values{}
	form.Set("at", at)
	form.Set("f.req", string(freqBytes))
	_, _ = sess.Client.Do(ctx, transport.RequestOptions{
		Method: "POST",
		URL:    Origin + "/_/BardChatUi/data/batchexecute?" + q.Encode(),
		Kind:   transport.ReqKindCORS,
		Headers: map[string]string{
			"content-type":  "application/x-www-form-urlencoded;charset=utf-8",
			"x-same-domain": "1",
			"cookie":        cookie,
		},
		Body:    []byte(form.Encode()),
		Timeout: 15 * time.Second,
	})
}

// DownloadMedia downloads a generated image or video URL using the account's
// authenticated session cookies, falling back to fallbackURL if provided.
func (ac *AccountClient) DownloadMedia(ctx context.Context, rawURL, fallbackURL string) ([]byte, string, error) {
	sess, err := ac.acquireSession(ctx)
	if err != nil {
		return nil, "", err
	}
	ac.mu.RLock()
	cookie := ac.cookieHeader
	ac.mu.RUnlock()

	candidates := []string{rawURL}
	if fallbackURL != "" && fallbackURL != rawURL {
		candidates = append(candidates, fallbackURL)
	}
	var lastStatus int
	for _, startURL := range candidates {
		if startURL == "" {
			continue
		}
		curURL := startURL
		for hop := 0; hop < 10; hop++ {
			ac.mu.RLock()
			cookie = ac.cookieHeader
			ac.mu.RUnlock()

			res, doErr := sess.Client.Do(ctx, transport.RequestOptions{
				Method: "GET",
				URL:    curURL,
				Kind:   transport.ReqKindNavigate,
				Headers: map[string]string{
					"cookie":  cookie,
					"referer": Origin + "/",
				},
				Timeout: 60 * time.Second,
			})
			if doErr != nil {
				break
			}
			ac.absorbAndPersistCookies(res.SetCookies)
			lastStatus = res.Status
			if res.Status == 301 || res.Status == 302 || res.Status == 303 || res.Status == 307 || res.Status == 308 {
				loc := strings.TrimSpace(res.Headers["location"])
				if loc == "" {
					break
				}
				if strings.HasPrefix(loc, "/") {
					if parsedBase, err := url.Parse(curURL); err == nil {
						loc = parsedBase.Scheme + "://" + parsedBase.Host + loc
					}
				}
				curURL = loc
				continue
			}
			if res.Status == 200 && len(res.Body) > 0 {
				ct := strings.ToLower(strings.TrimSpace(res.Headers["content-type"]))
				if strings.HasPrefix(ct, "text/plain") {
					bodyStr := strings.TrimSpace(res.Body)
					if strings.HasPrefix(bodyStr, "https://") || strings.HasPrefix(bodyStr, "http://") {
						curURL = bodyStr
						continue
					}
				}
				origCT := res.Headers["content-type"]
				if origCT == "" {
					origCT = "application/octet-stream"
				}
				return []byte(res.Body), origCT, nil
			}
			break
		}
	}
	return nil, "", fmt.Errorf("media download failed (HTTP %d)", lastStatus)
}
