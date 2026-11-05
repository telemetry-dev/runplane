package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/store"
)

// pullBranch is the branch of the pull request that moves jobs to runplane.
// It has no slash, so a branch named runplane cannot block it.
const pullBranch = "runplane-runs-on"

// pullPermissions are the installation permissions that OpenPullRequest
// uses. A change in .github/workflows needs workflows next to contents.
var pullPermissions = []string{"contents", "pull_requests", "workflows"}

// hostedRunsOn matches a runs-on line with one GitHub-hosted label that a
// pool can take: ubuntu-latest, or ubuntu-24.04, the release of the runplane
// base image. Quotes and a comment can follow the label.
var hostedRunsOn = regexp.MustCompile(`(?m)^([ \t]*(?:-[ \t]+)?runs-on:[ \t]*["']?)ubuntu-(?:latest|24\.04)(["']?[ \t]*(?:#[^\r\n]*)?\r?)$`)

// OpenPullRequest opens a pull request in the repository that sets runs-on
// to label in each job on ubuntu-latest or ubuntu-24.04, and returns its
// number and URL. While the pull request of an earlier call is open, it
// returns that one. base is the browser URL of runplane, for the
// description.
func (s *Service) OpenPullRequest(ctx context.Context, base, repository, label string) (int, string, error) {
	repo, err := s.st.GetRepositoryByName(ctx, repository)
	if errors.Is(err, store.ErrNotFound) {
		return 0, "", huma.Error404NotFound("The GitHub App has no access to " + repository + ".")
	}
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
		"message": "Run jobs on runplane\n\nThe jobs on ubuntu-latest and ubuntu-24.04 use the runplane label " + label + ".",
		"tree":    made.SHA, "parents": []string{branch.Commit.SHA},
	}, &made)
	if err != nil {
		return 0, "", err
	}
	err = r.json(ctx, http.MethodPost, "/git/refs", map[string]any{"ref": "refs/heads/" + pullBranch, "sha": made.SHA}, nil)
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusUnprocessableEntity {
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

// treeFile is a file of a new Git tree, with its full content.
type treeFile struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

// checkPullPermissions returns a 403 problem if the account of the
// installation did not accept pullPermissions. An app from before runplane
// asked for them gets them in its settings on GitHub.
func (s *Service) checkPullPermissions(ctx context.Context, installationID int64) error {
	var inst installation
	err := s.api.json(ctx, call{method: http.MethodGet, path: fmt.Sprintf("/app/installations/%d", installationID), auth: appAuth{s}}, &inst)
	if err != nil {
		return fmt.Errorf("read the installation: %w", err)
	}
	for _, name := range pullPermissions {
		if inst.Permissions[name] != "write" {
			return huma.Error403Forbidden("The GitHub App needs read and write access to Contents, Pull requests, and Workflows. " +
				"Add these permissions in the app settings on GitHub, then accept them for " + inst.Account.Login + ".")
		}
	}
	return nil
}

// repoREST sends REST calls about one repository as its installation.
type repoREST struct {
	api  *rest
	auth auth
	name string // owner/name
}

// json sends a call to path below /repos/owner/name.
func (r repoREST) json(ctx context.Context, method, path string, body, out any) error {
	return r.api.json(ctx, call{method: method, path: "/repos/" + r.name + path, body: body, auth: r.auth}, out)
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
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
		return nil, 0, huma.Error422UnprocessableEntity(r.name + " has no workflows in .github/workflows.")
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
		return nil, 0, huma.Error422UnprocessableEntity("No job in the workflows of " + r.name + " runs on ubuntu-latest or ubuntu-24.04.")
	}
	return files, jobs, nil
}

// moveJobs sets runs-on to label in each job of the workflow that runs on
// ubuntu-latest or ubuntu-24.04, and counts these jobs. The other lines
// stay as they are.
func moveJobs(workflow, label string) (string, int) {
	n := len(hostedRunsOn.FindAllStringIndex(workflow, -1))
	return hostedRunsOn.ReplaceAllString(workflow, "${1}"+label+"${2}"), n
}

func pullDescription(base, label string, jobs int, files []treeFile) string {
	var b strings.Builder
	noun := "jobs"
	if jobs == 1 {
		noun = "job"
	}
	fmt.Fprintf(&b, "This pull request sets `runs-on` to `%s` for %d %s on `ubuntu-latest` or `ubuntu-24.04`. "+
		"These jobs then run on your runplane servers. Workflows that run on pull requests use the change for this pull request too, "+
		"so you can see the jobs in runplane before you merge.\n\nChanged workflows:\n\n", label, jobs, noun)
	for _, f := range files {
		fmt.Fprintf(&b, "- `%s`\n", f.Path)
	}
	fmt.Fprintf(&b, "\nJobs in runplane: %s/jobs\n", base)
	return b.String()
}
