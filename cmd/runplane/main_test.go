package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

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
