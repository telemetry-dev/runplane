package api

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/store"
)

type NotificationChannel struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind" enum:"slack,discord,webhook"`
	URL       string    `json:"url" doc:"The webhook URL with the secret part masked, for example \"https://hooks.slack.com/…a1b2\"."`
	Events    []string  `json:"events" doc:"Event types that the channel gets."`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	LastSentAt *time.Time `json:"last_sent_at" doc:"Time of the last delivery, also of a test message. Null before the first."`
	LastError  string     `json:"last_error" doc:"Why the last delivery failed. Empty if it worked."`
}

// channels converts store channels and adds their last delivery.
func (h *Handler) channels(ctx context.Context, list []store.NotificationChannel) ([]NotificationChannel, error) {
	deliveries, err := h.d.Store.NotificationDeliveries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NotificationChannel, len(list))
	for i, c := range list {
		masked := ""
		if plain, err := h.d.Box.Open(c.URLEnc); err == nil {
			masked = maskURL(string(plain))
		} else {
			h.d.Log.Warn("open notification URL", "channel", c.ID, "err", err)
		}
		d := deliveries[c.ID]
		out[i] = NotificationChannel{
			ID: c.ID, Name: c.Name, Kind: c.Kind, URL: masked, Events: orEmpty(c.Events), Enabled: c.Enabled,
			CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, LastSentAt: d.SentAt, LastError: d.Error,
		}
	}
	return out, nil
}

// maskURL keeps the scheme, the host, and the last 4 characters. The path of
// a webhook URL is its secret.
func maskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "…"
	}
	tail := strings.TrimRight(u.EscapedPath(), "/")
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	return u.Scheme + "://" + u.Host + "/…" + tail
}

func (h *Handler) listNotifications(ctx context.Context, _ *struct{}) (*body[[]NotificationChannel], error) {
	list, err := h.d.Store.ListNotificationChannels(ctx)
	if err != nil {
		return nil, err
	}
	return reply(h.channels(ctx, list))
}

type CreateNotificationRequest struct {
	Name    string   `json:"name" minLength:"1" maxLength:"64"`
	Kind    string   `json:"kind" enum:"slack,discord,webhook" doc:"slack and discord need an https URL. webhook gets a JSON POST."`
	URL     string   `json:"url" minLength:"1" maxLength:"2048" doc:"Webhook URL. Write-only: reads show it masked."`
	Events  []string `json:"events" maxItems:"16" enum:"job.failed,server.unreachable,disk.low,runner.deprecation,update.available,backup.failed"`
	Enabled *bool    `json:"enabled,omitempty" doc:"Defaults to true."`
}

func (h *Handler) createNotification(ctx context.Context, in *body[CreateNotificationRequest]) (*body[NotificationChannel], error) {
	b := in.Body
	name, rawURL := strings.TrimSpace(b.Name), strings.TrimSpace(b.URL)
	var errs problems
	if name == "" {
		errs.add("body.name", "Enter a name.", b.Name)
	}
	checkChannelURL(&errs, b.Kind, rawURL)
	if err := errs.err(); err != nil {
		return nil, err
	}
	now := time.Now()
	c := store.NotificationChannel{
		ID: ids.New(), Name: name, Kind: b.Kind, URLEnc: h.d.Box.Seal([]byte(rawURL)), Events: dedupe(b.Events),
		Enabled: b.Enabled == nil || *b.Enabled, CreatedAt: now, UpdatedAt: now,
	}
	if err := h.d.Store.CreateNotificationChannel(ctx, c); err != nil {
		return nil, err
	}
	h.audit(ctx, "notification.create", c.ID, map[string]any{"name": c.Name, "kind": c.Kind})
	return reply(first(h.channels(ctx, []store.NotificationChannel{c})))
}

type UpdateNotificationRequest struct {
	Name    *string   `json:"name,omitempty" minLength:"1" maxLength:"64"`
	Kind    *string   `json:"kind,omitempty" enum:"slack,discord,webhook"`
	URL     *string   `json:"url,omitempty" minLength:"1" maxLength:"2048" doc:"Absent: keep the URL."`
	Events  *[]string `json:"events,omitempty" maxItems:"16" enum:"job.failed,server.unreachable,disk.low,runner.deprecation,update.available,backup.failed"`
	Enabled *bool     `json:"enabled,omitempty"`
}

