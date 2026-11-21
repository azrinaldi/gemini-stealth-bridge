package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/prompt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/toolbridge"
)

const (
	sessionCacheTTL        = 30 * time.Minute
	sessionCacheMaxEntries = 256
)

type persistedSessionEntry struct {
	Key  string    `json:"key"`
	Meta []any     `json:"meta"`
	At   time.Time `json:"at"`
}

func (s *Server) sessionCacheDiskPath() string {
	homeDir := s.cfgSnapshot().HomeDir
	if homeDir == "" {
		return ""
	}
	st, err := os.Stat(homeDir)
	if err != nil || !st.IsDir() {
		return ""
	}
	return filepath.Join(homeDir, "session-cache.json")
}

func (s *Server) loadSessionCacheFromDisk() {
	p := s.sessionCacheDiskPath()
	if p == "" {
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil || len(raw) == 0 {
		return
	}
	var entries []persistedSessionEntry
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		if e.Key == "" || len(e.Meta) == 0 || now.Sub(e.At) >= sessionCacheTTL {
			continue
		}
		if _, exists := s.sessionCache[e.Key]; !exists {
			if len(s.sessionOrder) >= sessionCacheMaxEntries {
				oldest := s.sessionOrder[0]
				s.sessionOrder = s.sessionOrder[1:]
				delete(s.sessionCache, oldest)
				delete(s.sessionCacheAt, oldest)
			}
			s.sessionOrder = append(s.sessionOrder, e.Key)
		}
		s.sessionCache[e.Key] = e.Meta
		s.sessionCacheAt[e.Key] = e.At
	}
}

// FlushSessionCacheNow writes the cache synchronously and waits for any
// asynchronous write already in flight. Callers that flush at shutdown, or a test
// flushing during cleanup, would otherwise return while a background writer is
// still creating its temp file in a directory that may already be removed.
func (s *Server) FlushSessionCacheNow() {
	s.cacheFlushWG.Wait()
	s.flushSessionCacheLocked()
}

// flushSessionCacheLocked performs the write without waiting on the wait group, so
// the background writer can call it without deadlocking on itself.
func (s *Server) flushSessionCacheLocked() {
	p := s.sessionCacheDiskPath()
	if p == "" {
		return
	}
	s.cacheFlushMu.Lock()
	defer s.cacheFlushMu.Unlock()
	s.cacheFlushPending.Store(false)

	now := time.Now()
	s.mu.RLock()
	entries := make([]persistedSessionEntry, 0, len(s.sessionOrder))
	for _, k := range s.sessionOrder {
		meta := s.sessionCache[k]
		at := s.sessionCacheAt[k]
		if len(meta) == 0 || (!at.IsZero() && now.Sub(at) >= sessionCacheTTL) {
			continue
		}
		cp := make([]any, len(meta))
		copy(cp, meta)
		entries = append(entries, persistedSessionEntry{Key: k, Meta: cp, At: at})
	}
	s.mu.RUnlock()

	b, err := json.Marshal(entries)
	if err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.tmp-%d", p, time.Now().UnixNano())
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		if err := os.Rename(tmp, p); err != nil {
			_ = os.Remove(tmp)
		}
	}
}

func (s *Server) flushSessionCacheToDisk() {
	if s.sessionCacheDiskPath() == "" {
		return
	}
	if !s.cacheFlushPending.CompareAndSwap(false, true) {
		return
	}
	s.cacheFlushMu.Lock()
	if !s.cacheFlushPending.Load() {
		s.cacheFlushMu.Unlock()
		return
	}
	s.cacheFlushWG.Add(1)
	s.cacheFlushMu.Unlock()
	go func() {
		defer s.cacheFlushWG.Done()
		s.flushSessionCacheLocked()
	}()
}

func (s *Server) sessionCacheLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessionCache)
}

// promoteSessionCacheLocked moves key to the newest position in sessionOrder.
// Callers must hold s.mu. A key missing from sessionOrder is appended so a live
// cache entry is never invisible to eviction.
func (s *Server) promoteSessionCacheLocked(key string) {
	for i, k := range s.sessionOrder {
		if k != key {
			continue
		}
		if i == len(s.sessionOrder)-1 {
			return
		}
		copy(s.sessionOrder[i:], s.sessionOrder[i+1:])
		s.sessionOrder[len(s.sessionOrder)-1] = key
		return
	}
	s.sessionOrder = append(s.sessionOrder, key)
}

