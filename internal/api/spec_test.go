package api

import (
	"context"
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/runplane/runplane/images"
	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/fleet"
	"github.com/runplane/runplane/internal/ghapp"
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/runner"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/tlsutil"
	"github.com/runplane/runplane/internal/update"
)

// specSchema is the part of a schema of the spec that the tests read.
type specSchema struct {
	Enum       []string              `json:"enum"`
	Pattern    string                `json:"pattern"`
	MinLength  *int                  `json:"minLength"`
	Items      *specSchema           `json:"items"`
	Properties map[string]specSchema `json:"properties"`
}

type spec struct {
	Components struct {
		Schemas map[string]specSchema `json:"schemas"`
	} `json:"components"`
	Paths map[string]map[string]struct {
		Parameters []struct {
			Name   string     `json:"name"`
			In     string     `json:"in"`
			Schema specSchema `json:"schema"`
		} `json:"parameters"`
		Responses map[string]struct {
			Headers map[string]struct {
				Schema specSchema `json:"schema"`
			} `json:"headers"`
		} `json:"responses"`
	} `json:"paths"`
}

func readSpec(t *testing.T) spec {
	t.Helper()
	raw, err := OpenAPI()
	must(t, err)
	var s spec
	must(t, json.Unmarshal(raw, &s))
	return s
}

// enums returns each enum of the spec: "Schema.key" for the keys of the
// schemas, and "method path name" for the parameters and headers.
func (s spec) enums() map[string][]string {
	out := map[string][]string{}
	for name, schema := range s.Components.Schemas {
		for key, p := range schema.Properties {
			if p.Items != nil {
				p = *p.Items
			}
			if len(p.Enum) > 0 {
				out[name+"."+key] = p.Enum
			}
		}
	}
	for path, ops := range s.Paths {
		for method, op := range ops {
			for _, p := range op.Parameters {
				if len(p.Schema.Enum) > 0 {
					out[method+" "+path+" "+p.Name] = p.Schema.Enum
				}
			}
			for _, r := range op.Responses {
				for name, h := range r.Headers {
					if len(h.Schema.Enum) > 0 {
						out[method+" "+path+" "+name] = h.Schema.Enum
					}
				}
			}
		}
	}
	return out
}

func strs[T ~string](list ...T) []string {
	out := make([]string, len(list))
	for i, v := range list {
		out[i] = string(v)
	}
	return out
}

