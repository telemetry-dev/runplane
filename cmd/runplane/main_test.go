package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/store"
)

func TestLogSetupCodeKeepsAValidCode(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	a := auth.NewService(st, nil)
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))

	// install.sh prints a code, then runplane restarts.
	printed, err := a.NewSetupCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := logSetupCode(ctx, st, a, log); err != nil {
		t.Fatal(err)
	}
	if loggedCode(out.String()) != "" {
		t.Errorf("serve made a new code while one was valid:\n%s", out.String())
	}
	if _, _, err := a.Setup(ctx, printed, "Ada", "ada@example.com", "a long password", "127.0.0.1", "test"); err != nil {
		t.Fatalf("the printed code stopped working after a restart: %v", err)
	}
}

func TestLogSetupCodeReplacesAnExpiredCode(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	a := auth.NewService(st, nil)
	old := time.Now().Add(-48 * time.Hour)
	if err := st.CreateSetupCode(ctx, store.SetupCode{CodeHash: "expired", CreatedAt: old, ExpiresAt: old.Add(auth.SetupCodeTTL)}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := logSetupCode(ctx, st, a, slog.New(slog.NewTextHandler(&out, nil))); err != nil {
		t.Fatal(err)
	}
	code := loggedCode(out.String())
	if code == "" {
		t.Fatalf("no new code in the log:\n%s", out.String())
	}
	if _, _, err := a.Setup(ctx, code, "Ada", "ada@example.com", "a long password", "127.0.0.1", "test"); err != nil {
		t.Fatalf("the logged code does not work: %v", err)
	}
}

func loggedCode(log string) string {
	m := regexp.MustCompile(`code=(\S+)`).FindStringSubmatch(log)
	if m == nil {
		return ""
	}
	return m[1]
}

// At a stop, every loop reports its canceled context. Those lines must go,
// and the other lines must stay.
func TestLoggerDropsCanceledContextsAtStop(t *testing.T) {
	var out bytes.Buffer
	ctx, stop := context.WithCancel(context.Background())
	log := slog.New(quietHandler{slog.NewTextHandler(&out, nil), ctx.Done()}).With("component", "scheduler")

	log.Warn("before the stop", "err", fmt.Errorf("list sandboxes: %w", context.Canceled))
	stop()
	log.Warn("canceled at the stop", "err", fmt.Errorf("resize scale set: %w", context.Canceled))
	log.Error("failure at the stop", "err", errors.New("disk full"))

	got := out.String()
	for _, want := range []string{"before the stop", "failure at the stop"} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "canceled at the stop") {
		t.Errorf("log has a canceled context after the stop:\n%s", got)
	}
}
