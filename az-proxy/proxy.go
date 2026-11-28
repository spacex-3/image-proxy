package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxRequestBodyBytes = 32 << 20

var taskIDPattern = regexp.MustCompile(`^azp_[A-Za-z0-9_-]{24}$`)
var sensitiveErrorPattern = regexp.MustCompile(`(?i)https?://[^\s<>]+|Bearer\s+\S+|sk-[A-Za-z0-9_-]{8,}`)
var errProxyStorage = errors.New("proxy storage failure")

type proxyService struct {
	cfg                  config
	store                *taskStore
	blobs                blobStore
	loader               *imageLoader
	httpClient           *http.Client
	resultClient         *http.Client
	resultFallbackClient *http.Client
	logger               *slog.Logger
	workers              chan struct{}
	polling              sync.Map
}

type upstreamJSONResult struct {
	body       map[string]any
	statusCode int
	err        error
}

func newProxyService(cfg config, store *taskStore, blobs blobStore, logger *slog.Logger) *proxyService {
	http1Transport := http.DefaultTransport.(*http.Transport).Clone()
	http1Protocols := new(http.Protocols)
	http1Protocols.SetHTTP1(true)
	http1Transport.Protocols = http1Protocols
	http1Transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.UpstreamProxyURL != nil {
		http1Transport.Proxy = http.ProxyURL(cfg.UpstreamProxyURL)
	}
	http2Transport := http.DefaultTransport.(*http.Transport).Clone()
	http2Protocols := new(http.Protocols)
	http2Protocols.SetHTTP1(true)
	http2Protocols.SetHTTP2(true)
	http2Transport.Protocols = http2Protocols
	http2Transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.UpstreamProxyURL != nil {
		http2Transport.Proxy = http.ProxyURL(cfg.UpstreamProxyURL)
	}
	return &proxyService{
		cfg:                  cfg,
		store:                store,
		blobs:                blobs,
		loader:               newReferenceLoader(cfg.MaxImageBytes, cfg.RequestTimeout, cfg.ReferenceProxyURL, cfg.ReferenceFetchURL, cfg.ReferenceFetchKey),
		httpClient:           &http.Client{Transport: http1Transport, Timeout: cfg.RequestTimeout},
		resultClient:         &http.Client{Transport: http1Transport, Timeout: cfg.ResultTimeout},
		resultFallbackClient: &http.Client{Transport: http2Transport, Timeout: cfg.ResultTimeout},
		logger:               logger,
		workers:              make(chan struct{}, cfg.UpstreamWorkers),
	}
}

func (s *proxyService) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /v1/images/generations", s.auth(s.submit("generations")))
	mux.HandleFunc("POST /v1/images/edits", s.auth(s.submit("edits")))
	mux.HandleFunc("POST /v1/images/generations/async", s.auth(s.submit("generations")))
	mux.HandleFunc("POST /v1/images/edits/async", s.auth(s.submit("edits")))
	mux.HandleFunc("GET /v1/images/generations/{task_id}", s.auth(s.query("generations")))
	mux.HandleFunc("GET /v1/images/edits/{task_id}", s.auth(s.query("edits")))
	mux.HandleFunc("GET /v1/images/tasks/{task_id}", s.auth(s.queryTask))
	return s.securityHeaders(mux)
}

func (s *proxyService) queryTask(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("task_id")
	if !taskIDPattern.MatchString(id) {
		writeAPIError(writer, http.StatusNotFound, "not_found", "Task not found")
		return
	}
	task, ok := s.store.get(id)
	if !ok {
		writeAPIError(writer, http.StatusNotFound, "not_found", "Task not found")
		return
	}
	writeJSON(writer, http.StatusOK, taskView(task))
}

func (s *proxyService) health(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

func (s *proxyService) auth(next http.HandlerFunc) http.HandlerFunc {
	expected := sha256.Sum256([]byte(s.cfg.ProxyAPIKey))
	return func(writer http.ResponseWriter, request *http.Request) {
		provided := strings.TrimSpace(request.Header.Get("Authorization"))
		if scheme, value, ok := strings.Cut(provided, " "); !ok || !strings.EqualFold(scheme, "Bearer") {
			writeAPIError(writer, http.StatusUnauthorized, "authentication_error", "Invalid proxy API key")
			return
		} else {
			actual := sha256.Sum256([]byte(strings.TrimSpace(value)))
			if subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
				writeAPIError(writer, http.StatusUnauthorized, "authentication_error", "Invalid proxy API key")
				return
			}
		}
		next(writer, request)
	}
}

