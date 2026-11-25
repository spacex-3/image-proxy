package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var allowedImageTypes = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/webp": "webp",
	"image/gif":  "gif",
}

type blobStore interface {
	Put(context.Context, string, []byte, string) error
	Delete(context.Context, ...string) error
	PublicURL(string) string
}

type r2Store struct {
	client    *s3.Client
	bucket    string
	publicURL string
}

func newR2Store(ctx context.Context, cfg config) (*r2Store, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.R2Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.R2AccessKey, cfg.R2SecretKey, "")),
	)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(cfg.R2Endpoint)
		options.UsePathStyle = true
	})
	return &r2Store{client: client, bucket: cfg.R2Bucket, publicURL: cfg.PublicMediaURL}, nil
}

func (s *r2Store) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(key),
		Body:         bytes.NewReader(data),
		ContentType:  aws.String(contentType),
		CacheControl: aws.String("public, max-age=86400, immutable"),
	})
	return err
}

func (s *r2Store) Delete(ctx context.Context, keys ...string) error {
	var first error
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *r2Store) PublicURL(key string) string {
	parts := strings.Split(key, "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return s.publicURL + "/" + strings.Join(parts, "/")
}

type imageLoader struct {
	client          *http.Client
	maxBytes        int64
	validateTargets bool
}

func newImageLoader(maxBytes int64, timeout time.Duration, proxyURL *url.URL) *imageLoader {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if proxyURL == nil {
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			addresses, err := resolvePublicReferenceHost(ctx, host)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].String(), port))
		}
	} else {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	loader := &imageLoader{
		maxBytes:        maxBytes,
		validateTargets: proxyURL != nil,
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many reference image redirects")
				}
				if request.URL.User != nil || request.URL.Fragment != "" || (via[0].URL.Scheme == "https" && request.URL.Scheme != "https") {
					return errors.New("unsafe reference image redirect")
				}
				if proxyURL != nil {
					if _, err := resolvePublicReferenceHost(request.Context(), request.URL.Hostname()); err != nil {
						return err
					}
				}
				return nil
			},
		},
	}
	return loader
}

func (l *imageLoader) Load(ctx context.Context, reference string) ([]byte, string, error) {
	reference = strings.TrimSpace(reference)
	if strings.HasPrefix(reference, "data:") {
		return decodeDataURL(reference, l.maxBytes)
	}
	parsed, err := url.Parse(reference)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, "", errors.New("reference image must be an HTTP(S) URL or image data URL")
	}
	if l.validateTargets {
		if _, err = resolvePublicReferenceHost(ctx, parsed.Hostname()); err != nil {
			return nil, "", err
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Accept", "image/png,image/jpeg,image/webp,image/gif")
	response, err := l.client.Do(request)
	if err != nil {
		return nil, "", fmt.Errorf("download reference image: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("reference image returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, l.maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	return validateImage(data, response.Header.Get("Content-Type"), l.maxBytes)
}

func resolvePublicReferenceHost(ctx context.Context, host string) ([]net.IP, error) {
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("reference image host did not resolve")
	}
	public := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		if isPublicIP(address.IP) {
			public = append(public, address.IP)
		}
	}
	if len(public) == 0 {
		return nil, errors.New("reference image host is not public")
	}
	return public, nil
}

func decodeDataURL(value string, maxBytes int64) ([]byte, string, error) {
	header, encoded, ok := strings.Cut(value, ",")
	if !ok || !strings.HasSuffix(header, ";base64") {
		return nil, "", errors.New("invalid image data URL")
	}
	contentType := strings.TrimPrefix(strings.TrimSuffix(header, ";base64"), "data:")
	if _, ok = allowedImageTypes[contentType]; !ok {
		return nil, "", errors.New("unsupported reference image type")
	}
	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))
	data, err := io.ReadAll(io.LimitReader(decoder, maxBytes+1))
	if err != nil {
		return nil, "", errors.New("invalid reference image Base64")
	}
	return validateImage(data, contentType, maxBytes)
}

func validateImage(data []byte, declaredType string, maxBytes int64) ([]byte, string, error) {
	if len(data) == 0 || int64(len(data)) > maxBytes {
		return nil, "", errors.New("reference image is empty or too large")
	}
	detected := http.DetectContentType(data)
	if _, ok := allowedImageTypes[detected]; !ok {
		return nil, "", errors.New("reference image is not PNG, JPEG, WebP, or GIF")
	}
	declaredType = strings.ToLower(strings.TrimSpace(strings.Split(declaredType, ";")[0]))
	if declaredType != "" && declaredType != "application/octet-stream" {
		if _, ok := allowedImageTypes[declaredType]; !ok || declaredType != detected {
			return nil, "", errors.New("reference image content type does not match its bytes")
		}
	}
	return data, detected, nil
}

func isPublicIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsMulticast() {
		return false
	}
	cgnat := netip.MustParsePrefix("100.64.0.0/10")
	return !cgnat.Contains(address.Unmap())
}

func randomID(prefix string) (string, error) {
	data := make([]byte, 18)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(data), nil
}

func objectKey(kind, taskID string, index int, contentType string) string {
	extension := allowedImageTypes[contentType]
	return path.Join("az-proxy", kind, taskID, fmt.Sprintf("%d-%d.%s", index+1, time.Now().UnixNano(), extension))
}
