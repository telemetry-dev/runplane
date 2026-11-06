package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/runplane/runplane/internal/field"
	"github.com/runplane/runplane/internal/ghapp"
	"github.com/runplane/runplane/internal/ghrest"
	"github.com/runplane/runplane/internal/pages"
	"github.com/runplane/runplane/internal/policy"
	"github.com/runplane/runplane/internal/store"
)

// pullBranch is the branch of the pull request that moves jobs to runplane.
// It has no slash, so a branch named runplane cannot block it.
const pullBranch = "runplane-runs-on"

// hostedRunsOn matches a runs-on line with one label of ghapp.HostedLabels.
// Quotes and a comment can follow the label.
var hostedRunsOn = regexp.MustCompile(`(?m)^([ \t]*(?:-[ \t]+)?runs-on:[ \t]*["']?)(?:` + anyOf(ghapp.HostedLabels) +
	`)(["']?[ \t]*(?:#[^\r\n]*)?\r?)$`)

// OpenPullRequest opens a pull request in the repository that sets runs-on
// to label in each job on one of ghapp.HostedLabels, and returns its number
// and URL. While the pull request of an earlier call is open, it returns
// that one. base is the browser URL of runplane, for the description.
//
// The jobs of the pull request must start, so it returns field.Problems at
// "label" when no enabled pool has the label, and at "repository" when the
// app has no access to the repository, runplane takes no jobs of it, or it
// has no job to move. It returns ghapp.ErrNoPullPermissions when the
// account has not accepted ghapp.PullPermissions.
func (s *Service) OpenPullRequest(ctx context.Context, base, repository, label string) (int, string, error) {
	repo, err := s.pullRepository(ctx, repository, label)
	if err != nil {
		return 0, "", err
	}
	if err := s.checkPullPermissions(ctx, repo.InstallationID); err != nil {
		return 0, "", err
	}
	r := repoREST{s.api, installAuth{s, repo.InstallationID}, repo.FullName}

	owner, _, _ := strings.Cut(repo.FullName, "/")
	var open []pullRequest
	if err := r.json(ctx, http.MethodGet, "/pulls?state=open&per_page=1&head="+url.QueryEscape(owner+":"+pullBranch), nil, &open); err != nil {
		return 0, "", err
	}
	if len(open) > 0 {
		return open[0].Number, open[0].HTMLURL, nil
	}

	var branch struct {
		Commit struct {
			SHA    string `json:"sha"`
			Commit struct {
				Tree struct {
					SHA string `json:"sha"`
				} `json:"tree"`
			} `json:"commit"`
		} `json:"commit"`
	}
	if err := r.json(ctx, http.MethodGet, "/branches/"+url.PathEscape(repo.DefaultBranch), nil, &branch); err != nil {
		return 0, "", err
	}
	files, jobs, err := r.moveWorkflows(ctx, branch.Commit.SHA, label)
	if err != nil {
		return 0, "", err
	}

	var made struct {
		SHA string `json:"sha"`
	}
	err = r.json(ctx, http.MethodPost, "/git/trees", map[string]any{"base_tree": branch.Commit.Commit.Tree.SHA, "tree": files}, &made)
	if err != nil {
		return 0, "", err
	}
	err = r.json(ctx, http.MethodPost, "/git/commits", map[string]any{
		"message": "Run jobs on runplane\n\nThe jobs on " + ghapp.HostedLabelsText("") + " use the runplane label " + label + ".",
		"tree":    made.SHA, "parents": []string{branch.Commit.SHA},
	}, &made)
	if err != nil {
		return 0, "", err
	}
	err = r.json(ctx, http.MethodPost, "/git/refs", map[string]any{"ref": "refs/heads/" + pullBranch, "sha": made.SHA}, nil)
	var apiErr *ghrest.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity {
		// The branch of an earlier pull request that is closed now.
		err = r.json(ctx, http.MethodPatch, "/git/refs/heads/"+pullBranch, map[string]any{"sha": made.SHA, "force": true}, nil)
	}
	if err != nil {
		return 0, "", err
	}
	var pr pullRequest
	err = r.json(ctx, http.MethodPost, "/pulls", map[string]any{
		"title": "Run jobs on runplane", "head": pullBranch, "base": repo.DefaultBranch,
		"body": pullDescription(base, label, jobs, files),
	}, &pr)
	if err != nil {
		return 0, "", err
	}
	s.log.Info("opened pull request", "repository", repo.FullName, "number", pr.Number, "jobs", jobs)
	return pr.Number, pr.HTMLURL, nil
}

type pullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

type treeFile struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

