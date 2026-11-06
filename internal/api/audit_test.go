package api

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

type okScheduler struct{}

func (okScheduler) StopSandbox(context.Context, string) error    { return nil }
func (okScheduler) RerunJob(context.Context, string, bool) error { return nil }

// The Activity sentences need the names and the old and new values of each
// change, and never a secret.
func TestAuditDetails(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Scheduler = okScheduler{} })
	b := e.login("ada@example.com", auth.RoleAdmin)
	ctx := context.Background()
	now := time.Now()
	for _, v := range []store.Server{
		{ID: "s1", Name: "fsn1-a", Kind: store.ServerAgent, Status: store.ServerOnline, CreatedAt: now, UpdatedAt: now},
		{ID: "s2", Name: "fsn1-b", Kind: store.ServerAgent, Status: store.ServerOnline, CreatedAt: now, UpdatedAt: now},
	} {
		must(t, e.st.CreateServer(ctx, v))
	}
	img := store.Image{ID: "img26", Name: "ubuntu-26.04", Ref: "builtin:ubuntu-26.04", Source: "builtin", CreatedAt: now, UpdatedAt: now}
	must(t, e.st.CreateImage(ctx, img))

	var pool Pool
	b.do("POST", "/api/pools", map[string]any{
		"label": "runplane-8cpu", "arch": "x64", "vcpu": 8, "memory_mb": 16384, "disk_gb": 40, "max_runners": 6, "server_ids": []string{"s1"},
	}, &pool).want(t, 200)
	change := map[string]any{"max_runners": 8, "image_id": "img26", "server_ids": []string{"s1", "s2"}}
	b.do("PATCH", "/api/pools/"+pool.ID, change, nil).want(t, 200)
	b.do("PATCH", "/api/pools/"+pool.ID, change, nil).want(t, 200) // no change, no entry
	b.do("PATCH", "/api/settings", map[string]any{
		"cpu_overcommit": 1.5, "backup_s3": map[string]any{"bucket": "backups", "access_key": "AKIA1", "secret_key": "s3-secret-value"},
	}, nil).want(t, 200)
	var invite CreatedInvite
	b.do("POST", "/api/invites", map[string]any{"email": "eve@example.com", "role": "member"}, &invite).want(t, 200)
	b.do("DELETE", "/api/invites/"+invite.Invite.ID, nil, nil).want(t, 204)

	job, _, err := e.st.UpsertJob(ctx, store.JobMessage{
		RunnerRequestID: 7, State: store.JobCompleted, Result: store.ResultFailed, Repository: "acme/api", JobName: "build",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", QueuedAt: now,
	}, now)
	must(t, err)
	must(t, e.st.SetJobEnrichment(ctx, job.ID, store.JobEnrichment{RunNumber: 2290}, now))
	b.do("POST", "/api/jobs/"+job.ID+"/rerun", map[string]any{"debug": true}, nil).want(t, 204)
	must(t, e.st.InsertSandbox(ctx, store.Sandbox{
		ID: "sb1", PoolID: pool.ID, ServerID: "s2", RunnerName: "runplane-8cpu-7q2m", Mode: sandbox.ModeContainer,
		State: sandbox.StateBusy, JobID: job.ID, VCPU: 8, MemoryMB: 16384, CreatedAt: now,
	}))
	b.do("POST", "/api/sandboxes/sb1/stop", nil, nil).want(t, 204)

	res := b.do("GET", "/api/audit", nil, nil)
	res.want(t, 200)
	if strings.Contains(string(res.body), "s3-secret-value") || strings.Contains(string(res.body), "secret_key_enc") {
		t.Errorf("the audit log holds the S3 secret key: %s", res.body)
	}
	var page struct {
		Items []struct {
			Action string          `json:"action"`
			Target string          `json:"target"`
			Detail json.RawMessage `json:"detail"`
		} `json:"items"`
	}
	must(t, json.Unmarshal(res.body, &page))
	got := map[string][]string{}
	for _, item := range page.Items {
		got[item.Action] = append(got[item.Action], string(item.Detail))
	}
	for action, want := range map[string]string{
		"pool.update": `{"label":"runplane-8cpu","changes":{"max_runners":[6,8],"image":["","ubuntu-26.04"],
			"servers":[["fsn1-a"],["fsn1-a","fsn1-b"]]}}`,
		"settings.update": `{"keys":["backup_s3","cpu_overcommit"],"changes":{"cpu_overcommit":[1,1.5],
			"backup_s3.bucket":["","backups"],"backup_s3.access_key":["","AKIA1"],"backup_s3.secret_key_set":[false,true]}}`,
		"invite.delete": `{"email":"eve@example.com","role":"member"}`,
		"job.rerun": `{"debug":true,"repository":"acme/api","workflow_file":".github/workflows/ci.yml","job_name":"build",
			"run_number":2290}`,
		"sandbox.stop": `{"runner_name":"runplane-8cpu-7q2m","job_id":"` + job.ID + `","repository":"acme/api",
			"workflow_file":".github/workflows/ci.yml","job_name":"build","run_number":2290}`,
	} {
		if len(got[action]) != 1 {
			t.Errorf("%s: %d entries %v, want 1", action, len(got[action]), got[action])
			continue
		}
		if !sameJSON(t, got[action][0], want) {
			t.Errorf("%s detail:\n got %s\nwant %s", action, got[action][0], want)
		}
	}
}

func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var va, vb any
	must(t, json.Unmarshal([]byte(a), &va))
	must(t, json.Unmarshal([]byte(b), &vb))
	return reflect.DeepEqual(va, vb)
}
