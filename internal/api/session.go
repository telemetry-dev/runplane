package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/auth"
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

type sessionOutput struct {
	SetCookie []string `header:"Set-Cookie"`
	Body      User
}

func (h *Handler) getSession(ctx context.Context, _ *struct{}) (*sessionOutput, error) {
	// Send the cookie again, so it lives as long as the sliding session.
	return &sessionOutput{
		SetCookie: []string{sessionCookieValue(ctx, clientOf(ctx).token)},
		Body:      userOut(currentUser(ctx)),
	}, nil
}

// newSession returns the output for a new session of u: the session cookie,
// and the device cookie that gives this browser a login rate limit of its
// own for u.
func (h *Handler) newSession(ctx context.Context, u store.User, token string) *sessionOutput {
	device := h.d.Auth.Device(u.ID, clientOf(ctx).device)
	return &sessionOutput{
		SetCookie: []string{sessionCookieValue(ctx, token), deviceCookieValue(ctx, device)},
		Body:      userOut(u),
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
	u := currentUser(ctx)
	b := in.Body
	out := &sessionOutput{}
	name := u.Name
	if b.Name != nil {
		if name = strings.TrimSpace(*b.Name); name == "" {
			return nil, invalid("body.name", sentence(auth.ErrBadName), *b.Name)
		}
	}
	if b.NewPassword != nil {
		if b.CurrentPassword == nil || *b.CurrentPassword == "" {
			return nil, invalid("body.current_password", "Enter your current password.", nil)
		}
		// The change ends the sessions on other devices. This device gets
		// a new session.
		c := clientOf(ctx)
		token, err := h.d.Auth.ChangePassword(ctx, u.ID, *b.CurrentPassword, *b.NewPassword, c.ip, c.userAgent)
		if err != nil {
			return nil, authErr(err, "body.new_password")
		}
		out = h.newSession(ctx, u, token)
		h.audit(ctx, "user.password", u.ID, nil)
	}
	if name != u.Name {
		u.Name = name
		if err := h.d.Store.UpdateUser(ctx, u); err != nil {
			return nil, err
		}
	}
	out.Body = userOut(u)
	return out, nil
}

// authErr maps the errors of auth to problems. passwordLoc is the field
// that ErrShortPassword belongs to.
func authErr(err error, passwordLoc string) error {
	msg := sentence(err)
	switch {
	case errors.Is(err, auth.ErrRateLimited):
		return huma.Error429TooManyRequests(msg)
	case errors.Is(err, auth.ErrBadLogin):
		return huma.Error401Unauthorized(msg)
	case errors.Is(err, auth.ErrSetupDone):
		return huma.Error409Conflict("The setup is done. Log in instead.")
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

// sentence makes an error message into a sentence for the UI.
func sentence(err error) string {
	msg := err.Error()
	if msg == "" {
		return msg
	}
	r := []rune(msg)
	r[0] = unicode.ToUpper(r[0])
	if !strings.HasSuffix(msg, ".") {
		r = append(r, '.')
	}
	return string(r)
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
