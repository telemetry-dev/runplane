// Package auth has the dashboard accounts: passwords, sessions, the one-time
// setup code, invites, the login rate limit, and roles.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/store"
)

const (
	// SessionTTL is the sliding lifetime of a session: each use extends it.
	SessionTTL   = 30 * 24 * time.Hour
	SetupCodeTTL = 24 * time.Hour
	InviteTTL    = 7 * 24 * time.Hour

	MinPasswordLen = 10

	// touchEvery limits the session writes to one per minute for each session.
	touchEvery = time.Minute
)

const (
	RoleAdmin  = store.RoleAdmin
	RoleMember = store.RoleMember
)

type Action int

const (
	ActionRead        Action = iota // read the dashboard data
	ActionRerunJob                  // re-run a job, also with debug
	ActionStopSandbox               // stop a sandbox
	ActionOpenShell                 // open the debug shell of a sandbox
	ActionAdmin                     // all other changes, and users, audit log, and backups
)

var (
	ErrSetupDone     = store.ErrSetupDone
	ErrBadSetupCode  = errors.New("the setup code is wrong, used, or expired")
	ErrBadLogin      = errors.New("the email or password is wrong")
	ErrBadPassword   = errors.New("the current password is wrong")
	ErrRateLimited   = errors.New("too many failed attempts, try again later")
	ErrNoSession     = errors.New("no valid session")
	ErrShortPassword = fmt.Errorf("the password must have at least %d characters", MinPasswordLen)
	ErrBadEmail      = errors.New("the email address is not valid")
	ErrBadName       = errors.New("enter a name")
	ErrBadRole       = errors.New("the role must be admin or member")
	ErrBadInvite     = errors.New("the invite is wrong, used, or expired")
	ErrEmailTaken    = errors.New("a user with this email exists")
)

type Service struct {
	store     *store.Store
	limiter   *limiter
	deviceKey []byte // signs device tokens
	now       func() time.Time
}

// NewService returns the accounts of st. deviceKey signs the device tokens
// of browsers that logged in; a service without one makes no valid tokens.
func NewService(st *store.Store, deviceKey []byte) *Service {
	return &Service{store: st, limiter: &limiter{fails: map[string][]time.Time{}}, deviceKey: deviceKey, now: time.Now}
}

// CanDo reports if a user with role can do the action. An unknown role can do nothing.
func CanDo(role string, a Action) bool {
	switch role {
	case RoleAdmin:
		return true
	case RoleMember:
		switch a {
		case ActionRead, ActionRerunJob, ActionStopSandbox, ActionOpenShell:
			return true
		}
	}
	return false
}

// NewSetupCode makes a new setup code in the format XXXX-XXXX-XXXX. It is
// valid for 24 hours, and the earlier codes stop working.
func (s *Service) NewSetupCode(ctx context.Context) (string, error) {
	n, err := s.store.CountUsers(ctx)
	if err != nil {
		return "", err
	}
	if n > 0 {
		return "", ErrSetupDone
	}
	code := newSetupCode()
	now := s.now()
	err = s.store.CreateSetupCode(ctx, store.SetupCode{
		CodeHash: hashCode(code), CreatedAt: now, ExpiresAt: now.Add(SetupCodeTTL),
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

// Setup uses the setup code to make the first admin, and returns the admin
// and a session token. It fails after the first user exists.
func (s *Service) Setup(ctx context.Context, code, name, email, password, ip, userAgent string) (store.User, string, error) {
	now := s.now()
	ipKey := "ip:" + ip
	if s.limiter.blocked(ipKey, now) {
		return store.User{}, "", ErrRateLimited
	}
	email = cleanEmail(email)
	if !validEmail(email) {
		return store.User{}, "", ErrBadEmail
	}
	u, err := newUser(name, password, now)
	if err != nil {
		return store.User{}, "", err
	}
	u.Email, u.Role = email, RoleAdmin
	token, sess := newSession(u.ID, ip, userAgent, now)
	err = s.store.CreateFirstUser(ctx, hashCode(code), now, u, sess)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.limiter.fail(ipKey, now)
		return store.User{}, "", ErrBadSetupCode
	case err != nil:
		return store.User{}, "", err
	}
	return u, token, nil
}

// SetupCodeValid reports whether code is an unused setup code that has not
// expired. Unlike Setup, it does not use the code.
func (s *Service) SetupCodeValid(ctx context.Context, code string) (bool, error) {
	var n int
	err := s.store.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM setup_codes WHERE code_hash = ? AND used_at IS NULL AND expires_at > ?",
		hashCode(code), s.now().UnixMilli()).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check setup code: %w", err)
	}
	return n > 0, nil
}

