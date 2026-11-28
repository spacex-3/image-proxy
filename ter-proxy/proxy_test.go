package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		assert.Equal(t, "/v1/images/generations", r.URL.Path)
		var body map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "gemini-3.1-flash-image", body["model"])
		assert.Equal(t, "2048x2048", body["size"])
		assert.Equal(t, "2k", body["quality"])
		assert.NotContains(t, body, "async")
		close(entered)
		<-release
		writeJSON(w, 200, map[string]any{"data": []any{map[string]string{"b64_json": testPNG}, map[string]string{"url": "https://unused.example/same-image"}}})
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
	assert.Contains(t, got.Outputs[0].URL, "ter-proxy/outputs/")
	assert.Equal(t, int32(1), calls.Load())
	_, err = os.Stat(filepath.Join(s.store.root, job.ID, "request.json"))
	assert.True(t, os.IsNotExist(err))
}

func TestReferenceUploadAndQuotaFailure(t *testing.T) {
	var calls atomic.Int32
	s, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "/v1/images/edits", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "4096x4096", body["size"])
		assert.Equal(t, "4k", body["quality"])
		assert.Equal(t, "b64_json", body["response_format"])
		images := body["images"].([]any)
		assert.Equal(t, "data:image/png;base64,"+testPNG, images[0].(map[string]any)["image_url"])
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
	for _, path := range []string{"/v1/images/generations", "/v1/images/edits/async", "/v1/images/tasks/ter_abcdefghijklmnopqrstuvwx"} {
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
	taskID, err := randomID("ter_")
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
	for _, c := range []struct{ size, quality, want, tier string }{{"1:1", "2k", "2048x2048", "2k"}, {"3:4", "4k", "3072x4096", "4k"}, {"1024x1024", "", "1024x1024", "1k"}, {"auto", "", "1024x1024", "1k"}, {"2K", "", "2048x2048", "2k"}} {
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
		writeJSON(w, 200, map[string]any{"data": map[string]string{"url": media.URL + "/image", "revised_prompt": "not an extra image"}})
	})
	// Test-only loopback image server; production uses the SSRF-safe loader.
	s.outputLoader = &imageLoader{client: media.Client(), maxBytes: 20 << 20}
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

// A reference-only proxy must never carry generated images or the upstream key.
func TestReferenceProxyIsIndependentOfGeneratedImageDownloads(t *testing.T) {
	image, err := base64.StdEncoding.DecodeString(testPNG)
	require.NoError(t, err)
	var proxyCalls, outputCalls atomic.Int32
	refProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		assert.Equal(t, "http://8.8.8.8/reference.png", r.URL.String())
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("x-goog-api-key"))
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(image)
	}))
	defer refProxy.Close()
	output := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outputCalls.Add(1)
		assert.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(image)
	}))
	defer output.Close()
	s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected generation") })
	proxyURL, err := url.Parse(refProxy.URL)
	require.NoError(t, err)
	c := s.cfg
	c.ReferenceProxyURL = proxyURL
	s = newProxyService(c, s.store, s.blobs, s.logger)
	// Override only the direct output dial target for a loopback fixture.
	transport := s.outputLoader.client.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(output.URL, "http://"))
	}
	got, mime, err := s.loader.Load(context.Background(), "http://8.8.8.8/reference.png")
	require.NoError(t, err)
	assert.Equal(t, image, got)
	assert.Equal(t, "image/png", mime)
	got, _, err = s.outputLoader.Load(context.Background(), "http://8.8.4.4/output.png")
	require.NoError(t, err)
	assert.Equal(t, image, got)
	assert.Equal(t, int32(1), proxyCalls.Load())
	assert.Equal(t, int32(1), outputCalls.Load())
	_, _, err = s.loader.Load(context.Background(), "http://127.0.0.1/private.png")
	assert.Error(t, err)
	assert.Equal(t, int32(1), proxyCalls.Load(), "private references must be rejected before the proxy")
}
