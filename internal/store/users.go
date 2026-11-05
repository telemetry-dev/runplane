package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// ErrSetupDone means that a user exists, so the first-admin setup is closed.
var ErrSetupDone = errors.New("setup is done")

type User struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	Name         string     `json:"name"`
	PasswordHash string     `json:"-"`
	Role         string     `json:"role"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLoginAt  *time.Time `json:"last_login_at"`
}

type Session struct {
	IDHash     string // SHA-256 of the token
	UserID     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	IP         string
	UserAgent  string
}

type Invite struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	Role       string     `json:"role"`
	TokenHash  string     `json:"-"`
	CreatedBy  string     `json:"created_by"` // empty if that user is gone
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at"`
}

type SetupCode struct {
	CodeHash  string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
}

const userCols = "id, email, name, password_hash, role, created_at, last_login_at"

func (u *User) cols() []any {
	return []any{&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, at(&u.CreatedAt), atPtr(&u.LastLoginAt)}
}

// CreateUser returns ErrConflict if the email is in use.
func (s *Store) CreateUser(ctx context.Context, u User) error {
	return exec(ctx, s.w, "create user", insert("users", userCols), u.cols()...)
}

func (s *Store) GetUser(ctx context.Context, id string) (User, error) {
	return queryOne(ctx, s.r, "get user", scanRow[User], "SELECT "+userCols+" FROM users WHERE id = ?", id)
}

// GetUserByEmail matches the email without regard to case.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (User, error) {
	return queryOne(ctx, s.r, "get user", scanRow[User], "SELECT "+userCols+" FROM users WHERE email = ?", email)
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	return queryAll(ctx, s.r, "list users", scanRow[User], "SELECT "+userCols+" FROM users ORDER BY created_at, id")
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	return queryOne(ctx, s.r, "count users", scanValue[int], "SELECT count(*) FROM users")
}

// UpdateUser writes the name, email, and role. It returns ErrConflict if
// the email is in use.
func (s *Store) UpdateUser(ctx context.Context, u User) error {
	return exec1(ctx, s.w, "update user", "UPDATE users SET name = ?, email = ?, role = ? WHERE id = ?",
		u.Name, u.Email, u.Role, u.ID)
}

func (s *Store) SetUserPassword(ctx context.Context, id, hash string) error {
	return exec1(ctx, s.w, "set user password", "UPDATE users SET password_hash = ? WHERE id = ?", hash, id)
}

func (s *Store) SetUserLastLogin(ctx context.Context, id string, at time.Time) error {
	return exec1(ctx, s.w, "set user last login", "UPDATE users SET last_login_at = ? WHERE id = ?", ms(at), id)
}

// DeleteUser also deletes the sessions of the user.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return exec1(ctx, s.w, "delete user", "DELETE FROM users WHERE id = ?", id)
}

// CreateFirstUser uses the setup code, makes the first user, and makes its
// session, in one transaction. It returns ErrSetupDone if a user exists and
// ErrNotFound if the code is unknown, used, or expired.
func (s *Store) CreateFirstUser(ctx context.Context, codeHash string, now time.Time, u User, sess Session) error {
	return s.tx(ctx, "create first user", func(tx *sql.Tx) error {
		n, err := queryOne(ctx, tx, "count users", scanValue[int], "SELECT count(*) FROM users")
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrSetupDone
		}
		err = exec1(ctx, tx, "use setup code",
			"UPDATE setup_codes SET used_at = ? WHERE code_hash = ? AND used_at IS NULL AND expires_at > ?",
			ms(now), codeHash, ms(now))
		if err != nil {
			return err
		}
		if err := exec(ctx, tx, "insert user", insert("users", userCols), u.cols()...); err != nil {
			return err
		}
		sess.UserID = u.ID
		return exec(ctx, tx, "insert session", insert("sessions", sessionCols), sess.cols()...)
	})
}

// CreateSetupCode stores a new code. The codes that were not used stop working.
func (s *Store) CreateSetupCode(ctx context.Context, c SetupCode) error {
	return s.tx(ctx, "create setup code", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM setup_codes WHERE used_at IS NULL"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO setup_codes (code_hash, created_at, expires_at, used_at) VALUES (?, ?, ?, ?)",
			c.CodeHash, ms(c.CreatedAt), ms(c.ExpiresAt), msPtr(c.UsedAt))
		return dbErr(err)
	})
}

// HasOpenSetupCode reports if a setup code exists that is not used and not expired.
func (s *Store) HasOpenSetupCode(ctx context.Context, now time.Time) (bool, error) {
	return queryOne(ctx, s.r, "find setup code", scanValue[bool],
		"SELECT EXISTS (SELECT 1 FROM setup_codes WHERE used_at IS NULL AND expires_at > ?)", ms(now))
}