func (s *proxyService) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(writer, request)
	})
}

func (s *proxyService) submit(action string) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBodyBytes)
		decoder := json.NewDecoder(request.Body)
		decoder.UseNumber()
		var body map[string]any
		if err := decoder.Decode(&body); err != nil {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Request body must be valid JSON")
			return
		}
		if err := ensureJSONEOF(decoder); err != nil {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Request body must contain one JSON object")
			return
		}

		task, upstreamBody, err := s.prepareSubmission(request.Context(), action, body)
		if err != nil {
			if errors.Is(err, errProxyStorage) {
				s.logger.Error("store reference image", "error", err)
				writeAPIError(writer, http.StatusBadGateway, "proxy_storage_error", "Failed to store reference image")
				return
			}
			writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if err = s.store.put(task); err != nil {
			_ = s.blobs.Delete(request.Context(), task.InputKeys...)
			s.logger.Error("persist submitting task", "task_id", task.ID, "error", err)
			writeAPIError(writer, http.StatusInternalServerError, "proxy_storage_error", "Failed to persist task")
			return
		}

		upstream, statusCode, err := s.upstreamJSON(request.Context(), http.MethodPost, "/v1/images/"+action+"/async", upstreamBody)
		if err != nil {
			_ = s.store.remove(task.ID)
			_ = s.blobs.Delete(request.Context(), task.InputKeys...)
			s.logger.Warn("upstream submission failed", "task_id", task.ID, "status", statusCode, "error", err)
			if statusCode == 0 {
				writeAPIError(writer, http.StatusBadGateway, "upstream_unavailable", "AZ upstream request failed")
				return
			}
			errorCode, errorType, message := upstreamError(upstream, statusCode)
			writeUpstreamError(writer, statusCode, upstream)
			if statusCode == http.StatusPaymentRequired || statusCode == http.StatusForbidden || statusCode >= 500 || isQuotaError(errorCode, errorType, message) {
				go s.notify("AZ proxy submission failed", fmt.Sprintf("HTTP %d while submitting %s", statusCode, task.ID))
			}
			return
		}
		upstreamID := stringValue(upstream["task_id"])
		if upstreamID == "" {
			upstreamID = stringValue(upstream["id"])
		}
		if upstreamID == "" {
			_ = s.store.remove(task.ID)
			_ = s.blobs.Delete(request.Context(), task.InputKeys...)
			writeAPIError(writer, http.StatusBadGateway, "upstream_invalid_response", "AZ upstream returned no task ID")
			return
		}
		task, err = s.store.update(task.ID, func(task *proxyTask) {
			task.UpstreamID = upstreamID
			task.Status = statusProcessing
			task.NextPollAt = time.Now().Add(s.cfg.PollInterval).Unix()
		})
		if err != nil {
			s.logger.Error("persist submitted task", "task_id", task.ID, "error", err)
			writeAPIError(writer, http.StatusInternalServerError, "proxy_storage_error", "Failed to persist task")
			return
		}
		s.logger.Info("task submitted", "task_id", task.ID, "action", action, "model", task.Model)
		writer.Header().Set("Location", "/v1/images/"+action+"/"+task.ID)
		writer.Header().Set("Retry-After", strconv.Itoa(max(1, int(s.cfg.PollInterval/time.Second))))
		writeJSON(writer, http.StatusAccepted, taskView(task))
	}
}

func (s *proxyService) query(action string) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id := request.PathValue("task_id")
		if !taskIDPattern.MatchString(id) {
			writeAPIError(writer, http.StatusNotFound, "not_found", "Task not found")
			return
		}
		task, ok := s.store.get(id)
		if !ok || task.Action != action {
			writeAPIError(writer, http.StatusNotFound, "not_found", "Task not found")
			return
		}
		writeJSON(writer, http.StatusOK, taskView(task))
	}
}

