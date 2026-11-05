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

const appCols = `id, app_id, slug, name, client_id, owner_login, owner_type, html_url, private_key_enc, client_secret_enc,
	webhook_secret_enc, created_at`

func (a *GitHubApp) cols() []any {
	return []any{&a.ID, &a.AppID, &a.Slug, &a.Name, &a.ClientID, &a.OwnerLogin, &a.OwnerType, &a.HTMLURL,
		&a.PrivateKeyEnc, &a.ClientSecretEnc, &a.WebhookSecretEnc, at(&a.CreatedAt)}
}

// GetGitHubApp returns the app, or ErrNotFound if runplane has no app.
func (s *Store) GetGitHubApp(ctx context.Context) (GitHubApp, error) {
	return queryOne(ctx, s.r, "get github app", scanRow[GitHubApp],
		"SELECT "+appCols+" FROM github_apps ORDER BY created_at DESC LIMIT 1")
}

// SaveGitHubApp stores the app in place of the earlier app. The installations
// of the earlier app go with it.
func (s *Store) SaveGitHubApp(ctx context.Context, a GitHubApp) error {
	return s.tx(ctx, "save github app", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM github_apps WHERE id != ?", a.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, upsert("github_apps", appCols, "id"), a.cols()...)
		return dbErr(err)
	})
}

// DeleteGitHubApp also deletes its installations and their repositories.
func (s *Store) DeleteGitHubApp(ctx context.Context, id string) error {
	return exec1(ctx, s.w, "delete github app", "DELETE FROM github_apps WHERE id = ?", id)
}

const manifestCols = "state_hash, account_type, organization, created_at, expires_at"

func (m *ManifestState) cols() []any {
	return []any{&m.StateHash, &m.AccountType, &m.Organization, at(&m.CreatedAt), at(&m.ExpiresAt)}
}

func (s *Store) CreateManifestState(ctx context.Context, m ManifestState) error {
	return exec(ctx, s.w, "create manifest state", insert("manifest_states", manifestCols), m.cols()...)
}

// ConsumeManifestState deletes the state and returns it. It returns
// ErrNotFound if the state is unknown or expired at now.
func (s *Store) ConsumeManifestState(ctx context.Context, stateHash string, now time.Time) (ManifestState, error) {
	m, err := queryOne(ctx, s.w, "use manifest state", scanRow[ManifestState],
		"DELETE FROM manifest_states WHERE state_hash = ? RETURNING "+manifestCols, stateHash)
	if err != nil {
		return ManifestState{}, err
	}
	if !m.ExpiresAt.After(now) {
		return ManifestState{}, fmt.Errorf("use manifest state: %w", ErrNotFound)
	}
	return m, nil
}

const installationCols = `id, app_id, account_login, account_id, account_type, avatar_url, repository_selection,
	suspended_at, created_at, updated_at`

func (i *Installation) cols() []any {
	return []any{&i.ID, &i.AppID, &i.AccountLogin, &i.AccountID, &i.AccountType, &i.AvatarURL, &i.RepositorySelection,
		atPtr(&i.SuspendedAt), at(&i.CreatedAt), at(&i.UpdatedAt)}
}

// UpsertInstallation keeps the created_at of a known installation.
func (s *Store) UpsertInstallation(ctx context.Context, i Installation) error {
	return exec(ctx, s.w, "save installation", upsert("installations", installationCols, "id"), i.cols()...)
}

func (s *Store) GetInstallation(ctx context.Context, id int64) (Installation, error) {
	return queryOne(ctx, s.r, "get installation", scanRow[Installation],
		"SELECT "+installationCols+" FROM installations WHERE id = ?", id)
}

func (s *Store) ListInstallations(ctx context.Context) ([]Installation, error) {
	return queryAll(ctx, s.r, "list installations", scanRow[Installation],
		"SELECT "+installationCols+" FROM installations ORDER BY account_login")
}

// DeleteInstallation also deletes its repositories.
func (s *Store) DeleteInstallation(ctx context.Context, id int64) error {
	return exec1(ctx, s.w, "delete installation", "DELETE FROM installations WHERE id = ?", id)
}

const repositoryCols = "id, installation_id, full_name, private, archived, default_branch, updated_at"

func (r *Repository) cols() []any {
	return []any{&r.ID, &r.InstallationID, &r.FullName, &r.Private, &r.Archived, &r.DefaultBranch, at(&r.UpdatedAt)}
}

