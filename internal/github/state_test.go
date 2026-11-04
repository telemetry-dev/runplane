package github

import (
	"context"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/events"
)

// gitHubStates counts the github.state events on ch so far.
func gitHubStates(ch <-chan events.Event) int {
	n := 0
	for {
		select {
		case e := <-ch:
			if e.Type == events.GitHubState {
				n++
			}
		default:
			return n
		}
	}
}

func TestPollEventsAtMostEach30Seconds(t *testing.T) {
	e := newEnv(t)
	ch, cancel := e.bus.Subscribe(16)
	defer cancel()
	t0 := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	for _, step := range []struct {
		at   time.Duration
		want int
	}{
		{0, 1},
		{10 * time.Second, 0},
		{29 * time.Second, 0},
		{31 * time.Second, 1},
		{40 * time.Second, 0},
	} {
		e.svc.polledChanged(t0.Add(step.at))
		if got := gitHubStates(ch); got != step.want {
			t.Errorf("poll at +%s: %d github.state events, want %d", step.at, got, step.want)
		}
	}
}

func TestDisconnectPublishesGitHubState(t *testing.T) {
	e := newEnv(t)
	e.connect()
	ch, cancel := e.bus.Subscribe(16)
	defer cancel()
	if err := e.svc.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gitHubStates(ch) == 0 {
		t.Fatal("no github.state event after the app was removed")
	}
}
