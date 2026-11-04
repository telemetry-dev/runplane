package auth

import "github.com/runplane/runplane/internal/store"

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

func ValidRole(role string) bool { return role == RoleAdmin || role == RoleMember }
