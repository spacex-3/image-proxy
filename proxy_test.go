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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

type memoryBlobStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
}

func newMemoryBlobStore() *memoryBlobStore {
	return &memoryBlobStore{objects: make(map[string][]byte), types: make(map[string]string)}
}

func (s *memoryBlobStore) Put(_ context.Context, key string, data []byte, contentType string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = append([]byte(nil), data...)
	s.types[key] = contentType
	return nil
}

func (s *memoryBlobStore) Delete(_ context.Context, keys ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range keys {
		delete(s.objects, key)
		delete(s.types, key)
	}
	return nil
}

func (s *memoryBlobStore) PublicURL(key string) string {
	return "https://media.example/" + key
}

func testService(t *testing.T, upstream http.Handler) (*proxyService, *memoryBlobStore, *httptest.Server) {
	t.Helper()
	upstreamServer := httptest.NewServer(upstream)
	t.Cleanup(upstreamServer.Close)
	store, err := openTaskStore(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	blobs := newMemoryBlobStore()
	cfg := config{
		ProxyAPIKey: "proxy-secret", UpstreamAPIKey: "upstream-secret", UpstreamBaseURL: upstreamServer.URL,
		MaxImageBytes: 1 << 20, PollInterval: time.Second, RequestTimeout: 5 * time.Second,
		TaskTimeout: time.Hour, TaskRetention: 2 * time.Hour, UpstreamWorkers: 2,
	}
	service := newProxyService(cfg, store, blobs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := httptest.NewServer(service.routes())
	t.Cleanup(server.Close)
	return service, blobs, server
}

func TestEditConvertsBase64ReferenceAndResultToPublicURLs(t *testing.T) {
	var submitted map[string]any
	service, blobs, server := testService(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Fatalf("unexpected upstream auth")
		}
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/images/edits/async":
			if err := json.NewDecoder(request.Body).Decode(&submitted); err != nil {
				t.Fatal(err)
			}
			writeJSON(writer, http.StatusAccepted, map[string]any{"task_id": "azimg_upstream"})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/images/tasks/azimg_upstream":
			writeJSON(writer, http.StatusOK, map[string]any{
				"status": "completed", "result": map[string]any{"data": []any{map[string]any{"b64_json": onePixelPNG}}},
			})
		default:
			http.NotFound(writer, request)
		}
	}))

	payload := map[string]any{
		"model": "gemini-3.1-flash-image-preview", "prompt": "edit", "size": "1:1", "quality": "2k", "n": 1,
		"images": []any{map[string]any{"image_url": "data:image/png;base64," + onePixelPNG}},
	}
	response := authenticatedJSON(t, http.MethodPost, server.URL+"/v1/images/edits/async", payload)
	if response.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d body=%s", response.Code, response.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	taskID := created["task_id"].(string)
	if submitted["size"] != "2048x2048" || submitted["response_format"] != "b64_json" {
		t.Fatalf("unexpected submitted fields: %#v", submitted)
	}
	images := submitted["images"].([]any)
	imageURL := images[0].(map[string]any)["image_url"].(string)
	if !strings.HasPrefix(imageURL, "https://media.example/az-proxy/inputs/") || strings.Contains(imageURL, "?") {
		t.Fatalf("upstream image URL = %q", imageURL)
	}

	task, ok := service.store.get(taskID)
	if !ok {
		t.Fatal("task not stored")
	}
	service.pollTask(context.Background(), task)
	query := authenticatedJSON(t, http.MethodGet, server.URL+"/v1/images/tasks/"+taskID, nil)
	if query.Code != http.StatusOK {
		t.Fatalf("query status = %d", query.Code)
	}
	var result map[string]any
	if err := json.Unmarshal(query.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["status"] != "completed" {
		t.Fatalf("result = %#v", result)
	}
	output := result["data"].([]any)[0].(map[string]any)["url"].(string)
	if !strings.HasPrefix(output, "https://media.example/az-proxy/outputs/") || strings.Contains(output, "?") {
		t.Fatalf("output URL = %q", output)
	}
	blobs.mu.Lock()
	defer blobs.mu.Unlock()
	if len(blobs.objects) != 1 {
		t.Fatalf("stored objects = %d, want only terminal output", len(blobs.objects))
	}
}

func TestAuthenticationAndReferenceValidation(t *testing.T) {
	_, _, server := testService(t, http.NotFoundHandler())
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/images/edits", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.StatusCode)
	}

	invalid := authenticatedJSON(t, http.MethodPost, server.URL+"/v1/images/edits", map[string]any{
		"model": "gemini-3.1-flash-image-preview", "prompt": "edit", "images": []any{map[string]any{"image_url": "file:///etc/passwd"}},
	})
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), "HTTP(S)") {
		t.Fatalf("invalid response = %d %s", invalid.Code, invalid.Body.String())
	}
}

func TestFailedTaskPreservesPublicErrorAndDeletesInput(t *testing.T) {
	service, blobs, server := testService(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			writeJSON(writer, http.StatusAccepted, map[string]any{"task_id": "azimg_failed"})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"status": "failed", "error": map[string]any{"code": "insufficient_quota", "type": "billing_error", "message": "Request failed"},
		})
	}))
	payload := map[string]any{
		"model": "gemini-3.1-flash-image-preview", "prompt": "edit",
		"images": []any{"data:image/png;base64," + onePixelPNG},
	}
	created := authenticatedJSON(t, http.MethodPost, server.URL+"/v1/images/edits", payload)
	var body map[string]any
	_ = json.Unmarshal(created.Body.Bytes(), &body)
	taskID := body["task_id"].(string)
	task, _ := service.store.get(taskID)
	service.pollTask(context.Background(), task)
	query := authenticatedJSON(t, http.MethodGet, server.URL+"/v1/images/edits/"+taskID, nil)
	if !strings.Contains(query.Body.String(), "insufficient_quota") || !strings.Contains(query.Body.String(), "billing_error") {
		t.Fatalf("query body = %s", query.Body.String())
	}
	blobs.mu.Lock()
	defer blobs.mu.Unlock()
	if len(blobs.objects) != 0 {
		t.Fatalf("input object was not deleted")
	}
}

func TestSubmissionPreservesUpstreamQuotaCode(t *testing.T) {
	_, _, server := testService(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusPaymentRequired, map[string]any{
			"error": map[string]any{"code": "insufficient_quota", "type": "billing_error", "message": "No upstream balance"},
		})
	}))
	response := authenticatedJSON(t, http.MethodPost, server.URL+"/v1/images/generations/async", map[string]any{
		"model": "gemini-3.1-flash-image-preview", "prompt": "a cat",
	})
	if response.Code != http.StatusPaymentRequired || !strings.Contains(response.Body.String(), `"code":"insufficient_quota"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func authenticatedJSON(t *testing.T, method, target string, value any) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, body)
	request.Header.Set("Authorization", "Bearer proxy-secret")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler := http.DefaultServeMux
	if parsed := request.URL; parsed.IsAbs() {
		clientRequest, _ := http.NewRequest(method, target, body)
		clientRequest.Header = request.Header.Clone()
		response, err := http.DefaultClient.Do(clientRequest)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		recorder.Code = response.StatusCode
		_, _ = io.Copy(recorder.Body, response.Body)
		return recorder
	}
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestDataURLFixtureIsValid(t *testing.T) {
	data, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = validateImage(data, "image/png", 1<<20); err != nil {
		t.Fatal(err)
	}
}
