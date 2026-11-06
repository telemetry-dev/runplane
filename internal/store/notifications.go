package store

import (
	"context"
	"time"
)

type NotificationChannel struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"` // slack | discord | webhook
	URLEnc    []byte    `json:"-"`    // sealed with internal/secret
	Events    []string  `json:"events"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NotificationDelivery is the last delivery to a channel.
type NotificationDelivery struct {
	SentAt *time.Time // nil before the first delivery
	Error  string     // empty if the delivery worked
}

const channelCols = "id, name, kind, url_enc, events_json, enabled, created_at, updated_at"

func (c *NotificationChannel) cols() []any {
	return []any{&c.ID, &c.Name, &c.Kind, &c.URLEnc, jsonCol(&c.Events), &c.Enabled, at(&c.CreatedAt), at(&c.UpdatedAt)}
}

func (s *Store) CreateNotificationChannel(ctx context.Context, c NotificationChannel) error {
	return exec(ctx, s.w, "create notification channel", insert("notification_channels", channelCols), c.cols()...)
}

func (s *Store) GetNotificationChannel(ctx context.Context, id string) (NotificationChannel, error) {
	return queryOne(ctx, s.r, "get notification channel", scanRow[NotificationChannel],
		"SELECT "+channelCols+" FROM notification_channels WHERE id = ?", id)
}

func (s *Store) ListNotificationChannels(ctx context.Context) ([]NotificationChannel, error) {
	return queryAll(ctx, s.r, "list notification channels", scanRow[NotificationChannel],
		"SELECT "+channelCols+" FROM notification_channels ORDER BY name, id")
}

// UpdateNotificationChannel writes all columns except id and created_at.
func (s *Store) UpdateNotificationChannel(ctx context.Context, c NotificationChannel) error {
	return exec1(ctx, s.w, "update notification channel", `UPDATE notification_channels SET name = ?, kind = ?,
		url_enc = ?, events_json = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		c.Name, c.Kind, c.URLEnc, jsonCol(&c.Events), c.Enabled, ms(c.UpdatedAt), c.ID)
}

func (s *Store) DeleteNotificationChannel(ctx context.Context, id string) error {
	return exec1(ctx, s.w, "delete notification channel", "DELETE FROM notification_channels WHERE id = ?", id)
}

// RecordNotificationDelivery records a delivery to a channel at t. sendErr
// is empty if it worked.
func (s *Store) RecordNotificationDelivery(ctx context.Context, channelID string, t time.Time, sendErr string) error {
	return exec1(ctx, s.w, "record notification delivery",
		"UPDATE notification_channels SET last_sent_at = ?, last_error = ? WHERE id = ?", ms(t), sendErr, channelID)
}

// NotificationDeliveries returns the last delivery of each channel.
func (s *Store) NotificationDeliveries(ctx context.Context) (map[string]NotificationDelivery, error) {
	return queryMap(ctx, s.r, "list notification deliveries", func(sc scanner) (string, NotificationDelivery, error) {
		var id string
		var d NotificationDelivery
		err := sc.Scan(&id, atPtr(&d.SentAt), &d.Error)
		return id, d, err
	}, "SELECT id, last_sent_at, last_error FROM notification_channels")
}
