package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const maxRequestBytes = 32 << 20

var taskIDPattern = regexp.MustCompile(`^zero_[A-Za-z0-9_-]{24}$`)
var sensitiveErrorPattern = regexp.MustCompile(`(?i)https?://[^\s<>]+|Bearer\s+\S+|sk-[A-Za-z0-9_-]{8,}`)

type proxyService struct {
	cfg     config
	store   *taskStore
	blobs   blobStore
	loader  *imageLoader
	client  *http.Client
	logger  *slog.Logger
	wake    chan struct{}
	active  sync.Map
	workers chan struct{}
}

func newProxyService(c config, store *taskStore, blobs blobStore, logger *slog.Logger) *proxyService {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	if c.UpstreamProxyURL != nil {
		t.Proxy = http.ProxyURL(c.UpstreamProxyURL)
	}
	return &proxyService{cfg: c, store: store, blobs: blobs, loader: newImageLoader(c.MaxImageBytes, c.RequestTimeout, c.ReferenceProxyURL), client: &http.Client{Transport: t, Timeout: c.TaskTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, logger: logger, wake: make(chan struct{}, 1), workers: make(chan struct{}, c.Workers)}
}

func (s *proxyService) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	for _, action := range []string{"generations", "edits"} {
		m.HandleFunc("POST /v1/images/"+action, s.auth(s.submit(action)))
		m.HandleFunc("POST /v1/images/"+action+"/async", s.auth(s.submit(action)))
		m.HandleFunc("GET /v1/images/"+action+"/{task_id}", s.auth(s.query(action)))
	}
	m.HandleFunc("GET /v1/images/tasks/{task_id}", s.auth(s.query("")))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		m.ServeHTTP(w, r)
	})
}

func (s *proxyService) auth(next http.HandlerFunc) http.HandlerFunc {
	expected := sha256.Sum256([]byte(s.cfg.ProxyAPIKey))
	return func(w http.ResponseWriter, r *http.Request) {
		scheme, key, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		provided := sha256.Sum256([]byte(key))
		if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(expected[:], provided[:]) != 1 {
			writeError(w, 401, "authentication_error", "Invalid proxy API key")
			return
		}
		next(w, r)
	}
}

func (s *proxyService) submit(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		d := json.NewDecoder(r.Body)
		d.UseNumber()
		var body map[string]any
		var extra any
		if err := d.Decode(&body); err != nil || body == nil {
			writeError(w, 400, "invalid_request_error", "Request must be a JSON object within 32 MiB")
			return
		}
		if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
			writeError(w, 400, "invalid_request_error", "Request must contain one JSON object")
			return
		}
		req, err := normalizeRequest(body, action)
		if err != nil {
			writeError(w, 400, "invalid_request_error", err.Error())
			return
		}
		id, err := randomID("zero_")
		if err != nil {
			writeError(w, 500, "internal_error", "Cannot allocate task")
			return
		}
		now := time.Now().Unix()
		t := task{ID: id, Model: req.Model, Action: action, Status: "queued", Stage: "queued", CreatedAt: now, UpdatedAt: now, Size: req.Size, Quality: req.Quality, ReferenceCount: len(req.References), Timings: map[string]int64{}}
		if err = s.store.create(t, req, s.cfg.QueueLimit); err != nil {
			if errors.Is(err, errQueueFull) {
				w.Header().Set("Retry-After", "10")
				writeError(w, 429, "queue_full", "ZeroFall relay queue is full; retry later")
			} else {
				writeError(w, 503, "proxy_storage_error", "Cannot persist task")
			}
			return
		}
		select {
		case s.wake <- struct{}{}:
		default:
		}
		w.Header().Set("Location", "/v1/images/"+action+"/"+id)
		w.Header().Set("Retry-After", "3")
		writeJSON(w, 202, taskView(t))
		s.logger.Info("task queued", "task_id", id, "model", req.Model, "size", req.Size, "quality", req.Quality, "references", len(req.References))
	}
}

