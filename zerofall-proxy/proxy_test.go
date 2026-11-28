package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a9e8AAAAASUVORK5CYII="

type memoryBlobs struct {
	mu   sync.Mutex
	data map[string][]byte
	mime map[string]string
}

func (b *memoryBlobs) Put(_ context.Context, k string, v []byte, m string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data[k] = append([]byte{}, v...)
	b.mime[k] = m
	return nil
}
func (b *memoryBlobs) Delete(_ context.Context, keys ...string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, k := range keys {
		delete(b.data, k)
	}
	return nil
}
func (b *memoryBlobs) PublicURL(k string) string { return "https://media.example/" + k }

func fixture(t *testing.T, handler http.HandlerFunc) (*proxyService, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	store, err := openTaskStore(t.TempDir())
	require.NoError(t, err)
	c := config{DataPath: store.root, UpstreamBaseURL: up.URL, UpstreamAPIKey: "upstream-secret-do-not-log", ProxyAPIKey: strings.Repeat("x", 40), MaxImageBytes: 20 << 20, RequestTimeout: time.Second, TaskTimeout: 10 * time.Second, TaskRetention: 24 * time.Hour, Workers: 1, QueueLimit: 2}
	s := newProxyService(c, store, &memoryBlobs{data: map[string][]byte{}, mime: map[string]string{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := httptest.NewServer(s.routes())
	t.Cleanup(server.Close)
	return s, server
}
func submitTest(t *testing.T, s *proxyService, server *httptest.Server, path string, body map[string]any) task {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	r, err := http.NewRequest("POST", server.URL+path, bytes.NewReader(data))
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer "+s.cfg.ProxyAPIKey)
	resp, err := server.Client().Do(r)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 202, resp.StatusCode, string(raw))
	var v map[string]any
	require.NoError(t, json.Unmarshal(raw, &v))
	got, ok := s.store.get(v["id"].(string))
	require.True(t, ok)
	return got
}

func TestAsyncSubmissionAndImageSettlement(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	s, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "/v1beta/models/gemini-3.1-flash-image-preview:generateContent", r.URL.Path)
		var body map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "upstream-secret-do-not-log", r.Header.Get("x-goog-api-key"))
		assert.Equal(t, "url", r.Header.Get("X-Image-Response"))
		assert.Equal(t, "Mozilla/5.0", r.Header.Get("User-Agent"))
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Equal(t, map[string]any{"imageSize": "2K", "aspectRatio": "1:1"}, body["generationConfig"].(map[string]any)["imageConfig"])
		assert.NotContains(t, body, "async")
		close(entered)
		<-release
		writeJSON(w, 200, geminiResponse(map[string]any{"inlineData": map[string]any{"data": testPNG, "mimeType": "image/png"}}))
	})
	job := submitTest(t, s, server, "/v1/images/generations", map[string]any{"model": "gemini-3.1-flash-image-preview", "prompt": "red circle", "size": "1:1", "quality": "2k", "async": true})
	require.Equal(t, "queued", job.Status)
	require.Zero(t, calls.Load())
	done := make(chan struct{})
	go func() { s.process(context.Background(), job); close(done) }()
	<-entered
	r, err := http.NewRequest("GET", server.URL+"/v1/images/generations/"+job.ID, nil)
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer "+s.cfg.ProxyAPIKey)
	resp, err := server.Client().Do(r)
	require.NoError(t, err)
	var view map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&view))
	resp.Body.Close()
	assert.Equal(t, "in_progress", view["status"])
	close(release)
	<-done
	got, ok := s.store.get(job.ID)
	require.True(t, ok)
	require.Equal(t, "completed", got.Status)
	require.Len(t, got.Outputs, 1)
	assert.Equal(t, "image/png", got.Outputs[0].MimeType)
	assert.Contains(t, got.Outputs[0].URL, "zerofall-proxy/outputs/")
	assert.Equal(t, int32(1), calls.Load())
	_, err = os.Stat(filepath.Join(s.store.root, job.ID, "request.json"))
	assert.True(t, os.IsNotExist(err))
}

