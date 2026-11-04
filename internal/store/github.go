package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type GitHubApp struct {
	ID               string
	AppID            int64
	Slug             string
	Name             string
	ClientID         string
	OwnerLogin       string
	OwnerType        string
	HTMLURL          string
	PrivateKeyEnc    []byte // sealed with internal/secret
	ClientSecretEnc  []byte
	WebhookSecretEnc []byte
	CreatedAt        time.Time
}

type ManifestState struct {
	StateHash    string
	AccountType  string // organization | user
	Organization string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

type Installation struct {
	ID                  int64      `json:"id"`     // GitHub installation ID
	AppID               int64      `json:"app_id"` // GitHub App ID
	AccountLogin        string     `json:"account_login"`
	AccountID           int64      `json:"account_id"`
	AccountType         string     `json:"account_type"` // Organization | User
	AvatarURL           string     `json:"avatar_url"`
	RepositorySelection string     `json:"repository_selection"` // all | selected
	SuspendedAt         *time.Time `json:"suspended_at"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

type Repository struct {
	ID             int64     `json:"id"` // GitHub repository ID
	InstallationID int64     `json:"installation_id"`
	FullName       string    `json:"full_name"` // owner/name
	Private        bool      `json:"private"`
	Archived       bool      `json:"archived"`
	DefaultBranch  string    `json:"default_branch"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Scale set statuses.
const (
	ScaleSetCreating  = "creating"
	ScaleSetListening = "listening"
	ScaleSetError     = "error"
	ScaleSetDeleting  = "deleting"
)

type ScaleSet struct {
	ID             string        `json:"id"`
	PoolID         string        `json:"pool_id"`
	InstallationID int64         `json:"installation_id"`
	RepositoryID   *int64        `json:"repository_id"` // nil for an organization
	GitHubID       int64         `json:"github_id"`     // 0 until GitHub makes the scale set
	RunnerGroupID  int64         `json:"runner_group_id"`
	Name           string        `json:"name"`
	Status         string        `json:"status"`
	Error          string        `json:"error"`
	LastPollAt     *time.Time    `json:"last_poll_at"`
	Stats          ScaleSetStats `json:"stats"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

// ScaleSetStats are the statistics of the last listener message.
type ScaleSetStats struct {
	AvailableJobs     int `json:"available_jobs"`
	AcquiredJobs      int `json:"acquired_jobs"`
	AssignedJobs      int `json:"assigned_jobs"`
	RunningJobs       int `json:"running_jobs"`
	RegisteredRunners int `json:"registered_runners"`
	BusyRunners       int `json:"busy_runners"`
	IdleRunners       int `json:"idle_runners"`
}

const appCols = "id, app_id, slug, name, client_id, owner_login, owner_type, html_url, private_key_enc, client_secret_enc, webhook_secret_enc, created_at"

// GetGitHubApp returns the app, or ErrNotFound if runplane has no app.
func (s *Store) GetGitHubApp(ctx context.Context) (GitHubApp, error) {
	var a GitHubApp
	err := s.r.QueryRowContext(ctx, "SELECT "+appCols+" FROM github_apps ORDER BY created_at DESC LIMIT 1").Scan(
		&a.ID, &a.AppID, &a.Slug, &a.Name, &a.ClientID, &a.OwnerLogin, &a.OwnerType, &a.HTMLURL,
		&a.PrivateKeyEnc, &a.ClientSecretEnc, &a.WebhookSecretEnc, at(&a.CreatedAt))
	if err != nil {
		return GitHubApp{}, fmt.Errorf("get github app: %w", dbErr(err))
	}
	return a, nil
}

// SaveGitHubApp stores the app in place of the earlier app. The installations
// of the earlier app go with it.
func (s *Store) SaveGitHubApp(ctx context.Context, a GitHubApp) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM github_apps WHERE id != ?", a.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO github_apps (`+appCols+`) VALUES (`+placeholders(12)+`)
			ON CONFLICT (id) DO UPDATE SET app_id = excluded.app_id, slug = excluded.slug, name = excluded.name,
				client_id = excluded.client_id, owner_login = excluded.owner_login, owner_type = excluded.owner_type,
				html_url = excluded.html_url, private_key_enc = excluded.private_key_enc,
				client_secret_enc = excluded.client_secret_enc, webhook_secret_enc = excluded.webhook_secret_enc`,
			a.ID, a.AppID, a.Slug, a.Name, a.ClientID, a.OwnerLogin, a.OwnerType, a.HTMLURL,
			a.PrivateKeyEnc, a.ClientSecretEnc, a.WebhookSecretEnc, ms(a.CreatedAt))
		return dbErr(err)
	})
	if err != nil {
		return fmt.Errorf("save github app: %w", err)
	}
	return nil
}

// DeleteGitHubApp also deletes its installations and their repositories.
func (s *Store) DeleteGitHubApp(ctx context.Context, id string) error {
	if err := exec1(ctx, s.w, "DELETE FROM github_apps WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete github app: %w", err)
	}
	return nil
}

