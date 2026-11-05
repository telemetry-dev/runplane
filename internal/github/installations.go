package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/runplane/runplane/internal/ghrest"
	"github.com/runplane/runplane/internal/store"
)

type account struct {
	Login     string `json:"login"`
	ID        int64  `json:"id"`
	Type      string `json:"type"` // Organization | User
	AvatarURL string `json:"avatar_url"`
}

type installation struct {
	ID                  int64      `json:"id"`
	AppID               int64      `json:"app_id"`
	Account             account    `json:"account"`
	RepositorySelection string     `json:"repository_selection"`
	SuspendedAt         *time.Time `json:"suspended_at"`
	CreatedAt           time.Time  `json:"created_at"`
}

// Setup records the installation that GitHub sends the browser back with,
// reads its repositories, and makes its scale sets. It returns the first job
// page the first time GitHub connects, and the GitHub settings page after.
func (s *Service) Setup(ctx context.Context, _ string, installationID int64, setupAction string) (string, error) {
	s.ops.Lock()
	defer s.ops.Unlock()
	c, err := s.creds(ctx)
	if err != nil {
		return "", err
	}
	known, err := s.st.ListInstallations(ctx)
	if err != nil {
		return "", err
	}
	connected := len(known) > 0
	if setupAction == "request" || installationID == 0 {
		// An organization owner must approve the request on GitHub first.
		if connected {
			return "/settings/github?requested=1", nil
		}
		return "/setup/github?requested=1", nil
	}

	var inst installation
	err = s.api.JSON(ctx, ghrest.Call{Method: http.MethodGet, Path: fmt.Sprintf("/app/installations/%d", installationID), Auth: appAuth{s}}, &inst)
	if err != nil {
		return "", fmt.Errorf("read the installation: %w", err)
	}
	if inst.AppID != c.app.AppID {
		return "", fmt.Errorf("installation %d belongs to a different GitHub App", installationID)
	}
	if err := s.saveInstallation(ctx, inst); err != nil {
		return "", err
	}
	if err := s.syncRepositories(ctx, inst.ID); err != nil {
		return "", err
	}
	s.reconcile(ctx)
	if connected {
		return "/settings/github", nil
	}
	return "/setup/first-job", nil
}

// Sync reads the installations, their repositories, and suspensions now,
// then makes the scale sets agree.
func (s *Service) Sync(ctx context.Context) error {
	s.ops.Lock()
	defer s.ops.Unlock()
	err := s.syncInstallations(ctx)
	s.reconcile(ctx)
	return err
}

// syncInstallations makes the installations and repositories in the store
// agree with GitHub. It deletes the rows of installations that GitHub no
// longer has; reconcile then removes their scale sets. s.ops must be held.
func (s *Service) syncInstallations(ctx context.Context) error {
	if _, err := s.creds(ctx); err != nil {
		return err
	}
	remote, err := ghrest.List[installation](ctx, s.api, appAuth{s}, "/app/installations?per_page=100", "")
	if err != nil {
		return fmt.Errorf("list installations: %w", err)
	}
	known, err := s.st.ListInstallations(ctx)
	if err != nil {
		return err
	}
	seen := map[int64]bool{}
	var errs []error
	for _, inst := range remote {
		if inst.Account.Type != "Organization" && inst.Account.Type != "User" {
			continue
		}
		seen[inst.ID] = true
		if err := s.saveInstallation(ctx, inst); err != nil {
			return err
		}
		// GitHub gives a suspended installation no token, so its last
		// known repositories stay.
		if inst.SuspendedAt != nil {
			continue
		}
		if err := s.syncRepositories(ctx, inst.ID); err != nil {
			errs = append(errs, err)
		}
	}
	for _, inst := range known {
		if seen[inst.ID] {
			continue
		}
		s.log.Info("GitHub App installation is gone", "account", inst.AccountLogin)
		if err := s.st.DeleteInstallation(ctx, inst.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return errors.Join(errs...)
}

func (s *Service) saveInstallation(ctx context.Context, inst installation) error {
	return s.st.UpsertInstallation(ctx, store.Installation{
		ID: inst.ID, AppID: inst.AppID, AccountLogin: inst.Account.Login, AccountID: inst.Account.ID,
		AccountType: inst.Account.Type, AvatarURL: inst.Account.AvatarURL,
		RepositorySelection: inst.RepositorySelection, SuspendedAt: inst.SuspendedAt,
		CreatedAt: inst.CreatedAt, UpdatedAt: time.Now(),
	})
}

// syncRepositories stores the repositories that the installation can use.
// The GitHub repository object has the JSON names of store.Repository.
func (s *Service) syncRepositories(ctx context.Context, installationID int64) error {
	repos, err := ghrest.List[store.Repository](ctx, s.api, installAuth{s, installationID}, "/installation/repositories?per_page=100", "repositories")
	if err != nil {
		return fmt.Errorf("list repositories of installation %d: %w", installationID, err)
	}
	now := time.Now()
	for i := range repos {
		repos[i].UpdatedAt = now
	}
	return s.st.ReplaceRepositories(ctx, installationID, repos)
}
