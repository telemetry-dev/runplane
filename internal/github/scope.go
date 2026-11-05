package github

import (
	"context"
	"net/url"

	"github.com/runplane/runplane/internal/ghrest"
)

// ReadScope returns the REST path of an organization or a repository that
// the app can read, with the auth of its installation. An organization
// comes first, because its answers do not depend on one repository. ok is
// false when runplane has no such installation.
func (s *Service) ReadScope(ctx context.Context) (string, ghrest.Auth, bool) {
	insts, err := s.st.ListInstallations(ctx)
	if err != nil {
		return "", nil, false
	}
	var repoScope string
	var repoAuth ghrest.Auth
	for _, inst := range insts {
		switch {
		case inst.SuspendedAt != nil:
		case inst.AccountType == "Organization":
			return "/orgs/" + url.PathEscape(inst.AccountLogin), installAuth{s, inst.ID}, true
		case repoScope == "":
			if repos, err := s.st.ListRepositories(ctx, inst.ID); err == nil && len(repos) > 0 {
				repoScope, repoAuth = "/repos/"+repos[0].FullName, installAuth{s, inst.ID}
			}
		}
	}
	return repoScope, repoAuth, repoScope != ""
}