// ReplaceRepositories makes repos the full set of repositories of the installation.
func (s *Store) ReplaceRepositories(ctx context.Context, installationID int64, repos []Repository) error {
	return s.tx(ctx, "replace repositories", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM repositories WHERE installation_id = ?", installationID); err != nil {
			return err
		}
		query := upsert("repositories", repositoryCols, "id")
		for _, r := range repos {
			r.InstallationID = installationID
			if _, err := tx.ExecContext(ctx, query, r.cols()...); err != nil {
				return dbErr(err)
			}
		}
		return nil
	})
}

func (s *Store) ListRepositories(ctx context.Context, installationID int64) ([]Repository, error) {
	return queryAll(ctx, s.r, "list repositories", scanRow[Repository],
		"SELECT "+repositoryCols+" FROM repositories WHERE installation_id = ? ORDER BY full_name", installationID)
}

// GetRepositoryByName matches owner/name without regard to case.
func (s *Store) GetRepositoryByName(ctx context.Context, fullName string) (Repository, error) {
	return queryOne(ctx, s.r, "get repository", scanRow[Repository],
		"SELECT "+repositoryCols+" FROM repositories WHERE full_name = ? LIMIT 1", fullName)
}

const scaleSetCols = `id, pool_id, installation_id, repository_id, github_id, runner_group_id, name, status, error,
	last_poll_at, stats_json, updated_at`

func (x *ScaleSet) cols() []any {
	return []any{&x.ID, &x.PoolID, &x.InstallationID, &x.RepositoryID, &x.GitHubID, &x.RunnerGroupID, &x.Name, &x.Status,
		&x.Error, atPtr(&x.LastPollAt), jsonCol(&x.Stats), at(&x.UpdatedAt)}
}

// UpsertScaleSet writes all columns of the scale set with this ID. It returns
// ErrConflict if a different scale set has the same pool, installation, and repository.
func (s *Store) UpsertScaleSet(ctx context.Context, x ScaleSet) error {
	return exec(ctx, s.w, "save scale set", upsert("scale_sets", scaleSetCols, "id"), x.cols()...)
}

func (s *Store) GetScaleSet(ctx context.Context, id string) (ScaleSet, error) {
	return queryOne(ctx, s.r, "get scale set", scanRow[ScaleSet], "SELECT "+scaleSetCols+" FROM scale_sets WHERE id = ?", id)
}

func (s *Store) ListScaleSets(ctx context.Context) ([]ScaleSet, error) {
	return queryAll(ctx, s.r, "list scale sets", scanRow[ScaleSet], "SELECT "+scaleSetCols+" FROM scale_sets ORDER BY name, id")
}

func (s *Store) SetScaleSetStatus(ctx context.Context, id, status, errMsg string) error {
	return exec1(ctx, s.w, "set scale set status", "UPDATE scale_sets SET status = ?, error = ?, updated_at = ? WHERE id = ?",
		status, errMsg, ms(time.Now()), id)
}

// SetScaleSetRemote records the GitHub side of a scale set. It does not
// touch the status, the statistics, or the poll time.
func (s *Store) SetScaleSetRemote(ctx context.Context, id string, githubID, runnerGroupID int64, name string) error {
	return exec1(ctx, s.w, "set scale set remote",
		"UPDATE scale_sets SET github_id = ?, runner_group_id = ?, name = ?, updated_at = ? WHERE id = ?",
		githubID, runnerGroupID, name, ms(time.Now()), id)
}

func (s *Store) SetScaleSetStats(ctx context.Context, id string, stats ScaleSetStats, polledAt time.Time) error {
	return exec1(ctx, s.w, "set scale set stats",
		"UPDATE scale_sets SET stats_json = ?, last_poll_at = ?, updated_at = ? WHERE id = ?",
		jsonCol(&stats), ms(polledAt), ms(time.Now()), id)
}

// TouchScaleSetPoll records a listener poll that brought no statistics.
func (s *Store) TouchScaleSetPoll(ctx context.Context, id string, polledAt time.Time) error {
	return exec1(ctx, s.w, "touch scale set poll", "UPDATE scale_sets SET last_poll_at = ? WHERE id = ?", ms(polledAt), id)
}

func (s *Store) DeleteScaleSet(ctx context.Context, id string) error {
	return exec1(ctx, s.w, "delete scale set", "DELETE FROM scale_sets WHERE id = ?", id)
}
