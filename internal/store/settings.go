package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/runplane/runplane/internal/sandbox"
)

// Settings is the typed form of the settings table. Each JSON field name is
// one key, and each key holds one JSON value.
type Settings struct {
	InstanceName            string       `json:"instance_name"`
	PublicURL               string       `json:"public_url"` // empty = the request host
	Domain                  string       `json:"domain"`
	ACMEEmail               string       `json:"acme_email"`
	Mode                    sandbox.Mode `json:"mode"`
	ReserveVCPU             int          `json:"reserve_vcpu"`
	ReserveMemoryMB         int          `json:"reserve_memory_mb"`
	CPUOvercommit           float64      `json:"cpu_overcommit"`
	MinFreeDiskPercent      float64      `json:"min_free_disk_percent"`
	DefaultTimeLimitMinutes int          `json:"default_time_limit_minutes"`
	AllowPublicRepos        bool         `json:"allow_public_repos"` // VM mode only
	AllowForkPRs            bool         `json:"allow_fork_prs"`
	EgressAllowlist         []string     `json:"egress_allowlist"` // empty = all public internet
	BandwidthMbps           int          `json:"bandwidth_mbps"`   // 0 = no limit
	DNSServers              []string     `json:"dns_servers"`
	RegistryMirror          bool         `json:"registry_mirror"`
	ActionsCache            bool         `json:"actions_cache"`
	CacheMaxGB              int          `json:"cache_max_gb"`
	JobRetentionDays        int          `json:"job_retention_days"`
	LogRetentionDays        int          `json:"log_retention_days"`
	DebugHoldMinutes        int          `json:"debug_hold_minutes"`
	AutoUpdate              bool         `json:"auto_update"`
	BackupEnabled           bool         `json:"backup_enabled"`
	BackupKeep              int          `json:"backup_keep"`
	BackupS3                BackupS3     `json:"backup_s3"`
}

// DashboardURL is the browser URL of runplane for links in notifications, or
// "" if runplane cannot know it without a request.
func (s Settings) DashboardURL() string {
	switch {
	case s.PublicURL != "":
		return s.PublicURL
	case s.Domain != "":
		return "https://" + s.Domain
	}
	return ""
}

// Reserve is the part of each server that the sandboxes do not get.
func (s Settings) Reserve() sandbox.Reserve {
	return sandbox.Reserve{VCPU: s.ReserveVCPU, MemoryMB: s.ReserveMemoryMB, Overcommit: s.CPUOvercommit}
}

type BackupS3 struct {
	Endpoint     string `json:"endpoint,omitempty"`
	Region       string `json:"region,omitempty"`
	Bucket       string `json:"bucket,omitempty"`
	Prefix       string `json:"prefix,omitempty"`
	AccessKey    string `json:"access_key,omitempty"`
	SecretKeyEnc []byte `json:"secret_key_enc,omitempty"` // sealed with internal/secret
}

// DefaultSettings returns the value of each key that is not in the table.
func DefaultSettings() Settings {
	return Settings{
		InstanceName:            "runplane",
		ReserveVCPU:             1,
		ReserveMemoryMB:         2048,
		CPUOvercommit:           1.0,
		MinFreeDiskPercent:      15,
		DefaultTimeLimitMinutes: 360,
		EgressAllowlist:         []string{},
		DNSServers:              []string{"1.1.1.1", "8.8.8.8"},
		RegistryMirror:          true,
		ActionsCache:            true,
		CacheMaxGB:              50,
		JobRetentionDays:        90,
		LogRetentionDays:        30,
		DebugHoldMinutes:        30,
		BackupEnabled:           true,
		BackupKeep:              7,
	}
}

func (s *Store) GetSettings(ctx context.Context) (Settings, error) {
	return getSettings(ctx, s.r)
}

// UpdateSettings reads the settings, calls fn, and writes the keys that fn
// changed, all in one transaction. It returns the new settings. The table
// keeps keys that Settings does not know.
func (s *Store) UpdateSettings(ctx context.Context, fn func(*Settings) error) (Settings, error) {
	var st Settings
	err := s.tx(ctx, "update settings", func(tx *sql.Tx) error {
		var err error
		if st, err = getSettings(ctx, tx); err != nil {
			return err
		}
		// Encode before fn runs, because fn can change slices in place.
		before, err := settingsKeys(st)
		if err != nil {
			return err
		}
		if err := fn(&st); err != nil {
			return err
		}
		after, err := settingsKeys(st)
		if err != nil {
			return err
		}
		for key, value := range after {
			if string(before[key]) == string(value) {
				continue
			}
			if _, err := tx.ExecContext(ctx, upsert("settings", "key, value", "key"), key, string(value)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Settings{}, err
	}
	return st, nil
}

func getSettings(ctx context.Context, q querier) (Settings, error) {
	stored, err := queryMap(ctx, q, "read settings", func(sc scanner) (string, json.RawMessage, error) {
		var key, value string
		err := sc.Scan(&key, &value)
		return key, json.RawMessage(value), err
	}, "SELECT key, value FROM settings")
	if err != nil {
		return Settings{}, err
	}
	// Decode the stored keys over the defaults. Unknown keys have no field.
	b, err := json.Marshal(stored)
	if err != nil {
		return Settings{}, fmt.Errorf("read settings: %w", err)
	}
	st := DefaultSettings()
	if err := json.Unmarshal(b, &st); err != nil {
		return Settings{}, fmt.Errorf("read settings: %w", err)
	}
	return st, nil
}

func settingsKeys(st Settings) (map[string]json.RawMessage, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	var keys map[string]json.RawMessage
	return keys, json.Unmarshal(b, &keys)
}
