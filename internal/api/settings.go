package api

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

type Settings struct {
	InstanceName            string   `json:"instance_name" doc:"Title in the sidebar."`
	PublicURL               string   `json:"public_url" doc:"Browser URL of the dashboard, for example https://ci.example.com. Empty: the URL of each request."`
	Domain                  string   `json:"domain" doc:"With a domain, runplane gets a certificate with ACME and serves HTTPS on ports 443 and 80. The DNS record of the domain must point to the server, and the two ports must be free and open in the firewall."`
	ACMEEmail               string   `json:"acme_email" doc:"Contact for the certificate authority. Optional."`
	Mode                    string   `json:"mode" enum:"container,vm,dev," doc:"Sandbox mode. Empty until the install sets it."`
	ReserveVCPU             int      `json:"reserve_vcpu" doc:"vCPUs that sandboxes do not get on each server."`
	ReserveMemoryMB         int      `json:"reserve_memory_mb" doc:"Memory that sandboxes do not get on each server."`
	CPUOvercommit           float64  `json:"cpu_overcommit" doc:"Factor for the vCPUs that sandboxes can get. 1 means no overcommit."`
	MinFreeDiskPercent      float64  `json:"min_free_disk_percent" doc:"No new sandboxes start on a server with less free disk."`
	DefaultTimeLimitMinutes int      `json:"default_time_limit_minutes" doc:"Time limit of a job in a pool without its own limit."`
	AllowPublicRepos        bool     `json:"allow_public_repos" doc:"Take jobs of public repositories. Only when the mode setting and each server are in VM mode."`
	AllowForkPRs            bool     `json:"allow_fork_prs" doc:"Take jobs of pull requests from forks."`
	EgressAllowlist         []string `json:"egress_allowlist" doc:"Host names, *.domains, IP addresses, or CIDRs that sandboxes can reach. Empty: all of the public internet."`
	BandwidthMbps           int      `json:"bandwidth_mbps" doc:"Bandwidth limit of each VM. 0: no limit."`
	DNSServers              []string `json:"dns_servers" doc:"IPv4 addresses of the DNS servers of the sandboxes."`
	RegistryMirror          bool     `json:"registry_mirror" doc:"Docker Hub pull-through cache for the sandboxes."`
	ActionsCache            bool     `json:"actions_cache" doc:"Local Actions cache server."`
	CacheMaxGB              int      `json:"cache_max_gb"`
	JobRetentionDays        int      `json:"job_retention_days"`
	LogRetentionDays        int      `json:"log_retention_days"`
	DebugHoldMinutes        int      `json:"debug_hold_minutes" doc:"Time that a debug sandbox stays after its job."`
	AutoUpdate              bool     `json:"auto_update"`
	BackupEnabled           bool     `json:"backup_enabled" doc:"Daily database backup."`
	BackupKeep              int      `json:"backup_keep" doc:"Backups to keep."`
	BackupS3                BackupS3 `json:"backup_s3" doc:"Optional upload of each backup to S3."`

	SSHPort int          `json:"ssh_port" doc:"Port of the SSH gateway for debug shells. Read-only."`
	HTTPS   *HTTPSStatus `json:"https,omitempty" doc:"Certificate of the domain. Read-only. Absent when this runplane process cannot report it."`
}

type HTTPSStatus struct {
	State    string     `json:"state" enum:"none,requesting,valid,error" doc:"none: no domain is set. requesting: runplane waits for the first certificate. error: port 443 or 80 does not open (runplane tries again each minute), or the last request failed and no valid certificate exists."`
	NotAfter *time.Time `json:"not_after" doc:"End of the current certificate. Null without a certificate."`
	Error    string     `json:"error" doc:"Why a port does not open, or why the last certificate request failed."`
}

type BackupS3 struct {
	Endpoint     string `json:"endpoint" doc:"Empty: AWS S3 in the region."`
	Region       string `json:"region"`
	Bucket       string `json:"bucket" doc:"Empty: no upload."`
	Prefix       string `json:"prefix"`
	AccessKey    string `json:"access_key"`
	SecretKeySet bool   `json:"secret_key_set" doc:"The secret key is write-only."`
}

