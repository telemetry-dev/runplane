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

func scanUser(sc scanner) (User, error) {
	var u User
	err := sc.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, at(&u.CreatedAt), atPtr(&u.LastLoginAt))
	return u, err
}

func insertUser(ctx context.Context, q querier, u User) error {
	_, err := q.ExecContext(ctx, "INSERT INTO users ("+userCols+") VALUES ("+placeholders(7)+")",
		u.ID, u.Email, u.Name, u.PasswordHash, u.Role, ms(u.CreatedAt), msPtr(u.LastLoginAt))
	return dbErr(err)
}

// CreateUser returns ErrConflict if the email is in use.
func (s *Store) CreateUser(ctx context.Context, u User) error {
	if err := insertUser(ctx, s.w, u); err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (s *Store) GetUser(ctx context.Context, id string) (User, error) {
	u, err := scanUser(s.r.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE id = ?", id))
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", dbErr(err))
	}
	return u, nil
}

// GetUserByEmail matches the email without regard to case.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (User, error) {
	u, err := scanUser(s.r.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE email = ?", email))
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", dbErr(err))
	}
	return u, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+userCols+" FROM users ORDER BY created_at, id")
	list, err := scanAll(rows, err, scanUser)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return list, nil
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// UpdateUser writes the name, email, and role. It returns ErrConflict if
// the email is in use.
func (s *Store) UpdateUser(ctx context.Context, u User) error {
	err := exec1(ctx, s.w, "UPDATE users SET name = ?, email = ?, role = ? WHERE id = ?", u.Name, u.Email, u.Role, u.ID)
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	return nil
}

func (s *Store) SetUserPassword(ctx context.Context, id, hash string) error {
	if err := exec1(ctx, s.w, "UPDATE users SET password_hash = ? WHERE id = ?", hash, id); err != nil {
		return fmt.Errorf("set user password: %w", err)
	}
	return nil
}

func (s *Store) SetUserLastLogin(ctx context.Context, id string, at time.Time) error {
	if err := exec1(ctx, s.w, "UPDATE users SET last_login_at = ? WHERE id = ?", ms(at), id); err != nil {
		return fmt.Errorf("set user last login: %w", err)
	}
	return nil
}

// DeleteUser also deletes the sessions of the user.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	if err := exec1(ctx, s.w, "DELETE FROM users WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return nil
}

// CreateFirstUser uses the setup code, makes the first user, and makes its
// session, in one transaction. It returns ErrSetupDone if a user exists and
// ErrNotFound if the code is unknown, used, or expired.
func (s *Store) CreateFirstUser(ctx context.Context, codeHash string, now time.Time, u User, sess Session) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrSetupDone
		}
		err := exec1(ctx, tx,
			"UPDATE setup_codes SET used_at = ? WHERE code_hash = ? AND used_at IS NULL AND expires_at > ?",
			ms(now), codeHash, ms(now))
		if err != nil {
			return err
		}
		if err := insertUser(ctx, tx, u); err != nil {
			return err
		}
		sess.UserID = u.ID
		return insertSession(ctx, tx, sess)
	})
	if err != nil {
		return fmt.Errorf("create first user: %w", err)
	}
	return nil
}

// CreateSetupCode stores a new code. The codes that were not used stop working.
func (s *Store) CreateSetupCode(ctx context.Context, c SetupCode) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM setup_codes WHERE used_at IS NULL"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO setup_codes (code_hash, created_at, expires_at, used_at) VALUES (?, ?, ?, ?)",
			c.CodeHash, ms(c.CreatedAt), ms(c.ExpiresAt), msPtr(c.UsedAt))
		return dbErr(err)
	})
	if err != nil {
		return fmt.Errorf("create setup code: %w", err)
	}
	return nil
}

const sessionCols = "id_hash, user_id, created_at, expires_at, last_seen_at, ip, user_agent"

func insertSession(ctx context.Context, q querier, se Session) error {
	_, err := q.ExecContext(ctx, "INSERT INTO sessions ("+sessionCols+") VALUES ("+placeholders(7)+")",
		se.IDHash, se.UserID, ms(se.CreatedAt), ms(se.ExpiresAt), ms(se.LastSeenAt), se.IP, se.UserAgent)
	return dbErr(err)
}

