package control

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox/guest"
)

// bigText makes lines of about 64 KiB, so that 1025 lines pass the 64 MiB cap.
var bigText = strings.Repeat("x", 64<<10)

func writeLines(t *testing.T, l *LogFile, from, to int) {
	t.Helper()
	at := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	for i := from; i < to; i++ {
		if err := l.Write(guest.LogLine{Time: at, Source: "runner", Text: fmt.Sprintf("%05d %s", i, bigText)}); err != nil {
			t.Fatal(err)
		}
	}
}

// checkLines reads all lines of r and checks that they are lines from..to-1 in order.
func checkLines(r io.Reader, from, to int) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	want := from
	for sc.Scan() {
		var n int
		if _, err := fmt.Sscanf(sc.Text(), "2026-11-04T10:00:00.000Z runner %05d", &n); err != nil {
			return fmt.Errorf("line %d: %v: %.60q", want, err, sc.Text())
		}
		if n != want {
			return fmt.Errorf("got line %d, want line %d", n, want)
		}
		want++
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if want != to {
		return fmt.Errorf("read lines %d to %d, want up to %d", from, want-1, to-1)
	}
	return nil
}

func TestLogFileRotates(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLogFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeLines(t, l, 0, 1100)
	for _, name := range []string{"console.log", "console.log.1"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() > 64<<20 {
			t.Errorf("%s has %d bytes, more than the 64 MiB cap", name, st.Size())
		}
	}
	r := l.Reader(context.Background(), false)
	if err := checkLines(r, 0, 1100); err != nil {
		t.Fatal(err)
	}
	r.Close()

	// The follower must see every line, also across a second rotation, and
	// stop at the end when the log closes.
	follower := l.Reader(context.Background(), true)
	defer follower.Close()
	done := make(chan error)
	go func() { done <- checkLines(follower, 0, 2200) }()
	writeLines(t, l, 1100, 2200)

	// The second rotation drops the oldest part.
	r = l.Reader(context.Background(), false)
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	sc.Scan()
	var first int
	fmt.Sscanf(sc.Text(), "2026-11-04T10:00:00.000Z runner %05d", &first)
	if first == 0 {
		t.Errorf("the oldest part is still there after a second rotation")
	}

	l.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the follower did not stop after Close")
	}
}

func TestLogFileFollowEnds(t *testing.T) {
	l, err := OpenLogFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	byCtx := l.Reader(ctx, true)
	byClose := l.Reader(context.Background(), true)
	results := make(chan error, 2)
	for _, r := range []io.Reader{byCtx, byClose} {
		go func() {
			_, err := io.ReadAll(r)
			results <- err
		}()
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	byClose.Close()
	for range 2 {
		select {
		case <-results:
		case <-time.After(5 * time.Second):
			t.Fatal("a follower kept waiting after its context ended or it closed")
		}
	}
}
