package mcp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryPreparationPermitFunc func(context.Context, string, string) error

func (permit memoryPreparationPermitFunc) AuthorizeSnapshot(ctx context.Context, repository, user string) error {
	return permit(ctx, repository, user)
}

type memoryPreparationSnapshotKey struct{}

type memoryPreparationIdentity struct {
	*app.IdentityService
	test             *testing.T
	tokens           []string
	resources        []string
	snapshotTokens   []string
	snapshotResource []string
	admissions       int
	snapshots        int
	swapUser         bool
}

func (identity *memoryPreparationIdentity) ResolveMCPUser(ctx context.Context, token string, resources ...string) (domain.User, error) {
	identity.tokens = append(identity.tokens, token)
	identity.resources = append(identity.resources, strings.Join(resources, ","))
	user, err := identity.IdentityService.ResolveMCPUser(ctx, token, resources...)
	if identity.swapUser {
		user.ID = "different-user"
	}
	return user, err
}

func (identity *memoryPreparationIdentity) ResolveMCPUserForSnapshot(ctx context.Context, token string, resources ...string) (domain.User, error) {
	identity.test.Helper()
	if ctx.Value(memoryPreparationSnapshotKey{}) != true {
		identity.test.Fatal("snapshot authentication ran outside the preparer's snapshot callback")
	}
	if actor, system := inbound.RepositoryActor(ctx); actor != "" || system {
		identity.test.Fatal("snapshot authentication borrowed previous actor")
	}
	identity.snapshotTokens = append(identity.snapshotTokens, token)
	identity.snapshotResource = append(identity.snapshotResource, strings.Join(resources, ","))
	user, err := identity.IdentityService.ResolveMCPUser(ctx, token, resources...)
	if identity.swapUser {
		user.ID = "different-user"
	}
	return user, err
}

func (identity *memoryPreparationIdentity) AdmitRepositoryRead(ctx context.Context, repository, user string) (inbound.RepositoryReadPermit, error) {
	identity.admissions++
	if _, err := identity.IdentityService.AdmitRepositoryRead(ctx, repository, user); err != nil {
		return nil, err
	}
	return memoryPreparationPermitFunc(func(bound context.Context, requestedRepository, requestedUser string) error {
		identity.snapshots++
		if bound.Value(memoryPreparationSnapshotKey{}) != true || requestedRepository != repository || requestedUser != user {
			identity.test.Fatal("snapshot permit lost its repository, user or snapshot context")
		}
		_, err := identity.IdentityService.AdmitRepositoryRead(bound, requestedRepository, requestedUser)
		return err
	}), nil
}

type memoryPreparationAllowance struct {
	key      string
	limit    int
	window   time.Duration
	deadline time.Time
	now      time.Time
}

type memoryPreparationRuntime struct {
	outbound.RuntimeStore
	calls []memoryPreparationAllowance
	allow func(context.Context, string, int, time.Duration, time.Time) (bool, error)
}

func (runtime *memoryPreparationRuntime) AllowRequest(ctx context.Context, key string, limit int, window time.Duration, now time.Time) (bool, error) {
	deadline, _ := ctx.Deadline()
	runtime.calls = append(runtime.calls, memoryPreparationAllowance{key, limit, window, deadline, now})
	if runtime.allow != nil {
		return runtime.allow(ctx, key, limit, window, now)
	}
	return true, nil
}

type memoryPreparationDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	failure   error
}

func (writer *memoryPreparationDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	writer.deadlines = append(writer.deadlines, deadline)
	return writer.failure
}
