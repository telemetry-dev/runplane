package api

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"

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
	Users   []User   `json:"users"`
	Invites []Invite `json:"invites" doc:"Invites that are not accepted and not expired."`
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
	out := Members{Users: make([]User, len(users)), Invites: make([]Invite, len(invites))}
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
	u, err := h.d.Store.GetUser(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No user has this ID.")
	}
	if u.Role == in.Body.Role {
		return &body[User]{userOut(u)}, nil
	}
	if err := h.keepAnAdmin(ctx, u); err != nil {
		return nil, err
	}
	u.Role = in.Body.Role
	if err := h.d.Store.UpdateUser(ctx, u); err != nil {
		return nil, notFound(err, "No user has this ID.")
	}
	h.audit(ctx, "user.update", u.ID, map[string]any{"email": u.Email, "role": u.Role})
	return &body[User]{userOut(u)}, nil
}

func (h *Handler) deleteUser(ctx context.Context, in *PathID) (*empty, error) {
	if in.ID == currentUser(ctx).ID {
		return nil, huma.Error409Conflict("You cannot delete your own account.")
	}
	u, err := h.d.Store.GetUser(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No user has this ID.")
	}
	if err := h.keepAnAdmin(ctx, u); err != nil {
		return nil, err
	}
	// The sessions of the user go with it.
	if err := h.d.Store.DeleteUser(ctx, u.ID); err != nil {
		return nil, notFound(err, "No user has this ID.")
	}
	h.audit(ctx, "user.delete", u.ID, map[string]any{"email": u.Email})
	return nil, nil
}

// keepAnAdmin returns a conflict when u is the only admin, before a change
// that takes the admin role from u.
func (h *Handler) keepAnAdmin(ctx context.Context, u store.User) error {
	if u.Role != auth.RoleAdmin {
		return nil
	}
	users, err := h.d.Store.ListUsers(ctx)
	if err != nil {
		return err
	}
	admins := 0
	for _, v := range users {
		if v.Role == auth.RoleAdmin {
			admins++
		}
	}
	if admins <= 1 {
		return huma.Error409Conflict("runplane needs at least one admin.")
	}
	return nil
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
	out, err := h.invite(ctx, in.Body.Email, in.Body.Role)
	if err != nil {
		return nil, err
	}
	h.audit(ctx, "invite.create", out.Invite.ID, map[string]any{"email": out.Invite.Email, "role": out.Invite.Role})
	return &body[CreatedInvite]{out}, nil
}

// newInviteLink replaces an open invite with a new one for the same email
// and role, because only the hash of the old token is stored.
func (h *Handler) newInviteLink(ctx context.Context, in *PathID) (*body[CreatedInvite], error) {
	invites, err := h.d.Store.ListOpenInvites(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(invites, func(inv store.Invite) bool { return inv.ID == in.ID })
	if i < 0 {
		return nil, huma.Error404NotFound("No open invite has this ID.")
	}
	out, err := h.invite(ctx, invites[i].Email, invites[i].Role)
	if err != nil {
		return nil, err
	}
	h.audit(ctx, "invite.link", out.Invite.ID, map[string]any{"email": out.Invite.Email, "old_id": in.ID})
	return &body[CreatedInvite]{out}, nil
}

// invite makes an invite from the current user. Open invites for the email
// stop working.
func (h *Handler) invite(ctx context.Context, email, role string) (CreatedInvite, error) {
	token, err := h.d.Auth.CreateInvite(ctx, currentUser(ctx).ID, email, role)
	if err != nil {
		return CreatedInvite{}, authErr(err, "")
	}
	inv, err := h.d.Auth.GetInvite(ctx, token)
	if err != nil {
		return CreatedInvite{}, err
	}
	base, err := h.baseURL(ctx)
	if err != nil {
		return CreatedInvite{}, err
	}
	return CreatedInvite{URL: base + "/invite/" + token, Invite: inviteOut(inv)}, nil
}

func (h *Handler) deleteInvite(ctx context.Context, in *PathID) (*empty, error) {
	inv, err := h.d.Store.DeleteInvite(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No invite has this ID.")
	}
	h.audit(ctx, "invite.delete", inv.ID, map[string]any{"email": inv.Email, "role": inv.Role})
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
	Password string `json:"password" maxLength:"1024"`
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
