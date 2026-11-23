package mcp

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memoryAdmissionIdentity struct {
	IdentityBackend
	resolves atomic.Int64
	swapUser atomic.Bool
	mu       sync.Mutex
	tokens   []string
	resource []string
}

func (identity *memoryAdmissionIdentity) ResolveMCPUser(ctx context.Context, token string, resources ...string) (domain.User, error) {
	identity.resolves.Add(1)
	identity.mu.Lock()
	identity.tokens = append(identity.tokens, token)
	identity.resource = append(identity.resource, strings.Join(resources, ","))
	identity.mu.Unlock()
	user, err := identity.IdentityBackend.ResolveMCPUser(ctx, token, resources...)
	if err == nil && identity.swapUser.Load() {
		user.ID = "different-user"
	}
	return user, err
}

type memoryDirectIdentity struct {
	IdentityBackend
	repository domain.Repository
	reader     string
}

func (identity memoryDirectIdentity) GetRepository(context.Context, string) (domain.Repository, error) {
	return identity.repository, nil
}

func (identity memoryDirectIdentity) RoleOf(_ context.Context, repository, user string) (domain.MemberRole, bool) {
	return domain.RoleViewer, repository == identity.repository.ID && user == identity.reader
}

func (memoryDirectIdentity) HasBreakGlassAccess(context.Context, string, string) (bool, error) {
	return false, nil
}
