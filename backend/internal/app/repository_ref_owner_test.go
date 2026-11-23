package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type refOwnerBoundKey struct{}

type refOwnerProbe struct {
	test   *testing.T
	repo   domain.ContentHash
	bound  context.Context
	active bool
	calls  []string
}

func (probe *refOwnerProbe) touch(ctx context.Context, repo domain.ContentHash, operation string) {
	probe.test.Helper()
	if !probe.active || ctx != probe.bound || ctx.Value(refOwnerBoundKey{}) != probe || repo != probe.repo {
		probe.test.Fatalf("%s escaped the original repository scope: repo=%s active=%v", operation, repo, probe.active)
	}
	probe.calls = append(probe.calls, operation)
}

func (probe *refOwnerProbe) count(operation string) int {
	count := 0
	for _, actual := range probe.calls {
		if actual == operation {
			count++
		}
	}
	return count
}

type refOwnerScopeFrame struct {
	Batch    bool
	Empty    bool
	Audit    string
	Revision string
}

type refOwnerScope struct {
	probe     *refOwnerProbe
	frames    []refOwnerScopeFrame
	beforeErr error
	afterErr  error
}

func (scope *refOwnerScope) enter(ctx context.Context, repo domain.ContentHash, batch, empty bool) context.Context {
	scope.probe.test.Helper()
	if scope.probe.active || repo != scope.probe.repo {
		scope.probe.test.Fatal("ref commands nested or changed repository scope")
	}
	audit, revision := "context.ref.updated", "graph"
	if batch {
		audit = "context.refs.updated"
	}
	if empty {
		revision = "pending"
	}
	bound := context.WithValue(auditOperation(ctx, audit), revisionScopeKey{}, revision)
	bound = context.WithValue(bound, refOwnerBoundKey{}, scope.probe)
	scope.frames = append(scope.frames, refOwnerScopeFrame{Batch: batch, Empty: empty, Audit: audit, Revision: revision})
	scope.probe.bound, scope.probe.active = bound, true
	return bound
}

type refOwnerHistory struct {
	probe  *refOwnerProbe
	events []domain.HistoryEvent
	err    error
}

func (history *refOwnerHistory) ListHistory(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	history.probe.touch(ctx, repo, "history")
	return history.events, history.err
}

type refOwnerRevisions struct {
	probe    *refOwnerProbe
	revision domain.RepositoryRevision
	err      error
}

func (revisions *refOwnerRevisions) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	revisions.probe.touch(ctx, repo, "revision")
	return revisions.revision, revisions.err
}

type refOwnerNotice struct {
	Ref     domain.Ref
	Forced  bool
	Created bool
}

type refOwnerNotifications struct {
	probe   *refOwnerProbe
	notices []refOwnerNotice
	err     error
}

func (notifications *refOwnerNotifications) NotifyRefUpdate(ctx context.Context, repo domain.ContentHash, ref domain.Ref, forced, created bool) error {
	notifications.probe.touch(ctx, repo, "notify")
	notifications.notices = append(notifications.notices, refOwnerNotice{Ref: ref, Forced: forced, Created: created})
	return notifications.err
}
