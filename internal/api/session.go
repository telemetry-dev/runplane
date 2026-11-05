package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/field"
	"github.com/runplane/runplane/internal/store"
)

type userKey struct{}

// currentUser is the user of the session. authorize sets it for all
// routes except the public ones.
func currentUser(ctx context.Context) store.User {
	u, _ := ctx.Value(userKey{}).(store.User)
	return u
}

// authorize is the Huma middleware that checks the session and the role
// for the auth.Action of the operation.
func (h *Handler) authorize(ctx huma.Context, next func(huma.Context)) {
	action, _ := ctx.Operation().Metadata[actionKey].(auth.Action)
	if action == public {
		next(ctx)
		return
	}
	u, err := h.d.Auth.UserForToken(ctx.Context(), clientOf(ctx.Context()).token)
	if errors.Is(err, auth.ErrNoSession) {
		huma.WriteErr(h.api, ctx, http.StatusUnauthorized, "Log in to continue.")
		return
	}
	if err != nil {
		h.d.Log.Error("read session", "err", err)
		huma.WriteErr(h.api, ctx, http.StatusInternalServerError, "The session could not be read.")
		return
	}
	if !auth.CanDo(u.Role, action) {
		huma.WriteErr(h.api, ctx, http.StatusForbidden, "Only an admin can do this.")
		return
	}
	next(huma.WithValue(ctx, userKey{}, u))
}

type User struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	Role        string     `json:"role" enum:"admin,member"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at"`
}

func userOut(u store.User) User {
	return User{ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role, CreatedAt: u.CreatedAt, LastLoginAt: u.LastLoginAt}
}

// Session is the user of the session, with what the user can do.
type Session struct {
	User
	Capabilities Capabilities `json:"capabilities"`
}

// Capabilities are the actions that a role allows. All roles can read the
// dashboard data.
type Capabilities struct {
	RerunJobs     bool `json:"rerun_jobs" doc:"Run a job again, also with a debug sandbox."`
	StopSandboxes bool `json:"stop_sandboxes"`
	OpenShells    bool `json:"open_shells" doc:"Open the debug shell of a sandbox."`
	Admin         bool `json:"admin" doc:"All other changes, and the members, the notification channels, the audit log, and the backups."`
}

func capabilities(role string) Capabilities {
	return Capabilities{
		RerunJobs:     auth.CanDo(role, auth.ActionRerunJob),
		StopSandboxes: auth.CanDo(role, auth.ActionStopSandbox),
		OpenShells:    auth.CanDo(role, auth.ActionOpenShell),
		Admin:         auth.CanDo(role, auth.ActionAdmin),
	}
}

func sessionOut(u store.User) Session {
	return Session{User: userOut(u), Capabilities: capabilities(u.Role)}
}

type sessionOutput struct {
	SetCookie []string `header:"Set-Cookie"`
	Body      Session
}

func (h *Handler) getSession(ctx context.Context, _ *struct{}) (*sessionOutput, error) {
	// Send the cookie again, so it lives as long as the sliding session.
	return &sessionOutput{
		SetCookie: []string{sessionCookieValue(ctx, clientOf(ctx).token)},
		Body:      sessionOut(currentUser(ctx)),
	}, nil
}

// newSession returns the output for a new session of u: the session cookie,
// and the device cookie that gives this browser a login rate limit of its
// own for u.
func (h *Handler) newSession(ctx context.Context, u store.User, token string) *sessionOutput {
	device := h.d.Auth.Device(u.ID, clientOf(ctx).device)
	return &sessionOutput{
		SetCookie: []string{sessionCookieValue(ctx, token), deviceCookieValue(ctx, device)},
		Body:      sessionOut(u),
	}
}

type LoginRequest struct {
	Email    string `json:"email" maxLength:"254"`
	Password string `json:"password" maxLength:"1024"`
}

func (h *Handler) login(ctx context.Context, in *body[LoginRequest]) (*sessionOutput, error) {
	c := clientOf(ctx)
	u, token, err := h.d.Auth.Login(ctx, in.Body.Email, in.Body.Password, c.device, c.ip, c.userAgent)
	if err != nil {
		return nil, authErr(err, "body.password")
	}
	// The new cookie replaces the old one, so the old session is of no use.
	if c.token != "" {
		if err := h.d.Auth.Logout(ctx, c.token); err != nil {
			h.d.Log.Warn("end earlier session", "err", err)
		}
	}
	return h.newSession(ctx, u, token), nil
}

