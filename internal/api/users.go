package api

import (
	"context"
	"errors"
	"time"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/store"
)

type Invite struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role" enum:"admin,member"`
	CreatedBy string    `json:"created_by" doc:"User ID. Empty if that user is deleted."`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Members struct {
	Users     []User   `json:"users"`
	Invites   []Invite `json:"invites" doc:"Invites that are not accepted and not expired."`
	LastAdmin string   `json:"last_admin" doc:"ID of the only admin, which cannot lose the role or be deleted, because runplane needs an admin. Empty when two or more users are admins."`
	Roles     Roles    `json:"roles" doc:"What each role can do."`
}

type Roles struct {
	Admin  Capabilities `json:"admin"`
	Member Capabilities `json:"member"`
}

func (h *Handler) listUsers(ctx context.Context, _ *struct{}) (*body[Members], error) {
	users, err := h.d.Store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	invites, err := h.d.Store.ListOpenInvites(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	out := Members{
		Users: make([]User, len(users)), Invites: make([]Invite, len(invites)), LastAdmin: auth.LastAdmin(users),
		Roles: Roles{Admin: capabilities(auth.RoleAdmin), Member: capabilities(auth.RoleMember)},
	}
	for i, u := range users {
		out.Users[i] = userOut(u)
	}
	for i, inv := range invites {
		out.Invites[i] = inviteOut(inv)
	}
	return &body[Members]{out}, nil
}

func inviteOut(inv store.Invite) Invite {
	return Invite{ID: inv.ID, Email: inv.Email, Role: inv.Role, CreatedBy: inv.CreatedBy, CreatedAt: inv.CreatedAt, ExpiresAt: inv.ExpiresAt}
}

type UpdateUserRequest struct {
	Role string `json:"role" enum:"admin,member"`
}

func (h *Handler) updateUser(ctx context.Context, in *struct {
	PathID
	Body UpdateUserRequest
}) (*body[User], error) {
	u, err := h.d.Auth.SetRole(ctx, actor(ctx), in.ID, in.Body.Role)
	if err != nil {
		return nil, notFound(authErr(err, ""), "No user has this ID.")
	}
	return &body[User]{userOut(u)}, nil
}

func (h *Handler) deleteUser(ctx context.Context, in *PathID) (*empty, error) {
	if err := h.d.Auth.DeleteUser(ctx, actor(ctx), in.ID); err != nil {
		return nil, notFound(authErr(err, ""), "No user has this ID.")
	}
	return nil, nil
}

type CreateInviteRequest struct {
	Email string `json:"email" maxLength:"254"`
	Role  string `json:"role" enum:"admin,member"`
}

type CreatedInvite struct {
	URL    string `json:"url" doc:"Link for the new user. It works one time, for 7 days."`
	Invite Invite `json:"invite"`
}

func (h *Handler) createInvite(ctx context.Context, in *body[CreateInviteRequest]) (*body[CreatedInvite], error) {
	inv, token, err := h.d.Auth.CreateInvite(ctx, actor(ctx), in.Body.Email, in.Body.Role)
	if err != nil {
		return nil, authErr(err, "")
	}
	return reply(h.createdInvite(ctx, inv, token))
}

func (h *Handler) newInviteLink(ctx context.Context, in *PathID) (*body[CreatedInvite], error) {
	inv, token, err := h.d.Auth.NewInviteLink(ctx, actor(ctx), in.ID)
	if err != nil {
		return nil, notFound(authErr(err, ""), "No open invite has this ID.")
	}
	return reply(h.createdInvite(ctx, inv, token))
}

func (h *Handler) createdInvite(ctx context.Context, inv store.Invite, token string) (CreatedInvite, error) {
	base, err := h.baseURL(ctx)
	if err != nil {
		return CreatedInvite{}, err
	}
	return CreatedInvite{URL: base + "/invite/" + token, Invite: inviteOut(inv)}, nil
}

func (h *Handler) deleteInvite(ctx context.Context, in *PathID) (*empty, error) {
	if err := h.d.Auth.DeleteInvite(ctx, actor(ctx), in.ID); err != nil {
		return nil, notFound(err, "No invite has this ID.")
	}
	return nil, nil
}

type InviteInfo struct {
	Email     string    `json:"email"`
	Role      string    `json:"role" enum:"admin,member"`
	ExpiresAt time.Time `json:"expires_at"`
	InvitedBy string    `json:"invited_by" doc:"Name of the admin who made the invite. Empty if that user is deleted."`
}

// InviteToken is the id path parameter of the invite routes, which share
// the path /api/invites/{id} with the delete route of the admins.
type InviteToken struct {
	Token string `path:"id" maxLength:"100" doc:"The token from the invite URL."`
}

func (h *Handler) getInvite(ctx context.Context, in *InviteToken) (*body[InviteInfo], error) {
	inv, err := h.d.Auth.GetInvite(ctx, in.Token)
	if err != nil {
		return nil, authErr(err, "")
	}
	out := InviteInfo{Email: inv.Email, Role: inv.Role, ExpiresAt: inv.ExpiresAt}
	if inv.CreatedBy != "" {
		u, err := h.d.Store.GetUser(ctx, inv.CreatedBy)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		out.InvitedBy = u.Name
	}
	return &body[InviteInfo]{out}, nil
}

type AcceptInviteRequest struct {
	Name     string `json:"name" maxLength:"100"`
	Password string `json:"password" minLength:"10" maxLength:"1024"`
}

func (h *Handler) acceptInvite(ctx context.Context, in *struct {
	InviteToken
	Body AcceptInviteRequest
}) (*sessionOutput, error) {
	c := clientOf(ctx)
	u, token, err := h.d.Auth.AcceptInvite(ctx, in.Token, in.Body.Name, in.Body.Password, c.ip, c.userAgent)
	if err != nil {
		return nil, authErr(err, "body.password")
	}
	return h.newSession(ctx, u, token), nil
}
