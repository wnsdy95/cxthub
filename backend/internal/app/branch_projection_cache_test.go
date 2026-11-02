package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type projectionCacheMetadataStore struct {
	outbound.MetadataStore
	outbound.HistoryStore
	outbound.RepositoryRevisions
	generation domain.RepositoryViewSourceCheckpoint
	readOnly   bool
	err        error
	read       func()
}

type metadataOnlyProjectionStore struct {
	outbound.MetadataStore
}

func (storage *metadataOnlyProjectionStore) InReadOnlyTransaction(context.Context) bool {
	return true
}

func (storage *metadataOnlyProjectionStore) ReadRepositoryMetadataGeneration(_ context.Context, repo domain.ContentHash) (domain.RepositoryMetadataGeneration, error) {
	generation := testProjectionGeneration(repo)
	return domain.RepositoryMetadataGeneration{RepoID: repo, Epoch: generation.Epoch, Sequence: generation.Sequence}, nil
}

func (storage *projectionCacheMetadataStore) InReadOnlyTransaction(context.Context) bool {
	return storage.readOnly
}

func (storage *projectionCacheMetadataStore) ReadRepositoryGraphSourceGeneration(context.Context, domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, error) {
	if storage.read != nil {
		storage.read()
	}
	return storage.generation, storage.err
}

func testProjectionGeneration(repo domain.ContentHash) domain.RepositoryViewSourceCheckpoint {
	return domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope, RepoID: repo, Epoch: "12345678-abcd-4321-9876-123456789abc", Sequence: 7}
}

func TestRepositoryCommandsRequireExplicitAuthority(t *testing.T) {
	svc, _ := newFsckSvc(t)
	_, err := svc.Commit(context.Background(), inbound.CommitInput{RepoID: hh("unbound")})
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("missing actor accepted: %v", err)
	}
}

func TestBranchProjectionCachePreservesPendingAndEvidenceBoundaries(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo, root, pending := hh("cache repo"), hh("root"), hh("pending")
	v := domain.RepositoryView{DefaultBranch: "main", Revision: domain.RepositoryRevision{Graph: 1}, Refs: []domain.Ref{{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: root}}, Snapshots: []domain.Snapshot{{RepoID: repo, ID: root, DocHash: root}}}
	// Legacy fixture has no bound Git origin; branch projection falls back safely.
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	storage := &projectionCacheMetadataStore{MetadataStore: st, HistoryStore: st, RepositoryRevisions: st, generation: testProjectionGeneration(repo), readOnly: true}
	svc.meta = storage
	first, err := svc.projectGraphState(ctx, v, "")
	if err != nil {
		t.Fatal(err)
	}
	if svc.branchCache.Stats().Entries != 1 {
		t.Fatal("committed generation not cached")
	}
	v.Revision.Pending++
	storage.generation.Sequence++
	v.Snapshots = append(v.Snapshots, domain.Snapshot{RepoID: repo, ID: pending, DocHash: pending, Parents: []domain.ContentHash{root}, Message: "hook: live"})
	v.Pending = []domain.Pending{{RepoID: repo, SessionID: "session", Target: pending}}
	next, err := svc.projectGraphState(ctx, v, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.BranchContexts, next.BranchContexts) || len(next.UncommittedIDs) != 1 || next.UncommittedIDs[0] != pending {
		t.Fatalf("pending response lost current capture: %+v", next)
	}
	v.Revision.Evidence++
	if _, err := svc.projectGraphState(ctx, v, ""); err != nil {
		t.Fatal(err)
	}
	if svc.branchCache.Keys()[0].Evidence != v.Revision.Evidence {
		t.Fatal("evidence did not invalidate projection")
	}
	v.Revision.Graph++
	if _, err := svc.projectGraphState(context.WithValue(ctx, afterCommitKey{}, &afterCommitActions{}), v, ""); err != nil {
		t.Fatal(err)
	}
	if svc.branchCache.Keys()[0].Graph == v.Revision.Graph {
		t.Fatal("uncommitted projection entered cache")
	}
}

func TestBranchProjectionCacheRejectsUnversionedSources(t *testing.T) {
	svc, storage := newFsckSvc(t)
	ctx := systemTestContext()
	repo, root, added := hh(t.Name()), hh("root"), hh("added")
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	view := domain.RepositoryView{Revision: domain.RepositoryRevision{Graph: 1}, Refs: []domain.Ref{{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: root}}, Snapshots: []domain.Snapshot{{RepoID: repo, ID: root, DocHash: root}, {RepoID: repo, ID: added, DocHash: added}}}
	first, err := svc.projectGraphState(ctx, view, "")
	if err != nil {
		t.Fatal(err)
	}
	view.Snapshots[0].GraftParents = []domain.ContentHash{added}
	next, err := svc.projectGraphState(ctx, view, "")
	if err != nil || len(first.BranchContexts["main"].SnapshotIDs) != 1 || len(next.BranchContexts["main"].SnapshotIDs) != 2 {
		t.Fatalf("unversioned graft reused branch facts: %+v %v", next.BranchContexts, err)
	}
	if svc.branchCache.Stats().Entries != 0 {
		t.Fatal("unproven storage populated branch cache")
	}
}

func TestBranchProjectionCacheRejectsUncoveredEvidenceCapabilities(t *testing.T) {
	svc, storage := newFsckSvc(t)
	repo := hh(t.Name())
	metadata := &metadataOnlyProjectionStore{MetadataStore: storage}
	for _, candidate := range []outbound.MetadataStore{
		struct {
			*metadataOnlyProjectionStore
			outbound.GitTreeStore
		}{metadata, storage},
		struct {
			*metadataOnlyProjectionStore
			outbound.GitDeltaReader
		}{metadata, storage},
		struct {
			*metadataOnlyProjectionStore
			outbound.PRContributionStore
		}{metadata, storage},
		struct {
			*metadataOnlyProjectionStore
			outbound.GitContributionStore
		}{metadata, storage},
	} {
		svc.meta = candidate
		if _, complete, err := svc.projectionGraphSourceGeneration(systemTestContext(), repo); err != nil || complete {
			t.Fatalf("catalog generation certified uncovered evidence for %T: %v", candidate, err)
		}
	}
}
