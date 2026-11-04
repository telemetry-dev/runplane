package notify

import (
	"strings"
	"time"
)

// payload returns the JSON body for the channel kind.
func payload(kind Kind, ev Event) any {
	switch kind {
	case Slack:
		return slackPayload(ev)
	case Discord:
		return discordPayload(ev)
	default:
		return webhookPayload(ev)
	}
}

type slackMessage struct {
	Text        string `json:"text"`
	UnfurlLinks bool   `json:"unfurl_links"`
	UnfurlMedia bool   `json:"unfurl_media"`
}

func slackPayload(ev Event) slackMessage {
	title := slackEscape(ev.Title)
	if ev.URL != "" {
		title = "<" + ev.URL + "|" + title + ">"
	}
	// No unfurl: the dashboard is private, and a preview shows only the login page.
	return slackMessage{Text: "*" + title + "*\n" + slackEscape(ev.Text)}
}

// slackEscape escapes the three characters that Slack reads as markup. It
// also stops text such as "<!channel>" in a job name from notifying people.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
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

// Discord rejects embeds with longer fields.
const (
	discordTitleMax       = 256
	discordDescriptionMax = 4096
)

func discordPayload(ev Event) discordMessage {
	m := discordMessage{Embeds: []discordEmbed{{
		Title:       truncate(ev.Title, discordTitleMax),
		Description: truncate(ev.Text, discordDescriptionMax),
		URL:         ev.URL,
		Color:       color(ev.Type),
		Timestamp:   ev.At.UTC().Format(time.RFC3339),
	}}}
	// An empty list turns off @everyone and other mentions in job names.
	m.AllowedMentions.Parse = []string{}
	return m
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

type webhookMessage struct {
	Event Type           `json:"event"`
	Title string         `json:"title"`
	Text  string         `json:"text"`
	URL   string         `json:"url"`
	Data  map[string]any `json:"data"`
	At    string         `json:"at"`
}

func webhookPayload(ev Event) webhookMessage {
	data := ev.Data
	if data == nil {
		data = map[string]any{}
	}
	return webhookMessage{
		Event: ev.Type,
		Title: ev.Title,
		Text:  ev.Text,
		URL:   ev.URL,
		Data:  data,
		At:    ev.At.UTC().Format(time.RFC3339),
	}
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
