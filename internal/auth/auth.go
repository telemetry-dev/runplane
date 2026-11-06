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
	"log/slog"
	"net/mail"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/runplane/runplane/internal/audit"
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

var Roles = []string{RoleAdmin, RoleMember}

type Action int

const (
	ActionRead        Action = iota // read the dashboard data
	ActionRerunJob                  // re-run a job, also with debug
	ActionStopSandbox               // stop a sandbox
	ActionOpenShell                 // open the debug shell of a sandbox
	ActionAdmin                     // all other changes, and users, audit log, and backups
)

var (
	ErrSetupDone         = store.ErrSetupDone
	ErrLastAdmin         = store.ErrLastAdmin
	ErrOwnAccount        = errors.New("you cannot delete your own account")
	ErrBadSetupCode      = errors.New("the setup code is wrong, used, or expired")
	ErrBadLogin          = errors.New("the email or password is wrong")
	ErrBadPassword       = errors.New("the current password is wrong")
	ErrNoCurrentPassword = errors.New("enter your current password")
	ErrRateLimited       = errors.New("too many failed attempts, try again later")
	ErrNoSession         = errors.New("no valid session")
	ErrShortPassword     = fmt.Errorf("the password must have at least %d characters", MinPasswordLen)
	ErrBadEmail          = errors.New("the email address is not valid")
	ErrBadName           = errors.New("enter a name")
	ErrBadRole           = errors.New("the role must be admin or member")
	ErrBadInvite         = errors.New("the invite is wrong, used, or expired")
	ErrEmailTaken        = errors.New("a user with this email exists")
)

type Service struct {
	store     *store.Store
	audit     audit.Log
	limiter   *limiter
	deviceKey []byte // signs device tokens
	now       func() time.Time
}

// NewService returns the accounts of st. deviceKey signs the device tokens
// of browsers that logged in; a service without one makes no valid tokens.
// log gets the failed writes of the audit log, and can be nil.
func NewService(st *store.Store, deviceKey []byte, log *slog.Logger) *Service {
	return &Service{
		store: st, audit: audit.Log{Store: st, Logger: log}, limiter: &limiter{fails: map[string][]time.Time{}},
		deviceKey: deviceKey, now: time.Now,
	}
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
	return s.setupCode(ctx, false)
}

// EnsureSetupCode makes a setup code as NewSetupCode does, but only when no
// code is valid. It returns "" while a code is valid, because install.sh
// printed that code and only its hash is stored.
func (s *Service) EnsureSetupCode(ctx context.Context) (string, error) {
	return s.setupCode(ctx, true)
}

func (s *Service) setupCode(ctx context.Context, keepValid bool) (string, error) {
	n, err := s.store.CountUsers(ctx)
	if err != nil {
		return "", err
	}
	if n > 0 {
		return "", ErrSetupDone
	}
	now := s.now()
	if keepValid {
		if open, err := s.store.HasOpenSetupCode(ctx, now); err != nil || open {
			return "", err
		}
	}
	code := newSetupCode()
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
	s.audit.Record(ctx, audit.Actor{UserID: u.ID, IP: ip}, audit.SetupDone, u.ID, map[string]any{"email": u.Email})
	return u, token, nil
}

// SetupCodeValid reports whether code is an unused setup code that has not
// expired. Unlike Setup, it does not use the code.
func (s *Service) SetupCodeValid(ctx context.Context, code string) (bool, error) {
	return s.store.SetupCodeOpen(ctx, hashCode(code), s.now())
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
	if err := s.setPassword(ctx, u.ID, next); err != nil {
		return "", err
	}
	token, sess := newSession(u.ID, ip, userAgent, now)
	if err := s.store.CreateSession(ctx, sess); err != nil {
		return "", err
	}
	s.audit.Record(ctx, audit.Actor{UserID: u.ID, IP: ip}, audit.UserPassword, u.ID, nil)
	return token, nil
}

// ResetPassword gives the user with the email a new random password, ends
// all sessions of the user, and returns the password. It returns
// store.ErrNotFound when no user has the email.
func (s *Service) ResetPassword(ctx context.Context, by audit.Actor, email string) (string, error) {
	u, err := s.store.GetUserByEmail(ctx, cleanEmail(email))
	if err != nil {
		return "", err
	}
	password := rand.Text()
	if err := s.setPassword(ctx, u.ID, password); err != nil {
		return "", err
	}
	s.audit.Record(ctx, by, audit.UserPasswordReset, u.ID, map[string]any{"email": u.Email})
	return password, nil
}

// setPassword changes the password of a user. It ends all sessions of the
// user, because each browser must log in with the new password.
func (s *Service) setPassword(ctx context.Context, userID, password string) error {
	if err := s.store.SetUserPassword(ctx, userID, HashPassword(password)); err != nil {
		return err
	}
	return s.store.DeleteUserSessions(ctx, userID)
}