func (s *proxyService) prepareSubmission(ctx context.Context, action string, body map[string]any) (proxyTask, map[string]any, error) {
	id, err := randomID("azp_")
	if err != nil {
		return proxyTask{}, nil, err
	}
	model := stringValue(body["model"])
	prompt := stringValue(body["prompt"])
	if model == "" || prompt == "" {
		return proxyTask{}, nil, errors.New("model and a non-empty prompt are required")
	}
	if value, exists := body["n"]; exists {
		number, ok := integerValue(value)
		if !ok || number != 1 {
			return proxyTask{}, nil, errors.New("AZ supports n=1")
		}
	}
	size, err := normalizeImageSize(stringValue(body["size"]), stringValue(body["quality"]))
	if err != nil {
		return proxyTask{}, nil, err
	}
	references, err := collectReferences(body)
	if err != nil {
		return proxyTask{}, nil, err
	}
	if action == "edits" && len(references) == 0 {
		return proxyTask{}, nil, errors.New("image editing requires at least one reference image")
	}
	if len(references) > 10 {
		return proxyTask{}, nil, errors.New("AZ accepts at most 10 reference images")
	}

	task := proxyTask{
		ID: id, Action: action, Model: model, Status: statusSubmitting,
		CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
	}
	upstream := map[string]any{
		"model": model, "prompt": prompt, "size": size, "n": 1, "response_format": "b64_json",
	}
	if len(references) > 0 {
		images := make([]map[string]string, 0, len(references))
		inlineBytes := 0
		for index, reference := range references {
			data, contentType, loadErr := s.loader.Load(ctx, reference)
			if loadErr != nil {
				_ = s.blobs.Delete(ctx, task.InputKeys...)
				return proxyTask{}, nil, loadErr
			}
			if s.cfg.ReferenceFormat == "base64" {
				encodedBytes := base64.StdEncoding.EncodedLen(len(data))
				if encodedBytes > (24<<20)-inlineBytes {
					return proxyTask{}, nil, errors.New("AZ reference request exceeds 24 MiB after Base64 encoding; reduce reference size or count")
				}
				inlineBytes += encodedBytes
				images = append(images, map[string]string{"image_url": "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data)})
				continue
			}
			key := objectKey("inputs", task.ID, index, contentType)
			if putErr := s.blobs.Put(ctx, key, data, contentType); putErr != nil {
				_ = s.blobs.Delete(ctx, task.InputKeys...)
				return proxyTask{}, nil, fmt.Errorf("%w: %v", errProxyStorage, putErr)
			}
			task.InputKeys = append(task.InputKeys, key)
			images = append(images, map[string]string{"image_url": s.blobs.PublicURL(key)})
		}
		upstream["images"] = images
		if s.cfg.ReferenceFormat == "base64" {
			encoded, encodeErr := json.Marshal(upstream)
			if encodeErr != nil {
				return proxyTask{}, nil, encodeErr
			}
			// AZ's 24 MiB request limit includes Base64 expansion and JSON.
			// Reject before a generation POST; never resubmit through another format.
			if len(encoded) > 24<<20 {
				return proxyTask{}, nil, errors.New("AZ reference request exceeds 24 MiB after Base64 encoding; reduce reference size or count")
			}
		}
	}
	return task, upstream, nil
}

func collectReferences(body map[string]any) ([]string, error) {
	fields := []string{"images", "image", "image_url", "reference", "reference_image", "reference_images", "reference_image_url", "reference_image_urls"}
	result := make([]string, 0)
	for _, field := range fields {
		value, ok := body[field]
		if !ok || value == nil {
			continue
		}
		values := []any{value}
		if array, ok := value.([]any); ok {
			values = array
		}
		for _, item := range values {
			reference := unwrapReference(item)
			if reference == "" {
				return nil, fmt.Errorf("%s contains an invalid reference image", field)
			}
			result = append(result, reference)
		}
	}
	return result, nil
}

func unwrapReference(value any) string {
	for depth := 0; depth < 4; depth++ {
		switch typed := value.(type) {
		case string:
			return strings.TrimSpace(typed)
		case map[string]any:
			if candidate := stringValue(typed["url"]); candidate != "" {
				value = candidate
			} else {
				value = typed["image_url"]
			}
		default:
			return ""
		}
	}
	return ""
}

func normalizeImageSize(size, quality string) (string, error) {
	size = strings.TrimSpace(size)
	quality = strings.ToUpper(strings.TrimSpace(quality))
	if quality != "" && quality != "1K" && quality != "2K" && quality != "4K" {
		return "", errors.New("quality must be 1k, 2k, or 4k")
	}
	if size == "" {
		size = "auto"
	}
	if size == "auto" || size == "1K" || size == "2K" || size == "4K" {
		if quality != "" {
			return quality, nil
		}
		return size, nil
	}
	if strings.Contains(size, "x") {
		if _, _, err := validateDimensions(size); err != nil {
			return "", err
		}
		if quality == "" {
			return size, nil
		}
	}
	separator := ":"
	if strings.Contains(size, "x") {
		separator = "x"
	}
	left, right, ok := strings.Cut(size, separator)
	if !ok {
		return "", errors.New("size must be auto, 1K, 2K, 4K, a ratio, or widthxheight")
	}
	width, widthErr := strconv.Atoi(left)
	height, heightErr := strconv.Atoi(right)
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
		return "", errors.New("size has invalid dimensions")
	}
	divisor := gcd(width, height)
	ratioWidth, ratioHeight := width/divisor, height/divisor
	ratio := fmt.Sprintf("%d:%d", ratioWidth, ratioHeight)
	allowed := map[string]bool{"1:1": true, "2:3": true, "3:2": true, "3:4": true, "4:3": true, "4:5": true, "5:4": true, "9:16": true, "16:9": true, "7:3": true}
	if !allowed[ratio] {
		return "", errors.New("size has an unsupported aspect ratio")
	}
	limit := 1024
	if quality == "2K" {
		limit = 2048
	} else if quality == "4K" {
		limit = 4096
	}
	scale := limit / max(ratioWidth, ratioHeight)
	return fmt.Sprintf("%dx%d", ratioWidth*scale, ratioHeight*scale), nil
}

func validateDimensions(value string) (int, int, error) {
	left, right, ok := strings.Cut(value, "x")
	if !ok {
		return 0, 0, errors.New("invalid dimensions")
	}
	width, widthErr := strconv.Atoi(left)
	height, heightErr := strconv.Atoi(right)
	if widthErr != nil || heightErr != nil || width < 1 || width > 99999 || height < 1 || height > 99999 {
		return 0, 0, errors.New("invalid dimensions")
	}
	return width, height, nil
}

func gcd(left, right int) int {
	for right != 0 {
		left, right = right, left%right
	}
	return left
}

func (s *proxyService) upstreamJSON(ctx context.Context, method, route string, body any) (map[string]any, int, error) {
	return s.upstreamJSONWithClient(ctx, s.httpClient, method, route, body)
}

func (s *proxyService) upstreamJSONWithClient(ctx context.Context, client *http.Client, method, route string, body any) (map[string]any, int, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, s.cfg.UpstreamBaseURL+route, payload)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+s.cfg.UpstreamAPIKey)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if readErr != nil {
		return nil, response.StatusCode, readErr
	}
	decoded := make(map[string]any)
	if len(data) > 0 {
		if err = json.Unmarshal(data, &decoded); err != nil {
			return nil, response.StatusCode, errors.New("AZ upstream returned invalid JSON")
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decoded, response.StatusCode, fmt.Errorf("AZ upstream HTTP %d", response.StatusCode)
	}
	return decoded, response.StatusCode, nil
}

