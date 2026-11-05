package fakegh

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func (s *Server) routes() {
	// REST is at the root, as on api.github.com, and under /api/v3, as on
	// GitHub Enterprise Server.
	rest := func(pattern string, h http.HandlerFunc) {
		method, path, _ := strings.Cut(pattern, " ")
		s.mux.HandleFunc(pattern, h)
		s.mux.HandleFunc(method+" /api/v3"+path, h)
	}
	rest("GET /app/installations", appAuth(s.listInstallations))
	rest("GET /app/installations/{id}", appAuth(s.getInstallation))
	rest("POST /app/installations/{id}/access_tokens", appAuth(s.createAccessToken))
	rest("POST /app-manifests/{code}/conversions", s.convertManifest)
	rest("GET /installation/repositories", s.listRepos)
	rest("POST /orgs/{org}/actions/runners/registration-token", s.registrationToken)
	rest("POST /repos/{owner}/{repo}/actions/runners/registration-token", s.registrationToken)
	rest("POST /actions/runner-registration", s.runnerRegistration)
	rest("GET /orgs/{org}/actions/runners", s.listRunners)
	rest("GET /repos/{owner}/{repo}/actions/runners", s.listRunners)
	rest("GET /repos/{owner}/{repo}", s.getRepo)
	rest("GET /repos/{owner}/{repo}/actions/runs/{run}", s.getRun)
	rest("GET /repos/{owner}/{repo}/actions/runs/{run}/jobs", s.listRunJobs)
	rest("GET /repos/{owner}/{repo}/actions/jobs/{job}", s.getJob)
	rest("GET /repos/{owner}/{repo}/actions/jobs/{job}/logs", s.jobLogs)
	rest("POST /repos/{owner}/{repo}/actions/jobs/{job}/rerun", s.rerunJob)
	rest("GET /repos/actions/runner/releases/latest", s.getRelease)
	rest("GET /repos/actions/runner/releases/tags/{tag}", s.getRelease)

	// Web pages and downloads of github.com.
	s.mux.HandleFunc("POST /settings/apps/new", s.newApp)
	s.mux.HandleFunc("POST /organizations/{org}/settings/apps/new", s.newApp)
	s.mux.HandleFunc("GET /apps/{slug}/installations/new", s.installApp)
	s.mux.HandleFunc("GET /actions/runner/releases/download/{tag}/{file}", s.downloadRunner)

	s.actionsRoutes()
	s.runnerRoutes()
	s.controlRoutes()
}

// appAuth lets only requests with an app JWT through.
func appAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := checkAppJWT(r); err != nil {
			restError(w, http.StatusUnauthorized, err.Error())
			return
		}
		h(w, r)
	}
}

var appPermissions = map[string]string{
	"actions":                          "write",
	"administration":                   "write",
	"metadata":                         "read",
	"organization_self_hosted_runners": "write",
}

// appJSON returns the app. Its owner is the account of the first
// installation.
func (s *Server) appJSON(r *http.Request) map[string]any {
	owner := accountJSON(r, "acme", "Organization")
	if len(s.installs) > 0 {
		owner = accountJSON(r, s.installs[0].Account, s.installs[0].AccountType)
	}
	return map[string]any{
		"id":                  AppID,
		"client_id":           AppClientID,
		"slug":                s.app.slug,
		"node_id":             "A_fake" + itoa(AppID),
		"name":                s.app.name,
		"owner":               owner,
		"description":         "",
		"external_url":        baseURL(r),
		"html_url":            baseURL(r) + "/apps/" + s.app.slug,
		"created_at":          "2026-01-01T00:00:00Z",
		"updated_at":          "2026-01-01T00:00:00Z",
		"permissions":         appPermissions,
		"events":              []string{},
		"installations_count": len(s.installs),
	}
}

func (s *Server) installationJSON(r *http.Request, inst *installation) map[string]any {
	base := baseURL(r)
	return map[string]any{
		"id":                   inst.ID,
		"account":              accountJSON(r, inst.Account, inst.AccountType),
		"repository_selection": inst.selection(),
		"access_tokens_url":    fmt.Sprintf("%s/app/installations/%d/access_tokens", base, inst.ID),
		"repositories_url":     base + "/installation/repositories",
		"html_url":             fmt.Sprintf("%s/settings/installations/%d", base, inst.ID),
		"app_id":               AppID,
		"app_slug":             s.app.slug,
		"client_id":            AppClientID,
		"target_id":            accountID(inst.Account),
		"target_type":          inst.AccountType,
		"permissions":          appPermissions,
		"events":               []string{},
		"created_at":           "2026-01-01T00:00:00Z",
		"updated_at":           "2026-01-01T00:00:00Z",
		"suspended_at":         nil,
		"suspended_by":         nil,
	}
}

