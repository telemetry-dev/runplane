// Package notify sends runplane events to Slack, Discord, and generic
// webhook channels.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Kind is the type of a notification channel.
type Kind string

const (
	Slack   Kind = "slack"   // Slack incoming webhook
	Discord Kind = "discord" // Discord webhook
	Webhook Kind = "webhook" // generic JSON POST
)

// Channel is a destination for events.
type Channel struct {
	Kind Kind
	URL  string
}

// Validate reports whether the channel can be used.
func (c Channel) Validate() error {
	switch c.Kind {
	case Slack, Discord, Webhook:
	default:
		return fmt.Errorf("unknown channel kind %q", c.Kind)
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("channel URL must be an http or https URL")
	}
	if c.Kind != Webhook && u.Scheme != "https" {
		return fmt.Errorf("%s webhook URL must use https", c.Kind)
	}
	return nil
}

// Retry and timeout values. Tests make them short.
var (
	attemptTimeout = 10 * time.Second
	retryDelays    = []time.Duration{time.Second, 3 * time.Second}
)

// Send posts the event to the channel. Each attempt has a 10-second timeout.
// Send tries again two times when the receiver answers with a 5xx status,
// 429, or a network error.
func Send(ctx context.Context, ch Channel, ev Event) error {
	if err := ch.Validate(); err != nil {
		return err
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	body, err := json.Marshal(payload(ch.Kind, ev))
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", ch.Kind, err)
	}

	for attempt := 0; ; attempt++ {
		err = post(ctx, ch.URL, body)
		var perm permanentError
		if err == nil || errors.As(err, &perm) || attempt == len(retryDelays) {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("send %s notification: %w", ch.Kind, ctx.Err())
		case <-time.After(retryDelays[attempt]):
		}
	}
	if err != nil {
		return fmt.Errorf("send %s notification: %w", ch.Kind, err)
	}
	return nil
}

// permanentError is a receiver answer that another attempt cannot fix.
type permanentError struct{ error }

func post(ctx context.Context, target string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return permanentError{err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "runplane")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 300 {
		return nil
	}
	err = fmt.Errorf("receiver answered %s: %s", resp.Status, bytes.TrimSpace(msg))
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return err
	}
	return permanentError{err}
}