func (s *Store) CreateManifestState(ctx context.Context, m ManifestState) error {
	_, err := s.w.ExecContext(ctx,
		"INSERT INTO manifest_states (state_hash, account_type, organization, created_at, expires_at) VALUES (?, ?, ?, ?, ?)",
		m.StateHash, m.AccountType, m.Organization, ms(m.CreatedAt), ms(m.ExpiresAt))
	if err != nil {
		return fmt.Errorf("create manifest state: %w", dbErr(err))
	}
	return nil
}

// ConsumeManifestState deletes the state and returns it. It returns
// ErrNotFound if the state is unknown or expired at now.
func (s *Store) ConsumeManifestState(ctx context.Context, stateHash string, now time.Time) (ManifestState, error) {
	var m ManifestState
	err := s.w.QueryRowContext(ctx, `DELETE FROM manifest_states WHERE state_hash = ?
		RETURNING state_hash, account_type, organization, created_at, expires_at`, stateHash).Scan(
		&m.StateHash, &m.AccountType, &m.Organization, at(&m.CreatedAt), at(&m.ExpiresAt))
	if err != nil {
		return ManifestState{}, fmt.Errorf("use manifest state: %w", dbErr(err))
	}
	if !m.ExpiresAt.After(now) {
		return ManifestState{}, fmt.Errorf("use manifest state: %w", ErrNotFound)
	}
	return m, nil
}

const installationCols = "id, app_id, account_login, account_id, account_type, avatar_url, repository_selection, suspended_at, created_at, updated_at"

func scanInstallation(sc scanner) (Installation, error) {
	var i Installation
	err := sc.Scan(&i.ID, &i.AppID, &i.AccountLogin, &i.AccountID, &i.AccountType, &i.AvatarURL,
		&i.RepositorySelection, atPtr(&i.SuspendedAt), at(&i.CreatedAt), at(&i.UpdatedAt))
	return i, err
}

// UpsertInstallation keeps the created_at of a known installation.
func (s *Store) UpsertInstallation(ctx context.Context, i Installation) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO installations (`+installationCols+`) VALUES (`+placeholders(10)+`)
		ON CONFLICT (id) DO UPDATE SET app_id = excluded.app_id, account_login = excluded.account_login,
			account_id = excluded.account_id, account_type = excluded.account_type, avatar_url = excluded.avatar_url,
			repository_selection = excluded.repository_selection, suspended_at = excluded.suspended_at,
			updated_at = excluded.updated_at`,
		i.ID, i.AppID, i.AccountLogin, i.AccountID, i.AccountType, i.AvatarURL, i.RepositorySelection,
		msPtr(i.SuspendedAt), ms(i.CreatedAt), ms(i.UpdatedAt))
	if err != nil {
		return fmt.Errorf("save installation: %w", dbErr(err))
	}
	return nil
}

func (s *Store) GetInstallation(ctx context.Context, id int64) (Installation, error) {
	i, err := scanInstallation(s.r.QueryRowContext(ctx, "SELECT "+installationCols+" FROM installations WHERE id = ?", id))
	if err != nil {
		return Installation{}, fmt.Errorf("get installation: %w", dbErr(err))
	}
	return i, nil
}

func (s *Store) ListInstallations(ctx context.Context) ([]Installation, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+installationCols+" FROM installations ORDER BY account_login")
	list, err := scanAll(rows, err, scanInstallation)
	if err != nil {
		return nil, fmt.Errorf("list installations: %w", err)
	}
	return list, nil
}

// SetInstallationSuspended sets or clears (nil) the suspension time.
func (s *Store) SetInstallationSuspended(ctx context.Context, id int64, suspendedAt *time.Time) error {
	err := exec1(ctx, s.w, "UPDATE installations SET suspended_at = ?, updated_at = ? WHERE id = ?",
		msPtr(suspendedAt), ms(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set installation suspended: %w", err)
	}
	return nil
}

// DeleteInstallation also deletes its repositories.
func (s *Store) DeleteInstallation(ctx context.Context, id int64) error {
	if err := exec1(ctx, s.w, "DELETE FROM installations WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete installation: %w", err)
	}
	return nil
}

const repositoryCols = "id, installation_id, full_name, private, archived, default_branch, updated_at"

func scanRepository(sc scanner) (Repository, error) {
	var r Repository
	err := sc.Scan(&r.ID, &r.InstallationID, &r.FullName, &r.Private, &r.Archived, &r.DefaultBranch, at(&r.UpdatedAt))
	return r, err
}

// ReplaceRepositories makes repos the full set of repositories of the installation.
func (s *Store) ReplaceRepositories(ctx context.Context, installationID int64, repos []Repository) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM repositories WHERE installation_id = ?", installationID); err != nil {
			return err
		}
		for _, r := range repos {
			_, err := tx.ExecContext(ctx, `INSERT INTO repositories (`+repositoryCols+`) VALUES (`+placeholders(7)+`)
				ON CONFLICT (id) DO UPDATE SET installation_id = excluded.installation_id, full_name = excluded.full_name,
					private = excluded.private, archived = excluded.archived, default_branch = excluded.default_branch,
					updated_at = excluded.updated_at`,
				r.ID, installationID, r.FullName, r.Private, r.Archived, r.DefaultBranch, ms(r.UpdatedAt))
			if err != nil {
				return dbErr(err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("replace repositories: %w", err)
	}
	return nil
}

func (s *Store) ListRepositories(ctx context.Context, installationID int64) ([]Repository, error) {
	rows, err := s.r.QueryContext(ctx,
		"SELECT "+repositoryCols+" FROM repositories WHERE installation_id = ? ORDER BY full_name", installationID)
	list, err := scanAll(rows, err, scanRepository)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	return list, nil
}

// GetRepositoryByName matches owner/name without regard to case.
func (s *Store) GetRepositoryByName(ctx context.Context, fullName string) (Repository, error) {
	r, err := scanRepository(s.r.QueryRowContext(ctx,
		"SELECT "+repositoryCols+" FROM repositories WHERE full_name = ? LIMIT 1", fullName))
	if err != nil {
		return Repository{}, fmt.Errorf("get repository: %w", dbErr(err))
	}
	return r, nil
}

func (s *Store) CountRepositories(ctx context.Context) (int, error) {
	var n int
	if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM repositories").Scan(&n); err != nil {
		return 0, fmt.Errorf("count repositories: %w", err)
	}
	return n, nil
}

const scaleSetCols = "id, pool_id, installation_id, repository_id, github_id, runner_group_id, name, status, error, last_poll_at, stats_json, updated_at"

func scanScaleSet(sc scanner) (ScaleSet, error) {
	var x ScaleSet
	err := sc.Scan(&x.ID, &x.PoolID, &x.InstallationID, &x.RepositoryID, &x.GitHubID, &x.RunnerGroupID,
		&x.Name, &x.Status, &x.Error, atPtr(&x.LastPollAt), jsonCol(&x.Stats), at(&x.UpdatedAt))
	return x, err
}

// UpsertScaleSet writes all columns of the scale set with this ID. It returns
// ErrConflict if a different scale set has the same pool, installation, and repository.
func (s *Store) UpsertScaleSet(ctx context.Context, x ScaleSet) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO scale_sets (`+scaleSetCols+`) VALUES (`+placeholders(12)+`)
		ON CONFLICT (id) DO UPDATE SET pool_id = excluded.pool_id, installation_id = excluded.installation_id,
			repository_id = excluded.repository_id, github_id = excluded.github_id,
			runner_group_id = excluded.runner_group_id, name = excluded.name, status = excluded.status,
			error = excluded.error, last_poll_at = excluded.last_poll_at, stats_json = excluded.stats_json,
			updated_at = excluded.updated_at`,
		x.ID, x.PoolID, x.InstallationID, x.RepositoryID, x.GitHubID, x.RunnerGroupID, x.Name, x.Status, x.Error,
		msPtr(x.LastPollAt), jsonArg(x.Stats), ms(x.UpdatedAt))
	if err != nil {
		return fmt.Errorf("save scale set: %w", dbErr(err))
	}
	return nil
}