func (s *Server) listInstallations(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []any
	for _, inst := range s.installs {
		out = append(out, s.installationJSON(r, inst))
	}
	writeJSON(w, http.StatusOK, page(w, r, out))
}

func (s *Server) getInstallation(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	inst := s.install(id)
	if inst == nil {
		restError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.installationJSON(r, inst))
}

func (s *Server) createAccessToken(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	inst := s.install(id)
	if inst == nil {
		restError(w, http.StatusNotFound, "Not Found")
		return
	}
	t := &token{kind: installToken, install: inst}
	v := s.issue(t)
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":                v,
		"expires_at":           t.expires.UTC().Format(time.RFC3339),
		"permissions":          appPermissions,
		"repository_selection": inst.selection(),
	})
}

// installation returns the installation of the token, or writes 401.
func (s *Server) installation(w http.ResponseWriter, r *http.Request) *installation {
	t := s.auth(r, installToken)
	if t == nil {
		restError(w, http.StatusUnauthorized, "Bad credentials")
		return nil
	}
	return t.install
}

// repoOf returns the repository of the path if the token can read it, or
// writes the error.
func (s *Server) repoOf(w http.ResponseWriter, r *http.Request) *repo {
	inst := s.installation(w, r)
	if inst == nil {
		return nil
	}
	rp := s.repos[strings.ToLower(r.PathValue("owner")+"/"+r.PathValue("repo"))]
	if rp == nil || rp.install != inst {
		restError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return rp
}

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.installation(w, r)
	if inst == nil {
		return
	}
	var repos []any
	for _, rp := range inst.repos {
		repos = append(repos, s.repoJSON(r, rp, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count":          len(repos),
		"repository_selection": inst.selection(),
		"repositories":         page(w, r, repos),
	})
}

func (s *Server) getRepo(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rp := s.repoOf(w, r); rp != nil {
		writeJSON(w, http.StatusOK, s.repoJSON(r, rp, false))
	}
}

// repoJSON returns the repository, or with fork its fork by the account
// "contributor".
func (s *Server) repoJSON(r *http.Request, rp *repo, fork bool) map[string]any {
	owner, name := rp.owner(), strings.SplitN(rp.name, "/", 2)[1]
	ownerType := "User"
	if rp.install != nil {
		ownerType = rp.install.AccountType
	}
	id := rp.id
	if fork {
		owner, ownerType, id = "contributor", "User", rp.id+1_000_000
	}
	full := owner + "/" + name
	visibility := "public"
	if rp.private {
		visibility = "private"
	}
	return map[string]any{
		"id":             id,
		"node_id":        "R_fake" + itoa(id),
		"name":           name,
		"full_name":      full,
		"private":        rp.private,
		"visibility":     visibility,
		"owner":          accountJSON(r, owner, ownerType),
		"html_url":       baseURL(r) + "/" + full,
		"url":            baseURL(r) + "/repos/" + full,
		"description":    nil,
		"fork":           fork,
		"archived":       false,
		"disabled":       false,
		"default_branch": "main",
	}
}

func accountJSON(r *http.Request, login, kind string) map[string]any {
	id := accountID(login)
	return map[string]any{
		"login":      login,
		"id":         id,
		"node_id":    "U_fake" + itoa(id),
		"type":       kind,
		"avatar_url": fmt.Sprintf("https://avatars.githubusercontent.com/u/%d?v=4", id),
		"html_url":   baseURL(r) + "/" + login,
	}
}

// page applies per_page and page to items and sets the Link header, as the
// list endpoints of GitHub do.
func page(w http.ResponseWriter, r *http.Request, items []any) []any {
	q := r.URL.Query()
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per <= 0 {
		per = 30
	}
	per = min(per, 100)
	n, _ := strconv.Atoi(q.Get("page"))
	n = max(n, 1)
	last := max((len(items)+per-1)/per, 1)
	link := func(p int, rel string) string {
		q.Set("page", strconv.Itoa(p))
		return fmt.Sprintf("<%s%s?%s>; rel=%q", baseURL(r), r.URL.Path, q.Encode(), rel)
	}
	var links []string
	if n < last {
		links = append(links, link(n+1, "next"), link(last, "last"))
	}
	if n > 1 {
		links = append(links, link(1, "first"), link(n-1, "prev"))
	}
	if len(links) > 0 {
		w.Header().Set("Link", strings.Join(links, ", "))
	}
	from := min((n-1)*per, len(items))
	out := items[from:min(from+per, len(items))]
	if out == nil {
		out = []any{}
	}
	return out
}

// scopeOf returns the runner scope of the path: an organization of the
// installation or one of its repositories. Else it writes 404.
func (s *Server) scopeOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	org := r.PathValue("org")
	if org == "" {
		rp := s.repoOf(w, r)
		if rp == nil {
			return "", false
		}
		return strings.ToLower(rp.name), true
	}
	inst := s.installation(w, r)
	if inst == nil {
		return "", false
	}
	if inst.AccountType != "Organization" || !strings.EqualFold(inst.Account, org) {
		restError(w, http.StatusNotFound, "Not Found")
		return "", false
	}
	return strings.ToLower(org), true
}

