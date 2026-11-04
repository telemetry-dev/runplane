package store

import (
	"context"
	"fmt"
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

const channelCols = "id, name, kind, url_enc, events_json, enabled, created_at, updated_at"

func scanChannel(sc scanner) (NotificationChannel, error) {
	var c NotificationChannel
	err := sc.Scan(&c.ID, &c.Name, &c.Kind, &c.URLEnc, jsonCol(&c.Events), &c.Enabled, at(&c.CreatedAt), at(&c.UpdatedAt))
	return c, err
}

func (s *Store) CreateNotificationChannel(ctx context.Context, c NotificationChannel) error {
	_, err := s.w.ExecContext(ctx, "INSERT INTO notification_channels ("+channelCols+") VALUES ("+placeholders(8)+")",
		c.ID, c.Name, c.Kind, c.URLEnc, jsonArg(c.Events), c.Enabled, ms(c.CreatedAt), ms(c.UpdatedAt))
	if err != nil {
		return fmt.Errorf("create notification channel: %w", dbErr(err))
	}
	return nil
}

func (s *Store) GetNotificationChannel(ctx context.Context, id string) (NotificationChannel, error) {
	c, err := scanChannel(s.r.QueryRowContext(ctx, "SELECT "+channelCols+" FROM notification_channels WHERE id = ?", id))
	if err != nil {
		return NotificationChannel{}, fmt.Errorf("get notification channel: %w", dbErr(err))
	}
	return c, nil
}

func (s *Store) ListNotificationChannels(ctx context.Context) ([]NotificationChannel, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+channelCols+" FROM notification_channels ORDER BY name, id")
	list, err := scanAll(rows, err, scanChannel)
	if err != nil {
		return nil, fmt.Errorf("list notification channels: %w", err)
	}
	return list, nil
}

// UpdateNotificationChannel writes all columns except id and created_at, and sets updated_at.
func (s *Store) UpdateNotificationChannel(ctx context.Context, c NotificationChannel) error {
	err := exec1(ctx, s.w, `UPDATE notification_channels SET name = ?, kind = ?, url_enc = ?, events_json = ?,
		enabled = ?, updated_at = ? WHERE id = ?`,
		c.Name, c.Kind, c.URLEnc, jsonArg(c.Events), c.Enabled, ms(time.Now()), c.ID)
	if err != nil {
		return fmt.Errorf("update notification channel: %w", err)
	}
	return nil
}

func (s *Store) DeleteNotificationChannel(ctx context.Context, id string) error {
	if err := exec1(ctx, s.w, "DELETE FROM notification_channels WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete notification channel: %w", err)
	}
	return nil
}
