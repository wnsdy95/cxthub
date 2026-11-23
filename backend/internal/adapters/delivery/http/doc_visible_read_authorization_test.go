package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docVisiblePolicyBackend struct {
	Backend
	repo  domain.Repo
	err   error
	calls int
}

func (backend *docVisiblePolicyBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	backend.calls++
	return backend.repo, backend.err
}

type docVisiblePolicyIdentity struct {
	IdentityBackend
	repository    domain.Repository
	repositoryErr error
	user          domain.User
	credentialErr error
	role          domain.MemberRole
	breakGlass    bool
	auditErr      error
	auditCalls    int
	resolveCalls  int
	checkAudit    func(context.Context)
}

func (identity *docVisiblePolicyIdentity) ResolveUser(context.Context, string) (domain.User, error) {
	identity.resolveCalls++
	return identity.user, identity.credentialErr
}

func (identity *docVisiblePolicyIdentity) GetRepository(context.Context, string) (domain.Repository, error) {
	return identity.repository, identity.repositoryErr
}

func (identity *docVisiblePolicyIdentity) RoleOf(context.Context, string, string) (domain.MemberRole, bool) {
	return identity.role, identity.role != ""
}

func (identity *docVisiblePolicyIdentity) HasBreakGlassAccess(ctx context.Context, _, _ string) (bool, error) {
	identity.auditCalls++
	if identity.checkAudit != nil {
		identity.checkAudit(ctx)
	}
	return identity.breakGlass, identity.auditErr
}