// registrationToken makes a runner registration token for an organization
// or a repository of the installation.
func (s *Server) registrationToken(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope, ok := s.scopeOf(w, r)
	if !ok {
		return
	}
	t := &token{kind: registrationToken, scope: scope}
	v := s.issue(t)
	writeJSON(w, http.StatusCreated, map[string]any{"token": v, "expires_at": t.expires.UTC().Format(time.RFC3339)})
}

// runnerRegistration trades a registration token for the Actions service
// URL and an admin token for the scope of the config URL.
func (s *Server) runnerRegistration(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.auth(r, registrationToken)
	if t == nil {
		restError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	var in struct {
		URL         string `json:"url"`
		RunnerEvent string `json:"runner_event"`
	}
	if err := readJSON(r, &in); err != nil {
		restError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	u, err := url.Parse(in.URL)
	if err != nil || strings.ToLower(strings.Trim(u.Path, "/")) != t.scope {
		restError(w, http.StatusNotFound, "The registration token is not for "+in.URL)
		return
	}
	admin := s.issue(&token{kind: adminToken, scope: t.scope})
	writeJSON(w, http.StatusOK, map[string]any{"url": baseURL(r) + "/", "token": admin, "token_schema": "OAuthAccessToken"})
}

func (s *Server) listRunners(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope, ok := s.scopeOf(w, r)
	if !ok {
		return
	}
	var runners []any
	for _, rn := range s.runnerList() {
		if rn.set.scope != scope {
			continue
		}
		status := "offline"
		if rn.online {
			status = "online"
		}
		var labels []any
		for i, l := range rn.set.labels {
			labels = append(labels, map[string]any{"id": i + 1, "name": l, "type": "read-only"})
		}
		runners = append(runners, map[string]any{
			"id": rn.id, "name": rn.name, "os": "Linux", "status": status, "busy": rn.job != nil,
			"ephemeral": true, "runner_group_id": defaultGroupID, "labels": labels,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runners), "runners": page(w, r, runners)})
}

// runOf returns the run of the path, or writes 404.
func (s *Server) runOf(w http.ResponseWriter, r *http.Request) *run {
	rp := s.repoOf(w, r)
	if rp == nil {
		return nil
	}
	id, _ := strconv.ParseInt(r.PathValue("run"), 10, 64)
	rn := s.runs[id]
	if rn == nil || rn.repo != rp {
		restError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return rn
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rn := s.runOf(w, r); rn != nil {
		writeJSON(w, http.StatusOK, s.runJSON(r, rn.latest()))
	}
}

// runJSON returns the run as it was at the attempt of j.
func (s *Server) runJSON(r *http.Request, j *job) map[string]any {
	rn, base := j.run, baseURL(r)
	full := rn.repo.name
	status, conclusion := restStatus(j)
	actor := accountJSON(r, rn.spec.Actor, "User")
	headRepo := s.repoJSON(r, rn.repo, rn.spec.Fork)
	// GitHub lists no pull requests for a run from a fork.
	prs := []any{}
	if rn.spec.PullRequest > 0 && !rn.spec.Fork {
		repoRef := map[string]any{"id": rn.repo.id, "name": headRepo["name"], "url": base + "/repos/" + full}
		prs = append(prs, map[string]any{
			"id":     int64(rn.spec.PullRequest) + 2_000_000_000,
			"number": rn.spec.PullRequest,
			"url":    fmt.Sprintf("%s/repos/%s/pulls/%d", base, full, rn.spec.PullRequest),
			"head":   map[string]any{"ref": rn.spec.Branch, "sha": rn.sha, "repo": repoRef},
			"base":   map[string]any{"ref": "main", "sha": strings.Repeat("0", 40), "repo": repoRef},
		})
	}
	commitAuthor := map[string]any{"name": rn.spec.Actor, "email": rn.spec.Actor + "@users.noreply.github.com"}
	title, _, _ := strings.Cut(rn.spec.CommitMessage, "\n")
	return map[string]any{
		"id":                 rn.id,
		"name":               rn.spec.Workflow,
		"node_id":            "WFR_fake" + itoa(rn.id),
		"head_branch":        rn.spec.Branch,
		"head_sha":           rn.sha,
		"path":               ".github/workflows/" + rn.spec.WorkflowFile,
		"display_title":      title,
		"run_number":         rn.number,
		"event":              rn.spec.Event,
		"status":             status,
		"conclusion":         conclusion,
		"workflow_id":        accountID(full + "/" + rn.spec.WorkflowFile),
		"url":                fmt.Sprintf("%s/repos/%s/actions/runs/%d", base, full, rn.id),
		"html_url":           fmt.Sprintf("%s/%s/actions/runs/%d", base, full, rn.id),
		"jobs_url":           fmt.Sprintf("%s/repos/%s/actions/runs/%d/jobs", base, full, rn.id),
		"logs_url":           fmt.Sprintf("%s/repos/%s/actions/runs/%d/logs", base, full, rn.id),
		"pull_requests":      prs,
		"created_at":         stamp(rn.created),
		"updated_at":         stamp(lastChange(j)),
		"run_attempt":        j.attempt,
		"run_started_at":     stamp(j.queued),
		"actor":              actor,
		"triggering_actor":   actor,
		"head_commit":        map[string]any{"id": rn.sha, "tree_id": rn.sha, "message": rn.spec.CommitMessage, "timestamp": stamp(rn.created), "author": commitAuthor, "committer": commitAuthor},
		"repository":         s.repoJSON(r, rn.repo, false),
		"head_repository":    headRepo,
		"head_repository_id": headRepo["id"],
	}
}

func (s *Server) listRunJobs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rn := s.runOf(w, r)
	if rn == nil {
		return
	}
	jobs := []*job{rn.latest()}
	if r.URL.Query().Get("filter") == "all" {
		jobs = rn.jobs
	}
	var out []any
	for _, j := range jobs {
		out = append(out, jobJSON(r, j))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(out), "jobs": page(w, r, out)})
}

// jobOf returns the job of the path, or writes 404.
func (s *Server) jobOf(w http.ResponseWriter, r *http.Request) *job {
	rp := s.repoOf(w, r)
	if rp == nil {
		return nil
	}
	id, _ := strconv.ParseInt(r.PathValue("job"), 10, 64)
	j := s.jobByID(id)
	if j == nil || j.run.repo != rp {
		restError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return j
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j := s.jobOf(w, r); j != nil {
		writeJSON(w, http.StatusOK, jobJSON(r, j))
	}
}

func jobJSON(r *http.Request, j *job) map[string]any {
	rn, base := j.run, baseURL(r)
	full := rn.repo.name
	status, conclusion := restStatus(j)
	var runnerID, runnerName, groupID, groupName any
	if j.runnerID != 0 {
		runnerID, runnerName, groupID, groupName = j.runnerID, j.runnerName, defaultGroupID, "Default"
	}
	return map[string]any{
		"id":                j.id,
		"run_id":            rn.id,
		"workflow_name":     rn.spec.Workflow,
		"head_branch":       rn.spec.Branch,
		"run_url":           fmt.Sprintf("%s/repos/%s/actions/runs/%d", base, full, rn.id),
		"run_attempt":       j.attempt,
		"node_id":           "CR_fake" + itoa(j.id),
		"head_sha":          rn.sha,
		"url":               fmt.Sprintf("%s/repos/%s/actions/jobs/%d", base, full, j.id),
		"html_url":          fmt.Sprintf("%s/%s/actions/runs/%d/job/%d", base, full, rn.id, j.id),
		"status":            status,
		"conclusion":        conclusion,
		"created_at":        stamp(j.queued),
		"started_at":        stamp(j.queued),
		"completed_at":      stampOrNil(j.finished),
		"name":              rn.spec.Name,
		"steps":             stepsJSON(j),
		"check_run_url":     fmt.Sprintf("%s/repos/%s/check-runs/%d", base, full, j.id),
		"labels":            rn.spec.Labels,
		"runner_id":         runnerID,
		"runner_name":       runnerName,
		"runner_group_id":   groupID,
		"runner_group_name": groupName,
	}
}

// restStatus maps the job state to the REST status and conclusion.
func restStatus(j *job) (status string, conclusion any) {
	switch j.state {
	case JobRunning:
		return "in_progress", nil
	case JobCompleted:
		return "completed", map[string]string{
			ResultSucceeded: "success",
			ResultFailed:    "failure",
			ResultCanceled:  "cancelled",
		}[j.result]
	}
	return "queued", nil
}

// stepsJSON lists "Set up job", the steps of the spec, and "Complete job".
// The step times follow the sleeps of the steps.
func stepsJSON(j *job) []any {
	if j.started.IsZero() {
		return []any{}
	}
	now, end := time.Now(), j.finished
	if end.IsZero() {
		end = now
	}
	type step struct {
		name       string
		start, end time.Time
		fail       bool
	}
	t := j.started
	steps := []step{{name: "Set up job", start: t, end: t}}
	for _, sp := range j.run.spec.Steps {
		next := t.Add(time.Duration(sp.Sleep * float64(time.Second)))
		steps = append(steps, step{sp.Name, earliest(t, end), earliest(next, end), sp.Fail})
		t = next
	}
	steps = append(steps, step{name: "Complete job", start: earliest(t, end), end: end})

	running := j.state != JobCompleted
	failed := false
	var out []any
	for i, st := range steps {
		m := map[string]any{
			"name": st.name, "number": i + 1, "status": "completed", "conclusion": "success",
			"started_at": stamp(st.start), "completed_at": stamp(st.end),
		}
		switch {
		case running && (i == len(steps)-1 || st.start.After(now)):
			m["status"], m["conclusion"], m["started_at"], m["completed_at"] = "queued", nil, nil, nil
		case running && st.end.After(now):
			m["status"], m["conclusion"], m["completed_at"] = "in_progress", nil, nil
		case failed && i < len(steps)-1:
			m["conclusion"] = "skipped"
		case st.fail:
			m["conclusion"], failed = "failure", true
		}
		out = append(out, m)
	}
	return out
}

func lastChange(j *job) time.Time {
	t := j.queued
	for _, u := range []time.Time{j.acquired, j.started, j.finished} {
		if u.After(t) {
			t = u
		}
	}
	return t
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func stampOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return stamp(t)
}

// jobLogs sends the client to the log file, as GitHub does with a short
// lived blob URL. Logs exist after the job completes.
func (s *Server) jobLogs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobOf(w, r)
	if j == nil {
		return
	}
	if j.log == nil {
		restError(w, http.StatusNotFound, "Not Found")
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s/_fake/logs/%d", baseURL(r), j.id), http.StatusFound)
}

// rerunJob queues the next attempt of a completed job.
func (s *Server) rerunJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobOf(w, r)
	if j == nil {
		return
	}
	if j.state != JobCompleted || j != j.run.latest() {
		restError(w, http.StatusForbidden, "This workflow job cannot be rerun.")
		return
	}
	s.newJob(j.run)
	writeJSON(w, http.StatusCreated, map[string]any{})
}

var (
	tagRE  = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	slugRE = regexp.MustCompile(`[^a-z0-9]+`)
)

// getRelease returns an actions/runner release. Each version has the fake
// runner as its archive, with the SHA-256 markers of the real release notes.
func (s *Server) getRelease(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	if tag == "" {
		tag = "v" + s.cfg.RunnerVersion
	}
	if !tagRE.MatchString(tag) {
		restError(w, http.StatusNotFound, "Not Found")
		return
	}
	version, base := tag[1:], baseURL(r)
	notes := "## What's Changed\n* Fake release of fakegh\n"
	assets := []any{}
	if data, sum, err := s.runnerArchive(); err == nil {
		for _, arch := range []string{"x64", "arm64"} {
			name := "actions-runner-linux-" + arch + "-" + version + ".tar.gz"
			notes += fmt.Sprintf("- linux-%s <!-- BEGIN SHA linux-%s -->%s<!-- END SHA linux-%s -->\n", arch, arch, sum, arch)
			assets = append(assets, map[string]any{
				"name":                 name,
				"content_type":         "application/gzip",
				"state":                "uploaded",
				"size":                 len(data),
				"digest":               "sha256:" + sum,
				"browser_download_url": fmt.Sprintf("%s/actions/runner/releases/download/%s/%s", base, tag, name),
			})
		}
	} else if s.cfg.RunnerDir != "" {
		s.log.Error("read the fake runner bundle", "err", err)
	}
	published := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           accountID(tag),
		"tag_name":     tag,
		"name":         tag,
		"draft":        false,
		"prerelease":   false,
		"created_at":   stamp(published),
		"published_at": stamp(published),
		"html_url":     base + "/actions/runner/releases/tag/" + tag,
		"body":         notes,
		"assets":       assets,
	})
}

