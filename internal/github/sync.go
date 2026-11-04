package github

import (
	"context"
	"errors"
	"fmt"
	"time"

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

type repository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch"`
}

// syncInstallations makes the installations and repositories in the store
// agree with GitHub. It deletes the rows of installations that GitHub no
// longer has; reconcile then removes their scale sets. s.ops must be held.
func (s *Service) syncInstallations(ctx context.Context) error {
	if _, err := s.creds(ctx); err != nil {
		return err
	}
	remote, err := list[installation](ctx, s.api, appAuth{s}, "/app/installations?per_page=100", "")
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
	c, err := s.creds(ctx)
	if err != nil {
		return err
	}
	return s.st.UpsertInstallation(ctx, store.Installation{
		ID: inst.ID, AppID: c.app.AppID, AccountLogin: inst.Account.Login, AccountID: inst.Account.ID,
		AccountType: inst.Account.Type, AvatarURL: inst.Account.AvatarURL,
		RepositorySelection: inst.RepositorySelection, SuspendedAt: inst.SuspendedAt,
		CreatedAt: inst.CreatedAt, UpdatedAt: time.Now(),
	})
}

// syncRepositories stores the repositories that the installation can use.
func (s *Service) syncRepositories(ctx context.Context, installationID int64) error {
	remote, err := list[repository](ctx, s.api, installAuth{s, installationID}, "/installation/repositories?per_page=100", "repositories")
	if err != nil {
		return fmt.Errorf("list repositories of installation %d: %w", installationID, err)
	}
	now := time.Now()
	repos := make([]store.Repository, len(remote))
	for i, r := range remote {
		repos[i] = store.Repository{
			ID: r.ID, InstallationID: installationID, FullName: r.FullName, Private: r.Private,
			Archived: r.Archived, DefaultBranch: r.DefaultBranch, UpdatedAt: now,
		}
	}
	return s.st.ReplaceRepositories(ctx, installationID, repos)
}