// pullRepository returns the repository of a pull request that moves jobs
// to the pool label, or field.Problems when the jobs would not start.
func (s *Service) pullRepository(ctx context.Context, repository, label string) (store.Repository, error) {
	pools, err := s.st.ListPools(ctx)
	if err != nil {
		return store.Repository{}, err
	}
	if !slices.ContainsFunc(pools, func(p store.Pool) bool { return p.Enabled && p.Label == label }) {
		return store.Repository{}, field.Problems{{Key: "label", Message: "No enabled pool has this label.", Value: label}}
	}
	repo, err := s.st.GetRepositoryByName(ctx, repository)
	if errors.Is(err, store.ErrNotFound) {
		return store.Repository{}, repositoryProblem(repository, "The GitHub App has no access to "+repository+".")
	}
	if err != nil {
		return store.Repository{}, err
	}
	inst, err := s.st.GetInstallation(ctx, repo.InstallationID)
	if err != nil {
		return store.Repository{}, err
	}
	if inst.SuspendedAt != nil {
		return store.Repository{}, repositoryProblem(repository, "GitHub suspended the app installation on "+inst.AccountLogin+".")
	}
	settings, err := s.st.GetSettings(ctx)
	if err != nil {
		return store.Repository{}, err
	}
	if !policy.TakesJobs(settings, repo) {
		return store.Repository{}, repositoryProblem(repository, "runplane takes no jobs of "+repo.FullName+", so the moved jobs would not start.")
	}
	return repo, nil
}

func (s *Service) checkPullPermissions(ctx context.Context, installationID int64) error {
	inst, err := s.readInstallation(ctx, installationID)
	if err != nil {
		return err
	}
	for _, p := range ghapp.PullPermissions {
		if inst.Permissions[p.Name] != p.Access {
			return fmt.Errorf("%w. Add these permissions in the app settings on GitHub, then accept them for %s",
				ghapp.ErrNoPullPermissions, inst.Account.Login)
		}
	}
	return nil
}

type repoREST struct {
	api  ghrest.Client
	auth ghrest.Auth
	name string // owner/name
}

func (r repoREST) json(ctx context.Context, method, path string, body, out any) error {
	return r.api.JSON(ctx, ghrest.Call{Method: method, Path: "/repos/" + r.name + path, Body: body, Auth: r.auth}, out)
}

// moveWorkflows reads the workflows at the commit ref and returns the ones
// that moveJobs changes, with the count of moved jobs.
func (r repoREST) moveWorkflows(ctx context.Context, ref, label string) ([]treeFile, int, error) {
	var entries []struct {
		Type string `json:"type"`
		Path string `json:"path"`
		SHA  string `json:"sha"`
	}
	err := r.json(ctx, http.MethodGet, "/contents/.github/workflows?ref="+ref, nil, &entries)
	var apiErr *ghrest.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil, 0, repositoryProblem(r.name, r.name+" has no workflows in .github/workflows.")
	}
	if err != nil {
		return nil, 0, err
	}
	var files []treeFile
	jobs := 0
	for _, e := range entries {
		if e.Type != "file" || !strings.HasSuffix(e.Path, ".yml") && !strings.HasSuffix(e.Path, ".yaml") {
			continue
		}
		var blob struct {
			Content string `json:"content"`
		}
		if err := r.json(ctx, http.MethodGet, "/git/blobs/"+e.SHA, nil, &blob); err != nil {
			return nil, 0, err
		}
		raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
		if err != nil {
			return nil, 0, fmt.Errorf("decode %s of %s: %w", e.Path, r.name, err)
		}
		moved, n := moveJobs(string(raw), label)
		if n == 0 {
			continue
		}
		files = append(files, treeFile{Path: e.Path, Mode: "100644", Type: "blob", Content: moved})
		jobs += n
	}
	if jobs == 0 {
		return nil, 0, repositoryProblem(r.name, "No job in the workflows of "+r.name+" runs on "+ghapp.HostedLabelsText("")+".")
	}
	return files, jobs, nil
}

func repositoryProblem(repository, msg string) error {
	return field.Problems{{Key: "repository", Message: msg, Value: repository}}
}

// moveJobs sets runs-on to label in each job of the workflow that runs on
// one of ghapp.HostedLabels, and counts these jobs. The other lines stay as
// they are.
func moveJobs(workflow, label string) (string, int) {
	n := len(hostedRunsOn.FindAllStringIndex(workflow, -1))
	return hostedRunsOn.ReplaceAllString(workflow, "${1}"+label+"${2}"), n
}

// anyOf is a regexp term that matches each of the texts.
func anyOf(texts []string) string {
	quoted := make([]string, len(texts))
	for i, t := range texts {
		quoted[i] = regexp.QuoteMeta(t)
	}
	return strings.Join(quoted, "|")
}

func pullDescription(base, label string, jobs int, files []treeFile) string {
	var b strings.Builder
	noun := "jobs"
	if jobs == 1 {
		noun = "job"
	}
	fmt.Fprintf(&b, "This pull request sets `runs-on` to `%s` for %d %s on %s. "+
		"These jobs then run on your runplane servers. Workflows that run on pull requests use the change for this pull request too, "+
		"so you can see the jobs in runplane before you merge.\n\nChanged workflows:\n\n", label, jobs, noun, ghapp.HostedLabelsText("`"))
	for _, f := range files {
		fmt.Fprintf(&b, "- `%s`\n", f.Path)
	}
	fmt.Fprintf(&b, "\nJobs in runplane: %s%s\n", base, pages.Jobs)
	return b.String()
}
