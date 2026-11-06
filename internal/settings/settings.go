// Package settings changes the dashboard settings. Each change goes through
// Update: it checks the change, writes it, records it in the audit log, and
// publishes events.SettingsChanged, which the other services follow.
package settings

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"

	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/field"
	"github.com/runplane/runplane/internal/policy"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

type Config struct {
	Store *store.Store
	Bus   *events.Bus
	Log   *slog.Logger // nil: no log
	// Mode is the mode of the configuration, RUNPLANE_MODE.
	Mode        sandbox.Mode
	DevInsecure bool
	// KVM tells if this server, the primary, can open /dev/kvm. nil: it
	// cannot.
	KVM func() bool
}

type Service struct {
	st          *store.Store
	bus         *events.Bus
	audit       audit.Log
	mode        sandbox.Mode
	devInsecure bool
	kvm         func() bool
}

func New(c Config) *Service {
	return &Service{
		st: c.Store, bus: c.Bus, audit: audit.Log{Store: c.Store, Logger: c.Log},
		mode: c.Mode, devInsecure: c.DevInsecure, kvm: c.KVM,
	}
}

// Mode is the sandbox mode of the primary with the settings st.
func (s *Service) Mode(st store.Settings) sandbox.Mode {
	return policy.Mode(st, s.mode)
}

// InitMode keeps the mode of the configuration in the settings at the first
// start, and returns the mode of the primary. It fails when runplane cannot
// operate sandboxes in that mode.
func (s *Service) InitMode(ctx context.Context) (sandbox.Mode, error) {
	st, err := s.st.GetSettings(ctx)
	if err != nil {
		return "", err
	}
	if st.Mode == "" && s.mode != "" {
		st, err = s.st.UpdateSettings(ctx, func(st *store.Settings) error {
			st.Mode = s.mode
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return st.Mode, policy.CheckMode(st.Mode, s.devInsecure)
}

// Update changes the settings with change, in one transaction with the
// checks. A refused value gives field.Problems. A change that changes
// nothing writes nothing.
func (s *Service) Update(ctx context.Context, by audit.Actor, change func(*store.Settings) error) (store.Settings, error) {
	servers, err := s.st.ListServers(ctx)
	if err != nil {
		return store.Settings{}, err
	}
	var old store.Settings
	next, err := s.st.UpdateSettings(ctx, func(st *store.Settings) error {
		// A copy, because change can write the new values into the slices of st.
		old = clone(*st)
		if err := change(st); err != nil {
			return err
		}
		return s.check(old, st, servers)
	})
	if err != nil {
		return store.Settings{}, err
	}
	changes := audit.Changes(old, next)
	if len(changes) == 0 {
		return next, nil
	}
	s.audit.Record(ctx, by, audit.SettingsUpdate, "", detail(old, next, changes))
	s.bus.Publish(events.SettingsChanged{Old: old, New: next})
	return next, nil
}

// SetMode changes the mode setting. A mode that cannot run jobs of public
// repositories also turns allow_public_repos off, because Update refuses to
// keep it on, and install.sh cannot ask the admin.
func (s *Service) SetMode(ctx context.Context, by audit.Actor, m sandbox.Mode) (store.Settings, error) {
	servers, err := s.st.ListServers(ctx)
	if err != nil {
		return store.Settings{}, err
	}
	return s.Update(ctx, by, func(st *store.Settings) error {
		st.Mode = m
		if policy.PublicRepos(m, servers) != "" {
			st.AllowPublicRepos = false
		}
		return nil
	})
}

// check cleans the changed values of next and checks them, and the rules
// between the settings and the servers.
func (s *Service) check(old store.Settings, next *store.Settings, servers []store.Server) error {
	p := clean(old, next)
	if next.Mode != old.Mode {
		if err := policy.CheckModeChange(next.Mode, s.devInsecure, s.kvm != nil && s.kvm()); err != nil {
			p.Add("mode", field.Sentence(err), next.Mode)
		}
	}
	if next.AllowPublicRepos && (!old.AllowPublicRepos || next.Mode != old.Mode) {
		if why := policy.PublicRepos(s.Mode(*next), servers); why != "" {
			p.Add("allow_public_repos", why, true)
		}
	}
	if b := next.BackupS3; b.Bucket != "" {
		if b.AccessKey == "" {
			p.Add("backup_s3.access_key", "Enter the access key for the bucket.", nil)
		}
		if len(b.SecretKeyEnc) == 0 {
			p.Add("backup_s3.secret_key", "Enter the secret key for the bucket.", nil)
		}
	}
	return p.Err()
}

// detail is the audit detail of a change: the changed keys, and the old and
// new value of each. The values of backup_s3 come one field at a time, as
// backup_s3.bucket for example, and without the secret key.
func detail(old, next store.Settings, changes map[string][2]any) map[string]any {
	keys := slices.Sorted(maps.Keys(changes))
	delete(changes, "backup_s3")
	for k, v := range audit.Changes(s3Fields(old.BackupS3), s3Fields(next.BackupS3)) {
		changes["backup_s3."+k] = v
	}
	return map[string]any{"keys": keys, "changes": changes}
}

// s3Fields are the S3 settings for the audit log. secret_key_set tells if
// the secret key is set, which the log does not hold.
func s3Fields(b store.BackupS3) map[string]any {
	var fields map[string]any
	raw, _ := json.Marshal(b)
	json.Unmarshal(raw, &fields)
	delete(fields, "secret_key_enc")
	fields["secret_key_set"] = len(b.SecretKeyEnc) > 0
	return fields
}

func clone(st store.Settings) store.Settings {
	var c store.Settings
	raw, _ := json.Marshal(st)
	json.Unmarshal(raw, &c)
	return c
}
