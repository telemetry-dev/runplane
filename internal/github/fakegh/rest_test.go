package fakegh_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/runplane/runplane/internal/github/fakegh"
	"github.com/runplane/runplane/internal/image"
)

// TestRunDataAfterJob reads what runplane needs after a job from the REST
// API: the run, the job steps, the log, and a re-run.
func TestRunDataAfterJob(t *testing.T) {
	needTools(t)
	f := start(t, fakegh.Config{})
	set := f.createScaleSet(t, "runplane")
	events := f.listen(t, set.ID, 1)
	req := f.srv.Queue(fakegh.JobSpec{
		Repo:          "acme/api",
		Name:          "build",
		Labels:        []string{"runplane"},
		Branch:        "fix-login",
		Actor:         "mona",
		CommitMessage: "Fix the login\n\nThe form sent the old token.",
		PullRequest:   7,
		Steps: []fakegh.Step{
			{Name: "Run make", Lines: []string{"compiling"}},
			{Name: "Run make test", Lines: []string{"FAIL example.com/login"}, Fail: true},
			{Name: "Run make deploy", Lines: []string{"deployed"}},
		},
	})
	f.startRunner(t, set.ID, "runplane-01")
	if got := events.wait(t, scaleset.MessageTypeJobCompleted, req).result; got != "failed" {
		t.Fatalf("result = %q, want failed", got)
	}
	job := f.job(t, req)
	api := f.api(t, 1)

	var run struct {
		RunNumber    int    `json:"run_number"`
		RunAttempt   int    `json:"run_attempt"`
		HeadBranch   string `json:"head_branch"`
		Event        string `json:"event"`
		DisplayTitle string `json:"display_title"`
		Conclusion   string `json:"conclusion"`
		Actor        struct {
			Login string `json:"login"`
		} `json:"actor"`
		HeadCommit struct {
			Message string `json:"message"`
		} `json:"head_commit"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	}
	api.get(t, fmt.Sprintf("/repos/acme/api/actions/runs/%d", job.RunID), &run)
	if run.RunNumber != 1 || run.RunAttempt != 1 || run.HeadBranch != "fix-login" || run.Event != "pull_request" ||
		run.DisplayTitle != "Fix the login" || run.Conclusion != "failure" || run.Actor.Login != "mona" ||
		!strings.HasPrefix(run.HeadCommit.Message, "Fix the login\n") || len(run.PullRequests) != 1 || run.PullRequests[0].Number != 7 {
		t.Errorf("run = %+v", run)
	}

	var jobs struct {
		Jobs []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			RunnerName string `json:"runner_name"`
			Steps      []struct {
				Name       string  `json:"name"`
				Conclusion string  `json:"conclusion"`
				Number     int     `json:"number"`
				Started    *string `json:"started_at"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	api.get(t, fmt.Sprintf("/repos/acme/api/actions/runs/%d/jobs", job.RunID), &jobs)
	if len(jobs.Jobs) != 1 || jobs.Jobs[0].ID != job.ID || jobs.Jobs[0].Conclusion != "failure" || jobs.Jobs[0].RunnerName != "runplane-01" {
		t.Fatalf("jobs = %+v", jobs)
	}
	var steps []string
	for _, s := range jobs.Jobs[0].Steps {
		steps = append(steps, fmt.Sprintf("%d %s: %s", s.Number, s.Name, s.Conclusion))
	}
	want := []string{"1 Set up job: success", "2 Run make: success", "3 Run make test: failure", "4 Run make deploy: skipped", "5 Complete job: success"}
	if strings.Join(steps, "\n") != strings.Join(want, "\n") {
		t.Errorf("steps:\n%s\nwant:\n%s", strings.Join(steps, "\n"), strings.Join(want, "\n"))
	}

	log := api.log(t, fmt.Sprintf("/repos/acme/api/actions/jobs/%d/logs", job.ID))
	if !bytes.HasPrefix(log, []byte("\xef\xbb\xbf")) {
		t.Errorf("the log has no byte order mark, as GitHub logs have")
	}
	stamp := `\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{7}Z `
	for _, re := range []string{`(?m)^` + stamp + `##\[group\]Run make test$`, `(?m)^` + stamp + `FAIL example\.com/login$`, `(?m)^` + stamp + `##\[error\]`} {
		if !regexp.MustCompile(re).Match(log) {
			t.Errorf("log has no line %s:\n%s", re, log)
		}
	}
	if bytes.Contains(log, []byte("deployed")) {
		t.Errorf("log has the output of a step after the failed step:\n%s", log)
	}

	api.post(t, fmt.Sprintf("/repos/acme/api/actions/jobs/%d/rerun", job.ID), http.StatusCreated)
	all := f.srv.Jobs()
	again := all[len(all)-1]
	if again.RunID != job.RunID || again.RunAttempt != 2 || again.RequestID == req {
		t.Fatalf("job after re-run = %+v, want attempt 2 of run %d", again, job.RunID)
	}
	events.wait(t, scaleset.MessageTypeJobAvailable, again.RequestID)
	api.post(t, fmt.Sprintf("/repos/acme/api/actions/jobs/%d/rerun", job.ID), http.StatusForbidden)
}

// TestPullRequestRuns checks the run fields that tell a fork pull request
// from others. Like GitHub, a run from a fork lists no pull requests.
func TestPullRequestRuns(t *testing.T) {
	f := start(t, fakegh.Config{})
	api := f.api(t, 1)
	tests := []struct {
		name     string
		spec     fakegh.JobSpec
		event    string
		prs      int
		headRepo string
		headFork bool
		repoPriv bool
		repoName string
	}{
		{"push", fakegh.JobSpec{Repo: "acme/api"}, "push", 0, "acme/api", false, true, "acme/api"},
		{"pull request", fakegh.JobSpec{Repo: "acme/api", PullRequest: 3}, "pull_request", 1, "acme/api", false, true, "acme/api"},
		{"fork pull request", fakegh.JobSpec{Repo: "acme/site", PullRequest: 4, Fork: true}, "pull_request", 0, "contributor/site", true, false, "acme/site"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.spec.Labels = []string{"runplane"}
			job := f.job(t, f.srv.Queue(tt.spec))
			var run struct {
				Event        string `json:"event"`
				PullRequests []any  `json:"pull_requests"`
				Repository   struct {
					FullName string `json:"full_name"`
					Private  bool   `json:"private"`
				} `json:"repository"`
				HeadRepository struct {
					FullName string `json:"full_name"`
					Fork     bool   `json:"fork"`
				} `json:"head_repository"`
			}
			api.get(t, fmt.Sprintf("/repos/%s/actions/runs/%d", tt.spec.Repo, job.RunID), &run)
			if run.Event != tt.event || len(run.PullRequests) != tt.prs || run.HeadRepository.FullName != tt.headRepo ||
				run.HeadRepository.Fork != tt.headFork || run.Repository.Private != tt.repoPriv || run.Repository.FullName != tt.repoName {
				t.Errorf("run = %+v", run)
			}
		})
	}
}

