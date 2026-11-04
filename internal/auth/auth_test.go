package auth

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/store"
)

type testEnv struct {
	svc *Service
	st  *store.Store
	now time.Time
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &testEnv{st: st, now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	e.svc = NewService(st, []byte("device key"))
	e.svc.now = func() time.Time { return e.now }
	return e
}

func (e *testEnv) wait(d time.Duration) { e.now = e.now.Add(d) }

// setup makes the first admin admin@example.com with the password "first password".
func (e *testEnv) setup(t *testing.T) (store.User, string) {
	t.Helper()
	ctx := context.Background()
	code, err := e.svc.NewSetupCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u, token, err := e.svc.Setup(ctx, code, "Ada", " Admin@Example.com ", "first password", "10.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	return u, token
}

func TestPasswordHashFormat(t *testing.T) {
	// Made with the reference implementation (libargon2):
	// echo -n "correct horse battery" | argon2 saltsaltsaltsalt -id -t 2 -k 19456 -p 1 -l 32
	const ref = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$iPOQ5f2O21FsjnBvo1AiFcDuSXciCnKUrXFO+yfgNoM"
	if got := hashWithSalt("correct horse battery", []byte("saltsaltsaltsalt")); got != ref {
		t.Errorf("hash:\n got %s\nwant %s", got, ref)
	}
	if ok, err := CheckPassword(ref, "correct horse battery"); !ok || err != nil {
		t.Errorf("right password: %v, %v", ok, err)
	}
	if ok, err := CheckPassword(ref, "correct horse batterY"); ok || err != nil {
		t.Errorf("wrong password: %v, %v", ok, err)
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA", "$argon2id$v=19$m=0,t=2,p=1$c2FsdA$aGFzaA"} {
		if _, err := CheckPassword(bad, "x"); err == nil {
			t.Errorf("CheckPassword(%q) has no error", bad)
		}
	}
}

func TestLoginWrongPassword(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	admin, _ := e.setup(t)
	if admin.Email != "admin@example.com" || admin.Role != RoleAdmin {
		t.Fatalf("admin = %+v", admin)
	}

	if _, _, err := e.svc.Login(ctx, "admin@example.com", "wrong password", "", "10.0.0.2", ""); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("wrong password: err = %v", err)
	}
	if _, _, err := e.svc.Login(ctx, "nobody@example.com", "first password", "", "10.0.0.2", ""); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("unknown email: err = %v", err)
	}
	u, token, err := e.svc.Login(ctx, "ADMIN@example.com", "first password", "", "10.0.0.2", "")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != admin.ID || token == "" {
		t.Fatalf("login: %+v, token %q", u, token)
	}
	if got, err := e.svc.UserForToken(ctx, token); err != nil || got.ID != admin.ID {
		t.Fatalf("session of login: %v, %v", got.ID, err)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.setup(t)
	// Ten failures for one email, from different addresses, lock the email.
	for i := range 10 {
		ip := "10.1.0." + string(rune('a'+i))
		if _, _, err := e.svc.Login(ctx, "admin@example.com", "wrong password", "", ip, ""); !errors.Is(err, ErrBadLogin) {
			t.Fatalf("failure %d: err = %v", i+1, err)
		}
	}
	if _, _, err := e.svc.Login(ctx, "admin@example.com", "first password", "", "10.9.9.9", ""); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("right password after 10 failures: err = %v, want ErrRateLimited", err)
	}
	e.wait(15 * time.Minute)
	if _, _, err := e.svc.Login(ctx, "admin@example.com", "first password", "", "10.9.9.9", ""); err != nil {
		t.Fatalf("after 15 minutes: %v", err)
	}

	// Ten failures from one address, for different emails, lock the address.
	for i := range 10 {
		email := string(rune('a'+i)) + "@example.com"
		if _, _, err := e.svc.Login(ctx, email, "wrong password", "", "10.2.0.1", ""); !errors.Is(err, ErrBadLogin) {
			t.Fatalf("failure %d: err = %v", i+1, err)
		}
	}
	if _, _, err := e.svc.Login(ctx, "admin@example.com", "first password", "", "10.2.0.1", ""); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("locked address: err = %v, want ErrRateLimited", err)
	}
	if _, _, err := e.svc.Login(ctx, "admin@example.com", "first password", "", "10.2.0.2", ""); err != nil {
		t.Fatalf("other address: %v", err)
	}
}

