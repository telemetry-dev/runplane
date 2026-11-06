package notify

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/field"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
)

// Channels keeps the notification channels: the rules of a channel, its
// sealed URL, and the record of each delivery.
type Channels struct {
	st    *store.Store
	box   *secret.Box
	audit audit.Log
	log   *slog.Logger
	now   func() time.Time
}

// NewChannels returns the channels of st. box seals the channel URLs. log
// can be nil.
func NewChannels(st *store.Store, box *secret.Box, log *slog.Logger) *Channels {
	return &Channels{
		st: st, box: box, audit: audit.Log{Store: st, Logger: log}, log: cmp.Or(log, slog.New(slog.DiscardHandler)),
		now: time.Now,
	}
}

// Channel is a channel as the dashboard shows it.
type Channel struct {
	store.NotificationChannel
	// URL keeps the scheme, the host, and the last 4 characters of the
	// channel URL. The path of a webhook URL is its secret.
	URL  string
	Last store.NotificationDelivery
}

// ChannelChange has the values of a channel to set. A nil field keeps the
// value. URL is the plain URL.
type ChannelChange struct {
	Name    *string
	Kind    *string
	URL     *string
	Events  *[]string
	Enabled *bool
}

func (c *Channels) List(ctx context.Context) ([]Channel, error) {
	list, err := c.st.ListNotificationChannels(ctx)
	if err != nil {
		return nil, err
	}
	return c.view(ctx, list...)
}

// Create adds a channel. It needs a name, a kind, and a URL, and a nil
// Enabled turns it on. A refused value gives field.Problems.
func (c *Channels) Create(ctx context.Context, by audit.Actor, change ChannelChange) (Channel, error) {
	now := c.now()
	ch := store.NotificationChannel{ID: ids.New(), Events: []string{}, Enabled: true, CreatedAt: now, UpdatedAt: now}
	rawURL, err := c.apply(&ch, change, "")
	if err != nil {
		return Channel{}, err
	}
	ch.URLEnc = c.box.Seal([]byte(rawURL))
	if err := c.st.CreateNotificationChannel(ctx, ch); err != nil {
		return Channel{}, err
	}
	c.audit.Record(ctx, by, audit.ChannelCreate, ch.ID, map[string]any{"name": ch.Name, "kind": ch.Kind})
	return first(c.view(ctx, ch))
}

// Update changes the channel id. The URL rules depend on the kind, so a new
// kind checks the kept URL again. It returns store.ErrNotFound for an
// unknown ID.
func (c *Channels) Update(ctx context.Context, by audit.Actor, id string, change ChannelChange) (Channel, error) {
	ch, err := c.st.GetNotificationChannel(ctx, id)
	if err != nil {
		return Channel{}, err
	}
	kept, err := c.box.Open(ch.URLEnc)
	if err != nil {
		c.log.Warn("open notification URL", "channel", ch.ID, "err", err)
	}
	rawURL, err := c.apply(&ch, change, string(kept))
	if err != nil {
		return Channel{}, err
	}
	if change.URL != nil {
		ch.URLEnc = c.box.Seal([]byte(rawURL))
	}
	ch.UpdatedAt = c.now()
	if err := c.st.UpdateNotificationChannel(ctx, ch); err != nil {
		return Channel{}, err
	}
	c.audit.Record(ctx, by, audit.ChannelUpdate, ch.ID, map[string]any{"name": ch.Name, "kind": ch.Kind, "url_changed": change.URL != nil})
	return first(c.view(ctx, ch))
}

// Delete removes the channel id. It returns store.ErrNotFound for an
// unknown ID.
func (c *Channels) Delete(ctx context.Context, by audit.Actor, id string) error {
	if err := c.st.DeleteNotificationChannel(ctx, id); err != nil {
		return err
	}
	c.audit.Record(ctx, by, audit.ChannelDelete, id, nil)
	return nil
}

// Test sends the test event to the channel id and records the delivery.
// The link of the event goes to the dashboard at the base URL dashboard.
func (c *Channels) Test(ctx context.Context, id, dashboard string) error {
	ch, err := c.st.GetNotificationChannel(ctx, id)
	if err != nil {
		return err
	}
	return c.deliver(ctx, ch, TestEvent(), dashboard)
}