const sessionCols = "id_hash, user_id, created_at, expires_at, last_seen_at, ip, user_agent"

func (se *Session) cols() []any {
	return []any{&se.IDHash, &se.UserID, at(&se.CreatedAt), at(&se.ExpiresAt), at(&se.LastSeenAt), &se.IP, &se.UserAgent}
}

func (s *Store) CreateSession(ctx context.Context, se Session) error {
	return exec(ctx, s.w, "create session", insert("sessions", sessionCols), se.cols()...)
}

// SessionUser returns a session that has not expired at now, and its user.
func (s *Store) SessionUser(ctx context.Context, idHash string, now time.Time) (Session, User, error) {
	var se Session
	var u User
	err := s.r.QueryRowContext(ctx, `
		SELECT s.id_hash, s.user_id, s.created_at, s.expires_at, s.last_seen_at, s.ip, s.user_agent,
		       u.id, u.email, u.name, u.password_hash, u.role, u.created_at, u.last_login_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id_hash = ? AND s.expires_at > ?`, idHash, ms(now)).Scan(append(se.cols(), u.cols()...)...)
	if err != nil {
		return Session{}, User{}, fmt.Errorf("get session: %w", dbErr(err))
	}
	return se, u, nil
}

func (s *Store) TouchSession(ctx context.Context, idHash string, seen, expires time.Time) error {
	return exec1(ctx, s.w, "touch session", "UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id_hash = ?",
		ms(seen), ms(expires), idHash)
}

// DeleteSession does nothing if the session does not exist.
func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	return exec(ctx, s.w, "delete session", "DELETE FROM sessions WHERE id_hash = ?", idHash)
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	return exec(ctx, s.w, "delete sessions", "DELETE FROM sessions WHERE user_id = ?", userID)
}

const inviteCols = "id, email, role, token_hash, created_by, created_at, expires_at, accepted_at"

// selectOpenInvite selects the invite with a token hash if it is not
// accepted and not expired at a time.
const selectOpenInvite = "SELECT " + inviteCols +
	" FROM invites WHERE token_hash = ? AND accepted_at IS NULL AND expires_at > ?"

func (i *Invite) cols() []any {
	return []any{&i.ID, &i.Email, &i.Role, &i.TokenHash, text(&i.CreatedBy), at(&i.CreatedAt), at(&i.ExpiresAt),
		atPtr(&i.AcceptedAt)}
}

// CreateInvite stores the invite. Open invites for the same email stop working.
func (s *Store) CreateInvite(ctx context.Context, i Invite) error {
	return s.tx(ctx, "create invite", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM invites WHERE email = ? AND accepted_at IS NULL", i.Email); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, insert("invites", inviteCols), i.cols()...)
		return dbErr(err)
	})
}

// GetOpenInvite returns an invite that is not accepted and not expired at now.
func (s *Store) GetOpenInvite(ctx context.Context, tokenHash string, now time.Time) (Invite, error) {
	return queryOne(ctx, s.r, "get invite", scanRow[Invite], selectOpenInvite, tokenHash, ms(now))
}

// ListOpenInvites returns the invites that are not accepted and not expired at now.
func (s *Store) ListOpenInvites(ctx context.Context, now time.Time) ([]Invite, error) {
	return queryAll(ctx, s.r, "list invites", scanRow[Invite],
		"SELECT "+inviteCols+" FROM invites WHERE accepted_at IS NULL AND expires_at > ? ORDER BY created_at DESC", ms(now))
}

// DeleteInvite deletes an invite and returns it.
func (s *Store) DeleteInvite(ctx context.Context, id string) (Invite, error) {
	return queryOne(ctx, s.w, "delete invite", scanRow[Invite], "DELETE FROM invites WHERE id = ? RETURNING "+inviteCols, id)
}

// AcceptInvite marks the open invite accepted and makes the user (with the
// email and role of the invite) and its session, in one transaction. It
// returns ErrNotFound if the invite is not open and ErrConflict if the email
// is in use.
func (s *Store) AcceptInvite(ctx context.Context, tokenHash string, now time.Time, u User, sess Session) (User, error) {
	err := s.tx(ctx, "accept invite", func(tx *sql.Tx) error {
		inv, err := queryOne(ctx, tx, "get invite", scanRow[Invite], selectOpenInvite, tokenHash, ms(now))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE invites SET accepted_at = ? WHERE id = ?", ms(now), inv.ID); err != nil {
			return err
		}
		u.Email, u.Role = inv.Email, inv.Role
		if err := exec(ctx, tx, "insert user", insert("users", userCols), u.cols()...); err != nil {
			return err
		}
		sess.UserID = u.ID
		return exec(ctx, tx, "insert session", insert("sessions", sessionCols), sess.cols()...)
	})
	if err != nil {
		return User{}, err
	}
	return u, nil
}