func (h *Handler) settingsOut(ctx context.Context, st store.Settings) Settings {
	out := Settings{
		InstanceName: st.InstanceName, PublicURL: st.PublicURL, Domain: st.Domain, ACMEEmail: st.ACMEEmail,
		Mode: string(st.Mode), ReserveVCPU: st.ReserveVCPU, ReserveMemoryMB: st.ReserveMemoryMB,
		CPUOvercommit: st.CPUOvercommit, MinFreeDiskPercent: st.MinFreeDiskPercent,
		DefaultTimeLimitMinutes: st.DefaultTimeLimitMinutes, AllowPublicRepos: st.AllowPublicRepos,
		AllowForkPRs: st.AllowForkPRs, EgressAllowlist: orEmpty(st.EgressAllowlist), BandwidthMbps: st.BandwidthMbps,
		DNSServers: orEmpty(st.DNSServers), RegistryMirror: st.RegistryMirror, ActionsCache: st.ActionsCache,
		CacheMaxGB: st.CacheMaxGB, JobRetentionDays: st.JobRetentionDays, LogRetentionDays: st.LogRetentionDays,
		DebugHoldMinutes: st.DebugHoldMinutes, AutoUpdate: st.AutoUpdate, BackupEnabled: st.BackupEnabled,
		BackupKeep: st.BackupKeep, BackupS3: backupS3Out(st.BackupS3), SSHPort: h.sshPort(),
	}
	if h.d.HTTPS != nil {
		s := h.d.HTTPS.CertificateStatus(ctx)
		out.HTTPS = &s
	}
	return out
}

func backupS3Out(b store.BackupS3) BackupS3 {
	return BackupS3{
		Endpoint: b.Endpoint, Region: b.Region, Bucket: b.Bucket, Prefix: b.Prefix, AccessKey: b.AccessKey,
		SecretKeySet: len(b.SecretKeyEnc) > 0,
	}
}

func (h *Handler) getSettings(ctx context.Context, _ *struct{}) (*body[Settings], error) {
	st, err := h.d.Store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	return &body[Settings]{h.settingsOut(ctx, st)}, nil
}

type SettingsPatch struct {
	InstanceName            *string        `json:"instance_name,omitempty" minLength:"1" maxLength:"64"`
	PublicURL               *string        `json:"public_url,omitempty" maxLength:"2048"`
	Domain                  *string        `json:"domain,omitempty" maxLength:"253"`
	ACMEEmail               *string        `json:"acme_email,omitempty" maxLength:"254"`
	Mode                    *string        `json:"mode,omitempty" enum:"container,vm,dev" doc:"vm needs KVM; dev needs RUNPLANE_DEV_INSECURE=1. A change takes effect after a restart."`
	ReserveVCPU             *int           `json:"reserve_vcpu,omitempty" minimum:"0" maximum:"256"`
	ReserveMemoryMB         *int           `json:"reserve_memory_mb,omitempty" minimum:"0" maximum:"1048576"`
	CPUOvercommit           *float64       `json:"cpu_overcommit,omitempty" minimum:"0.5" maximum:"8"`
	MinFreeDiskPercent      *float64       `json:"min_free_disk_percent,omitempty" minimum:"0" maximum:"90"`
	DefaultTimeLimitMinutes *int           `json:"default_time_limit_minutes,omitempty" minimum:"1" maximum:"7200"`
	AllowPublicRepos        *bool          `json:"allow_public_repos,omitempty"`
	AllowForkPRs            *bool          `json:"allow_fork_prs,omitempty"`
	EgressAllowlist         *[]string      `json:"egress_allowlist,omitempty" maxItems:"256"`
	BandwidthMbps           *int           `json:"bandwidth_mbps,omitempty" minimum:"0" maximum:"100000"`
	DNSServers              *[]string      `json:"dns_servers,omitempty" minItems:"1" maxItems:"4"`
	RegistryMirror          *bool          `json:"registry_mirror,omitempty"`
	ActionsCache            *bool          `json:"actions_cache,omitempty"`
	CacheMaxGB              *int           `json:"cache_max_gb,omitempty" minimum:"1" maximum:"100000"`
	JobRetentionDays        *int           `json:"job_retention_days,omitempty" minimum:"1" maximum:"3650"`
	LogRetentionDays        *int           `json:"log_retention_days,omitempty" minimum:"1" maximum:"3650"`
	DebugHoldMinutes        *int           `json:"debug_hold_minutes,omitempty" minimum:"1" maximum:"1440"`
	AutoUpdate              *bool          `json:"auto_update,omitempty"`
	BackupEnabled           *bool          `json:"backup_enabled,omitempty"`
	BackupKeep              *int           `json:"backup_keep,omitempty" minimum:"1" maximum:"365"`
	BackupS3                *BackupS3Patch `json:"backup_s3,omitempty"`
}

