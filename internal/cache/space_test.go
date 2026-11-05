package cache

import (
	"bytes"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// dirBytes is the size of all files under dir.
func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err == nil {
			n += info.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Unfinished uploads count against the limit, so a job cannot fill the disk
// with many uploads that it never finalizes.
func TestUnfinishedUploadsShareTheLimit(t *testing.T) {
	f := newFixture(t, 100)
	c := client{t: t, url: f.url}
	var created, refused int
	for i := range 20 {
		u := c.reserve(fmt.Sprintf("junk-%d", i), version)
		switch status := c.put(u+"&comp=block&blockid="+url.QueryEscape(blockID(0)), bytes.Repeat([]byte("x"), 60), nil); status {
		case http.StatusCreated:
			created++
		case http.StatusRequestEntityTooLarge:
			refused++
		default:
			t.Fatalf("Put Block %d: %d", i, status)
		}
	}
	if created != 1 || refused != 19 {
		t.Errorf("%d blocks stored and %d refused, want 1 and 19", created, refused)
	}
	if n := dirBytes(t, f.dir); n > 100 {
		t.Errorf("%d bytes in the cache folder, want at most the limit of 100", n)
	}

	// A committed blob of an unfinished entry counts too.
	u := c.reserve("blob", version)
	if status := c.put(u, bytes.Repeat([]byte("x"), 50), nil); status != http.StatusRequestEntityTooLarge {
		t.Errorf("Put Blob past the room left: %d, want 413", status)
	}
}

func TestOpenUploadsPerSandbox(t *testing.T) {
	f := newFixture(t, 0)
	c := client{t: t, url: f.url}
	for i := range maxOpenUploads {
		c.reserve(fmt.Sprintf("layer-%d", i), version)
	}
	status, out := c.call("CreateCacheEntry", map[string]any{"key": "one-more", "version": version})
	if status != http.StatusOK || out["ok"] == true {
		t.Errorf("CreateCacheEntry past the open uploads: %d %v, want a denial", status, out)
	}
	// Another sandbox is not affected, and a finalized upload frees a place.
	f.via(t, "sb-feature").reserve("feature", version)
	c.finalize("layer-0", version, 0)
	c.reserve("one-more", version)
}

// The cache keeps the disk above min_free_disk_percent: it takes no upload
// below it and removes old entries until the disk has a margin.
func TestCacheLeavesFreeDisk(t *testing.T) {
	f := newFixture(t, 0)
	var used atomic.Int64 // bytes of other programs on a 10 000-byte disk
	f.srv.cfg.DiskSpace = func() (int64, int64, error) {
		return 10_000, 10_000 - used.Load() - dirBytes(t, f.dir), nil
	}
	f.srv.cfg.MinFreePercent = func() float64 { return 15 }
	c := client{t: t, url: f.url}
	c.save("a", version, bytes.Repeat([]byte("a"), 1000))
	c.save("b", version, bytes.Repeat([]byte("b"), 1000))

	// Other programs fill the disk to 13% free: a must go, b can stay.
	used.Store(6_700)
	f.srv.cleanup(t.Context())
	for key, want := range map[string]bool{"a": false, "b": true} {
		if matched, _ := c.lookup(version, key); (matched != "") != want {
			t.Errorf("entry %q kept = %v, want %v", key, matched != "", want)
		}
	}

	// No entry can free enough.
	used.Store(9_000)
	u := c.reserve("c", version)
	if status := c.put(u, []byte("c"), nil); status != http.StatusRequestEntityTooLarge {
		t.Errorf("upload with the disk below 15%% free: %d, want 413", status)
	}
}

// With a small disk, the limit is a quarter of the disk, not cache_max_gb.
func TestLimitIsAQuarterOfTheDiskAtMost(t *testing.T) {
	f := newFixture(t, 50<<30)
	f.srv.cfg.DiskSpace = func() (int64, int64, error) { return 400, 400, nil }
	c := client{t: t, url: f.url}
	u := c.reserve("big", version)
	if status := c.put(u, bytes.Repeat([]byte("x"), 101), nil); status != http.StatusRequestEntityTooLarge {
		t.Errorf("101 bytes on a 400-byte disk: %d, want 413", status)
	}
	if status := c.put(u, bytes.Repeat([]byte("x"), 100), nil); status != http.StatusCreated {
		t.Errorf("100 bytes on a 400-byte disk: %d, want 201", status)
	}
}
