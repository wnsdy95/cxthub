//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGHistoryMemorySelectionAcceptance(t *testing.T) {
	runHistoryMemorySelectionAcceptance(t, func(t *testing.T) historyMemorySelectionFixture {
		svc, st, repo := collaborationPG(t)
		return newHistoryMemorySelectionFixture(t, svc, st, repo)
	})
}

type historyMemorySelectionTxKey struct{}
type historyMemorySelectionTxPG struct {
	*store.PostgresStore
	histories, memories, applies int
	failAfterApply               bool
}

func (s *historyMemorySelectionTxPG) WithinRepository(ctx context.Context, repo domain.ContentHash, fn func(context.Context) error) error {
	return s.PostgresStore.WithinRepository(ctx, repo, func(bound context.Context) error {
		return fn(context.WithValue(bound, historyMemorySelectionTxKey{}, s))
	})
}
func (s *historyMemorySelectionTxPG) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	if ctx.Value(historyMemorySelectionTxKey{}) != s {
		return nil, domain.ErrConflict
	}
	s.histories++
	return s.PostgresStore.ListHistoryEvents(ctx, repo)
}
func (s *historyMemorySelectionTxPG) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if ctx.Value(historyMemorySelectionTxKey{}) != s {
		return domain.MemoryDigest{}, domain.ErrConflict
	}
	s.memories++
	return s.PostgresStore.GetMemory(ctx, repo, hash)
}
func (s *historyMemorySelectionTxPG) ApplyHistoryEvent(ctx context.Context, e domain.HistoryEvent) error {
	if ctx.Value(historyMemorySelectionTxKey{}) != s {
		return domain.ErrConflict
	}
	s.applies++
	if err := s.PostgresStore.ApplyHistoryEvent(ctx, e); err != nil {
		return err
	}
	if s.failAfterApply {
		return domain.ErrIntegrity
	}
	return nil
}

func TestPGHistoryMemorySelectionTransactionRollbackAndAuthorization(t *testing.T) {
	svc, st, repo := collaborationPG(t)
	f := newHistoryMemorySelectionFixture(t, svc, st, repo)
	if err := svc.RecordHistory(f.ctx, f.before); err != nil {
		t.Fatal(err)
	}
	eventsBefore, err := st.ListHistoryEvents(f.ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	refsBefore, err := st.ListRefs(f.ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	revisionBefore, err := st.RepositoryRevision(f.ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	probe := &historyMemorySelectionTxPG{PostgresStore: st, failAfterApply: true}
	writer := NewService(probe, probe, nil, nil, probe)
	if err = writer.RecordHistory(f.ctx, f.after); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("injected post-apply failure: %v", err)
	}
	if probe.histories == 0 || probe.memories == 0 || probe.applies != 1 {
		t.Fatalf("predecessor/digest/write did not share transaction: histories=%d memories=%d applies=%d", probe.histories, probe.memories, probe.applies)
	}
	events, err := st.ListHistoryEvents(f.ctx, repo)
	if err != nil || !reflect.DeepEqual(eventsBefore, events) {
		t.Fatal("history escaped rollback", err)
	}
	refs, err := st.ListRefs(f.ctx, repo)
	if err != nil || !reflect.DeepEqual(refsBefore, refs) {
		t.Fatal("retention refs escaped rollback", err)
	}
	revision, err := st.RepositoryRevision(f.ctx, repo)
	if err != nil || revision != revisionBefore {
		t.Fatal("revision escaped rollback", err)
	}
	role := func(want domain.MemberRole) {
		t.Helper()
		if err := st.WithinIdentity(f.ctx, func(bound context.Context) error {
			return st.AddMember(bound, domain.Membership{RepositoryID: f.repository, UserID: f.member, Role: want})
		}); err != nil {
			t.Fatal(err)
		}
	}
	role(domain.RoleViewer)
	if err = writer.RecordHistory(f.ctx, f.after); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("revoked write role accepted: %v", err)
	}
	if probe.applies != 1 {
		t.Fatal("revoked write reached persistence")
	}
	role(domain.RoleMember)
	probe.failAfterApply = false
	if err = writer.RecordHistory(f.ctx, f.after); err != nil {
		t.Fatal("authorized retry", err)
	}
	if probe.applies != 2 {
		t.Fatal("successful retry did not apply exactly once")
	}
	role(domain.RoleViewer)
	if err = writer.RecordHistory(f.ctx, f.after); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("replay bypassed current authorization: %v", err)
	}
	role(domain.RoleMember)
	applies, memories := probe.applies, probe.memories
	if err = writer.RecordHistory(f.ctx, f.after); err != nil {
		t.Fatal("accepted exact replay", err)
	}
	if probe.applies != applies || probe.memories != memories {
		t.Fatal("accepted replay reapplied or revalidated immutable evidence")
	}
}