func (s *Store) GetScaleSet(ctx context.Context, id string) (ScaleSet, error) {
	x, err := scanScaleSet(s.r.QueryRowContext(ctx, "SELECT "+scaleSetCols+" FROM scale_sets WHERE id = ?", id))
	if err != nil {
		return ScaleSet{}, fmt.Errorf("get scale set: %w", dbErr(err))
	}
	return x, nil
}

func (s *Store) ListScaleSets(ctx context.Context) ([]ScaleSet, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+scaleSetCols+" FROM scale_sets ORDER BY name, id")
	list, err := scanAll(rows, err, scanScaleSet)
	if err != nil {
		return nil, fmt.Errorf("list scale sets: %w", err)
	}
	return list, nil
}

func (s *Store) ListPoolScaleSets(ctx context.Context, poolID string) ([]ScaleSet, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+scaleSetCols+" FROM scale_sets WHERE pool_id = ? ORDER BY name, id", poolID)
	list, err := scanAll(rows, err, scanScaleSet)
	if err != nil {
		return nil, fmt.Errorf("list scale sets: %w", err)
	}
	return list, nil
}

func (s *Store) SetScaleSetStatus(ctx context.Context, id, status, errMsg string) error {
	err := exec1(ctx, s.w, "UPDATE scale_sets SET status = ?, error = ?, updated_at = ? WHERE id = ?",
		status, errMsg, ms(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set scale set status: %w", err)
	}
	return nil
}

// SetScaleSetStats records the statistics of a listener message received at polledAt.
func (s *Store) SetScaleSetStats(ctx context.Context, id string, stats ScaleSetStats, polledAt time.Time) error {
	err := exec1(ctx, s.w, "UPDATE scale_sets SET stats_json = ?, last_poll_at = ?, updated_at = ? WHERE id = ?",
		jsonArg(stats), ms(polledAt), ms(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set scale set stats: %w", err)
	}
	return nil
}

func (s *Store) DeleteScaleSet(ctx context.Context, id string) error {
	if err := exec1(ctx, s.w, "DELETE FROM scale_sets WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete scale set: %w", err)
	}
	return nil
}