// Anyone who knows the email of the admin can lock the email. A browser
// that logged in to the account before must still get in.
func TestLoginFromAKnownDevice(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	admin, _ := e.setup(t)
	device := e.svc.Device(admin.ID, "")
	if again := e.svc.Device(admin.ID, device); again != device {
		t.Fatalf("a valid device token changed at a new login: %q, want %q", again, device)
	}
	login := func(password, device, ip string) error {
		_, _, err := e.svc.Login(ctx, "admin@example.com", password, device, ip, "")
		return err
	}
	for i := range 10 {
		if err := login("wrong password", "", "10.1.0."+string(rune('a'+i))); !errors.Is(err, ErrBadLogin) {
			t.Fatalf("failure %d: err = %v", i+1, err)
		}
	}

	other := e.svc.Device("someone else", "")
	id, mac, _ := strings.Cut(device, ".")
	for name, token := range map[string]string{
		"no device":                     "",
		"device of another user":        other,
		"device with a changed ID":      "x" + id + "." + mac,
		"device of a key that is wrong": NewService(e.st, []byte("other key")).Device(admin.ID, ""),
	} {
		if err := login("first password", token, "10.9.9.9"); !errors.Is(err, ErrRateLimited) {
			t.Errorf("%s with the email locked: err = %v, want ErrRateLimited", name, err)
		}
	}
	if err := login("first password", device, "10.9.9.9"); err != nil {
		t.Fatalf("known device with the email locked: %v", err)
	}

	// The device has a limit of its own.
	for i := range 10 {
		if err := login("wrong password", device, "10.3.0."+string(rune('a'+i))); !errors.Is(err, ErrBadLogin) {
			t.Fatalf("device failure %d: err = %v", i+1, err)
		}
	}
	if err := login("first password", device, "10.9.9.10"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("right password after 10 failures of the device: err = %v, want ErrRateLimited", err)
	}
}

func TestSessionSlidingExpiry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	admin, token := e.setup(t)

	// Each use extends the session by 30 days.
	for range 3 {
		e.wait(29 * 24 * time.Hour)
		if u, err := e.svc.UserForToken(ctx, token); err != nil || u.ID != admin.ID {
			t.Fatalf("after 29 days: %q, %v", u.ID, err)
		}
	}
	e.wait(30*24*time.Hour + time.Second)
	if _, err := e.svc.UserForToken(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after 30 days without use: err = %v, want ErrNoSession", err)
	}
	if _, err := e.svc.UserForToken(ctx, "not a token"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("unknown token: err = %v", err)
	}

	_, token, err := e.svc.Login(ctx, "admin@example.com", "first password", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UserForToken(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after logout: err = %v", err)
	}
}

