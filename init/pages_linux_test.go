//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

func TestTailPages(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var lines []string
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(lines)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- tailPages(ctx, dir, func(line string) {
			mu.Lock()
			lines = append(lines, line)
			mu.Unlock()
		})
	}()
	create := func(name, text string) *os.File {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		f.WriteString(text)
		return f
	}

	// tailPages reads the first page after it watches the folder.
	create("t1_r1_1.log", "warm\n")
	waitFor(t, "the first page", func() bool { return count() == 1 })

	// The runner deletes a page after the upload. The page starts with a
	// byte order mark, and its last text has no line end.
	page := create("t1_r2_1.log", "\xef\xbb\xbfline 1\nline 2\r\npart")
	waitFor(t, "the lines of the second page", func() bool { return count() == 3 })
	page.WriteString("ial")
	os.Remove(page.Name())
	waitFor(t, "the rest of the deleted page", func() bool { return count() == 4 })

	// A page that is still open when the runner exits.
	create("t1_r3_1.log", "last line\nlast")
	waitFor(t, "the third page", func() bool { return count() == 5 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	want := []string{"warm", "line 1", "line 2", "partial", "last line", "last"}
	if !slices.Equal(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
}