func (s *proxyService) upstreamResultJSON(ctx context.Context, route string) (map[string]any, int, error) {
	body, statusCode, err := s.upstreamJSONWithClient(ctx, s.resultClient, http.MethodGet, route, nil)
	if err == nil || statusCode != http.StatusOK || s.cfg.ResultRaceRequests == 1 {
		return body, statusCode, err
	}

	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan upstreamJSONResult, s.cfg.ResultRaceRequests)
	for attempt := range s.cfg.ResultRaceRequests {
		go func() {
			client := s.resultClient
			if attempt%2 == 0 {
				client = s.resultFallbackClient
			}
			body, statusCode, err := s.upstreamJSONWithClient(raceCtx, client, http.MethodGet, route, nil)
			results <- upstreamJSONResult{body: body, statusCode: statusCode, err: err}
		}()
	}
	last := upstreamJSONResult{body: body, statusCode: statusCode, err: err}
	for range s.cfg.ResultRaceRequests {
		result := <-results
		if result.err == nil {
			cancel()
			return result.body, result.statusCode, nil
		}
		last = result
	}
	return last.body, last.statusCode, last.err
}

func (s *proxyService) run(ctx context.Context) {
	for _, task := range s.store.submitting() {
		s.failTask(ctx, task, "proxy_restarted", "proxy_error", "The proxy restarted before submission completed; retry the request")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			for _, task := range s.store.due(now) {
				task := task
				if _, loaded := s.polling.LoadOrStore(task.ID, struct{}{}); loaded {
					continue
				}
				go func() {
					defer s.polling.Delete(task.ID)
					select {
					case s.workers <- struct{}{}:
						defer func() { <-s.workers }()
					case <-ctx.Done():
						return
					}
					s.pollTask(ctx, task)
				}()
			}
			if now.Unix()%60 == 0 {
				s.cleanup(ctx, now)
			}
		}
	}
}