func TestSetupCode(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	old, err := e.svc.NewSetupCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[A-HJ-NP-Z2-9]{4}-[A-HJ-NP-Z2-9]{4}-[A-HJ-NP-Z2-9]{4}$`).MatchString(old) {
		t.Fatalf("code %q has the wrong format", old)
	}
	code, err := e.svc.NewSetupCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	setup := func(code string) (store.User, error) {
		u, _, err := e.svc.Setup(ctx, code, "Ada", "admin@example.com", "first password", "10.0.0.1", "")
		return u, err
	}
	if _, err := setup(old); !errors.Is(err, ErrBadSetupCode) {
		t.Fatalf("replaced code: err = %v, want ErrBadSetupCode", err)
	}
	if _, err := setup("AAAA-BBBB-CCCC"); !errors.Is(err, ErrBadSetupCode) {
		t.Fatalf("wrong code: err = %v", err)
	}
	if _, _, err := e.svc.Setup(ctx, code, "Ada", "admin@example.com", "short", "", ""); !errors.Is(err, ErrShortPassword) {
		t.Fatalf("short password: err = %v", err)
	}

	// Lowercase without dashes also works.
	typed := code[0:4] + code[5:9] + code[10:14]
	u, err := setup(" " + strings.ToLower(typed) + " ")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// The code is used: it fails even when no user exists again.
	if err := e.st.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup(code); !errors.Is(err, ErrBadSetupCode) {
		t.Fatalf("used code: err = %v, want ErrBadSetupCode", err)
	}

	expiring, err := e.svc.NewSetupCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.wait(24 * time.Hour)
	if _, err := setup(expiring); !errors.Is(err, ErrBadSetupCode) {
		t.Fatalf("expired code: err = %v, want ErrBadSetupCode", err)
	}
}

func TestSetupRefusedAfterFirstAdmin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	code, err := e.svc.NewSetupCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A user appears after the code was made.
	if err := e.st.CreateUser(ctx, store.User{ID: "u1", Email: "first@example.com", Name: "First",
		PasswordHash: HashPassword("first password"), Role: RoleAdmin, CreatedAt: e.now}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Setup(ctx, code, "Eve", "eve@example.com", "eve password", "", ""); !errors.Is(err, ErrSetupDone) {
		t.Fatalf("setup with a valid code after the first user: err = %v, want ErrSetupDone", err)
	}
	if _, err := e.svc.NewSetupCode(ctx); !errors.Is(err, ErrSetupDone) {
		t.Fatalf("new code after the first user: err = %v, want ErrSetupDone", err)
	}
	if n, _ := e.st.CountUsers(ctx); n != 1 {
		t.Fatalf("users = %d, want 1", n)
	}
}

func TestInviteSingleUse(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	admin, _ := e.setup(t)

	if _, err := e.svc.CreateInvite(ctx, admin.ID, "admin@example.com", RoleMember); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("invite for a user: err = %v", err)
	}
	if _, err := e.svc.CreateInvite(ctx, admin.ID, "bob@example.com", "owner"); !errors.Is(err, ErrBadRole) {
		t.Fatalf("bad role: err = %v", err)
	}
	token, err := e.svc.CreateInvite(ctx, admin.ID, " Bob@Example.com", RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := e.svc.GetInvite(ctx, token)
	if err != nil || inv.Email != "bob@example.com" || inv.CreatedBy != admin.ID {
		t.Fatalf("get invite: %+v, %v", inv, err)
	}

	bob, session, err := e.svc.AcceptInvite(ctx, token, "Bob", "bob password!", "10.0.0.3", "")
	if err != nil {
		t.Fatal(err)
	}
	if bob.Email != "bob@example.com" || bob.Role != RoleMember || bob.Name != "Bob" {
		t.Fatalf("bob = %+v", bob)
	}
	if u, err := e.svc.UserForToken(ctx, session); err != nil || u.ID != bob.ID {
		t.Fatalf("session of bob: %q, %v", u.ID, err)
	}
	if _, err := e.svc.GetInvite(ctx, token); !errors.Is(err, ErrBadInvite) {
		t.Fatalf("get used invite: err = %v", err)
	}
	if _, _, err := e.svc.AcceptInvite(ctx, token, "Mallory", "mallory password", "", ""); !errors.Is(err, ErrBadInvite) {
		t.Fatalf("second accept: err = %v, want ErrBadInvite", err)
	}

	late, err := e.svc.CreateInvite(ctx, admin.ID, "carol@example.com", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	e.wait(InviteTTL)
	if _, _, err := e.svc.AcceptInvite(ctx, late, "Carol", "carol password", "", ""); !errors.Is(err, ErrBadInvite) {
		t.Fatalf("expired invite: err = %v, want ErrBadInvite", err)
	}
}

func TestChangePassword(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	admin, oldToken := e.setup(t)
	if _, err := e.svc.ChangePassword(ctx, admin.ID, "wrong password", "second password", "", ""); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("wrong current: err = %v", err)
	}
	if _, err := e.svc.ChangePassword(ctx, admin.ID, "first password", "short", "", ""); !errors.Is(err, ErrShortPassword) {
		t.Fatalf("short new: err = %v", err)
	}
	token, err := e.svc.ChangePassword(ctx, admin.ID, "first password", "second password", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// The change ends the other sessions and keeps the browser logged in.
	if _, err := e.svc.UserForToken(ctx, oldToken); !errors.Is(err, ErrNoSession) {
		t.Errorf("old session after the change: err = %v, want ErrNoSession", err)
	}
	if u, err := e.svc.UserForToken(ctx, token); err != nil || u.ID != admin.ID {
		t.Errorf("new session: %q, %v", u.ID, err)
	}
	if _, _, err := e.svc.Login(ctx, "admin@example.com", "first password", "", "", ""); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("old password: err = %v", err)
	}
	if _, _, err := e.svc.Login(ctx, "admin@example.com", "second password", "", "", ""); err != nil {
		t.Fatalf("new password: %v", err)
	}
}

func TestCanDo(t *testing.T) {
	tests := []struct {
		role string
		a    Action
		want bool
	}{
		{RoleAdmin, ActionAdmin, true},
		{RoleMember, ActionRead, true},
		{RoleMember, ActionRerunJob, true},
		{RoleMember, ActionStopSandbox, true},
		{RoleMember, ActionOpenShell, true},
		{RoleMember, ActionAdmin, false},
		{"", ActionRead, false},
		{"owner", ActionRead, false},
	}
	for _, tt := range tests {
		if got := CanDo(tt.role, tt.a); got != tt.want {
			t.Errorf("CanDo(%q, %d) = %v, want %v", tt.role, tt.a, got, tt.want)
		}
	}
}