func (s *Server) downloadRunner(w http.ResponseWriter, r *http.Request) {
	tag, file := r.PathValue("tag"), r.PathValue("file")
	data, _, err := s.runnerArchive()
	if err != nil || !tagRE.MatchString(tag) ||
		(file != "actions-runner-linux-x64-"+tag[1:]+".tar.gz" && file != "actions-runner-linux-arm64-"+tag[1:]+".tar.gz") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

type manifest struct {
	Name     string `json:"name"`
	Redirect string `json:"redirect_url"`
	SetupURL string `json:"setup_url"`
}

// newApp takes the manifest form that the browser posts and returns at once
// to the redirect URL with a code, as GitHub does after the user confirms.
func (s *Server) newApp(w http.ResponseWriter, r *http.Request) {
	var m manifest
	if err := json.Unmarshal([]byte(r.PostFormValue("manifest")), &m); err != nil || m.Redirect == "" {
		http.Error(w, "The manifest is not valid JSON or has no redirect_url.", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(m.Redirect)
	if err != nil {
		http.Error(w, "Bad redirect_url.", http.StatusBadRequest)
		return
	}
	code := strings.ToLower(rand.Text())
	s.mu.Lock()
	s.codes[code] = m
	s.mu.Unlock()
	q := u.Query()
	q.Set("code", code)
	if state := r.URL.Query().Get("state"); state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// convertManifest returns the app with its secrets and private key for a
// manifest code. A code works one time.
func (s *Server) convertManifest(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.codes[r.PathValue("code")]
	if !ok {
		restError(w, http.StatusNotFound, "Not Found")
		return
	}
	delete(s.codes, r.PathValue("code"))
	if m.Name != "" {
		s.app.name = m.Name
		s.app.slug = strings.Trim(slugRE.ReplaceAllString(strings.ToLower(m.Name), "-"), "-")
	}
	s.app.setupURL = m.SetupURL
	out := s.appJSON(r)
	out["client_secret"] = strings.ToLower(rand.Text() + rand.Text())
	out["webhook_secret"] = nil
	out["pem"] = string(s.keyPEM)
	writeJSON(w, http.StatusCreated, out)
}

// installApp installs the app on the first installation account and
// returns to the setup URL, as GitHub does after the user confirms.
func (s *Server) installApp(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	setup := s.app.setupURL
	var id int64
	if len(s.installs) > 0 {
		id = s.installs[0].ID
	}
	s.mu.Unlock()
	if setup == "" || id == 0 {
		fmt.Fprintln(w, "The app is installed.")
		return
	}
	u, err := url.Parse(setup)
	if err != nil {
		http.Error(w, "Bad setup_url.", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("installation_id", itoa(id))
	q.Set("setup_action", "install")
	if state := r.URL.Query().Get("state"); state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}