// The web takes the enums of the spec as the values that the API sends and
// accepts. Each enum must have the values of the Go code.
func TestSpecEnumsHaveTheGoValues(t *testing.T) {
	var (
		roles         = auth.Roles
		arches        = runner.Arches
		modes         = strs(sandbox.ModeContainer, sandbox.ModeVM, sandbox.ModeDev)
		sandboxStates = strs(append(sandbox.LiveStates(), sandbox.DoneStates()...)...)
		jobStates     = []string{store.JobQueued, store.JobAssigned, store.JobRunning, store.JobCompleted}
		results       = []string{store.ResultSucceeded, store.ResultFailed, store.ResultCancelled, store.ResultSkipped}
		jobStatuses   = append([]string{store.StatusQueued, store.StatusRunning}, results...)
		rangeKeys     = slices.Collect(maps.Keys(ranges))
		kinds         = notify.Kinds
		types         = strs(notify.Types...)
		serverKinds   = []string{store.ServerLocal, store.ServerAgent}
		serverStates  = []string{store.ServerOnline, store.ServerInstalling, store.ServerUnreachable, store.ServerDraining, store.ServerDisabled}
		permissions   = map[string]bool{}
		accesses      = map[string]bool{}
	)
	for _, owner := range []ghapp.Account{{Type: ghapp.Organization}, {Type: ghapp.User}} {
		for _, p := range owner.AppPermissions() {
			permissions[p.Name], accesses[p.Access] = true, true
		}
	}
	want := map[string][]string{
		"AppPermission.access":                       slices.Collect(maps.Keys(accesses)),
		"AppPermission.name":                         slices.Collect(maps.Keys(permissions)),
		"AuditEntry.action":                          audit.Actions,
		"Backup.status":                              {store.BackupRunning, store.BackupDone, store.BackupUploading, store.BackupUploaded, store.BackupFailed},
		"Check.status":                               strs(doctor.StatusOK, doctor.StatusWarn, doctor.StatusFail),
		"CreateInviteRequest.role":                   roles,
		"CreateNotificationRequest.events":           types,
		"CreateNotificationRequest.kind":             kinds,
		"CreatePoolRequest.arch":                     arches,
		"DomainDNS.state":                            {dnsNone, dnsOK, dnsMissing, dnsOther, dnsUnknown, dnsError},
		"HTTPSStatus.state":                          {tlsutil.CertNone, tlsutil.CertRequesting, tlsutil.CertValid, tlsutil.CertError},
		"HostCheck.arch":                             arches,
		"HostCheck.mode":                             modes,
		"Image.source":                               {"registry", images.Source},
		"ImageBuild.status":                          {store.BuildPending, store.BuildPulling, store.BuildBuilding, store.BuildConverting, store.BuildReady, store.BuildFailed},
		"Installation.account_type":                  {ghapp.Organization, ghapp.User},
		"Installation.repository_selection":          {"all", "selected"}, // GitHub sets them
		"Invite.role":                                roles,
		"InviteInfo.role":                            roles,
		"Job.log_state":                              {store.LogNone, store.LogLive, store.LogStored, store.LogExpired},
		"Job.result":                                 results,
		"Job.sandbox_state":                          append(sandboxStates, ""),
		"Job.state":                                  jobStates,
		"Job.status":                                 jobStatuses,
		"JobSummary.result":                          results,
		"JobSummary.state":                           jobStates,
		"JobSummary.status":                          jobStatuses,
		"ManifestRequest.account_type":               {"organization", "user"}, // github.Service.Manifest takes them
		"NotificationChannel.kind":                   kinds,
		"Overview.range":                             rangeKeys,
		"Pool.arch":                                  arches,
		"RunnerVersion.arch":                         arches,
		"RunnerVersion.status":                       {store.BundlePending, store.BundleReady, store.BundleFailed},
		"Sandbox.state":                              sandboxStates,
		"ScaleSetStatus.status":                      {store.ScaleSetCreating, store.ScaleSetListening, store.ScaleSetError, store.ScaleSetDeleting},
		"Server.kind":                                serverKinds,
		"Server.status":                              serverStates,
		"Server.update_state":                        strs(fleet.UpdateCurrent, fleet.UpdateAvailable, fleet.UpdateUpdating, fleet.UpdateFailed, fleet.UpdateNone),
		"ServerSummary.kind":                         serverKinds,
		"ServerSummary.status":                       serverStates,
		"Session.role":                               roles,
		"Settings.mode":                              modes,
		"SettingsPatch.mode":                         modes,
		"SizeAdvice.kind":                            {adviceLarger, adviceSmaller, adviceNone},
		"Status.mode":                                modes,
		"TestChannelRequest.kind":                    kinds,
		"UpdateNotificationRequest.events":           types,
		"UpdateNotificationRequest.kind":             kinds,
		"UpdatePoolRequest.arch":                     arches,
		"UpdateServerRequest.status":                 {store.ServerOnline, store.ServerDraining, store.ServerDisabled}, // the statuses that an admin sets
		"UpdateStatus.state":                         strs(update.Idle, update.Applying, update.Restarting),
		"UpdateUserRequest.role":                     roles,
		"User.role":                                  roles,
		"get /api/jobs status":                       jobStatuses,
		"get /api/jobs range":                        rangeKeys,
		"get /api/jobs/{id}/log " + stepFilterHeader: {stepFilterIgnored},
		"get /api/overview range":                    rangeKeys,
		"get /api/pools/placement arch":              arches,
		"get /api/sandboxes state":                   sandboxStates,
	}
	got := readSpec(t).enums()
	for key, values := range got {
		w, ok := want[key]
		if !ok {
			t.Errorf("the enum %s %q has no Go values in this test", key, values)
			continue
		}
		if !sameSet(values, w) {
			t.Errorf("the enum %s is %q, but the Go values are %q", key, values, w)
		}
	}
	for key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("the spec has no enum %s", key)
		}
	}
}

func sameSet(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// The forms read the password minimum and the setup code pattern from the
// spec, so the spec must have the rules of auth.
func TestSpecHasTheRulesOfAuth(t *testing.T) {
	s := readSpec(t)
	for _, f := range []struct{ schema, key string }{
		{"SetupRequest", "password"}, {"AcceptInviteRequest", "password"}, {"UpdateMeRequest", "new_password"},
	} {
		if min := s.Components.Schemas[f.schema].Properties[f.key].MinLength; min == nil || *min != auth.MinPasswordLen {
			t.Errorf("minLength of %s.%s is %v, want %d", f.schema, f.key, min, auth.MinPasswordLen)
		}
	}

	patterns := map[string]string{"SetupRequest.code": s.Components.Schemas["SetupRequest"].Properties["code"].Pattern}
	for _, p := range s.Paths["/api/setup/host"]["get"].Parameters {
		if p.Name == "code" {
			patterns["get /api/setup/host code"] = p.Schema.Pattern
		}
	}
	code, err := newEnv(t).d.Auth.NewSetupCode(context.Background())
	must(t, err)
	// Setup takes a code without dashes, with spaces, and in lowercase too.
	codes := []string{code, strings.ReplaceAll(code, "-", ""), strings.ReplaceAll(code, "-", " "), strings.ToLower(code)}
	for name, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		for _, c := range codes {
			if !re.MatchString(c) {
				t.Errorf("the pattern %s %q refuses the setup code %q", name, pattern, c)
			}
		}
	}
	if len(patterns) != 2 {
		t.Errorf("found the setup code patterns %v, want 2", patterns)
	}
}
