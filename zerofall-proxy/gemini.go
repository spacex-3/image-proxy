package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Only final image parts count toward the one-image task contract. Thinking
// images, text-only replies, blocked replies and multiple candidates never settle.
func geminiImageResult(body map[string]any) (encoded, outputURL string, err error) {
	if feedback, ok := body["promptFeedback"].(map[string]any); ok && stringValue(feedback["blockReason"]) != "" {
		return "", "", errors.New("Upstream blocked the image request")
	}
	candidates, _ := body["candidates"].([]any)
	if len(candidates) != 1 {
		return "", "", errors.New("Upstream did not return one image candidate")
	}
	candidate, _ := candidates[0].(map[string]any)
	finish := stringValue(candidate["finishReason"])
	if finish != "" && finish != "STOP" {
		return "", "", fmt.Errorf("Upstream image generation ended with %s", finish)
	}
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	count := 0
	for _, item := range parts {
		part, _ := item.(map[string]any)
		if part["thought"] == true {
			continue
		}
		inline, _ := part["inlineData"].(map[string]any)
		file, _ := part["fileData"].(map[string]any)
		if v := stringValue(inline["data"]); v != "" {
			encoded = v
			count++
		}
		if v := stringValue(file["fileUri"]); v != "" {
			outputURL = v
			count++
		}
	}
	if count != 1 {
		return "", "", errors.New("Upstream did not return exactly one final image")
	}
	return encoded, outputURL, nil
}

var authenticatedImagePath = regexp.MustCompile(`^/v1/images/[A-Za-z0-9_-]+/content$`)

// The upstream key is allowed only on its documented cache GET endpoint at the
// configured origin. All public R2/other image links use the credentialless loader.
// Redirects are processed separately without copying headers or credentials.
func (s *proxyService) downloadGeneratedImage(ctx context.Context, rawURL string) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.User != nil || u.Fragment != "" {
		return nil, "", errors.New("Invalid output URL")
	}
	base, err := url.Parse(s.cfg.UpstreamBaseURL)
	if err != nil {
		return nil, "", errors.New("Invalid upstream URL")
	}
	if u.Scheme != base.Scheme || !strings.EqualFold(u.Host, base.Host) || !authenticatedImagePath.MatchString(u.Path) || u.RawPath != "" {
		return s.outputLoader.Load(ctx, rawURL)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q) != 1 || len(q["index"]) != 1 || q.Get("index") != "0" {
		return nil, "", errors.New("Invalid image cache index")
	}
	for attempt := range 3 {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, "", ctx.Err()
			case <-timer.C:
			}
		}
		image, mime, retry, loadErr := s.readCachedImage(ctx, u.String())
		if !retry || attempt == 2 {
			return image, mime, loadErr
		}
	}
	return nil, "", errors.New("Cannot download generated image")
}

func (s *proxyService) readCachedImage(ctx context.Context, rawURL string) ([]byte, string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.UpstreamAPIKey)
	req.Header.Set("Accept", "image/png,image/jpeg,image/webp,image/gif")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		dest, err := resp.Location()
		if err != nil || dest.Scheme != "https" || dest.User != nil || dest.Fragment != "" {
			return nil, "", false, errors.New("Unsafe output redirect")
		}
		image, mime, err := s.outputLoader.Load(ctx, dest.String())
		return image, mime, false, err
	}
	if resp.StatusCode != http.StatusOK {
		retry := resp.StatusCode == 429 || resp.StatusCode >= 500
		return nil, "", retry, fmt.Errorf("Image cache returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, s.cfg.MaxImageBytes+1))
	if err != nil {
		return nil, "", true, err
	}
	image, mime, err := validateImage(data, resp.Header.Get("Content-Type"), s.cfg.MaxImageBytes)
	return image, mime, false, err
}