// Login checks the password and returns the user and a new session token.
// After 10 failures in 15 minutes for the IP address or the email, it
// returns ErrRateLimited. device is the device token of the browser (see
// Device). With a valid one for the user, the browser has a count of its
// own in place of the count of the email, so failures from elsewhere
// cannot lock out a browser that logged in before.
func (s *Service) Login(ctx context.Context, email, password, device, ip, userAgent string) (store.User, string, error) {
	now := s.now()
	email = cleanEmail(email)
	ipKey := "ip:" + ip
	if s.limiter.blocked(ipKey, now) {
		return store.User{}, "", ErrRateLimited
	}
	u, err := s.store.GetUserByEmail(ctx, email)
	hash := u.PasswordHash
	if errors.Is(err, store.ErrNotFound) {
		hash = dummyHash()
	} else if err != nil {
		return store.User{}, "", err
	}
	key := "email:" + email
	if id, ok := s.deviceID(u.ID, device); ok {
		key = "device:" + id
	}
	if s.limiter.blocked(key, now) {
		return store.User{}, "", ErrRateLimited
	}
	ok, err := CheckPassword(hash, password)
	if err != nil {
		return store.User{}, "", fmt.Errorf("check password of %s: %w", u.ID, err)
	}
	if !ok || u.ID == "" {
		s.limiter.fail(ipKey, now)
		s.limiter.fail(key, now)
		return store.User{}, "", ErrBadLogin
	}
	// The count of the IP address does not reset: an attacker who also has
	// an account must not reset it.
	s.limiter.reset(key)

	token, sess := newSession(u.ID, ip, userAgent, now)
	if err := s.store.CreateSession(ctx, sess); err != nil {
		return store.User{}, "", err
	}
	if err := s.store.SetUserLastLogin(ctx, u.ID, now); err != nil {
		return store.User{}, "", err
	}
	u.LastLoginAt = &now
	return u, token, nil
}

// Device returns the device token for the browser of a user after a login:
// token if it is valid for the user, or a new one. The dashboard keeps it in
// a cookie, and Login gives a browser with the token a rate limit of its own.
func (s *Service) Device(userID, token string) string {
	if _, ok := s.deviceID(userID, token); ok {
		return token
	}
	b := make([]byte, 16)
	rand.Read(b)
	id := base64.RawURLEncoding.EncodeToString(b)
	return id + "." + s.deviceMAC(userID, id)
}

func (s *Service) Logout(ctx context.Context, token string) error {
	return s.store.DeleteSession(ctx, hashToken(token))
}

// UserForToken returns the user of a valid session and extends the session.
func (s *Service) UserForToken(ctx context.Context, token string) (store.User, error) {
	if token == "" {
		return store.User{}, ErrNoSession
	}
	now := s.now()
	hash := hashToken(token)
	sess, u, err := s.store.SessionUser(ctx, hash, now)
	if err == nil && now.Sub(sess.LastSeenAt) >= touchEvery {
		err = s.store.TouchSession(ctx, hash, now, now.Add(SessionTTL))
	}
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, ErrNoSession
	}
	if err != nil {
		return store.User{}, err
	}
	return u, nil
}

// ChangePassword sets a new password after it checks the current one. It
// ends all sessions of the user and returns the token of a new session for
// the browser that made the change. The login rate limit does not apply,
// so a lock of the email cannot log out the user.
func (s *Service) ChangePassword(ctx context.Context, userID, current, next, ip, userAgent string) (string, error) {
	now := s.now()
	key := "user:" + userID
	if s.limiter.blocked(key, now) {
		return "", ErrRateLimited
	}
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return "", err
	}
	ok, err := CheckPassword(u.PasswordHash, current)
	if err != nil {
		return "", fmt.Errorf("check password of %s: %w", u.ID, err)
	}
	if !ok {
		s.limiter.fail(key, now)
		return "", ErrBadPassword
	}
	if utf8.RuneCountInString(next) < MinPasswordLen {
		return "", ErrShortPassword
	}
	if err := s.store.SetUserPassword(ctx, u.ID, HashPassword(next)); err != nil {
		return "", err
	}
	if err := s.store.DeleteUserSessions(ctx, u.ID); err != nil {
		return "", err
	}
	token, sess := newSession(u.ID, ip, userAgent, now)
	if err := s.store.CreateSession(ctx, sess); err != nil {
		return "", err
	}
	return token, nil
}