// takeSessionCache consumes one continuation entry, promoting it only when a
// generation actually uses it. Hit/miss counters are updated once per consume
// attempt; affinity scans use peekSessionCache and never reach this method.
func (s *Server) takeSessionCache(key string) ([]any, bool) {
	s.mu.Lock()
	now := time.Now()
	src := s.sessionCache[key]
	at := s.sessionCacheAt[key]
	if len(src) == 0 || (!at.IsZero() && now.Sub(at) >= sessionCacheTTL) {
		s.mu.Unlock()
		s.cacheMisses.Add(1)
		return nil, false
	}
	cp := make([]any, len(src))
	copy(cp, src)
	s.promoteSessionCacheLocked(key)
	s.mu.Unlock()
	s.cacheHits.Add(1)
	return cp, true
}

// getSessionCache returns a copy of a live entry and records the lookup. It is
// retained for direct callers and tests; generation lookup uses a single
// selected key through takeSessionCache.
func (s *Server) getSessionCache(key string) []any {
	cp, _ := s.takeSessionCache(key)
	return cp
}

// peekSessionCache returns a copy of a live entry without recording hit/miss
// telemetry or changing LRU order. Account-affinity scans use this so routing
// probes do not distort /v1/sessions counters or eviction order.
func (s *Server) peekSessionCache(key string) []any {
	if key == "" {
		return nil
	}
	s.mu.RLock()
	now := time.Now()
	src := s.sessionCache[key]
	at := s.sessionCacheAt[key]
	if len(src) == 0 || (!at.IsZero() && now.Sub(at) >= sessionCacheTTL) {
		s.mu.RUnlock()
		return nil
	}
	cp := make([]any, len(src))
	copy(cp, src)
	s.mu.RUnlock()
	return cp
}

// deleteSessionCache removes a continuation entry and schedules a disk flush so
// the invalidation survives a restart. Concurrent callers and an empty key are
// safe; the flush is coalesced by the existing atomic writer.
func (s *Server) deleteSessionCache(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessionCache, key)
	delete(s.sessionCacheAt, key)
	if len(s.sessionOrder) > 0 {
		kept := s.sessionOrder[:0]
		for _, k := range s.sessionOrder {
			if k != key {
				kept = append(kept, k)
			}
		}
		s.sessionOrder = kept
	}
	s.mu.Unlock()
	s.flushSessionCacheToDisk()
}

// shouldEvictSessionCacheOnError reports whether a failed continuation attempt
// means the cached Gemini conversation should be discarded so later turns do
// not repeatedly hit a stalled or broken thread.
func shouldEvictSessionCacheOnError(errCode string, errStatus int) bool {
	switch errCode {
	case "empty_upstream_response",
		"incomplete_upstream_response",
		"upstream_error_reply",
		"upstream_error",
		"upstream_stalled",
		"upstream_timeout",
		"tool_call_error":
		return true
	case "upstream_http_error":
		return errStatus >= 400
	default:
		return false
	}
}

func (s *Server) putSessionCache(key string, meta []any) {
	if len(meta) == 0 {
		return
	}
	cp := make([]any, len(meta))
	copy(cp, meta)
	now := time.Now()
	s.mu.Lock()
	if _, exists := s.sessionCache[key]; !exists {
		if len(s.sessionOrder) >= sessionCacheMaxEntries {
			oldest := s.sessionOrder[0]
			s.sessionOrder = s.sessionOrder[1:]
			delete(s.sessionCache, oldest)
			delete(s.sessionCacheAt, oldest)
		}
	}
	s.sessionCache[key] = cp
	s.sessionCacheAt[key] = now
	s.promoteSessionCacheLocked(key)
	s.mu.Unlock()
	s.flushSessionCacheToDisk()
}

func (s *Server) saveSessionContinuation(
	accountID string,
	modelID string,
	messages []map[string]any,
	toolsRaw any,
	toolChoiceRaw any,
	content any,
	oaCalls []toolbridge.OpenAIToolCall,
	cid, rid, rcid, contextToken string,
) {
	if !s.cfgSnapshot().SessionCache || cid == "" || rid == "" || rcid == "" {
		return
	}
	var replyMsg map[string]any
	if len(oaCalls) > 0 {
		tcMaps := make([]any, 0, len(oaCalls))
		for _, tc := range oaCalls {
			tcMaps = append(tcMaps, map[string]any{
				"id":   tc.ID,
				"type": tc.Type,
				"function": map[string]any{
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				},
			})
		}
		replyMsg = map[string]any{
			"role":       "assistant",
			"content":    content,
			"tool_calls": tcMaps,
		}
	} else {
		replyMsg = map[string]any{
			"role":    "assistant",
			"content": content,
		}
	}
	nextHistory := make([]map[string]any, 0, len(messages)+1)
	nextHistory = append(nextHistory, messages...)
	nextHistory = append(nextHistory, replyMsg)
	nextKey := prompt.HashHistoryWithTools(accountID, modelID, nextHistory, toolsRaw, toolChoiceRaw)
	s.putSessionCache(nextKey, gemini.BuildContinuationMetadata(cid, rid, rcid, contextToken))
}