// TestAppAuth checks that only well-formed app JWTs get installation
// tokens, and that a token reads only its own installation.
func TestAppAuth(t *testing.T) {
	f := start(t, fakegh.Config{Installations: []fakegh.Installation{
		{ID: 1, Account: "acme", AccountType: "Organization", Repos: []fakegh.Repo{{Name: "acme/api", Private: true}}},
		{ID: 2, Account: "mona", AccountType: "User", Selected: true, Repos: []fakegh.Repo{{Name: "mona/dotfiles"}}},
	}})
	now := time.Now()
	jwt := func(header, claims string) string {
		enc := base64.RawURLEncoding.EncodeToString
		return enc([]byte(header)) + "." + enc([]byte(claims)) + ".c2ln"
	}
	tests := []struct {
		name   string
		token  string
		path   string
		status int
	}{
		{"client ID issuer", jwt(`{"alg":"RS256"}`, fmt.Sprintf(`{"iss":"Iv23x","iat":%d,"exp":%d}`, now.Unix()-60, now.Unix()+540)), "/app/installations/1/access_tokens", 201},
		{"app ID issuer", jwt(`{"alg":"RS256"}`, fmt.Sprintf(`{"iss":1001,"exp":%d}`, now.Unix()+600)), "/app/installations/2/access_tokens", 201},
		{"expired", jwt(`{"alg":"RS256"}`, fmt.Sprintf(`{"iss":"Iv23x","exp":%d}`, now.Unix()-1)), "/app/installations/1/access_tokens", 401},
		{"expiry too far", jwt(`{"alg":"RS256"}`, fmt.Sprintf(`{"iss":"Iv23x","exp":%d}`, now.Unix()+20*60)), "/app/installations/1/access_tokens", 401},
		{"no issuer", jwt(`{"alg":"RS256"}`, fmt.Sprintf(`{"exp":%d}`, now.Unix()+300)), "/app/installations/1/access_tokens", 401},
		{"alg none", jwt(`{"alg":"none"}`, fmt.Sprintf(`{"iss":"Iv23x","exp":%d}`, now.Unix()+300)), "/app/installations/1/access_tokens", 401},
		{"not a JWT", "ghs_abc", "/app/installations/1/access_tokens", 401},
		{"unknown installation", jwt(`{"alg":"RS256"}`, fmt.Sprintf(`{"iss":"Iv23x","exp":%d}`, now.Unix()+300)), "/app/installations/3/access_tokens", 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, f.url+"/api/v3"+tt.path, nil)
			req.Header.Set("Authorization", "Bearer "+tt.token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.status)
			}
		})
	}

	mona := f.api(t, 2)
	mona.status(t, http.MethodGet, "/repos/mona/dotfiles", http.StatusOK)
	mona.status(t, http.MethodGet, "/repos/acme/api", http.StatusNotFound)
	mona.status(t, http.MethodPost, "/orgs/acme/actions/runners/registration-token", http.StatusNotFound)
	mona.status(t, http.MethodPost, "/repos/mona/dotfiles/actions/runners/registration-token", http.StatusCreated)
}

