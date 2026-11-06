package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/runplane/runplane/internal/sandbox"
)

// Settings is the typed form of the settings table. Each JSON field name is
// one key, and each key holds one JSON value. The key is also the name of the
// setting in the API and in the audit log. The tags give the doc and the
// limits of each setting to the API.
type Settings struct {
	InstanceName            string       `json:"instance_name" minLength:"1" maxLength:"64" doc:"Title in the sidebar."`
	PublicURL               string       `json:"public_url" maxLength:"2048" doc:"Browser URL of the dashboard, for example https://ci.example.com. Empty: the URL of each request."`
	Domain                  string       `json:"domain" maxLength:"253" doc:"With a domain, runplane gets a certificate with ACME and serves HTTPS on ports 443 and 80. The DNS record of the domain must point to the server, and the two ports must be free and open in the firewall."`
	ACMEEmail               string       `json:"acme_email" maxLength:"254" doc:"Contact for the certificate authority. Optional."`
	Mode                    sandbox.Mode `json:"mode" enum:"container,vm,dev" doc:"Sandbox mode of the primary. vm needs KVM, and dev needs RUNPLANE_DEV_INSECURE=1. A change takes effect after a restart."`
	ReserveVCPU             int          `json:"reserve_vcpu" minimum:"0" maximum:"256" doc:"vCPUs that sandboxes do not get on each server."`
	ReserveMemoryMB         int          `json:"reserve_memory_mb" minimum:"0" maximum:"1048576" doc:"Memory that sandboxes do not get on each server."`
	CPUOvercommit           float64      `json:"cpu_overcommit" minimum:"0.5" maximum:"8" doc:"Factor for the vCPUs that sandboxes can get. 1 means no overcommit."`
	MinFreeDiskPercent      float64      `json:"min_free_disk_percent" minimum:"0" maximum:"90" doc:"No new sandboxes start on a server with less free disk."`
	DefaultTimeLimitMinutes int          `json:"default_time_limit_minutes" minimum:"1" maximum:"7200" doc:"Time limit of a job in a pool without its own limit."`
	AllowPublicRepos        bool         `json:"allow_public_repos" doc:"Take jobs of public repositories. Only when the mode setting and each server are in VM mode."`
	AllowForkPRs            bool         `json:"allow_fork_prs" doc:"Take jobs of pull requests from forks."`
	EgressAllowlist         []string     `json:"egress_allowlist" maxItems:"256" doc:"Host names, *.domains, IP addresses, or CIDRs that sandboxes can reach. Empty: all of the public internet."`
	BandwidthMbps           int          `json:"bandwidth_mbps" minimum:"0" maximum:"100000" doc:"Bandwidth limit of each VM. 0: no limit."`
	DNSServers              []string     `json:"dns_servers" minItems:"1" maxItems:"4" doc:"IPv4 addresses of the DNS servers of the sandboxes."`
	RegistryMirror          bool         `json:"registry_mirror" doc:"Docker Hub pull-through cache for the sandboxes."`
	ActionsCache            bool         `json:"actions_cache" doc:"Local Actions cache server."`
	CacheMaxGB              int          `json:"cache_max_gb" minimum:"1" maximum:"100000"`
	JobRetentionDays        int          `json:"job_retention_days" minimum:"1" maximum:"3650"`
	LogRetentionDays        int          `json:"log_retention_days" minimum:"1" maximum:"3650"`
	DebugHoldMinutes        int          `json:"debug_hold_minutes" minimum:"1" maximum:"1440" doc:"Time that a debug sandbox stays after its job."`
	AutoUpdate              bool         `json:"auto_update"`
	BackupEnabled           bool         `json:"backup_enabled" doc:"Daily database backup."`
	BackupKeep              int          `json:"backup_keep" minimum:"1" maximum:"365" doc:"Backups to keep."`
	BackupS3                BackupS3     `json:"backup_s3" doc:"Optional upload of each backup to S3."`
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
	Endpoint     string `json:"endpoint"`
	Region       string `json:"region"`
	Bucket       string `json:"bucket"`
	Prefix       string `json:"prefix"`
	AccessKey    string `json:"access_key"`
	SecretKeyEnc []byte `json:"secret_key_enc"` // sealed with internal/secret
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