func (s *Store) CreateSession(ctx context.Context, se Session) error {
	if err := insertSession(ctx, s.w, se); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// SessionUser returns a session that has not expired at now, and its user.
func (s *Store) SessionUser(ctx context.Context, idHash string, now time.Time) (Session, User, error) {
	var se Session
	var u User
	err := s.r.QueryRowContext(ctx, `
		SELECT s.id_hash, s.user_id, s.created_at, s.expires_at, s.last_seen_at, s.ip, s.user_agent,
		       u.id, u.email, u.name, u.password_hash, u.role, u.created_at, u.last_login_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id_hash = ? AND s.expires_at > ?`, idHash, ms(now)).Scan(
		&se.IDHash, &se.UserID, at(&se.CreatedAt), at(&se.ExpiresAt), at(&se.LastSeenAt), &se.IP, &se.UserAgent,
		&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, at(&u.CreatedAt), atPtr(&u.LastLoginAt))
	if err != nil {
		return Session{}, User{}, fmt.Errorf("get session: %w", dbErr(err))
	}
	return se, u, nil
}

// TouchSession sets the last use and the new expiry of a session.
func (s *Store) TouchSession(ctx context.Context, idHash string, seen, expires time.Time) error {
	err := exec1(ctx, s.w, "UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id_hash = ?",
		ms(seen), ms(expires), idHash)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// DeleteSession does nothing if the session does not exist.
func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	if _, err := s.w.ExecContext(ctx, "DELETE FROM sessions WHERE id_hash = ?", idHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	if _, err := s.w.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", userID); err != nil {
		return fmt.Errorf("delete sessions: %w", err)
	}
	return nil
}

const inviteCols = "id, email, role, token_hash, created_by, created_at, expires_at, accepted_at"

func scanInvite(sc scanner) (Invite, error) {
	var i Invite
	var by sql.NullString
	err := sc.Scan(&i.ID, &i.Email, &i.Role, &i.TokenHash, &by, at(&i.CreatedAt), at(&i.ExpiresAt), atPtr(&i.AcceptedAt))
	i.CreatedBy = by.String
	return i, err
}

// CreateInvite stores the invite. Open invites for the same email stop working.
func (s *Store) CreateInvite(ctx context.Context, i Invite) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM invites WHERE email = ? AND accepted_at IS NULL", i.Email); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO invites ("+inviteCols+") VALUES ("+placeholders(8)+")",
			i.ID, i.Email, i.Role, i.TokenHash, nullStr(i.CreatedBy), ms(i.CreatedAt), ms(i.ExpiresAt), msPtr(i.AcceptedAt))
		return dbErr(err)
	})
	if err != nil {
		return fmt.Errorf("create invite: %w", err)
	}
	return nil
}

// GetOpenInvite returns an invite that is not accepted and not expired at now.
func (s *Store) GetOpenInvite(ctx context.Context, tokenHash string, now time.Time) (Invite, error) {
	i, err := scanInvite(s.r.QueryRowContext(ctx,
		"SELECT "+inviteCols+" FROM invites WHERE token_hash = ? AND accepted_at IS NULL AND expires_at > ?",
		tokenHash, ms(now)))
	if err != nil {
		return Invite{}, fmt.Errorf("get invite: %w", dbErr(err))
	}
	return i, nil
}

// ListOpenInvites returns the invites that are not accepted and not expired at now.
func (s *Store) ListOpenInvites(ctx context.Context, now time.Time) ([]Invite, error) {
	rows, err := s.r.QueryContext(ctx,
		"SELECT "+inviteCols+" FROM invites WHERE accepted_at IS NULL AND expires_at > ? ORDER BY created_at DESC", ms(now))
	list, err := scanAll(rows, err, scanInvite)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	return list, nil
}

// DeleteInvite deletes an invite and returns it.
func (s *Store) DeleteInvite(ctx context.Context, id string) (Invite, error) {
	i, err := scanInvite(s.w.QueryRowContext(ctx, "DELETE FROM invites WHERE id = ? RETURNING "+inviteCols, id))
	if err != nil {
		return Invite{}, fmt.Errorf("delete invite: %w", dbErr(err))
	}
	return i, nil
}

// AcceptInvite marks the open invite accepted and makes the user (with the
// email and role of the invite) and its session, in one transaction. It
// returns ErrNotFound if the invite is not open and ErrConflict if the email
// is in use.
func (s *Store) AcceptInvite(ctx context.Context, tokenHash string, now time.Time, u User, sess Session) (User, error) {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		inv, err := scanInvite(tx.QueryRowContext(ctx,
			"SELECT "+inviteCols+" FROM invites WHERE token_hash = ? AND accepted_at IS NULL AND expires_at > ?",
			tokenHash, ms(now)))
		if err != nil {
			return dbErr(err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE invites SET accepted_at = ? WHERE id = ?", ms(now), inv.ID); err != nil {
			return err
		}
		u.Email, u.Role = inv.Email, inv.Role
		if err := insertUser(ctx, tx, u); err != nil {
			return err
		}
		sess.UserID = u.ID
		return insertSession(ctx, tx, sess)
	})
	if err != nil {
		return User{}, fmt.Errorf("accept invite: %w", err)
	}
	return u, nil
}