func TestReferenceUploadAndQuotaFailure(t *testing.T) {
	var calls atomic.Int32
	s, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "/v1beta/models/gemini-3-pro-image-preview:generateContent", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, map[string]any{"imageSize": "4K", "aspectRatio": "1:1"}, body["generationConfig"].(map[string]any)["imageConfig"])
		contents := body["contents"].([]any)
		parts := contents[0].(map[string]any)["parts"].([]any)
		assert.Equal(t, map[string]any{"mimeType": "image/png", "data": testPNG}, parts[1].(map[string]any)["inlineData"])
		writeJSON(w, 402, map[string]any{"error": map[string]any{"code": "insufficient_quota", "message": "Insufficient balance upstream-secret-do-not-log https://private.example/token"}})
	})
	job := submitTest(t, s, server, "/v1/images/edits/async", map[string]any{"model": "gemini-3.0-pro-image", "prompt": "blue circle", "size": "1:1", "quality": "4k", "images": []any{map[string]string{"url": "data:image/png;base64," + testPNG}}})
	s.process(context.Background(), job)
	got, _ := s.store.get(job.ID)
	require.Equal(t, "failed", got.Status)
	require.NotNil(t, got.Error)
	assert.Equal(t, "insufficient_quota", got.Error.Code)
	assert.Equal(t, 402, got.Error.HTTPStatus)
	assert.NotContains(t, got.Error.Message, "upstream-secret")
	assert.NotContains(t, got.Error.Message, "private.example")
	assert.Equal(t, int32(1), calls.Load())
}

func TestAuthenticationValidationAndQueueLimits(t *testing.T) {
	s, server := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected upstream request") })
	for _, path := range []string{"/v1/images/generations", "/v1/images/edits/async", "/v1/images/tasks/zero_abcdefghijklmnopqrstuvwx"} {
		method := "POST"
		if strings.Contains(path, "tasks") {
			method = "GET"
		}
		for _, key := range []string{"", "Bearer incorrect", "Basic " + s.cfg.ProxyAPIKey} {
			r, err := http.NewRequest(method, server.URL+path, strings.NewReader("{}"))
			require.NoError(t, err)
			r.Header.Set("Authorization", key)
			resp, err := server.Client().Do(r)
			require.NoError(t, err)
			assert.Equal(t, 401, resp.StatusCode)
			resp.Body.Close()
		}
	}
	for _, body := range []string{`{"model":"gemini-3.1-flash-image","prompt":"x","n":-1}`, `{"model":"gemini-3.1-flash-image","prompt":"x","n":2}`, `{"model":"other","prompt":"x"}`, `{"model":"gemini-3.1-flash-image","prompt":"x","quality":"high"}`, `{"model":"gemini-3.1-flash-image","prompt":"x","size":"99:1"}`} {
		r, err := http.NewRequest("POST", server.URL+"/v1/images/generations", strings.NewReader(body))
		require.NoError(t, err)
		r.Header.Set("Authorization", "Bearer "+s.cfg.ProxyAPIKey)
		resp, err := server.Client().Do(r)
		require.NoError(t, err)
		assert.Equal(t, 400, resp.StatusCode)
		resp.Body.Close()
	}
	for range 2 {
		submitTest(t, s, server, "/v1/images/generations", map[string]any{"model": "gemini-3.1-flash-image", "prompt": "x"})
	}
	_, err := normalizeRequest(map[string]any{"model": "gemini-3.1-flash-image", "prompt": "x"}, "edits")
	assert.Error(t, err)
	taskID, err := randomID("zero_")
	require.NoError(t, err)
	assert.ErrorIs(t, s.store.create(task{ID: taskID}, imageRequest{}, 2), errQueueFull)
}

