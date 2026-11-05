// Package blob holds the sha256 digest form and a file serve that counts
// the bytes. The Actions cache, the registry mirror, and the images use
// them for the content that they keep on disk.
package blob

import (
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"time"
)

const digestPrefix = "sha256:"

// Digest returns the digest of a SHA-256 sum: "sha256:<hex>".
func Digest(sum []byte) string {
	return digestPrefix + hex.EncodeToString(sum)
}

// ValidDigest reports if d is "sha256:" and 64 hex digits in lower case.
func ValidDigest(d string) bool {
	h, ok := strings.CutPrefix(d, digestPrefix)
	if !ok || len(h) != 64 {
		return false
	}
	for _, c := range h {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// Serve answers r with content as http.ServeContent does, and returns the
// count of body bytes that it sent.
func Serve(w http.ResponseWriter, r *http.Request, modtime time.Time, content io.ReadSeeker) int64 {
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, "", modtime, content)
	return cw.n
}

type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

// ReadFrom keeps the sendfile path of the ResponseWriter.
func (c *countingWriter) ReadFrom(r io.Reader) (int64, error) {
	n, err := io.Copy(c.ResponseWriter, r)
	c.n += n
	return n, err
}
