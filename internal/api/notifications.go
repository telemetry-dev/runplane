package api

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/notify"
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

func channelOut(c notify.Channel) NotificationChannel {
	return NotificationChannel{
		ID: c.ID, Name: c.Name, Kind: c.Kind, URL: c.URL, Events: orEmpty(c.Events), Enabled: c.Enabled,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, LastSentAt: c.Last.SentAt, LastError: c.Last.Error,
	}
}

func (h *Handler) listNotifications(ctx context.Context, _ *struct{}) (*body[[]NotificationChannel], error) {
	list, err := h.d.Channels.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NotificationChannel, len(list))
	for i, c := range list {
		out[i] = channelOut(c)
	}
	return &body[[]NotificationChannel]{out}, nil
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
	c, err := h.d.Channels.Create(ctx, actor(ctx), notify.ChannelChange{
		Name: &b.Name, Kind: &b.Kind, URL: &b.URL, Events: &b.Events, Enabled: b.Enabled,
	})
	if err != nil {
		return nil, err
	}
	return &body[NotificationChannel]{channelOut(c)}, nil
}

// UpdateNotificationRequest has the limits of CreateNotificationRequest.
type UpdateNotificationRequest struct {
	Name    *string   `json:"name,omitempty"`
	Kind    *string   `json:"kind,omitempty"`
	URL     *string   `json:"url,omitempty" doc:"Absent: keep the URL."`
	Events  *[]string `json:"events,omitempty"`
	Enabled *bool     `json:"enabled,omitempty"`
}

func (UpdateNotificationRequest) TransformSchema(r huma.Registry, s *huma.Schema) *huma.Schema {
	return patchOf[CreateNotificationRequest](r, s)
}

func (h *Handler) updateNotification(ctx context.Context, in *struct {
	PathID
	Body UpdateNotificationRequest
}) (*body[NotificationChannel], error) {
	c, err := h.d.Channels.Update(ctx, actor(ctx), in.ID, notify.ChannelChange(in.Body))
	if err != nil {
		return nil, notFound(err, "No notification channel has this ID.")
	}
	return &body[NotificationChannel]{channelOut(c)}, nil
}

func (h *Handler) deleteNotification(ctx context.Context, in *PathID) (*empty, error) {
	if err := h.d.Channels.Delete(ctx, actor(ctx), in.ID); err != nil {
		return nil, notFound(err, "No notification channel has this ID.")
	}
	return nil, nil
}

func (h *Handler) testNotification(ctx context.Context, in *PathID) (*empty, error) {
	base, err := h.baseURL(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.d.Channels.Test(ctx, in.ID, base); err != nil {
		return nil, notFound(serviceErr(err), "No notification channel has this ID.")
	}
	return nil, nil
}

type TestChannelRequest struct {
	Kind string `json:"kind" enum:"slack,discord,webhook"`
	URL  string `json:"url" minLength:"1" maxLength:"2048"`
}

func (h *Handler) testChannel(ctx context.Context, in *body[TestChannelRequest]) (*empty, error) {
	base, err := h.baseURL(ctx)
	if err != nil {
		return nil, err
	}
	return nil, serviceErr(h.d.Channels.TestURL(ctx, in.Body.Kind, in.Body.URL, base))
}
