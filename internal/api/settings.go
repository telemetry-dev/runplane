package api

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/policy"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
)

// Settings has the keys of store.Settings, with their docs and limits, and
// the facts that come from them.
type Settings struct {
	store.Settings
	BackupS3 BackupS3 `json:"backup_s3" doc:"Optional upload of each backup to S3."`

	SSHPort             int          `json:"ssh_port" doc:"Port of the SSH gateway for debug shells. Read-only."`
	HTTPS               *HTTPSStatus `json:"https,omitempty" doc:"Certificate of the domain. Read-only. Absent when this runplane process cannot report it."`
	PublicReposPossible bool         `json:"public_repos_possible" doc:"Jobs of public repositories can run: the mode setting and each server are in VM mode. Read-only."`
	PublicReposRefusal  string       `json:"public_repos_refusal" doc:"Why jobs of public repositories cannot run. Empty when they can. Read-only."`
	BandwidthApplies    bool         `json:"bandwidth_applies" doc:"bandwidth_mbps limits sandboxes: only VMs have the limit, so the mode setting or a server is in VM mode. Read-only."`
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

func (h *Handler) settingsOut(ctx context.Context, st store.Settings) (Settings, error) {
	servers, err := h.d.Store.ListServers(ctx)
	if err != nil {
		return Settings{}, err
	}
	mode := h.d.Settings.Mode(st)
	st.Mode = mode
	st.EgressAllowlist, st.DNSServers = orEmpty(st.EgressAllowlist), orEmpty(st.DNSServers)
	out := Settings{
		Settings: st, BackupS3: backupS3Out(st.BackupS3), SSHPort: h.sshPort(),
		PublicReposRefusal: policy.PublicRepos(mode, servers), BandwidthApplies: policy.BandwidthApplies(mode, servers),
	}
	out.PublicReposPossible = out.PublicReposRefusal == ""
	if h.d.HTTPS != nil {
		s := h.d.HTTPS.Status(ctx)
		out.HTTPS = &HTTPSStatus{State: s.State, Error: s.Error}
		if !s.NotAfter.IsZero() {
			out.HTTPS.NotAfter = &s.NotAfter
		}
	}
	return out, nil
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
	return reply(h.settingsOut(ctx, st))
}

// SettingsPatch changes some settings. Its keys are the keys of
// store.Settings, with their limits, except that backup_s3 is a
// BackupS3Patch. The handler reads the keys from the raw body.
type SettingsPatch struct{}

func (SettingsPatch) TransformSchema(r huma.Registry, s *huma.Schema) *huma.Schema {
	t := reflect.TypeFor[store.Settings]()
	s.Properties = map[string]*huma.Schema{}
	for i := range t.NumField() {
		f := t.Field(i)
		key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if key == "backup_s3" {
			s.Properties[key] = r.Schema(reflect.TypeFor[BackupS3Patch](), true, "")
			continue
		}
		s.Properties[key] = huma.SchemaFromField(r, f, "")
	}
	return s
}

type BackupS3Patch struct {
	Endpoint  *string `json:"endpoint,omitempty" maxLength:"2048"`
	Region    *string `json:"region,omitempty" maxLength:"64"`
	Bucket    *string `json:"bucket,omitempty" maxLength:"63"`
	Prefix    *string `json:"prefix,omitempty" maxLength:"256"`
	AccessKey *string `json:"access_key,omitempty" maxLength:"256"`
	SecretKey *string `json:"secret_key,omitempty" maxLength:"256" doc:"Write-only. An empty string removes the key."`
}

func (h *Handler) updateSettings(ctx context.Context, in *struct {
	Body    SettingsPatch
	RawBody []byte
}) (*body[Settings], error) {
	next, err := h.d.Settings.Update(ctx, actor(ctx), func(st *store.Settings) error {
		return applyPatch(st, in.RawBody, h.d.Box)
	})
	if err != nil {
		return nil, changeErr(err)
	}
	return reply(h.settingsOut(ctx, next))
}

// applyPatch sets the keys of a SettingsPatch on st. The secret key of S3
// replaces the sealed key; an empty key removes it.
func applyPatch(st *store.Settings, patch []byte, box *secret.Box) error {
	var s3 struct {
		BackupS3 struct {
			SecretKey *string `json:"secret_key"`
		} `json:"backup_s3"`
	}
	if err := json.Unmarshal(patch, &s3); err != nil {
		return err
	}
	if err := json.Unmarshal(patch, st); err != nil {
		return err
	}
	if k := s3.BackupS3.SecretKey; k != nil {
		st.BackupS3.SecretKeyEnc = nil
		if *k != "" {
			st.BackupS3.SecretKeyEnc = box.Seal([]byte(*k))
		}
	}
	return nil
}