type BackupS3Patch struct {
	Endpoint  *string `json:"endpoint,omitempty" maxLength:"2048"`
	Region    *string `json:"region,omitempty" maxLength:"64"`
	Bucket    *string `json:"bucket,omitempty" maxLength:"63"`
	Prefix    *string `json:"prefix,omitempty" maxLength:"256"`
	AccessKey *string `json:"access_key,omitempty" maxLength:"256"`
	SecretKey *string `json:"secret_key,omitempty" maxLength:"256" doc:"Write-only. An empty string removes the key."`
}

func (h *Handler) updateSettings(ctx context.Context, in *body[SettingsPatch]) (*body[Settings], error) {
	p := in.Body
	if err := p.check(); err != nil {
		return nil, err
	}
	servers, err := h.d.Store.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	var old store.Settings
	next, err := h.d.Store.UpdateSettings(ctx, func(st *store.Settings) error {
		old = *st
		p.apply(st, h)
		return h.checkSettings(*st, old, servers)
	})
	if err != nil {
		return nil, err
	}
	if changes := fieldChanges(old, next); len(changes) > 0 {
		h.audit(ctx, "settings.update", "", settingsDetail(old, next, changes))
		h.d.Bus.Publish(events.SettingsChanged(next))
		if h.d.SettingsHook != nil {
			h.d.SettingsHook.SettingsChanged(context.WithoutCancel(ctx), old, next)
		}
	}
	return &body[Settings]{h.settingsOut(ctx, next)}, nil
}