// UpdateMe changes the name or the password of a user, for the user, and
// returns the user. A nil name or next keeps it. A new password needs the
// current one (ErrNoCurrentPassword), works as in ChangePassword, and the
// token is "" without one. A refused name changes nothing.
func (s *Service) UpdateMe(ctx context.Context, userID string, name, current, next *string, ip, userAgent string) (store.User, string, error) {
	var newName string
	if name != nil {
		var err error
		if newName, err = userName(*name); err != nil {
			return store.User{}, "", err
		}
	}
	var token string
	if next != nil {
		if current == nil || *current == "" {
			return store.User{}, "", ErrNoCurrentPassword
		}
		var err error
		if token, err = s.ChangePassword(ctx, userID, *current, *next, ip, userAgent); err != nil {
			return store.User{}, "", err
		}
	}
	if name == nil {
		u, err := s.store.GetUser(ctx, userID)
		return u, token, err
	}
	u, err := s.store.SetUserName(ctx, userID, newName)
	return u, token, err
}

// SetRole gives a user a role, and returns the user. It returns ErrLastAdmin
// for the only admin, because runplane needs one.
func (s *Service) SetRole(ctx context.Context, by audit.Actor, userID, role string) (store.User, error) {
	if !slices.Contains(Roles, role) {
		return store.User{}, ErrBadRole
	}
	u, err := s.store.SetUserRole(ctx, userID, role)
	if err != nil {
		return store.User{}, err
	}
	if u.Role != role {
		u.Role = role
		s.audit.Record(ctx, by, audit.UserUpdate, u.ID, map[string]any{"email": u.Email, "role": u.Role})
	}
	return u, nil
}

// DeleteUser deletes a user with its sessions. A user cannot delete its own
// account, and runplane keeps its last admin (ErrLastAdmin).
func (s *Service) DeleteUser(ctx context.Context, by audit.Actor, userID string) error {
	if userID == by.UserID {
		return ErrOwnAccount
	}
	u, err := s.store.DeleteUser(ctx, userID)
	if err != nil {
		return err
	}
	s.audit.Record(ctx, by, audit.UserDelete, u.ID, map[string]any{"email": u.Email})
	return nil
}

// LastAdmin returns the ID of the only admin of users, or "" when two or more
// users are admins. That user cannot lose the role or be deleted.
func LastAdmin(users []store.User) string {
	var admins []string
	for _, u := range users {
		if u.Role == RoleAdmin {
			admins = append(admins, u.ID)
		}
	}
	if len(admins) != 1 {
		return ""
	}
	return admins[0]
}

// CreateInvite makes an invite, and returns it with the token for the invite
// URL. The invite is valid for 7 days, and earlier open invites for the email
// stop working.
func (s *Service) CreateInvite(ctx context.Context, by audit.Actor, email, role string) (store.Invite, string, error) {
	inv, token, err := s.invite(ctx, by, email, role)
	if err != nil {
		return store.Invite{}, "", err
	}
	s.audit.Record(ctx, by, audit.InviteCreate, inv.ID, map[string]any{"email": inv.Email, "role": inv.Role})
	return inv, token, nil
}

// NewInviteLink replaces the open invite id with a new one for the same
// email and role, because only the hash of the old token is stored. It
// returns store.ErrNotFound when no open invite has the ID.
func (s *Service) NewInviteLink(ctx context.Context, by audit.Actor, id string) (store.Invite, string, error) {
	invites, err := s.store.ListOpenInvites(ctx, s.now())
	if err != nil {
		return store.Invite{}, "", err
	}
	i := slices.IndexFunc(invites, func(inv store.Invite) bool { return inv.ID == id })
	if i < 0 {
		return store.Invite{}, "", fmt.Errorf("new invite link: %w", store.ErrNotFound)
	}
	inv, token, err := s.invite(ctx, by, invites[i].Email, invites[i].Role)
	if err != nil {
		return store.Invite{}, "", err
	}
	s.audit.Record(ctx, by, audit.InviteLink, inv.ID, map[string]any{"email": inv.Email, "old_id": id})
	return inv, token, nil
}

func (s *Service) DeleteInvite(ctx context.Context, by audit.Actor, id string) error {
	inv, err := s.store.DeleteInvite(ctx, id)
	if err != nil {
		return err
	}
	s.audit.Record(ctx, by, audit.InviteDelete, inv.ID, map[string]any{"email": inv.Email, "role": inv.Role})
	return nil
}

func (s *Service) invite(ctx context.Context, by audit.Actor, email, role string) (store.Invite, string, error) {
	email = cleanEmail(email)
	if !validEmail(email) {
		return store.Invite{}, "", ErrBadEmail
	}
	if !slices.Contains(Roles, role) {
		return store.Invite{}, "", ErrBadRole
	}
	_, err := s.store.GetUserByEmail(ctx, email)
	if err == nil {
		return store.Invite{}, "", ErrEmailTaken
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Invite{}, "", err
	}
	token, hash := newToken()
	now := s.now()
	inv := store.Invite{
		ID: ids.New(), Email: email, Role: role, TokenHash: hash, CreatedBy: by.UserID,
		CreatedAt: now, ExpiresAt: now.Add(InviteTTL),
	}
	if err := s.store.CreateInvite(ctx, inv); err != nil {
		return store.Invite{}, "", err
	}
	return inv, token, nil
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
	name, err := userName(name)
	if err != nil {
		return store.User{}, err
	}
	if utf8.RuneCountInString(password) < MinPasswordLen {
		return store.User{}, ErrShortPassword
	}
	return store.User{
		ID: ids.New(), Name: name, PasswordHash: HashPassword(password), CreatedAt: now, LastLoginAt: &now,
	}, nil
}

func userName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrBadName
	}
	return name, nil
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