func (s *Server) getUploadCache(key string) (gemini.UploadedFile, bool) {
	if key == "" {
		return gemini.UploadedFile{}, false
	}
	now := time.Now()
	s.mu.Lock()
	cached, ok := s.uploadCache[key]
	if !ok || now.Sub(cached.At) >= 25*time.Minute {
		s.mu.Unlock()
		return gemini.UploadedFile{}, false
	}
	for i, k := range s.uploadOrder {
		if k != key {
			continue
		}
		if i != len(s.uploadOrder)-1 {
			copy(s.uploadOrder[i:], s.uploadOrder[i+1:])
			s.uploadOrder[len(s.uploadOrder)-1] = key
		}
		break
	}
	s.mu.Unlock()
	return cached.File, true
}

func (s *Server) uploadAttachments(
	ctx context.Context,
	acctID string,
	c UpstreamClient,
	parts []prompt.AttachmentPart,
) ([]gemini.UploadedFile, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	files := make([]gemini.UploadedFile, 0, len(parts))
	for _, att := range parts {
		uf, err := s.uploadCached(ctx, acctID, c, att)
		if err != nil {
			return nil, err
		}
		files = append(files, uf)
	}
	return files, nil
}

func (s *Server) uploadCached(ctx context.Context, acctID string, c UpstreamClient, att prompt.AttachmentPart) (gemini.UploadedFile, error) {
	if len(att.Data) == 0 && att.RemoteURL != "" {
		urlSum := sha256.Sum256([]byte(att.RemoteURL))
		urlKey := acctID + ":url:" + hex.EncodeToString(urlSum[:])
		if cachedURL, okURL := s.getUploadCache(urlKey); okURL {
			return cachedURL, nil
		}
		fetched, err := prompt.FetchRemoteImage(ctx, att.RemoteURL, 0)
		if err != nil {
			return gemini.UploadedFile{}, err
		}
		att = *fetched
		uf, err := s.uploadCachedBytes(ctx, acctID, c, att)
		if err != nil {
			return gemini.UploadedFile{}, err
		}
		s.putUploadCache(urlKey, uf)
		return uf, nil
	}
	return s.uploadCachedBytes(ctx, acctID, c, att)
}

func (s *Server) uploadCachedBytes(ctx context.Context, acctID string, c UpstreamClient, att prompt.AttachmentPart) (gemini.UploadedFile, error) {
	sum := sha256.Sum256(att.Data)
	cacheKey := acctID + ":" + hex.EncodeToString(sum[:])

	if cached, ok := s.getUploadCache(cacheKey); ok {
		return cached, nil
	}

	uf, err := c.UploadFile(ctx, att.Data, att.Filename, att.ContentType)
	if err != nil {
		return gemini.UploadedFile{}, err
	}

	s.putUploadCache(cacheKey, uf)
	return uf, nil
}

func (s *Server) putUploadCache(cacheKey string, uf gemini.UploadedFile) {
	s.mu.Lock()
	if _, exists := s.uploadCache[cacheKey]; !exists {
		if len(s.uploadOrder) >= 128 {
			oldest := s.uploadOrder[0]
			s.uploadOrder = s.uploadOrder[1:]
			delete(s.uploadCache, oldest)
		}
		s.uploadOrder = append(s.uploadOrder, cacheKey)
	}
	s.uploadCache[cacheKey] = cachedUpload{File: uf, At: time.Now()}
	s.mu.Unlock()
}

func (s *Server) evictUploadCacheForAccount(acctID string) {
	if acctID == "" {
		return
	}
	prefix := acctID + ":"
	s.mu.Lock()
	for k := range s.uploadCache {
		if strings.HasPrefix(k, prefix) {
			delete(s.uploadCache, k)
		}
	}
	if len(s.uploadOrder) > 0 {
		kept := s.uploadOrder[:0]
		for _, k := range s.uploadOrder {
			if !strings.HasPrefix(k, prefix) {
				kept = append(kept, k)
			}
		}
		s.uploadOrder = kept
	}
	s.mu.Unlock()
}