// CreateInvite returns the token for the invite URL. The invite is valid
// for 7 days, and earlier open invites for the email stop working.
func (s *Service) CreateInvite(ctx context.Context, by, email, role string) (string, error) {
	email = cleanEmail(email)
	if !validEmail(email) {
		return "", ErrBadEmail
	}
	if role != RoleAdmin && role != RoleMember {
		return "", ErrBadRole
	}
	_, err := s.store.GetUserByEmail(ctx, email)
	if err == nil {
		return "", ErrEmailTaken
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	token, hash := newToken()
	now := s.now()
	err = s.store.CreateInvite(ctx, store.Invite{
		ID: ids.New(), Email: email, Role: role, TokenHash: hash, CreatedBy: by,
		CreatedAt: now, ExpiresAt: now.Add(InviteTTL),
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// GetInvite returns an open invite, or ErrBadInvite.
func (s *Service) GetInvite(ctx context.Context, token string) (store.Invite, error) {
	inv, err := s.store.GetOpenInvite(ctx, hashToken(token), s.now())
	if errors.Is(err, store.ErrNotFound) {
		return store.Invite{}, ErrBadInvite
	}
	return inv, err
}

// AcceptInvite makes the user of the invite and returns it with a session token.
func (s *Service) AcceptInvite(ctx context.Context, token, name, password, ip, userAgent string) (store.User, string, error) {
	now := s.now()
	u, err := newUser(name, password, now) // the invite gives the email and the role
	if err != nil {
		return store.User{}, "", err
	}
	sessToken, sess := newSession(u.ID, ip, userAgent, now)
	u, err = s.store.AcceptInvite(ctx, hashToken(token), now, u, sess)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.User{}, "", ErrBadInvite
	case errors.Is(err, store.ErrConflict):
		return store.User{}, "", ErrEmailTaken
	case err != nil:
		return store.User{}, "", err
	}
	return u, sessToken, nil
}

// deviceID returns the ID of a device token that Device made for userID.
func (s *Service) deviceID(userID, token string) (string, bool) {
	id, mac, ok := strings.Cut(token, ".")
	if !ok || userID == "" || len(s.deviceKey) == 0 {
		return "", false
	}
	return id, hmac.Equal([]byte(mac), []byte(s.deviceMAC(userID, id)))
}

func (s *Service) deviceMAC(userID, id string) string {
	m := hmac.New(sha256.New, s.deviceKey)
	m.Write([]byte(userID + "." + id))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// newUser checks the name and the password, and returns a user with no email and role.
func newUser(name, password string, now time.Time) (store.User, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.User{}, ErrBadName
	}
	if utf8.RuneCountInString(password) < MinPasswordLen {
		return store.User{}, ErrShortPassword
	}
	return store.User{
		ID: ids.New(), Name: name, PasswordHash: HashPassword(password), CreatedAt: now, LastLoginAt: &now,
	}, nil
}

func newSession(userID, ip, userAgent string, now time.Time) (string, store.Session) {
	token, hash := newToken()
	return token, store.Session{
		IDHash: hash, UserID: userID, CreatedAt: now, ExpiresAt: now.Add(SessionTTL), LastSeenAt: now,
		IP: ip, UserAgent: userAgent,
	}
}

// newToken returns 32 random bytes in base64url and the hash to store.
func newToken() (token, hash string) {
	b := make([]byte, 32)
	rand.Read(b)
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// codeAlphabet has no 0, O, 1, or I.
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func newSetupCode() string {
	b := make([]byte, 12)
	rand.Read(b)
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)] // 256 is a multiple of 32: no bias
	}
	return string(b[0:4]) + "-" + string(b[4:8]) + "-" + string(b[8:12])
}

// hashCode hashes the code without dashes and spaces, in uppercase, so
// "abcd efgh jkmn" also works.
func hashCode(code string) string {
	code = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
	return hashToken(code)
}

func cleanEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func validEmail(email string) bool {
	a, err := mail.ParseAddress(email)
	return err == nil && a.Address == email
}
