package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// S3 is a bucket on AWS S3 or an S3-compatible service (Cloudflare R2,
// Backblaze B2, MinIO).
type S3 struct {
	Endpoint  string // for example "https://<account>.r2.cloudflarestorage.com"; empty means AWS in Region
	Region    string // "us-east-1" if empty; R2 uses "auto"
	Bucket    string
	Prefix    string // key prefix, for example "runplane/"
	AccessKey string
	SecretKey string
}

// Upload stores the file at Prefix/<file name> with one signed PUT and
// returns the object key.
func (s S3) Upload(ctx context.Context, file string) (string, error) {
	if s.Bucket == "" || s.AccessKey == "" || s.SecretKey == "" {
		return "", errors.New("upload backup: S3 bucket, access key, and secret key are required")
	}
	key := path.Join(strings.Trim(s.Prefix, "/"), filepath.Base(file))
	u, err := s.objectURL(key)
	if err != nil {
		return "", err
	}

	f, err := os.Open(file)
	if err != nil {
		return "", fmt.Errorf("upload backup: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", fmt.Errorf("upload backup: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("upload backup: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), f)
	if err != nil {
		return "", fmt.Errorf("upload backup: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/gzip")
	sign(req, s.AccessKey, s.SecretKey, s.region(), hex.EncodeToString(h.Sum(nil)), time.Now())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload backup to %s: %w", u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upload backup to %s: %s", u.Host, s3Error(resp))
	}
	return key, nil
}

func (s S3) region() string {
	if s.Region == "" {
		return "us-east-1"
	}
	return s.Region
}

// objectURL returns the URL of the object key. Path and RawPath are both
// set, so the request sends the same escaped path that sign uses.
func (s S3) objectURL(key string) (*url.URL, error) {
	endpoint := s.Endpoint
	if endpoint == "" {
		endpoint = "https://s3." + s.region() + ".amazonaws.com"
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("parse S3 endpoint %q: invalid URL", s.Endpoint)
	}
	base := strings.TrimRight(u.Path, "/")
	isAWS := strings.HasSuffix(u.Hostname(), ".amazonaws.com")
	if isAWS && dnsBucket(s.Bucket) {
		u.Host = s.Bucket + "." + u.Host
		u.Path = base + "/" + key
	} else {
		u.Path = base + "/" + s.Bucket + "/" + key
	}
	u.RawPath = escapePath(u.Path)
	return u, nil
}

// dnsBucket reports whether the bucket name works as a host name label
// under a TLS wildcard certificate. Names with dots do not.
func dnsBucket(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func s3Error(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Code    string
		Message string
	}
	if xml.Unmarshal(body, &e) == nil && e.Code != "" {
		return fmt.Sprintf("%s: %s (%s)", resp.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s", resp.Status, bytes.TrimSpace(body))
}

// sign adds AWS Signature Version 4 headers to a request without a query
// string. It signs the host and every header that is set on req.
func sign(req *http.Request, accessKey, secretKey, region, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	day := amzDate[:8]
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	values := map[string]string{"host": host}
	for name, vs := range req.Header {
		name = strings.ToLower(name)
		if name == "authorization" {
			continue
		}
		trimmed := make([]string, len(vs))
		for i, v := range vs {
			trimmed[i] = strings.Join(strings.Fields(v), " ")
		}
		values[name] = strings.Join(trimmed, ",")
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var headers strings.Builder
	for _, name := range names {
		headers.WriteString(name + ":" + values[name] + "\n")
	}
	signed := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		escapePath(req.URL.Path),
		"", // query string
		headers.String(),
		signed,
		payloadHash,
	}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	canonicalHash := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(canonicalHash[:])

	key := hmacSHA256([]byte("AWS4"+secretKey), day)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, toSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
		",SignedHeaders="+signed+",Signature="+signature)
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// escapePath encodes each path segment the way S3 expects in a signature:
// all bytes except A-Z, a-z, 0-9, '-', '.', '_', '~', and '/' become %XX.
func escapePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~' || c == '/' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