// check validates the values that do not depend on other settings, and
// trims them.
func (p *SettingsPatch) check() error {
	var errs problems
	trim := func(list ...*string) {
		for _, v := range list {
			if v != nil {
				*v = strings.TrimSpace(*v)
			}
		}
	}
	trim(p.InstanceName, p.PublicURL, p.Domain, p.ACMEEmail)
	if p.InstanceName != nil && *p.InstanceName == "" {
		errs.add("body.instance_name", "Enter a name.", *p.InstanceName)
	}
	if p.PublicURL != nil && *p.PublicURL != "" {
		u, err := url.Parse(*p.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			errs.add("body.public_url", "Enter a URL such as https://ci.example.com, without a path.", *p.PublicURL)
		} else {
			*p.PublicURL = u.Scheme + "://" + u.Host
		}
	}
	if p.Domain != nil {
		*p.Domain = strings.ToLower(*p.Domain)
		if *p.Domain != "" && !ValidDomain(*p.Domain) {
			errs.add("body.domain", "Enter a domain name such as ci.example.com.", *p.Domain)
		}
	}
	if p.ACMEEmail != nil && *p.ACMEEmail != "" {
		if a, err := mail.ParseAddress(*p.ACMEEmail); err != nil || a.Address != *p.ACMEEmail {
			errs.add("body.acme_email", "Enter an email address.", *p.ACMEEmail)
		}
	}
	if p.EgressAllowlist != nil {
		for i, v := range *p.EgressAllowlist {
			v = strings.ToLower(strings.TrimSpace(v))
			(*p.EgressAllowlist)[i] = v
			if !validEgress(v) {
				errs.add(fmt.Sprintf("body.egress_allowlist[%d]", i), "Enter a host name, *.domain, IP address, or CIDR.", v)
			}
		}
	}
	if p.DNSServers != nil {
		for i, v := range *p.DNSServers {
			v = strings.TrimSpace(v)
			(*p.DNSServers)[i] = v
			if a, err := netip.ParseAddr(v); err != nil || !a.Is4() {
				errs.add(fmt.Sprintf("body.dns_servers[%d]", i), "Enter an IPv4 address.", v)
			}
		}
	}
	if b := p.BackupS3; b != nil {
		trim(b.Endpoint, b.Region, b.Bucket, b.Prefix, b.AccessKey)
		if b.Endpoint != nil && *b.Endpoint != "" {
			u, err := url.Parse(*b.Endpoint)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				errs.add("body.backup_s3.endpoint", "Enter a URL such as https://s3.example.com.", *b.Endpoint)
			}
		}
		if b.Region != nil && !s3Region.MatchString(*b.Region) {
			errs.add("body.backup_s3.region", "Enter a region such as us-east-1.", *b.Region)
		}
		if b.Bucket != nil && *b.Bucket != "" && !s3Bucket.MatchString(*b.Bucket) {
			errs.add("body.backup_s3.bucket", "Use 3 to 63 lowercase letters, digits, dots, and dashes.", *b.Bucket)
		}
		if b.Prefix != nil {
			*b.Prefix = strings.TrimPrefix(*b.Prefix, "/")
		}
	}
	return errs.err()
}