func (s *proxyService) query(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("task_id")
		if !taskIDPattern.MatchString(id) {
			writeError(w, 404, "not_found", "Task not found")
			return
		}
		t, ok := s.store.get(id)
		if !ok || (action != "" && action != t.Action) {
			writeError(w, 404, "not_found", "Task not found")
			return
		}
		writeJSON(w, 200, taskView(t))
	}
}

func (s *proxyService) run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	cleanup := time.NewTicker(time.Hour)
	defer cleanup.Stop()
	for {
		for _, t := range s.store.list() {
			if t.Status == "in_progress" && time.Since(time.Unix(t.UpdatedAt, 0)) > s.cfg.TaskTimeout {
				if _, running := s.active.Load(t.ID); !running {
					s.fail(t, "proxy_persistence_error", "Task processing stopped before its final state could be persisted", 0)
				}
			}
			if t.Status != "queued" {
				continue
			}
			if _, loaded := s.active.LoadOrStore(t.ID, true); loaded {
				continue
			}
			select {
			case s.workers <- struct{}{}:
				go func() { defer func() { <-s.workers; s.active.Delete(t.ID) }(); s.process(ctx, t) }()
			default:
				s.active.Delete(t.ID)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-ticker.C:
		case <-cleanup.C:
			s.cleanup(ctx)
		}
	}
}

func (s *proxyService) stage(t *task, stage string) error {
	t.Stage = stage
	t.UpdatedAt = time.Now().Unix()
	snapshot := *t
	snapshot.Timings = cloneTimings(t.Timings)
	return s.store.save(snapshot)
}

