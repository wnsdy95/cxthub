package mcp

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type contextMCPSnapshotKey struct{}

type contextMCPSnapshotIdentity struct {
	IdentityBackend
	test             *testing.T
	user             domain.User
	repository       string
	token            string
	resource         string
	resolves         int
	admissions       int
	snapshotResolves int
	snapshotPermits  int
	snapshotErr      error
}

type contextMCPPermit func(context.Context, string, string) error

func (permit contextMCPPermit) AuthorizeSnapshot(ctx context.Context, repository, actor string) error {
	return permit(ctx, repository, actor)
}

func (identity *contextMCPSnapshotIdentity) ResolveMCPUser(ctx context.Context, token string, resources ...string) (domain.User, error) {
	if ctx.Value(contextMCPSnapshotKey{}) == true {
		identity.test.Fatal("general MCP authentication ran inside a pinned context snapshot")
	}
	identity.resolves++
	return identity.IdentityBackend.ResolveMCPUser(ctx, token, resources...)
}

func (identity *contextMCPSnapshotIdentity) AdmitRepositoryRead(ctx context.Context, repository, actor string) (inbound.RepositoryReadPermit, error) {
	if ctx.Value(contextMCPSnapshotKey{}) == true || repository != identity.repository || actor != identity.user.ID {
		identity.test.Fatal("context admission did not precede the snapshot with current authority", repository, actor)
	}
	identity.admissions++
	return contextMCPPermit(func(bound context.Context, requested, current string) error {
		if bound.Value(contextMCPSnapshotKey{}) != true || requested != repository || current != actor {
			identity.test.Fatal("context snapshot permit lost its bound context or identity", requested, current)
		}
		identity.snapshotPermits++
		return identity.snapshotErr
	}), nil
}

func (*contextMCPSnapshotIdentity) ResolveUserForSnapshot(context.Context, string) (domain.User, error) {
	panic("MCP context queries must not resolve HTTP snapshot identities")
}

func (identity *contextMCPSnapshotIdentity) ResolveMCPUserForSnapshot(ctx context.Context, token string, resources ...string) (domain.User, error) {
	if ctx.Value(contextMCPSnapshotKey{}) != true || token != identity.token || len(resources) != 1 || resources[0] != identity.resource {
		identity.test.Fatal("context snapshot authentication lost its original credential, resource or snapshot")
	}
	identity.snapshotResolves++
	return identity.user, nil
}
