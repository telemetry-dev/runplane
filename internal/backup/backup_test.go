package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The values come from the AWS documentation, "Signature Calculations for
// the Authorization Header: Transferring Payload in a Single Chunk".
func TestSignAWSExamples(t *testing.T) {
	const (
		accessKey = "AKIAIOSFODNN7EXAMPLE"
		secretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	)
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		method      string
		url         string
		headers     map[string]string
		payloadHash string
		want        string
	}{
		{
			name:        "GET Object",
			method:      "GET",
			url:         "https://examplebucket.s3.amazonaws.com/test.txt",
			headers:     map[string]string{"Range": "bytes=0-9"},
			payloadHash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			want:        "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=host;range;x-amz-content-sha256;x-amz-date,Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name:   "PUT Object",
			method: "PUT",
			url:    "https://examplebucket.s3.amazonaws.com/test$file.text",
			headers: map[string]string{
				"Date":                "Fri, 24 May 2013 00:00:00 GMT",
				"x-amz-storage-class": "REDUCED_REDUNDANCY",
			},
			payloadHash: "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			want:        "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class,Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, tt.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			sign(req, accessKey, secretKey, "us-east-1", tt.payloadHash, at)
			if got := req.Header.Get("Authorization"); got != tt.want {
				t.Fatalf("Authorization:\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestObjectURL(t *testing.T) {
	tests := []struct {
		name string
		s3   S3
		key  string
		want string
	}{
		{"AWS default endpoint", S3{Region: "eu-central-1", Bucket: "rp-backups"}, "db/a.gz", "https://rp-backups.s3.eu-central-1.amazonaws.com/db/a.gz"},
		{"AWS bucket with dots uses path style", S3{Bucket: "backups.example.com"}, "a.gz", "https://s3.us-east-1.amazonaws.com/backups.example.com/a.gz"},
		{"R2", S3{Endpoint: "https://acc123.r2.cloudflarestorage.com", Region: "auto", Bucket: "rp"}, "a.gz", "https://acc123.r2.cloudflarestorage.com/rp/a.gz"},
		{"MinIO with a final slash", S3{Endpoint: "https://minio.lan:9000/", Bucket: "rp"}, "a.gz", "https://minio.lan:9000/rp/a.gz"},
		{"key with special characters", S3{Endpoint: "http://127.0.0.1:9000", Bucket: "rp"}, "my backups/a+b$.gz", "http://127.0.0.1:9000/rp/my%20backups/a%2Bb%24.gz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := tt.s3.objectURL(tt.key)
			if err != nil {
				t.Fatal(err)
			}
			if got := u.String(); got != tt.want {
				t.Fatalf("URL %s, want %s", got, tt.want)
			}
		})
	}
}

func TestUpload(t *testing.T) {
	file := filepath.Join(t.TempDir(), "runplane-20261104T030000Z.db.gz")
	content := []byte("gzip bytes of the database")
	os.WriteFile(file, content, 0o600)

	type request struct {
		method, path, host, contentHash, date, auth string
		body                                        []byte
	}
	got := make(chan request, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- request{r.Method, r.URL.EscapedPath(), r.Host, r.Header.Get("X-Amz-Content-Sha256"),
			r.Header.Get("X-Amz-Date"), r.Header.Get("Authorization"), body}
	}))
	defer srv.Close()

	s3 := S3{Endpoint: srv.URL, Region: "auto", Bucket: "rp", Prefix: "/nightly/", AccessKey: "AK", SecretKey: "SK"}
	before := time.Now().UTC().Truncate(time.Second)
	key, err := s3.Upload(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	if key != "nightly/runplane-20261104T030000Z.db.gz" {
		t.Fatalf("key %q", key)
	}
	r := <-got
	if r.method != "PUT" || r.path != "/rp/nightly/runplane-20261104T030000Z.db.gz" {
		t.Fatalf("request %s %s", r.method, r.path)
	}
	if string(r.body) != string(content) {
		t.Fatalf("body %q", r.body)
	}
	sum := sha256.Sum256(content)
	if r.contentHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("x-amz-content-sha256 %s does not match the body", r.contentHash)
	}
	// The scope of the signature has the day of X-Amz-Date, also at midnight.
	at, err := time.Parse("20060102T150405Z", r.date)
	if err != nil || at.Before(before) || at.After(after) {
		t.Fatalf("X-Amz-Date %q, want a time between %s and %s", r.date, before, after)
	}
	if !strings.HasPrefix(r.auth, "AWS4-HMAC-SHA256 Credential=AK/"+at.Format("20060102")+"/auto/s3/aws4_request,SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date,") {
		t.Fatalf("Authorization %q", r.auth)
	}
}

func TestUploadError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "b.db.gz")
	os.WriteFile(file, []byte("x"), 0o600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>SignatureDoesNotMatch</Code><Message>The request signature we calculated does not match.</Message></Error>`)
	}))
	defer srv.Close()

	_, err := S3{Endpoint: srv.URL, Bucket: "rp", AccessKey: "AK", SecretKey: "SK"}.Upload(context.Background(), file)
	if err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Fatalf("err = %v, want the S3 error code", err)
	}
}

// Two backups in one millisecond, for example two clicks, must not write
// the same file. The second name sorts after the first, because the
// pruning keeps the names that sort last.
func TestFreeName(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)
	first, path := freeName(dir, at)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if next, _ := freeName(dir, at); next <= first {
		t.Fatalf("name after %s: %q, want a name that sorts after it", first, next)
	}
}

func TestNextRun(t *testing.T) {
	berlin := time.FixedZone("CET", 3600)
	tests := []struct {
		now  time.Time
		want time.Time
	}{
		{time.Date(2026, 11, 4, 2, 59, 59, 0, time.UTC), time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)},
		{time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC), time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 31, 18, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 3, 0, 0, 0, time.UTC)},
		{time.Date(2026, 11, 4, 3, 30, 0, 0, berlin), time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)}, // 02:30 UTC
	}
	for _, tt := range tests {
		if got := nextRun(tt.now); !got.Equal(tt.want) {
			t.Errorf("nextRun(%v) = %v, want %v", tt.now, got, tt.want)
		}
	}
}