func (s *proxyService) process(parent context.Context, t task) {
	ctx, cancel := context.WithTimeout(parent, s.cfg.TaskTimeout)
	defer cancel()
	t.Status = "in_progress"
	t.Timings["queue"] = time.Since(time.Unix(t.CreatedAt, 0)).Milliseconds()
	if err := s.stage(&t, "reference_download"); err != nil {
		s.logger.Error("cannot start persisted task", "task_id", t.ID)
		return
	}
	data, err := os.ReadFile(filepath.Join(s.store.root, t.ID, "request.json"))
	var input imageRequest
	if err != nil || json.Unmarshal(data, &input) != nil {
		s.fail(t, "proxy_storage_error", "Cannot read persisted request", 0)
		return
	}
	start := time.Now()
	var payload bytes.Buffer
	parts := []map[string]any{{"text": input.Prompt}}
	for _, ref := range input.References {
		image, mime, loadErr := s.loader.Load(ctx, ref)
		if loadErr != nil {
			s.fail(t, "reference_download_failed", s.publicError(loadErr.Error()), 0)
			return
		}
		t.ReferenceBytes += int64(len(image))
		if t.ReferenceBytes > s.cfg.MaxImageBytes {
			s.fail(t, "reference_too_large", "Combined reference images exceed 20 MiB", 0)
			return
		}
		parts = append(parts, map[string]any{"inlineData": map[string]string{"mimeType": mime, "data": base64.StdEncoding.EncodeToString(image)}})
	}
	var w, h int
	_, _ = fmt.Sscanf(input.Size, "%dx%d", &w, &h)
	g, d := w, h
	for d != 0 {
		g, d = d, g%d
	}
	if g < 1 {
		s.fail(t, "proxy_storage_error", "Invalid persisted image size", 0)
		return
	}
	ratio := fmt.Sprintf("%d:%d", w/g, h/g)
	if ratio == "7:3" {
		ratio = "21:9"
	} // Gemini names its ultrawide preset 21:9.
	upstreamBody := map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": parts}},
		"generationConfig": map[string]any{"imageConfig": map[string]string{"imageSize": strings.ToUpper(input.Quality), "aspectRatio": ratio}},
	}
	if err = json.NewEncoder(&payload).Encode(upstreamBody); err != nil {
		s.fail(t, "proxy_internal_error", "Cannot encode upstream request", 0)
		return
	}
	t.Timings["reference_download"] = time.Since(start).Milliseconds()
	if err = s.stage(&t, "upstream_request"); err != nil {
		s.logger.Error("cannot persist pre-submit stage", "task_id", t.ID)
		return
	}
	// Deliberately one POST, with no Idempotency-Key or automatic retry. A
	// timeout cannot tell us whether the upstream generated and charged.
	request, err := http.NewRequestWithContext(ctx, "POST", s.cfg.UpstreamBaseURL+"/v1beta/models/"+url.PathEscape(input.Model)+":generateContent", &payload)
	if err != nil {
		s.fail(t, "proxy_configuration_error", "Invalid upstream configuration", 0)
		return
	}
	request.Header.Set("x-goog-api-key", s.cfg.UpstreamAPIKey)
	request.Header.Set("X-Image-Response", "url")
	// Match the user agent used by successful native Gemini probes.
	request.Header.Set("User-Agent", "Mozilla/5.0")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	start = time.Now()
	resp, err := s.client.Do(request)
	if err != nil {
		t.Timings["upstream_request"] = time.Since(start).Milliseconds()
		s.fail(t, "upstream_transport_error", "Upstream request failed or timed out; completion is unknown", 0)
		return
	}
	defer resp.Body.Close()
	t.Timings["upstream_headers"] = time.Since(start).Milliseconds()
	t.UpstreamRequestID = s.publicError(resp.Header.Get("X-Oneapi-Request-Id"))
	if len(t.UpstreamRequestID) > 128 {
		t.UpstreamRequestID = ""
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20+1))
	t.Timings["upstream_request"] = time.Since(start).Milliseconds()
	if err != nil || len(data) > 64<<20 {
		s.fail(t, "upstream_read_error", "Failed to read complete upstream response; completion is unknown", resp.StatusCode)
		return
	}
	var body map[string]any
	parseErr := json.Unmarshal(data, &body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || body["error"] != nil {
		code, message := "upstream_error", fmt.Sprintf("Upstream returned HTTP %d", resp.StatusCode)
		if e, ok := body["error"].(map[string]any); ok {
			if v := e["code"]; v != nil && fmt.Sprint(v) != "" {
				code = fmt.Sprint(v)
			} else if v := stringValue(e["type"]); v != "" {
				code = v
			}
			if v := stringValue(e["message"]); v != "" {
				message = v
			}
		}
		if resp.StatusCode == 402 || isQuotaError(code, message) {
			code = "insufficient_quota"
		}
		s.fail(t, code, message, resp.StatusCode)
		return
	}
	if parseErr != nil {
		s.fail(t, "upstream_invalid_response", "Upstream returned invalid JSON", resp.StatusCode)
		return
	}
	encoded, outputURL, resultErr := geminiImageResult(body)
	if resultErr != nil {
		s.fail(t, "upstream_invalid_response", resultErr.Error(), resp.StatusCode)
		return
	}
	if err = s.stage(&t, "output_download"); err != nil {
		s.fail(t, "proxy_storage_error", "Cannot persist output stage", 0)
		return
	}
	start = time.Now()
	var image []byte
	var mime string
	if encoded != "" {
		if strings.HasPrefix(encoded, "data:") {
			image, mime, err = decodeDataURL(encoded, s.cfg.MaxImageBytes)
		} else {
			image, err = base64.StdEncoding.DecodeString(encoded)
			if err == nil {
				image, mime, err = validateImage(image, "", s.cfg.MaxImageBytes)
			}
		}
	} else {
		image, mime, err = s.downloadGeneratedImage(ctx, outputURL)
	}
	t.Timings["output_download"] = time.Since(start).Milliseconds()
	if err != nil {
		s.fail(t, "output_download_failed", "Cannot read the generated image", 0)
		return
	}
	key := objectKey("outputs", t.ID, 0, mime)
	t.OutputKeys = []string{key}
	if err = s.stage(&t, "r2_upload"); err != nil {
		s.fail(t, "proxy_storage_error", "Cannot persist storage stage", 0)
		return
	}
	start = time.Now()
	if err = s.blobs.Put(ctx, key, image, mime); err != nil {
		s.fail(t, "proxy_storage_error", "Cannot store generated image in R2", 0)
		return
	}
	t.Timings["r2_upload"] = time.Since(start).Milliseconds()
	t.Outputs = []imageOutput{{URL: s.blobs.PublicURL(key), MimeType: mime}}
	t.Status = "completed"
	if err = s.stage(&t, "completed"); err != nil {
		s.logger.Error("cannot persist completed task", "task_id", t.ID)
		return
	}
	s.logger.Info("task completed", "task_id", t.ID, "timings_ms", t.Timings, "reference_bytes", t.ReferenceBytes, "output_bytes", len(image), "mime_type", mime)
}