var (
	s3Region = regexp.MustCompile(`^[a-z0-9-]*$`)
	s3Bucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// ValidDomain accepts a lowercase host name with at least two labels, not an
// IP address.
func ValidDomain(d string) bool {
	labels := strings.Split(d, ".")
	if len(d) > 253 || len(labels) < 2 || net.ParseIP(d) != nil {
		return false
	}
	for _, l := range labels {
		if !dnsLabel.MatchString(l) {
			return false
		}
	}
	return true
}

func validEgress(v string) bool {
	if _, err := netip.ParseAddr(v); err == nil {
		return true
	}
	if _, err := netip.ParsePrefix(v); err == nil {
		return true
	}
	return ValidDomain(strings.TrimPrefix(v, "*."))
}

func (p *SettingsPatch) apply(st *store.Settings, h *Handler) {
	set(&st.InstanceName, p.InstanceName)
	set(&st.PublicURL, p.PublicURL)
	set(&st.Domain, p.Domain)
	set(&st.ACMEEmail, p.ACMEEmail)
	if p.Mode != nil {
		st.Mode = sandbox.Mode(*p.Mode)
	}
	set(&st.ReserveVCPU, p.ReserveVCPU)
	set(&st.ReserveMemoryMB, p.ReserveMemoryMB)
	set(&st.CPUOvercommit, p.CPUOvercommit)
	set(&st.MinFreeDiskPercent, p.MinFreeDiskPercent)
	set(&st.DefaultTimeLimitMinutes, p.DefaultTimeLimitMinutes)
	set(&st.AllowPublicRepos, p.AllowPublicRepos)
	set(&st.AllowForkPRs, p.AllowForkPRs)
	set(&st.EgressAllowlist, p.EgressAllowlist)
	set(&st.BandwidthMbps, p.BandwidthMbps)
	set(&st.DNSServers, p.DNSServers)
	set(&st.RegistryMirror, p.RegistryMirror)
	set(&st.ActionsCache, p.ActionsCache)
	set(&st.CacheMaxGB, p.CacheMaxGB)
	set(&st.JobRetentionDays, p.JobRetentionDays)
	set(&st.LogRetentionDays, p.LogRetentionDays)
	set(&st.DebugHoldMinutes, p.DebugHoldMinutes)
	set(&st.AutoUpdate, p.AutoUpdate)
	set(&st.BackupEnabled, p.BackupEnabled)
	set(&st.BackupKeep, p.BackupKeep)
	if b := p.BackupS3; b != nil {
		set(&st.BackupS3.Endpoint, b.Endpoint)
		set(&st.BackupS3.Region, b.Region)
		set(&st.BackupS3.Bucket, b.Bucket)
		set(&st.BackupS3.Prefix, b.Prefix)
		set(&st.BackupS3.AccessKey, b.AccessKey)
		if b.SecretKey != nil {
			st.BackupS3.SecretKeyEnc = nil
			if *b.SecretKey != "" {
				st.BackupS3.SecretKeyEnc = h.d.Box.Seal([]byte(*b.SecretKey))
			}
		}
	}
}

// checkSettings validates the rules between settings and the servers.
func (h *Handler) checkSettings(st, old store.Settings, servers []store.Server) error {
	var errs problems
	var local store.Server
	if i := slices.IndexFunc(servers, func(v store.Server) bool { return v.Kind == store.ServerLocal }); i >= 0 {
		local = servers[i]
	}
	if st.Mode != old.Mode {
		switch {
		case st.Mode == sandbox.ModeDev && !h.d.Config.DevInsecure:
			errs.add("body.mode", "Mode dev is for development only and needs RUNPLANE_DEV_INSECURE=1.", st.Mode)
		case st.Mode == sandbox.ModeVM && local.ID != "" && local.VCPU > 0 && !local.KVM:
			errs.add("body.mode", "VM mode needs KVM, and the primary server has no /dev/kvm.", st.Mode)
		}
	}
	// A server reports the mode of its running driver, which can differ
	// from the mode setting until a restart. The scheduler checks each
	// runner again, but the setting must not claim what the servers cannot do.
	if st.AllowPublicRepos && (!old.AllowPublicRepos || st.Mode != old.Mode) {
		if detail := notVM(h.mode(st), servers); detail != "" {
			errs.add("body.allow_public_repos", detail, true)
		}
	}
	if b := st.BackupS3; b.Bucket != "" {
		if b.AccessKey == "" {
			errs.add("body.backup_s3.access_key", "Enter the access key for the bucket.", nil)
		}
		if len(b.SecretKeyEnc) == 0 {
			errs.add("body.backup_s3.secret_key", "Enter the secret key for the bucket.", nil)
		}
	}
	return errs.err()
}

// notVM tells why public repositories are not safe: the mode setting or a
// server that is not in VM mode. It returns "" when all are in VM mode.
func notVM(mode sandbox.Mode, servers []store.Server) string {
	if mode != sandbox.ModeVM {
		return fmt.Sprintf("Jobs of public repositories need VM mode, and the mode setting is %s.", cmp.Or(string(mode), "not set"))
	}
	for _, v := range servers {
		if v.Mode != "" && v.Mode != sandbox.ModeVM {
			return fmt.Sprintf("Jobs of public repositories need VM mode on each server, and the server %s runs %s mode.", v.Name, v.Mode)
		}
	}
	return ""
}

// settingsDetail is the audit detail of a settings change: the changed keys,
// and the old and new value of each. The values of backup_s3 come one field
// at a time, as backup_s3.bucket for example, and without the secret key.
func settingsDetail(old, next store.Settings, changes map[string][2]any) map[string]any {
	keys := slices.Sorted(maps.Keys(changes))
	delete(changes, "backup_s3")
	for k, v := range fieldChanges(backupS3Out(old.BackupS3), backupS3Out(next.BackupS3)) {
		changes["backup_s3."+k] = v
	}
	return map[string]any{"keys": keys, "changes": changes}
}

// mode is the sandbox mode from the settings, or from the install.
func (h *Handler) mode(st store.Settings) sandbox.Mode {
	return cmp.Or(st.Mode, h.d.Config.Mode)
}