type cookieOutput struct {
	SetCookie string `header:"Set-Cookie"`
}

func (h *Handler) logout(ctx context.Context, _ *struct{}) (*cookieOutput, error) {
	if token := clientOf(ctx).token; token != "" {
		if err := h.d.Auth.Logout(ctx, token); err != nil {
			return nil, err
		}
	}
	return &cookieOutput{SetCookie: sessionCookieValue(ctx, "")}, nil
}

type UpdateMeRequest struct {
	Name            *string `json:"name,omitempty" maxLength:"100"`
	CurrentPassword *string `json:"current_password,omitempty" maxLength:"1024" doc:"Required with new_password."`
	NewPassword     *string `json:"new_password,omitempty" minLength:"10" maxLength:"1024" doc:"Logs out the other sessions of the user."`
}

func (h *Handler) updateMe(ctx context.Context, in *body[UpdateMeRequest]) (*sessionOutput, error) {
	b := in.Body
	if b.NewPassword != nil && (b.CurrentPassword == nil || *b.CurrentPassword == "") {
		return nil, invalid("body.current_password", "Enter your current password.", nil)
	}
	c := clientOf(ctx)
	u, token, err := h.d.Auth.UpdateMe(ctx, currentUser(ctx).ID, b.Name, b.CurrentPassword, b.NewPassword, c.ip, c.userAgent)
	if err != nil {
		return nil, authErr(err, "body.new_password")
	}
	// A new password ends the sessions on other devices. This device gets
	// a new session.
	if token != "" {
		return h.newSession(ctx, u, token), nil
	}
	return &sessionOutput{Body: sessionOut(u)}, nil
}

// authErr maps the errors of auth to problems. passwordLoc is the field
// that ErrShortPassword belongs to.
func authErr(err error, passwordLoc string) error {
	msg := field.Sentence(err)
	switch {
	case errors.Is(err, auth.ErrRateLimited):
		return huma.Error429TooManyRequests(msg)
	case errors.Is(err, auth.ErrBadLogin):
		return huma.Error401Unauthorized(msg)
	case errors.Is(err, auth.ErrSetupDone):
		return huma.Error409Conflict("The setup is done. Log in instead.")
	case errors.Is(err, auth.ErrLastAdmin):
		return huma.Error409Conflict("runplane needs at least one admin.")
	case errors.Is(err, auth.ErrOwnAccount):
		return huma.Error409Conflict(msg)
	case errors.Is(err, auth.ErrBadInvite):
		return huma.Error404NotFound(msg)
	case errors.Is(err, auth.ErrEmailTaken):
		return conflict("body.email", msg)
	case errors.Is(err, auth.ErrBadSetupCode):
		return invalid("body.code", msg, nil)
	case errors.Is(err, auth.ErrBadName):
		return invalid("body.name", msg, nil)
	case errors.Is(err, auth.ErrBadEmail):
		return invalid("body.email", msg, nil)
	case errors.Is(err, auth.ErrBadRole):
		return invalid("body.role", msg, nil)
	case errors.Is(err, auth.ErrShortPassword):
		return invalid(passwordLoc, msg, nil)
	case errors.Is(err, auth.ErrBadPassword):
		return invalid("body.current_password", msg, nil)
	}
	return err
}

// deviceCookieValue returns the Set-Cookie value for a device token. The
// cookie outlives logouts and sessions: it marks a browser that logged in.
// The browser sends it only to /api/session, where the login reads it.
func deviceCookieValue(ctx context.Context, device string) string {
	c := http.Cookie{
		Name:     deviceCookie,
		Value:    device,
		Path:     "/api/session",
		MaxAge:   400 * 24 * 60 * 60, // the longest time that browsers keep a cookie
		HttpOnly: true,
		Secure:   clientOf(ctx).https,
		SameSite: http.SameSiteStrictMode,
	}
	return c.String()
}

// sessionCookieValue returns the Set-Cookie value for token. An empty token
// deletes the cookie.
func sessionCookieValue(ctx context.Context, token string) string {
	c := http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(auth.SessionTTL / time.Second),
		HttpOnly: true,
		Secure:   clientOf(ctx).https,
		SameSite: http.SameSiteLaxMode,
	}
	if token == "" {
		c.MaxAge = -1
	}
	return c.String()
}