func (s *proxyService) publicError(message string) string {
	for _, secret := range []string{s.cfg.UpstreamAPIKey, s.cfg.ProxyAPIKey, s.cfg.R2AccessKey, s.cfg.R2SecretKey} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	message = sensitiveErrorPattern.ReplaceAllString(message, "[redacted]")
	if len(message) > 1000 {
		message = string([]rune(message)[:min(250, len([]rune(message)))])
	}
	return strings.TrimSpace(message)
}
func (s *proxyService) fail(t task, code, message string, httpStatus int) {
	t.Status = "failed"
	t.Error = &taskError{Code: s.publicError(code), Type: "upstream_error", Message: s.publicError(message), HTTPStatus: httpStatus}
	if err := s.stage(&t, t.Stage); err != nil {
		s.logger.Error("cannot persist failed task", "task_id", t.ID)
		return
	}
	s.logger.Warn("task failed", "task_id", t.ID, "stage", t.Stage, "code", t.Error.Code, "http_status", httpStatus, "message", t.Error.Message, "timings_ms", t.Timings, "upstream_request_id", t.UpstreamRequestID)
	if s.cfg.BarkURL != "" {
		go s.notify(t)
	}
}
func (s *proxyService) notify(t task) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u := strings.TrimRight(s.cfg.BarkURL, "/") + "/" + url.PathEscape("ZeroFall image task failed") + "/" + url.PathEscape(t.ID+" · "+t.Stage+" · "+t.Error.Code+" · "+t.Error.Message)
	r, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return
	}
	resp, err := s.client.Do(r)
	if err == nil {
		resp.Body.Close()
	}
}
func (s *proxyService) cleanup(ctx context.Context) {
	for _, t := range s.store.list() {
		if t.Status != "completed" && t.Status != "failed" {
			continue
		}
		if time.Since(time.Unix(t.UpdatedAt, 0)) < s.cfg.TaskRetention {
			continue
		}
		if err := s.blobs.Delete(ctx, t.OutputKeys...); err != nil {
			continue
		}
		s.store.mu.Lock()
		if err := os.RemoveAll(filepath.Join(s.store.root, t.ID)); err == nil {
			delete(s.store.tasks, t.ID)
		}
		s.store.mu.Unlock()
	}
}
func taskView(t task) map[string]any {
	progress := "0%"
	if t.Status == "in_progress" {
		progress = "50%"
	}
	if t.Status == "completed" || t.Status == "failed" {
		progress = "100%"
	}
	v := map[string]any{"id": t.ID, "task_id": t.ID, "object": "image.task", "model": t.Model, "status": t.Status, "progress": progress, "created": t.CreatedAt, "created_at": t.CreatedAt, "stage": t.Stage, "timings_ms": t.Timings}
	if t.Status == "completed" {
		v["data"] = t.Outputs
	}
	if t.Status == "failed" {
		v["error"] = t.Error
	}
	return v
}
func isQuotaError(values ...string) bool {
	v := strings.ToLower(strings.Join(values, " "))
	for _, s := range []string{"quota", "balance", "credit", "billing", "余额", "欠费", "额度不足"} {
		if strings.Contains(v, s) {
			return true
		}
	}
	return false
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "type": code, "message": message}})
}
