package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ inbound.SnapshotReadIdentity = (*IdentityService)(nil)

type repositoryReadPermit struct {
	identity   *IdentityService
	request    context.Context
	repository string
	user       string
	grant      domain.BreakGlassGrant
}

func (service *IdentityService) requireIdentityReadSnapshot(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	state, ok := service.repositories.(outbound.RepositoryTransactionState)
	if !ok || !state.InReadOnlyTransaction(ctx) {
		return domain.ErrConflict
	}
	return nil
}

func (service *IdentityService) snapshotSession(ctx context.Context, token string) (domain.Session, domain.User, error) {
	session, err := service.repositories.GetSession(ctx, domain.HashToken(token))
	if errors.Is(err, domain.ErrNotFound) {
		session, err = service.repositories.GetSession(ctx, token)
	}
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			err = domain.ErrUnauthorized
		}
		return domain.Session{}, domain.User{}, err
	}
	if !time.Now().Before(session.ExpiresAt) || session.UserID == "" {
		return domain.Session{}, domain.User{}, domain.ErrUnauthorized
	}
	user, err := service.repositories.GetUser(ctx, session.UserID)
	if errors.Is(err, domain.ErrNotFound) {
		err = domain.ErrUnauthorized
	}
	return session, user, err
}

func (service *IdentityService) ResolveUserForSnapshot(ctx context.Context, token string) (domain.User, error) {
	if err := service.requireIdentityReadSnapshot(ctx); err != nil {
		return domain.User{}, err
	}
	if token == "" || strings.HasPrefix(token, "refresh_") {
		return domain.User{}, domain.ErrUnauthorized
	}
	if strings.HasPrefix(token, "sess_") {
		session, user, err := service.snapshotSession(ctx, token)
		if err != nil {
			return domain.User{}, err
		}
		if session.Kind == "mcp_access" || session.Kind == "mcp_refresh" {
			return domain.User{}, domain.ErrUnauthorized
		}
		return user, nil
	}
	verifier, ok := service.verifier.(outbound.CachedIdentityVerifier)
	if !ok {
		return domain.User{}, domain.ErrUnauthorized
	}
	verified, err := verifier.VerifyCached(ctx, token)
	if err != nil {
		return domain.User{}, err
	}
	user, err := service.repositories.GetUser(ctx, verified.ID)
	if errors.Is(err, domain.ErrNotFound) {
		err = domain.ErrUnauthorized
	}
	return user, err
}

func (service *IdentityService) ResolveMCPUserForSnapshot(ctx context.Context, token string, resources ...string) (domain.User, error) {
	if err := service.requireIdentityReadSnapshot(ctx); err != nil {
		return domain.User{}, err
	}
	if !strings.HasPrefix(token, "sess_") {
		return domain.User{}, domain.ErrUnauthorized
	}
	session, user, err := service.snapshotSession(ctx, token)
	if err != nil {
		return domain.User{}, err
	}
	if session.Kind != "mcp_access" {
		return domain.User{}, domain.ErrUnauthorized
	}
	if err := service.checkMCPGrant(ctx, session, resources...); err != nil {
		return domain.User{}, err
	}
	return user, nil
}

func (service *IdentityService) repositoryReadAllowed(ctx context.Context, repositoryID, userID string) (bool, error) {
	repository, err := service.repositories.GetRepository(ctx, repositoryID)
	if err != nil {
		return false, err
	}
	if repository.IsPublic() && repository.PublicBaseRole().AtLeast(domain.RoleViewer) {
		return true, nil
	}
	if userID == "" {
		return false, domain.ErrUnauthorized
	}
	role, allowed, err := repositoryRoleFor(ctx, service.repositories, repository, userID)
	return allowed && role.AtLeast(domain.RoleViewer), err
}

func (service *IdentityService) AdmitRepositoryRead(ctx context.Context, repositoryID, userID string) (inbound.RepositoryReadPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, repositoryTransactions := service.repositories.(outbound.RepositoryTransactions)
	_, identityTransactions := service.repositories.(outbound.IdentityTransactions)
	state, known := service.repositories.(outbound.RepositoryTransactionPresence)
	if known && state.InTransaction(ctx) || (repositoryTransactions || identityTransactions) && !known {
		return nil, domain.ErrConflict
	}
	allowed, err := service.repositoryReadAllowed(ctx, repositoryID, userID)
	if err != nil {
		return nil, err
	}
	permit := &repositoryReadPermit{identity: service, request: ctx, repository: repositoryID, user: userID}
	if allowed {
		return permit, nil
	}
	grant, err := identityResult(ctx, service, func(bound context.Context) (domain.BreakGlassGrant, error) {
		return service.useBreakGlassAccess(bound, repositoryID, userID)
	})
	if err != nil {
		return nil, errors.Join(domain.ErrBreakGlassAuditUnavailable, err)
	}
	if grant.ID == "" {
		return nil, domain.ErrForbidden
	}
	permit.grant = grant
	return permit, nil
}

func (permit *repositoryReadPermit) AuthorizeSnapshot(ctx context.Context, repositoryID, userID string) error {
	if permit == nil || permit.identity == nil || permit.request == nil || permit.repository != repositoryID || userID != "" && permit.user != userID {
		return domain.ErrForbidden
	}
	if err := permit.request.Err(); err != nil {
		return err
	}
	identity := permit.identity
	if err := identity.requireIdentityReadSnapshot(ctx); err != nil {
		return err
	}
	allowed, err := identity.repositoryReadAllowed(ctx, repositoryID, userID)
	if err != nil || allowed {
		return err
	}
	if permit.grant.ID == "" {
		return domain.ErrForbidden
	}
	organization, err := identity.breakGlassOrganization(ctx, repositoryID, userID)
	if err != nil {
		return err
	}
	if organization != permit.grant.OrganizationID {
		return domain.ErrForbidden
	}
	reader, ok := identity.organization.(outbound.BreakGlassGrantReader)
	if !ok {
		return domain.ErrForbidden
	}
	grant, err := reader.GetBreakGlassGrant(ctx, organization, repositoryID, userID, permit.grant.ID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrForbidden
	}
	if err != nil {
		return err
	}
	if grant.ID != permit.grant.ID || grant.OrganizationID != organization || grant.RepositoryID != repositoryID || grant.UserID != userID ||
		grant.Reason != permit.grant.Reason || !grant.CreatedAt.Equal(permit.grant.CreatedAt) || !grant.ExpiresAt.Equal(permit.grant.ExpiresAt) || !time.Now().Before(grant.ExpiresAt) {
		return domain.ErrForbidden
	}
	return nil
}
