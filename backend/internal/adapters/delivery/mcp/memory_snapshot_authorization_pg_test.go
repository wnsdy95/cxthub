//go:build postgres

package mcp

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type memorySnapshotPGAuditFailureIdentity struct {
	*app.IdentityService
	cause  error
	admits int
}

func (identity *memorySnapshotPGAuditFailureIdentity) AdmitRepositoryRead(context.Context, string, string) (inbound.RepositoryReadPermit, error) {
	identity.admits++
	return nil, errors.Join(domain.ErrBreakGlassAuditUnavailable, identity.cause)
}