func TestRestartNeverReplaysStartedGeneration(t *testing.T) {
	s, server := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected upstream replay") })
	queued := submitTest(t, s, server, "/v1/images/generations", map[string]any{"model": "gemini-3.1-flash-image", "prompt": "x"})
	started := submitTest(t, s, server, "/v1/images/generations", map[string]any{"model": "gemini-3.1-flash-image", "prompt": "x"})
	started.Status = "in_progress"
	require.NoError(t, s.stage(&started, "upstream_request"))
	reopened, err := openTaskStore(s.store.root)
	require.NoError(t, err)
	a, _ := reopened.get(queued.ID)
	b, _ := reopened.get(started.ID)
	assert.Equal(t, "queued", a.Status)
	assert.Equal(t, "failed", b.Status)
	assert.Equal(t, "upstream_interrupted", b.Error.Code)
}

func TestSizeAndSSRF(t *testing.T) {
	for _, c := range []struct{ size, quality, want, tier string }{{"1:1", "2k", "2048x2048", "2k"}, {"3:4", "4k", "3072x4096", "4k"}, {"1024x1024", "", "1024x1024", "1k"}, {"1280x1280", "", "2048x2048", "2k"}, {"auto", "", "1024x1024", "1k"}, {"2K", "", "2048x2048", "2k"}} {
		size, q, err := normalizeSize(c.size, c.quality)
		require.NoError(t, err)
		assert.Equal(t, c.want, size)
		assert.Equal(t, c.tier, q)
	}
	loader := newImageLoader(20<<20, time.Second, nil)
	for _, u := range []string{"http://127.0.0.1/", "http://169.254.169.254/", "http://[::1]/", "https://user:pass@example.com/image", "file:///etc/passwd"} {
		_, _, err := loader.Load(context.Background(), u)
		assert.Error(t, err, u)
	}
}

func TestURLResultAndUpstreamRedirectBoundary(t *testing.T) {
	image, err := base64.StdEncoding.DecodeString(testPNG)
	require.NoError(t, err)
	var downloaded atomic.Int32
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloaded.Add(1)
		assert.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(image)
	}))
	defer media.Close()
	s, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, geminiResponse(map[string]any{"fileData": map[string]any{"fileUri": media.URL + "/image", "mimeType": "image/png"}}))
	})
	// Test-only loopback image server; production uses the SSRF-safe loader.
	s.loader = &imageLoader{client: media.Client(), maxBytes: 20 << 20}
	job := submitTest(t, s, server, "/v1/images/generations", map[string]any{"model": "gemini-3.1-flash-image", "prompt": "circle"})
	s.process(context.Background(), job)
	got, _ := s.store.get(job.ID)
	require.Equal(t, "completed", got.Status)
	assert.Equal(t, int32(1), downloaded.Load())
	require.Len(t, got.Outputs, 1)
	assert.Equal(t, "image/png", got.Outputs[0].MimeType)
	redirect, redirectServer := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", media.URL+"/must-not-receive-credentials")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	job = submitTest(t, redirect, redirectServer, "/v1/images/generations", map[string]any{"model": "gemini-3.1-flash-image", "prompt": "circle"})
	redirect.process(context.Background(), job)
	got, _ = redirect.store.get(job.ID)
	assert.Equal(t, "failed", got.Status)
	assert.Equal(t, int32(1), downloaded.Load(), "the authenticated POST must not follow redirects")
}

func geminiResponse(parts ...any) map[string]any {
	return map[string]any{"candidates": []any{map[string]any{"finishReason": "STOP", "content": map[string]any{"parts": parts}}}}
}

