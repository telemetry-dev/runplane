// Package notify keeps the notification channels (Slack, Discord, and
// generic webhooks) and sends the runplane alerts to them.
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
	"slices"
	"strings"
	"time"
)

// Channel kinds.
const (
	Slack   = "slack"   // Slack incoming webhook
	Discord = "discord" // Discord webhook
	Webhook = "webhook" // generic JSON POST
)

var Kinds = []string{Slack, Discord, Webhook}

// Retry and timeout values. Tests make them short.
var (
	attemptTimeout = 10 * time.Second
	retryDelays    = []time.Duration{time.Second, 3 * time.Second}
)

// send posts the event to the channel of kind at target. The link of the
// event goes to the dashboard at the base URL dashboard, and an empty
// dashboard gives no link. Each attempt has a 10-second timeout. send tries
// again two times when the receiver answers with a 5xx status, 429, or a
// network error.
func send(ctx context.Context, kind, target, dashboard string, ev Event) error {
	if !slices.Contains(Kinds, kind) {
		return fmt.Errorf("unknown channel kind %q", kind)
	}
	if err := checkURL(kind, target); err != nil {
		return err
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	body, err := json.Marshal(payload(kind, ev, ev.link(dashboard)))
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

// checkURL applies the rules of the URL of a channel of kind to target.
func checkURL(kind, target string) error {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("enter an http or https URL")
	}
	if kind != Webhook && u.Scheme != "https" {
		return fmt.Errorf("a %s webhook URL starts with https://", kind)
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

// payload is the message of ev for a channel of kind, with the link to the
// dashboard page of the event.
func payload(kind string, ev Event, link string) any {
	at := ev.At.UTC().Format(time.RFC3339)
	switch kind {
	case Slack:
		title := slackEscape(ev.Title)
		if link != "" {
			title = "<" + link + "|" + title + ">"
		}
		// No unfurl: the dashboard is private, and a preview shows only the login page.
		return slackMessage{Text: "*" + title + "*\n" + slackEscape(ev.Text)}
	case Discord:
		m := discordMessage{Embeds: []discordEmbed{{
			// Discord rejects embeds with longer fields.
			Title:       truncate(ev.Title, 256),
			Description: truncate(ev.Text, 4096),
			URL:         link,
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
		return webhookMessage{Event: ev.Type, Title: ev.Title, Text: ev.Text, URL: link, Data: data, At: at}
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
