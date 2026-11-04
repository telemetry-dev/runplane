package api

import (
	"context"
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

func (h *Handler) listUsers(ctx context.Context, _ *struct{}) (*struct{ Body Members }, error) {
	users, err := h.d.Store.ListUsers(ctx)
	if err != nil {
		return nil, h.internal(err)
	}
	invites, err := h.d.Store.ListOpenInvites(ctx, time.Now())
	if err != nil {
		return nil, h.internal(err)
	}
	out := Members{Users: make([]User, len(users)), Invites: make([]Invite, len(invites))}
	for i, u := range users {
		out.Users[i] = userOut(u)
	}
	for i, inv := range invites {
		out.Invites[i] = inviteOut(inv)
	}
	return &struct{ Body Members }{out}, nil
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
}) (*struct{ Body User }, error) {
	u, err := h.d.Store.GetUser(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No user has this ID.")
	}
	if u.Role == in.Body.Role {
		return &struct{ Body User }{userOut(u)}, nil
	}
	if u.Role == auth.RoleAdmin {
		if err := h.keepAnAdmin(ctx); err != nil {
			return nil, err
		}
	}
	u.Role = in.Body.Role
	if err := h.d.Store.UpdateUser(ctx, u); err != nil {
		return nil, h.storeErr(err, "No user has this ID.")
	}
	h.audit(ctx, "user.update", u.ID, map[string]any{"email": u.Email, "role": u.Role})
	return &struct{ Body User }{userOut(u)}, nil
}

func (h *Handler) deleteUser(ctx context.Context, in *PathID) (*empty, error) {
	if in.ID == currentUser(ctx).ID {
		return nil, huma.Error409Conflict("You cannot delete your own account.")
	}
	u, err := h.d.Store.GetUser(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No user has this ID.")
	}
	if u.Role == auth.RoleAdmin {
		if err := h.keepAnAdmin(ctx); err != nil {
			return nil, err
		}
	}
	// The sessions of the user go with it.
	if err := h.d.Store.DeleteUser(ctx, u.ID); err != nil {
		return nil, h.storeErr(err, "No user has this ID.")
	}
	h.audit(ctx, "user.delete", u.ID, map[string]any{"email": u.Email})
	return nil, nil
}

// keepAnAdmin returns a conflict if only one admin exists, before a change
// that takes away an admin.
func (h *Handler) keepAnAdmin(ctx context.Context) error {
	users, err := h.d.Store.ListUsers(ctx)
	if err != nil {
		return h.internal(err)
	}
	admins := 0
	for _, u := range users {
		if u.Role == auth.RoleAdmin {
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

func (h *Handler) createInvite(ctx context.Context, in *struct{ Body CreateInviteRequest }) (*struct{ Body CreatedInvite }, error) {
	out, err := h.invite(ctx, in.Body.Email, in.Body.Role)
	if err != nil {
		return nil, err
	}
	h.audit(ctx, "invite.create", out.Invite.ID, map[string]any{"email": out.Invite.Email, "role": out.Invite.Role})
	return &struct{ Body CreatedInvite }{out}, nil
}

// newInviteLink replaces an open invite with a new one for the same email
// and role, because only the hash of the old token is stored. The old link
// stops working.
func (h *Handler) newInviteLink(ctx context.Context, in *PathID) (*struct{ Body CreatedInvite }, error) {
	invites, err := h.d.Store.ListOpenInvites(ctx, time.Now())
	if err != nil {
		return nil, h.internal(err)
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
	return &struct{ Body CreatedInvite }{out}, nil
}

// invite makes an invite from the current user. Open invites for the email
// stop working.
func (h *Handler) invite(ctx context.Context, email, role string) (CreatedInvite, error) {
	token, err := h.d.Auth.CreateInvite(ctx, currentUser(ctx).ID, email, role)
	if err != nil {
		return CreatedInvite{}, h.authErr(err, "")
	}
	inv, err := h.d.Auth.GetInvite(ctx, token)
	if err != nil {
		return CreatedInvite{}, h.internal(err)
	}
	base, err := h.baseURL(ctx)
	if err != nil {
		return CreatedInvite{}, h.internal(err)
	}
	return CreatedInvite{URL: base + "/invite/" + token, Invite: inviteOut(inv)}, nil
}

func (h *Handler) deleteInvite(ctx context.Context, in *PathID) (*empty, error) {
	inv, err := h.d.Store.DeleteInvite(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No invite has this ID.")
	}
	h.audit(ctx, "invite.delete", inv.ID, map[string]any{"email": inv.Email, "role": inv.Role})
	return nil, nil
}