// TestURL sends the test event to a channel that is not saved. A refused
// kind or URL gives field.Problems.
func (c *Channels) TestURL(ctx context.Context, kind, rawURL, dashboard string) error {
	rawURL = strings.TrimSpace(rawURL)
	var errs field.Problems
	checkTarget(&errs, kind, rawURL)
	if err := errs.Err(); err != nil {
		return err
	}
	return send(ctx, kind, rawURL, dashboard, TestEvent())
}

// apply sets the change on ch and checks the result. It returns the plain
// URL of the channel: the new one, or kept.
func (c *Channels) apply(ch *store.NotificationChannel, change ChannelChange, kept string) (string, error) {
	if change.Name != nil {
		ch.Name = strings.TrimSpace(*change.Name)
	}
	if change.Kind != nil {
		ch.Kind = *change.Kind
	}
	if change.Events != nil {
		ch.Events = []string{}
		for _, e := range *change.Events {
			if !slices.Contains(ch.Events, e) {
				ch.Events = append(ch.Events, e)
			}
		}
	}
	if change.Enabled != nil {
		ch.Enabled = *change.Enabled
	}
	rawURL := kept
	if change.URL != nil {
		rawURL = strings.TrimSpace(*change.URL)
	}
	return rawURL, check(*ch, rawURL)
}

func check(ch store.NotificationChannel, rawURL string) error {
	var errs field.Problems
	if ch.Name == "" {
		errs.Add("name", "Enter a name.", ch.Name)
	}
	checkTarget(&errs, ch.Kind, rawURL)
	for i, e := range ch.Events {
		if !slices.Contains(Types, Type(e)) {
			errs.Add(fmt.Sprintf("events[%d]", i), "runplane has no event "+strconv.Quote(e)+".", e)
		}
	}
	return errs.Err()
}

func checkTarget(errs *field.Problems, kind, rawURL string) {
	if !slices.Contains(Kinds, kind) {
		errs.Add("kind", "runplane has no channel kind "+strconv.Quote(kind)+".", kind)
	} else if err := checkURL(kind, rawURL); err != nil {
		errs.Add("url", field.Sentence(err), nil)
	}
}

// notify sends e to each enabled channel that subscribes to its type, and
// records each delivery.
func (c *Channels) notify(ctx context.Context, e Event, dashboard string) error {
	list, err := c.st.ListNotificationChannels(ctx)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	errs := make([]error, len(list))
	for i, ch := range list {
		if !ch.Enabled || !slices.Contains(ch.Events, string(e.Type)) {
			continue
		}
		wg.Go(func() {
			if err := c.deliver(ctx, ch, e, dashboard); err != nil {
				errs[i] = fmt.Errorf("channel %s: %w", ch.Name, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// deliver sends e to the channel ch and records the delivery.
func (c *Channels) deliver(ctx context.Context, ch store.NotificationChannel, e Event, dashboard string) error {
	target, err := c.box.Open(ch.URLEnc)
	if err == nil {
		err = send(ctx, ch.Kind, string(target), dashboard, e)
	} else {
		err = fmt.Errorf("open the channel URL: %w", err)
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if err := c.st.RecordNotificationDelivery(context.WithoutCancel(ctx), ch.ID, c.now(), msg); err != nil {
		c.log.Warn("record notification delivery", "channel", ch.Name, "err", err)
	}
	return err
}

func (c *Channels) view(ctx context.Context, list ...store.NotificationChannel) ([]Channel, error) {
	deliveries, err := c.st.NotificationDeliveries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Channel, len(list))
	for i, ch := range list {
		masked := ""
		if plain, err := c.box.Open(ch.URLEnc); err == nil {
			masked = maskURL(string(plain))
		} else {
			c.log.Warn("open notification URL", "channel", ch.ID, "err", err)
		}
		out[i] = Channel{NotificationChannel: ch, URL: masked, Last: deliveries[ch.ID]}
	}
	return out, nil
}

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

func first(list []Channel, err error) (Channel, error) {
	if err != nil {
		return Channel{}, err
	}
	return list[0], nil
}
