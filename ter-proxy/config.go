package main

import (
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	ReferenceFetchURL, ReferenceFetchKey                     string
	ListenAddr, DataPath, PublicMediaURL                     string
	R2Endpoint, R2Region, R2Bucket, R2AccessKey, R2SecretKey string
	UpstreamBaseURL, UpstreamAPIKey, ProxyAPIKey, BarkURL    string
	UpstreamProxyURL, ReferenceProxyURL                      *url.URL
	MaxImageBytes                                            int64
	RequestTimeout, TaskTimeout, TaskRetention               time.Duration
	Workers, QueueLimit                                      int
}

func loadConfig() (config, error) {
	c := config{ListenAddr: ":8080", DataPath: "/data", R2Region: "auto", UpstreamBaseURL: "https://api.terrakinc.com", MaxImageBytes: 20 << 20, RequestTimeout: 120 * time.Second, TaskTimeout: 15 * time.Minute, TaskRetention: 7 * 24 * time.Hour, Workers: 2, QueueLimit: 16}
	for name, target := range map[string]*string{
		"REFERENCE_FETCH_URL": &c.ReferenceFetchURL, "REFERENCE_FETCH_KEY": &c.ReferenceFetchKey,
		"LISTEN_ADDR": &c.ListenAddr, "DATA_PATH": &c.DataPath, "PUBLIC_MEDIA_URL": &c.PublicMediaURL,
		"R2_ENDPOINT": &c.R2Endpoint, "R2_REGION": &c.R2Region, "R2_BUCKET": &c.R2Bucket,
		"R2_ACCESS_KEY": &c.R2AccessKey, "R2_SECRET_KEY": &c.R2SecretKey,
		"UPSTREAM_BASE_URL": &c.UpstreamBaseURL, "UPSTREAM_API_KEY": &c.UpstreamAPIKey,
		"PROXY_API_KEY": &c.ProxyAPIKey, "BARK_URL": &c.BarkURL,
	} {
		if file := os.Getenv(name + "_FILE"); file != "" {
			data, err := os.ReadFile(file)
			if err != nil {
				return c, errors.New("cannot read " + name + "_FILE")
			}
			*target = strings.TrimSpace(string(data))
		} else if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			*target = value
		}
	}
	for name, target := range map[string]*time.Duration{"REFERENCE_TIMEOUT_SECONDS": &c.RequestTimeout, "TASK_TIMEOUT_SECONDS": &c.TaskTimeout, "TASK_RETENTION_SECONDS": &c.TaskRetention} {
		if value := os.Getenv(name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 30*24*3600 {
				return c, errors.New("invalid " + name)
			}
			*target = time.Duration(n) * time.Second
		}
	}
	for name, target := range map[string]*int{"UPSTREAM_WORKERS": &c.Workers, "QUEUE_LIMIT": &c.QueueLimit} {
		if value := os.Getenv(name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 32 {
				return c, errors.New("invalid " + name)
			}
			*target = n
		}
	}
	for name, target := range map[string]**url.URL{"UPSTREAM_PROXY_URL": &c.UpstreamProxyURL, "REFERENCE_PROXY_URL": &c.ReferenceProxyURL} {
		if raw := os.Getenv(name); raw != "" {
			u, err := url.Parse(raw)
			if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") {
				return c, errors.New("invalid " + name)
			}
			*target = u
		}
	}
	for name, value := range map[string]string{"PUBLIC_MEDIA_URL": c.PublicMediaURL, "R2_ENDPOINT": c.R2Endpoint, "UPSTREAM_BASE_URL": c.UpstreamBaseURL} {
		u, err := url.Parse(value)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return c, errors.New("invalid HTTPS URL for " + name)
		}
	}
	if c.R2Bucket == "" || c.R2AccessKey == "" || c.R2SecretKey == "" || c.UpstreamAPIKey == "" || len(c.ProxyAPIKey) < 32 {
		return c, errors.New("R2 and upstream credentials and a proxy key of at least 32 characters are required")
	}
	if c.TaskTimeout > time.Hour || c.TaskRetention < c.TaskTimeout || c.RequestTimeout > c.TaskTimeout {
		return c, errors.New("invalid timeout or retention")
	}
	if err := validateReferenceWorker(c.ReferenceFetchURL, c.ReferenceFetchKey); err != nil {
		return c, err
	}
	c.PublicMediaURL = strings.TrimRight(c.PublicMediaURL, "/")
	c.UpstreamBaseURL = strings.TrimSuffix(strings.TrimRight(c.UpstreamBaseURL, "/"), "/v1")
	return c, nil
}
