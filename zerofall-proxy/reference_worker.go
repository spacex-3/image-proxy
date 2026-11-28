package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Each provider owns this adapter snapshot so its Docker build stays isolated.
// Worker mode takes precedence over the optional reference proxy. It never
// silently falls back to direct fetching and does not affect output downloads.
type referenceWorker struct {
	endpoint, key string
	client        *http.Client
}

func validateReferenceWorker(endpoint, key string) error {
	if endpoint == "" && key == "" {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(key) < 32 {
		return errors.New("REFERENCE_FETCH_URL must be HTTPS and REFERENCE_FETCH_KEY must contain at least 32 characters")
	}
	return nil
}

func newReferenceLoader(maxBytes int64, timeout time.Duration, proxyURL *url.URL, endpoint, key string) *imageLoader {
	loader := newImageLoader(maxBytes, timeout, proxyURL)
	if endpoint != "" {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		loader.worker = &referenceWorker{endpoint: endpoint, key: key, client: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	}
	return loader
}

func (w *referenceWorker) load(ctx context.Context, reference string, maxBytes int64) ([]byte, string, error) {
	u, err := url.Parse(reference)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || len(reference) > 8192 {
		return nil, "", errors.New("reference worker requires an HTTPS image URL without credentials")
	}
	payload, err := json.Marshal(map[string]string{"url": reference})
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, "", errors.New("invalid reference worker endpoint")
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(w.key))
	_, _ = mac.Write([]byte(timestamp + "\n"))
	_, _ = mac.Write(payload)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Reference-Timestamp", timestamp)
	req.Header.Set("X-Reference-Signature", hex.EncodeToString(mac.Sum(nil)))
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, "", errors.New("reference download worker connection failed or timed out")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("reference download worker returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", errors.New("reference download worker returned an incomplete image")
	}
	return validateImage(data, strings.TrimSpace(resp.Header.Get("Content-Type")), maxBytes)
}