// TestPersonalAccount makes a scale set for a repository of a user, as
// runplane does for a personal account, and checks that the installation
// cannot make one for an organization it is not on.
func TestPersonalAccount(t *testing.T) {
	f := start(t, fakegh.Config{Installations: []fakegh.Installation{
		{ID: 1, Account: "acme", AccountType: "Organization"},
		{ID: 2, Account: "mona", AccountType: "User", Selected: true, Repos: []fakegh.Repo{{Name: "mona/app", Private: true}}},
	}})
	client := func(configURL string) *scaleset.Client {
		c, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: configURL,
			GitHubAppAuth:   scaleset.GitHubAppAuth{ClientID: fakegh.AppClientID, InstallationID: 2, PrivateKey: string(f.srv.PrivateKey())},
		}, scaleset.WithRetry(scaleset.RetryConfig{}))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	set, err := client(f.url+"/mona/app").CreateRunnerScaleSet(t.Context(), &scaleset.RunnerScaleSet{Name: "runplane", RunnerGroupID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.srv.ScaleSets(); len(got) != 1 || got[0].ID != set.ID || got[0].Scope != "mona/app" {
		t.Errorf("scale sets = %+v, want one in mona/app", got)
	}
	if _, err := client(f.url+"/acme").CreateRunnerScaleSet(t.Context(), &scaleset.RunnerScaleSet{Name: "runplane", RunnerGroupID: 1}); err == nil {
		t.Errorf("installation of mona made a scale set for acme")
	}
}

// TestGitHubURLsThroughTransport runs the scaleset client with the real
// github.com config URL and the transport of the fake.
func TestGitHubURLsThroughTransport(t *testing.T) {
	f := start(t, fakegh.Config{})
	tr, err := fakegh.Transport(f.url)
	if err != nil {
		t.Fatal(err)
	}
	c, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: "https://github.com/acme",
		GitHubAppAuth:   scaleset.GitHubAppAuth{ClientID: fakegh.AppClientID, InstallationID: 1, PrivateKey: string(f.srv.PrivateKey())},
	}, scaleset.WithHTTPClient(&http.Client{Transport: tr}))
	if err != nil {
		t.Fatal(err)
	}
	set, err := c.CreateRunnerScaleSet(t.Context(), &scaleset.RunnerScaleSet{Name: "runplane", RunnerGroupID: 1})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := c.MessageSessionClient(t.Context(), set.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if u := sess.Session().MessageQueueURL; !strings.HasPrefix(u, f.url+"/") {
		t.Errorf("message queue URL = %s, want one on the fake at %s", u, f.url)
	}
}

// TestRunnerRelease downloads the runner with the runplane bundle code
// through the transport: the release must give the SHA-256 of the archive,
// and the archive must keep run.sh executable.
func TestRunnerRelease(t *testing.T) {
	f := start(t, fakegh.Config{RunnerVersion: "2.331.0"})
	tr, err := fakegh.Transport(f.url)
	if err != nil {
		t.Fatal(err)
	}
	bundles := image.NewBundles(image.BundleConfig{Dir: t.TempDir(), HTTP: &http.Client{Transport: tr}})
	b, err := bundles.Ensure(t.Context(), "2.331.0", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if !b.Patched {
		t.Errorf("bundle is not patched; bin/Runner.Worker.dll must have the text that the patch changes")
	}
	info, err := os.Stat(filepath.Join(b.Dir, "run.sh"))
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("run.sh in the bundle: %v, mode %v; want an executable file", err, info.Mode())
	}
	got, _ := os.ReadFile(filepath.Join(b.Dir, "run.sh"))
	want, _ := os.ReadFile(filepath.Join(runnerDir, "run.sh"))
	if !bytes.Equal(got, want) {
		t.Errorf("run.sh in the bundle differs from testdata/fakerunner/run.sh")
	}
}

