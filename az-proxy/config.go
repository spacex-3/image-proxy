package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	ReferenceFetchURL, ReferenceFetchKey string
	ReferenceFormat                      string
	ListenAddr                           string
	DataPath                             string
	PublicMediaURL                       string
	R2Endpoint                           string
	R2Region                             string
	R2Bucket                             string
	R2AccessKey                          string
	R2SecretKey                          string
	UpstreamBaseURL                      string
	UpstreamProxyURL                     *url.URL
	ReferenceProxyURL                    *url.URL
	UpstreamAPIKey                       string
	ProxyAPIKey                          string
	BarkURL                              string
	MaxImageBytes                        int64
	PollInterval                         time.Duration
	RequestTimeout                       time.Duration
	ResultTimeout                        time.Duration
	ResultRaceRequests                   int
	TaskTimeout                          time.Duration
	TaskRetention                        time.Duration
	UpstreamWorkers                      int
}

func loadConfig() (config, error) {
	cfg := config{
		ReferenceFormat:    strings.ToLower(envOr("UPSTREAM_REFERENCE_FORMAT", "url")),
		ListenAddr:         envOr("LISTEN_ADDR", ":8080"),
		DataPath:           envOr("DATA_PATH", "/data/tasks.json"),
		PublicMediaURL:     strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_MEDIA_URL")), "/"),
		R2Endpoint:         strings.TrimRight(strings.TrimSpace(os.Getenv("R2_ENDPOINT")), "/"),
		R2Region:           envOr("R2_REGION", "auto"),
		R2Bucket:           strings.TrimSpace(os.Getenv("R2_BUCKET")),
		UpstreamBaseURL:    strings.TrimRight(envOr("UPSTREAM_BASE_URL", "https://cnmoai.com"), "/"),
		MaxImageBytes:      envInt64("MAX_IMAGE_BYTES", 20<<20),
		PollInterval:       time.Duration(envInt64("POLL_INTERVAL_SECONDS", 3)) * time.Second,
		RequestTimeout:     time.Duration(envInt64("REQUEST_TIMEOUT_SECONDS", 60)) * time.Second,
		ResultTimeout:      time.Duration(envInt64("UPSTREAM_RESULT_TIMEOUT_SECONDS", 240)) * time.Second,
		ResultRaceRequests: int(envInt64("UPSTREAM_RESULT_RACE_REQUESTS", 3)),
		TaskTimeout:        time.Duration(envInt64("TASK_TIMEOUT_SECONDS", 60*60)) * time.Second,
		TaskRetention:      time.Duration(envInt64("TASK_RETENTION_SECONDS", 7*24*60*60)) * time.Second,
		UpstreamWorkers:    int(envInt64("UPSTREAM_WORKERS", 4)),
	}
	var err error
	if cfg.UpstreamProxyURL, err = parseProxyURL("UPSTREAM_PROXY_URL", os.Getenv("UPSTREAM_PROXY_URL")); err != nil {
		return config{}, err
	}
	if cfg.ReferenceProxyURL, err = parseProxyURL("REFERENCE_PROXY_URL", os.Getenv("REFERENCE_PROXY_URL")); err != nil {
		return config{}, err
	}
	if cfg.R2AccessKey, err = readSecret("R2_ACCESS_KEY", "R2_ACCESS_KEY_FILE"); err != nil {
		return config{}, err
	}
	if cfg.R2SecretKey, err = readSecret("R2_SECRET_KEY", "R2_SECRET_KEY_FILE"); err != nil {
		return config{}, err
	}
	if cfg.UpstreamAPIKey, err = readSecret("UPSTREAM_API_KEY", "UPSTREAM_API_KEY_FILE"); err != nil {
		return config{}, err
	}
	if cfg.ProxyAPIKey, err = readSecret("PROXY_API_KEY", "PROXY_API_KEY_FILE"); err != nil {
		return config{}, err
	}
	cfg.BarkURL, _ = readSecret("BARK_URL", "BARK_URL_FILE")
	cfg.ReferenceFetchURL = strings.TrimSpace(os.Getenv("REFERENCE_FETCH_URL"))
	if cfg.ReferenceFetchKey, err = readSecret("REFERENCE_FETCH_KEY", "REFERENCE_FETCH_KEY_FILE"); err != nil {
		return config{}, err
	}
	if err = validateReferenceWorker(cfg.ReferenceFetchURL, cfg.ReferenceFetchKey); err != nil {
		return config{}, err
	}

	required := map[string]string{
		"PUBLIC_MEDIA_URL": cfg.PublicMediaURL,
		"R2_ENDPOINT":      cfg.R2Endpoint,
		"R2_BUCKET":        cfg.R2Bucket,
		"R2_ACCESS_KEY":    cfg.R2AccessKey,
		"R2_SECRET_KEY":    cfg.R2SecretKey,
		"UPSTREAM_API_KEY": cfg.UpstreamAPIKey,
		"PROXY_API_KEY":    cfg.ProxyAPIKey,
	}
	for name, value := range required {
		if value == "" {
			return config{}, fmt.Errorf("%s is required", name)
		}
	}
	if !strings.HasPrefix(cfg.PublicMediaURL, "https://") || !strings.HasPrefix(cfg.UpstreamBaseURL, "https://") || !strings.HasPrefix(cfg.R2Endpoint, "https://") {
		return config{}, errors.New("public media, upstream, and R2 endpoints must use HTTPS")
	}
	if cfg.MaxImageBytes <= 0 || cfg.MaxImageBytes > 64<<20 || cfg.UpstreamWorkers < 1 || cfg.UpstreamWorkers > 16 ||
		cfg.ResultRaceRequests < 1 || cfg.ResultRaceRequests > 4 ||
		cfg.RequestTimeout <= 0 || cfg.ResultTimeout < cfg.RequestTimeout ||
		cfg.TaskTimeout < time.Minute || cfg.TaskRetention < cfg.TaskTimeout {
		return config{}, errors.New("invalid image size, timeout, or worker limit")
	}
	if cfg.ReferenceFormat != "url" && cfg.ReferenceFormat != "base64" {
		return config{}, errors.New("UPSTREAM_REFERENCE_FORMAT must be url or base64")
	}
	return cfg, nil
}

func parseProxyURL(name, raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("%s must be a proxy URL without credentials, path, query, or fragment", name)
	}
	switch parsed.Scheme {
	case "http", "https", "socks5":
	default:
		return nil, fmt.Errorf("%s must use HTTP, HTTPS, or SOCKS5", name)
	}
	parsed.Path = ""
	return parsed, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt64(name string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func readSecret(valueName, fileName string) (string, error) {
	if path := strings.TrimSpace(os.Getenv(fileName)); path != "" {
		content, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", fileName, err)
		}
		return strings.TrimSpace(string(content)), nil
	}
	return strings.TrimSpace(os.Getenv(valueName)), nil
}
