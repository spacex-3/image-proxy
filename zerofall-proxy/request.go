package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type imageRequest struct {
	Model      string   `json:"model"`
	Prompt     string   `json:"prompt"`
	Size       string   `json:"size"`
	Quality    string   `json:"quality"`
	References []string `json:"references,omitempty"`
}

func normalizeRequest(body map[string]any, action string) (imageRequest, error) {
	r := imageRequest{Model: stringValue(body["model"]), Prompt: stringValue(body["prompt"])}
	for k := range body {
		switch k {
		case "model", "prompt", "size", "quality", "n", "response_format", "output_format", "async", "images", "image", "image_url", "reference", "reference_image", "reference_images", "reference_image_url", "reference_image_urls":
		default:
			return r, fmt.Errorf("unsupported image parameter: %s", k)
		}
	}
	switch r.Model {
	case "gemini-3.1-flash-image":
		r.Model = "gemini-3.1-flash-image-preview"
	case "gemini-3.0-pro-image":
		r.Model = "gemini-3-pro-image-preview"
	case "gemini-3.1-flash-image-preview", "gemini-3-pro-image-preview":
	default:
		return r, errors.New("unsupported ZeroFall model")
	}
	if r.Prompt == "" {
		return r, errors.New("prompt is required")
	}
	if n, ok := body["n"]; ok && n != json.Number("1") && n != float64(1) && n != 1 {
		return r, errors.New("n must be 1; submit separate tasks for multiple images")
	}
	if a, ok := body["async"]; ok {
		if _, valid := a.(bool); !valid {
			return r, errors.New("async must be a boolean")
		}
	}
	if f, ok := body["response_format"]; ok && f != "url" && f != "b64_json" {
		return r, errors.New("response_format must be url or b64_json")
	}
	if f, ok := body["output_format"]; ok && f != "png" && f != "jpeg" && f != "jpg" && f != "webp" {
		return r, errors.New("unsupported output_format")
	}
	for _, k := range []string{"size", "quality"} {
		if v, ok := body[k]; ok {
			if _, valid := v.(string); !valid {
				return r, fmt.Errorf("%s must be a string", k)
			}
		}
	}
	var err error
	r.Size, r.Quality, err = normalizeSize(stringValue(body["size"]), stringValue(body["quality"]))
	if err != nil {
		return r, err
	}
	for _, k := range []string{"images", "image", "image_url", "reference", "reference_image", "reference_images", "reference_image_url", "reference_image_urls"} {
		v, ok := body[k]
		if !ok {
			continue
		}
		values, ok := v.([]any)
		if !ok {
			values = []any{v}
		}
		for _, v := range values {
			for range 4 {
				m, ok := v.(map[string]any)
				if !ok {
					break
				}
				if u := stringValue(m["url"]); u != "" {
					v = u
				} else {
					v = m["image_url"]
				}
			}
			u := stringValue(v)
			if u == "" {
				return r, errors.New("invalid reference image")
			}
			if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "data:image/") {
				return r, errors.New("references must be HTTPS URLs or image data URLs")
			}
			r.References = append(r.References, u)
		}
	}
	if len(r.References) > 14 {
		return r, errors.New("at most 14 reference images are allowed")
	}
	if action == "edits" && len(r.References) == 0 {
		return r, errors.New("image editing requires a reference image")
	}
	return r, nil
}

// Preserve Cangyuan's aspect-ratio/quality combination while supplying the
// normalized dimensions used by the plugin billing and Gemini conversion.
func normalizeSize(size, quality string) (string, string, error) {
	quality = strings.ToLower(quality)
	if quality != "" && quality != "1k" && quality != "2k" && quality != "4k" {
		return "", "", errors.New("quality must be 1k, 2k, or 4k")
	}
	if size == "" || size == "auto" {
		size = "1:1"
	}
	if size == "1K" || size == "2K" || size == "4K" {
		if quality == "" {
			quality = strings.ToLower(size)
		}
		size = "1:1"
	}
	separator := ":"
	if strings.Contains(size, "x") {
		separator = "x"
	}
	a, b, ok := strings.Cut(size, separator)
	w, e1 := strconv.Atoi(a)
	h, e2 := strconv.Atoi(b)
	if !ok || e1 != nil || e2 != nil || w < 1 || h < 1 || w > 99999 || h > 99999 {
		return "", "", errors.New("invalid image size")
	}
	if quality == "" {
		quality = "1k"
		if separator == "x" && max(w, h) > 1024 {
			quality = "2k"
		}
		if separator == "x" && max(w, h) > 2048 {
			quality = "4k"
		}
	}
	g, bh := w, h
	for bh != 0 {
		g, bh = bh, g%bh
	}
	rw, rh := w/g, h/g
	switch fmt.Sprintf("%d:%d", rw, rh) {
	case "1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4", "9:16", "16:9", "7:3":
	default:
		return "", "", errors.New("unsupported aspect ratio")
	}
	limit := 1024
	if quality == "2k" {
		limit = 2048
	}
	if quality == "4k" {
		limit = 4096
	}
	scale := limit / max(rw, rh)
	return fmt.Sprintf("%dx%d", rw*scale, rh*scale), quality, nil
}

func stringValue(v any) string { s, _ := v.(string); return strings.TrimSpace(s) }