func TestGeminiBlockedAndThinkingImagesNeverSettle(t *testing.T) {
	for _, body := range []map[string]any{
		{"promptFeedback": map[string]any{"blockReason": "SAFETY"}},
		geminiResponse(map[string]any{"text": "I cannot generate that image"}),
		geminiResponse(map[string]any{"thought": true, "inlineData": map[string]any{"data": testPNG}}),
		geminiResponse(map[string]any{"inlineData": map[string]any{"data": testPNG}}, map[string]any{"fileData": map[string]any{"fileUri": "https://example.com/extra"}}),
		{"candidates": []any{map[string]any{"finishReason": "SAFETY", "content": map[string]any{"parts": []any{map[string]any{"inlineData": map[string]any{"data": testPNG}}}}}}},
	} {
		_, _, err := geminiImageResult(body)
		assert.Error(t, err)
	}
	encoded, output, err := geminiImageResult(geminiResponse(map[string]any{"thought": true, "inlineData": map[string]any{"data": "skip"}}, map[string]any{"inlineData": map[string]any{"data": testPNG}}))
	require.NoError(t, err)
	assert.Equal(t, testPNG, encoded)
	assert.Empty(t, output)
}

func TestAuthenticatedCacheAndCredentiallessRedirects(t *testing.T) {
	image, err := base64.StdEncoding.DecodeString(testPNG)
	require.NoError(t, err)
	var publicCalls atomic.Int32
	media := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicCalls.Add(1)
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("x-goog-api-key"))
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(image)
	}))
	defer media.Close()
	s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/images/direct/content" || r.URL.Path == "/v1/images/redirect/content" {
			assert.Equal(t, "Bearer upstream-secret-do-not-log", r.Header.Get("Authorization"))
			assert.Empty(t, r.Header.Get("x-goog-api-key"))
			if strings.Contains(r.URL.Path, "redirect") {
				http.Redirect(w, r, media.URL+"/result", 302)
				return
			}
		} else {
			assert.Empty(t, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(image)
	})
	s.loader = &imageLoader{client: media.Client(), maxBytes: 20 << 20}
	for _, path := range []string{"/v1/images/direct/content?index=0", "/v1/images/redirect/content?index=0", "/p/images/public", "/v1/images/direct/content/other"} {
		data, mime, err := s.downloadGeneratedImage(context.Background(), s.cfg.UpstreamBaseURL+path)
		require.NoError(t, err, path)
		assert.Equal(t, image, data)
		assert.Equal(t, "image/png", mime)
	}
	assert.Equal(t, int32(1), publicCalls.Load())
	for _, suffix := range []string{"?index=1", "?index=0&token=unexpected", "?index=0&index=0"} {
		_, _, err := s.downloadGeneratedImage(context.Background(), s.cfg.UpstreamBaseURL+"/v1/images/direct/content"+suffix)
		assert.Error(t, err)
	}
	// The real loader must still block private redirect targets.
	s.loader = newImageLoader(20<<20, time.Second, nil)
	_, _, err = s.downloadGeneratedImage(context.Background(), s.cfg.UpstreamBaseURL+"/v1/images/redirect/content?index=0")
	assert.Error(t, err)
	assert.Equal(t, int32(1), publicCalls.Load())
}

func TestCachedImageRetryDoesNotRegenerate(t *testing.T) {
	var posts, gets atomic.Int32
	var upstreamURL string
	s, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			w.Header().Set("X-Oneapi-Request-Id", "request_123")
			writeJSON(w, 200, geminiResponse(map[string]any{"fileData": map[string]any{"fileUri": upstreamURL + "/v1/images/request_123/content?index=0"}}))
			return
		}
		assert.Equal(t, "Bearer upstream-secret-do-not-log", r.Header.Get("Authorization"))
		if gets.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		data, err := base64.StdEncoding.DecodeString(testPNG)
		assert.NoError(t, err)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)
	})
	upstreamURL = s.cfg.UpstreamBaseURL
	job := submitTest(t, s, server, "/v1/images/generations", map[string]any{"model": "gemini-3.1-flash-image", "prompt": "cat"})
	s.process(context.Background(), job)
	got, _ := s.store.get(job.ID)
	require.Equal(t, "completed", got.Status)
	assert.Equal(t, "request_123", got.UpstreamRequestID)
	assert.Equal(t, int32(1), posts.Load())
	assert.Equal(t, int32(2), gets.Load())
}
