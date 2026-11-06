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
	"strings"
	"time"
)

// Channel kinds.
const (
	Slack   = "slack"   // Slack incoming webhook
	Discord = "discord" // Discord webhook
	Webhook = "webhook" // generic JSON POST
)

// Retry and timeout values. Tests make them short.
var (
	attemptTimeout = 10 * time.Second
	retryDelays    = []time.Duration{time.Second, 3 * time.Second}
)

// Send posts the event to the channel of kind at target. Each attempt has a
// 10-second timeout. Send tries again two times when the receiver answers
// with a 5xx status, 429, or a network error.
func Send(ctx context.Context, kind, target string, ev Event) error {
	if err := validate(kind, target); err != nil {
		return err
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	body, err := json.Marshal(payload(kind, ev))
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", kind, err)
	}

	for attempt := 0; ; attempt++ {
		err = post(ctx, target, body)
		var perm permanentError
		if err == nil || errors.As(err, &perm) || attempt == len(retryDelays) {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("send %s notification: %w", kind, ctx.Err())
		case <-time.After(retryDelays[attempt]):
		}
	}
	if err != nil {
		return fmt.Errorf("send %s notification: %w", kind, err)
	}
	return nil
}

func validate(kind, target string) error {
	switch kind {
	case Slack, Discord, Webhook:
	default:
		return fmt.Errorf("unknown channel kind %q", kind)
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("channel URL must be an http or https URL")
	}
	if kind != Webhook && u.Scheme != "https" {
		return fmt.Errorf("%s webhook URL must use https", kind)
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

type slackMessage struct {
	Text        string `json:"text"`
	UnfurlLinks bool   `json:"unfurl_links"`
	UnfurlMedia bool   `json:"unfurl_media"`
}

type discordMessage struct {
	Embeds          []discordEmbed `json:"embeds"`
	AllowedMentions struct {
		Parse []string `json:"parse"`
	} `json:"allowed_mentions"`
}

type discordEmbed struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url,omitempty"`
	Color       int    `json:"color"`
	Timestamp   string `json:"timestamp"`
}

type webhookMessage struct {
	Event Type           `json:"event"`
	Title string         `json:"title"`
	Text  string         `json:"text"`
	URL   string         `json:"url"`
	Data  map[string]any `json:"data"`
	At    string         `json:"at"`
}

func payload(kind string, ev Event) any {
	at := ev.At.UTC().Format(time.RFC3339)
	switch kind {
	case Slack:
		title := slackEscape(ev.Title)
		if ev.URL != "" {
			title = "<" + ev.URL + "|" + title + ">"
		}
		// No unfurl: the dashboard is private, and a preview shows only the login page.
		return slackMessage{Text: "*" + title + "*\n" + slackEscape(ev.Text)}
	case Discord:
		m := discordMessage{Embeds: []discordEmbed{{
			// Discord rejects embeds with longer fields.
			Title:       truncate(ev.Title, 256),
			Description: truncate(ev.Text, 4096),
			URL:         ev.URL,
			Color:       color(ev.Type),
			Timestamp:   at,
		}}}
		// An empty list turns off @everyone and other mentions in job names.
		m.AllowedMentions.Parse = []string{}
		return m
	default:
		data := ev.Data
		if data == nil {
			data = map[string]any{}
		}
		return webhookMessage{Event: ev.Type, Title: ev.Title, Text: ev.Text, URL: ev.URL, Data: data, At: at}
	}
}

// slackEscape escapes the three characters that Slack reads as markup. It
// also stops text such as "<!channel>" in a job name from notifying people.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func color(t Type) int {
	switch t {
	case JobFailed, ServerUnreachable, BackupFailed:
		return 0xd92d20 // red
	case DiskLow, RunnerDeprecation:
		return 0xdc6803 // amber
	default:
		return 0x2e90fa // blue
	}
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