func (s *proxyService) pollTask(ctx context.Context, task proxyTask) {
	if time.Since(time.Unix(task.CreatedAt, 0)) > s.cfg.TaskTimeout {
		s.failTask(ctx, task, "task_timeout", "upstream_timeout", "AZ image generation timed out")
		return
	}
	body, statusCode, err := s.upstreamResultJSON(ctx, "/v1/images/tasks/"+url.PathEscape(task.UpstreamID))
	if err != nil {
		s.logger.Warn("task poll failed", "task_id", task.ID, "status", statusCode, "error", err)
		if statusCode >= 400 && statusCode < 500 && statusCode != http.StatusTooManyRequests {
			errorCode, errorType, message := upstreamError(body, statusCode)
			s.failTask(ctx, task, errorCode, errorType, message)
			return
		}
		_, _ = s.store.update(task.ID, func(current *proxyTask) { current.NextPollAt = time.Now().Add(s.cfg.PollInterval).Unix() })
		return
	}
	switch strings.ToLower(stringValue(body["status"])) {
	case "processing", "queued", "pending", "in_progress":
		_, _ = s.store.update(task.ID, func(current *proxyTask) { current.NextPollAt = time.Now().Add(s.cfg.PollInterval).Unix() })
	case "completed":
		s.completeTask(ctx, task, body)
	case "failed", "cancelled":
		errorObject, _ := body["error"].(map[string]any)
		typeName := stringValue(errorObject["type"])
		message := publicErrorMessage(stringValue(errorObject["message"]))
		if typeName == "" {
			typeName = "upstream_error"
		}
		if message == "" {
			message = "AZ image generation failed"
		}
		code := stringValue(errorObject["code"])
		if code == "" {
			code = typeName
		}
		s.failTask(ctx, task, code, typeName, message)
	default:
		s.logger.Warn("unrecognized task status", "task_id", task.ID)
		_, _ = s.store.update(task.ID, func(current *proxyTask) { current.NextPollAt = time.Now().Add(s.cfg.PollInterval).Unix() })
	}
}

func (s *proxyService) completeTask(ctx context.Context, task proxyTask, body map[string]any) {
	result, _ := body["result"].(map[string]any)
	entries, _ := result["data"].([]any)
	if len(entries) != 1 {
		s.failTask(ctx, task, "upstream_invalid_response", "upstream_error", "AZ completed without exactly one image")
		return
	}
	outputs := make([]imageOutput, 0, len(entries))
	keys := make([]string, 0, len(entries))
	for index, raw := range entries {
		entry, _ := raw.(map[string]any)
		encoded := stringValue(entry["b64_json"])
		if encoded == "" {
			encoded = stringValue(entry["url"])
		}
		var data []byte
		var contentType string
		var err error
		if strings.HasPrefix(encoded, "data:") {
			data, contentType, err = decodeDataURL(encoded, s.cfg.MaxImageBytes)
		} else {
			data, err = base64.StdEncoding.DecodeString(encoded)
			if err == nil {
				data, contentType, err = validateImage(data, "", s.cfg.MaxImageBytes)
			}
		}
		if err != nil {
			_ = s.blobs.Delete(ctx, keys...)
			s.failTask(ctx, task, "upstream_invalid_response", "upstream_error", "AZ returned invalid image data")
			return
		}
		key := objectKey("outputs", task.ID, index, contentType)
		if err = s.blobs.Put(ctx, key, data, contentType); err != nil {
			_ = s.blobs.Delete(ctx, keys...)
			s.failTask(ctx, task, "proxy_storage_error", "storage_error", "Failed to store generated image")
			return
		}
		keys = append(keys, key)
		outputs = append(outputs, imageOutput{URL: s.blobs.PublicURL(key)})
	}
	updated, err := s.store.update(task.ID, func(current *proxyTask) {
		current.Status = statusCompleted
		current.OutputKeys = keys
		current.Outputs = outputs
		current.NextPollAt = 0
	})
	if err != nil {
		_ = s.blobs.Delete(ctx, keys...)
		s.logger.Error("persist completed task", "task_id", task.ID, "error", err)
		return
	}
	_ = s.blobs.Delete(ctx, updated.InputKeys...)
	s.logger.Info("task completed", "task_id", task.ID, "images", len(outputs))
}