func (h *Handler) updateNotification(ctx context.Context, in *struct {
	PathID
	Body UpdateNotificationRequest
}) (*body[NotificationChannel], error) {
	c, err := h.d.Store.GetNotificationChannel(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No notification channel has this ID.")
	}
	b := in.Body
	var errs problems
	if b.Name != nil {
		if c.Name = strings.TrimSpace(*b.Name); c.Name == "" {
			errs.add("body.name", "Enter a name.", *b.Name)
		}
	}
	set(&c.Kind, b.Kind)
	set(&c.Enabled, b.Enabled)
	if b.Events != nil {
		c.Events = dedupe(*b.Events)
	}
	// The URL rules depend on the kind, so check the URL again when the
	// kind changes.
	rawURL := ""
	if b.URL != nil {
		rawURL = strings.TrimSpace(*b.URL)
	} else if plain, err := h.d.Box.Open(c.URLEnc); err == nil {
		rawURL = string(plain)
	}
	checkChannelURL(&errs, c.Kind, rawURL)
	if err := errs.err(); err != nil {
		return nil, err
	}
	if b.URL != nil {
		c.URLEnc = h.d.Box.Seal([]byte(rawURL))
	}
	if err := h.d.Store.UpdateNotificationChannel(ctx, c); err != nil {
		return nil, notFound(err, "No notification channel has this ID.")
	}
	h.audit(ctx, "notification.update", c.ID, map[string]any{"name": c.Name, "kind": c.Kind, "url_changed": b.URL != nil})
	return reply(first(h.channels(ctx, []store.NotificationChannel{c})))
}

func (h *Handler) deleteNotification(ctx context.Context, in *PathID) (*empty, error) {
	if err := h.d.Store.DeleteNotificationChannel(ctx, in.ID); err != nil {
		return nil, notFound(err, "No notification channel has this ID.")
	}
	h.audit(ctx, "notification.delete", in.ID, nil)
	return nil, nil
}

func (h *Handler) testNotification(ctx context.Context, in *PathID) (*empty, error) {
	if h.d.Notifier == nil {
		return nil, unavailable("Notifications")
	}
	c, err := h.d.Store.GetNotificationChannel(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No notification channel has this ID.")
	}
	plain, err := h.d.Box.Open(c.URLEnc)
	if err != nil {
		return nil, err
	}
	base, err := h.baseURL(ctx)
	if err != nil {
		return nil, err
	}
	sendErr := h.d.Notifier.Test(ctx, base, c.Kind, string(plain))
	msg := ""
	if sendErr != nil {
		msg = sendErr.Error()
	}
	if err := h.d.Store.RecordNotificationDelivery(ctx, c.ID, time.Now(), msg); err != nil {
		h.d.Log.Warn("record notification delivery", "channel", c.ID, "err", err)
	}
	if sendErr != nil {
		return nil, h.serviceErr(sendErr)
	}
	return nil, nil
}

type TestChannelRequest struct {
	Kind string `json:"kind" enum:"slack,discord,webhook"`
	URL  string `json:"url" minLength:"1" maxLength:"2048"`
}

func (h *Handler) testChannel(ctx context.Context, in *body[TestChannelRequest]) (*empty, error) {
	rawURL := strings.TrimSpace(in.Body.URL)
	var errs problems
	checkChannelURL(&errs, in.Body.Kind, rawURL)
	if err := errs.err(); err != nil {
		return nil, err
	}
	if h.d.Notifier == nil {
		return nil, unavailable("Notifications")
	}
	base, err := h.baseURL(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.d.Notifier.Test(ctx, base, in.Body.Kind, rawURL); err != nil {
		return nil, h.serviceErr(err)
	}
	return nil, nil
}

func checkChannelURL(errs *problems, kind, raw string) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		errs.add("body.url", "Enter an http or https URL.", nil)
	case kind != "webhook" && u.Scheme != "https":
		errs.add("body.url", "A "+kind+" webhook URL starts with https://.", nil)
	}
}

func dedupe(list []string) []string {
	out := []string{}
	for _, v := range list {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
