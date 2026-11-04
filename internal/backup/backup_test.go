package backup

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
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
		{"MinIO without scheme", S3{Endpoint: "minio.lan:9000/", Bucket: "rp"}, "a.gz", "https://minio.lan:9000/rp/a.gz"},
		{"forced virtual host", S3{Endpoint: "https://s3.eu-central-003.backblazeb2.com", Bucket: "rp", VirtualHost: true}, "a.gz", "https://rp.s3.eu-central-003.backblazeb2.com/a.gz"},
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
		method, path, host, contentHash, auth string
		body                                  []byte
	}
	got := make(chan request, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- request{r.Method, r.URL.EscapedPath(), r.Host, r.Header.Get("X-Amz-Content-Sha256"), r.Header.Get("Authorization"), body}
	}))
	defer srv.Close()

	s3 := S3{Endpoint: srv.URL, Region: "auto", Bucket: "rp", Prefix: "/nightly/", AccessKey: "AK", SecretKey: "SK"}
	key, err := s3.Upload(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
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
	day := time.Now().UTC().Format("20060102")
	if !strings.HasPrefix(r.auth, "AWS4-HMAC-SHA256 Credential=AK/"+day+"/auto/s3/aws4_request,SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date,") {
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

func TestRun(t *testing.T) {
	dir := t.TempDir()
	// No automatic checkpoint: the rows stay in the WAL file, so a plain
	// copy of runplane.db would not have them.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "runplane.db")+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE jobs(id TEXT PRIMARY KEY, repository TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"j1", "j2", "j3"} {
		if _, err := db.Exec(`INSERT INTO jobs VALUES (?, 'acme/api')`, id); err != nil {
			t.Fatal(err)
		}
	}

	backups := filepath.Join(dir, "backups")
	res, err := Run(context.Background(), db, backups, 7)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != res.Size {
		t.Fatalf("size %d, file has %d", res.Size, info.Size())
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("backup mode %o, want 600: it holds password hashes", mode)
	}
	if !strings.HasPrefix(filepath.Base(res.Path), "runplane-") || !strings.HasSuffix(res.Path, "Z.db.gz") {
		t.Fatalf("backup name %s", res.Path)
	}
	entries, _ := os.ReadDir(backups)
	if len(entries) != 1 {
		t.Fatalf("backup dir has %d files, want only the backup", len(entries))
	}

	restored := gunzip(t, res.Path)
	rdb, err := sql.Open("sqlite", "file:"+restored+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	var ids []string
	rows, err := rdb.Query(`SELECT id FROM jobs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	if !slices.Equal(ids, []string{"j1", "j2", "j3"}) {
		t.Fatalf("restored jobs %v", ids)
	}
}

func TestRunKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	old := []string{
		"runplane-20250101T030000.000Z.db.gz",
		"runplane-20250102T030000.000Z.db.gz",
		"runplane-20250103T030000.000Z.db.gz",
		"runplane-20250104T030000.000Z.db.gz",
	}
	others := []string{"runplane-before-upgrade.db.gz", "notes.txt"}
	for _, name := range append(slices.Clone(old), others...) {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	res, err := Run(context.Background(), db, dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	wantRemoved := []string{filepath.Join(dir, old[0]), filepath.Join(dir, old[1])}
	if !slices.Equal(res.Removed, wantRemoved) {
		t.Fatalf("removed %v, want %v", res.Removed, wantRemoved)
	}
	var left []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	want := []string{"notes.txt", old[2], old[3], "runplane-before-upgrade.db.gz", filepath.Base(res.Path)}
	slices.Sort(want)
	if !slices.Equal(left, want) {
		t.Fatalf("files %v, want %v", left, want)
	}
}

// Two backups in one millisecond, for example two clicks, must not write
// the same file.
func TestFreeName(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)
	first, _ := freeName(dir, at)
	if first != "runplane-20261104T030000.000Z.db.gz" {
		t.Fatalf("name %q", first)
	}
	os.WriteFile(filepath.Join(dir, first), []byte("x"), 0o600)
	if next, _ := freeName(dir, at); next != "runplane-20261104T030000.001Z.db.gz" {
		t.Fatalf("name after %s: %q, want the next millisecond", first, next)
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

func gunzip(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "restored.db")
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(out, data, 0o600)
	return out
}