func (s *proxyService) failTask(ctx context.Context, task proxyTask, errorCode, errorType, message string) {
	message = publicErrorMessage(message)
	updated, err := s.store.update(task.ID, func(current *proxyTask) {
		current.Status = statusFailed
		current.ErrorCode = errorCode
		current.ErrorType = errorType
		current.ErrorMessage = message
		current.NextPollAt = 0
	})
	if err != nil {
		s.logger.Error("persist failed task", "task_id", task.ID, "error", err)
		return
	}
	_ = s.blobs.Delete(ctx, updated.InputKeys...)
	s.logger.Warn("task failed", "task_id", task.ID, "error_type", errorType, "message", message)
	if !updated.FailureNotified {
		go s.notify("AZ proxy task failed", task.ID+": "+message)
		_, _ = s.store.update(task.ID, func(current *proxyTask) { current.FailureNotified = true })
	}
}

func (s *proxyService) cleanup(ctx context.Context, now time.Time) {
	for _, task := range s.store.expired(now.Add(-s.cfg.TaskRetention).Unix()) {
		_ = s.blobs.Delete(ctx, append(task.InputKeys, task.OutputKeys...)...)
		if err := s.store.remove(task.ID); err != nil {
			s.logger.Warn("remove expired task", "task_id", task.ID, "error", err)
		}
	}
}

func (s *proxyService) notify(title, message string) {
	if s.cfg.BarkURL == "" {
		return
	}
	base := strings.TrimRight(s.cfg.BarkURL, "/")
	request, err := http.NewRequest(http.MethodGet, base+"/"+url.PathEscape(title)+"/"+url.PathEscape(message), nil)
	if err != nil {
		return
	}
	response, err := s.httpClient.Do(request)
	if err == nil {
		response.Body.Close()
	}
}

func taskView(task proxyTask) map[string]any {
	status := string(task.Status)
	if task.Status == statusSubmitting {
		status = string(statusProcessing)
	}
	progress := "50%"
	if task.Status == statusCompleted || task.Status == statusFailed {
		progress = "100%"
	}
	view := map[string]any{
		"id": task.ID, "task_id": task.ID, "object": "image.task", "model": task.Model,
		"status": status, "progress": progress, "created_at": task.CreatedAt,
	}
	if task.Status == statusCompleted {
		view["data"] = task.Outputs
	}
	if task.Status == statusFailed {
		view["error"] = map[string]string{"code": task.ErrorCode, "type": task.ErrorType, "message": task.ErrorMessage}
	}
	return view
}

func writeUpstreamError(writer http.ResponseWriter, status int, body map[string]any) {
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	code, typeName, message := upstreamError(body, status)
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code, "type": typeName, "message": message}})
}

func upstreamError(body map[string]any, status int) (string, string, string) {
	errorObject, _ := body["error"].(map[string]any)
	code := stringValue(errorObject["code"])
	typeName := stringValue(errorObject["type"])
	message := publicErrorMessage(stringValue(errorObject["message"]))
	if typeName == "" {
		typeName = "upstream_error"
	}
	if code == "" {
		code = typeName
	}
	if message == "" {
		message = fmt.Sprintf("AZ upstream returned HTTP %d", status)
	}
	return code, typeName, message
}

func publicErrorMessage(message string) string {
	return sensitiveErrorPattern.ReplaceAllString(strings.TrimSpace(message), "[redacted]")
}

func isQuotaError(values ...string) bool {
	combined := strings.ToLower(strings.Join(values, " "))
	return strings.Contains(combined, "quota") || strings.Contains(combined, "billing") || strings.Contains(combined, "balance") || strings.Contains(combined, "credit")
}

func writeAPIError(writer http.ResponseWriter, status int, errorType, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"type": errorType, "message": message}})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func stringValue(value any) string {
	stringValue, _ := value.(string)
	return strings.TrimSpace(stringValue)
}

func integerValue(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	case float64:
		return int64(typed), typed == float64(int64(typed))
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	default:
		return 0, false
	}
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return errors.New("extra JSON value")
}