// TestAppManifestFlow goes through the browser steps of the manifest flow:
// create the app, convert the code, and install the app.
func TestAppManifestFlow(t *testing.T) {
	f := start(t, fakegh.Config{})
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	manifest := `{"name":"runplane ACME","url":"https://ci.example.com","redirect_url":"https://ci.example.com/api/github/callback","setup_url":"https://ci.example.com/api/github/setup","public":false}`
	resp, err := noRedirect.PostForm(f.url+"/organizations/acme/settings/apps/new?state=s1", url.Values{"manifest": {manifest}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	back, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || back.Path != "/api/github/callback" || back.Query().Get("state") != "s1" || back.Query().Get("code") == "" {
		t.Fatalf("create app: %s to %s", resp.Status, back)
	}

	convert := func() (*http.Response, map[string]any) {
		resp, err := http.Post(f.url+"/api/v3/app-manifests/"+back.Query().Get("code")+"/conversions", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var app map[string]any
		json.NewDecoder(resp.Body).Decode(&app)
		return resp, app
	}
	resp, app := convert()
	if resp.StatusCode != http.StatusCreated || app["slug"] != "runplane-acme" || app["pem"] != string(f.srv.PrivateKey()) || app["client_secret"] == "" {
		t.Fatalf("conversion: %s %v", resp.Status, app)
	}
	if resp, _ := convert(); resp.StatusCode != http.StatusNotFound {
		t.Errorf("second conversion of a code: %s, want 404", resp.Status)
	}

	resp, err = noRedirect.Get(f.url + "/apps/runplane-acme/installations/new")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "https://ci.example.com/api/github/setup?installation_id=1&setup_action=install" {
		t.Errorf("install app: %s to %q", resp.Status, loc)
	}
}

// TestPagination follows the Link header of a list, as GitHub clients do.
func TestPagination(t *testing.T) {
	f := start(t, fakegh.Config{})
	api := f.api(t, 1)
	var names []string
	next := f.url + "/api/v3/installation/repositories?per_page=2"
	for pages := 0; next != ""; pages++ {
		if pages > 3 {
			t.Fatal("the Link header does not end")
		}
		var list struct {
			Total int `json:"total_count"`
			Repos []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		resp := api.do(t, http.MethodGet, next)
		json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if list.Total != 3 {
			t.Errorf("total_count = %d, want 3", list.Total)
		}
		for _, r := range list.Repos {
			names = append(names, r.FullName)
		}
		next = ""
		if m := regexp.MustCompile(`<([^>]+)>; rel="next"`).FindStringSubmatch(resp.Header.Get("Link")); m != nil {
			next = m[1]
		}
	}
	if strings.Join(names, " ") != "acme/api acme/web acme/site" {
		t.Errorf("repositories = %v", names)
	}
}

func (f *fake) job(t *testing.T, requestID int64) fakegh.Job {
	t.Helper()
	for _, j := range f.srv.Jobs() {
		if j.RequestID == requestID {
			return j
		}
	}
	t.Fatalf("no job %d", requestID)
	return fakegh.Job{}
}

// rest calls the REST API with an installation token.
type rest struct {
	base, token string
}

func (f *fake) api(t *testing.T, installation int64) rest {
	t.Helper()
	now := time.Now().Unix()
	enc := base64.RawURLEncoding.EncodeToString
	jwt := enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(fmt.Appendf(nil, `{"iss":"%s","iat":%d,"exp":%d}`, fakegh.AppClientID, now-60, now+540)) + ".c2ln"
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v3/app/installations/%d/access_tokens", f.url, installation), nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("installation token: %s, %v", resp.Status, err)
	}
	return rest{base: f.url + "/api/v3", token: tok.Token}
}

func (a rest) do(t *testing.T, method, u string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, u, nil)
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (a rest) get(t *testing.T, path string, v any) {
	t.Helper()
	resp := a.do(t, http.MethodGet, a.base+path)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: %s %s", path, resp.Status, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
}

func (a rest) post(t *testing.T, path string, want int) {
	t.Helper()
	a.status(t, http.MethodPost, path, want)
}

func (a rest) status(t *testing.T, method, path string, want int) {
	t.Helper()
	resp := a.do(t, method, a.base+path)
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Errorf("%s %s: %s, want %d", method, path, resp.Status, want)
	}
}

// log follows the redirect of the logs endpoint without the token, as a
// client must for the blob URL of GitHub.
func (a rest) log(t *testing.T, path string) []byte {
	t.Helper()
	resp := a.do(t, http.MethodGet, a.base+path)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET %s: %s, want 302", path, resp.Status)
	}
	blob, err := http.Get(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	defer blob.Body.Close()
	body, _ := io.ReadAll(blob.Body)
	if blob.StatusCode != http.StatusOK {
		t.Fatalf("GET log blob: %s", blob.Status)
	}
	return body
}
