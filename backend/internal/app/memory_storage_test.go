package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestMemoryDigestUsesSnapshotPointerWithoutMetaDoubleWrite(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("memory-pointer-repo")
	snapshotID := hh("memory-pointer-snapshot")
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: snapshotID, RepoID: repo, DocHash: snapshotID}); err != nil {
		t.Fatal(err)
	}
	digest := domain.MemoryDigest{
		SnapshotID: snapshotID,
		Summary:    "authoritative blob",
		KeyFacts:   []string{"snapshot.memory_hash resolves the body"},
		OpenTasks:  []string{},
		Provider:   domain.ProviderCodex,
		GraftCoverage: &domain.MemoryGraftCoverage{
			ProjectionVersion:  domain.MemoryProjectionVersion,
			ProjectionComplete: true,
			LineageFingerprint: hh("memory-lineage-state"),
			GraftSeq:           4,
			GraftParents:       []domain.ContentHash{hh("memory-graft-parent")},
			PinnedSources:      []domain.ContentHash{hh("memory-pinned-source")},
		},
	}
	hash, err := svc.PutMemoryDigest(ctx, repo, digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMemoryMeta(ctx, repo, snapshotID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("service still wrote a full memmeta copy: %v", err)
	}
	snapshot, err := st.GetSnapshot(ctx, repo, snapshotID)
	if err != nil || snapshot.MemoryHash != hash {
		t.Fatalf("memory pointer=%s want=%s err=%v", snapshot.MemoryHash, hash, err)
	}
	got, err := svc.GetMemoryDigest(ctx, repo, snapshotID)
	if err != nil || got.Summary != digest.Summary || got.GraftCoverage == nil ||
		!got.GraftCoverage.ProjectionComplete ||
		got.GraftCoverage.GraftSeq != digest.GraftCoverage.GraftSeq ||
		len(got.GraftCoverage.GraftParents) != 1 || got.GraftCoverage.GraftParents[0] != digest.GraftCoverage.GraftParents[0] ||
		len(got.GraftCoverage.PinnedSources) != 1 || got.GraftCoverage.PinnedSources[0] != digest.GraftCoverage.PinnedSources[0] {
		t.Fatalf("pointer-backed GetMemoryDigest: %+v err=%v", got, err)
	}
}

func TestMemoryDigestAcceptsExplicitIncompleteProjectionWithoutFingerprint(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("incomplete-memory-coverage-repo")
	snapshotID := hh("incomplete-memory-coverage-snapshot")
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: snapshotID, RepoID: repo, DocHash: snapshotID}); err != nil {
		t.Fatal(err)
	}
	digest := domain.MemoryDigest{
		SnapshotID: snapshotID, Summary: "available partial projection", Provider: domain.ProviderCodex,
		GraftCoverage: &domain.MemoryGraftCoverage{
			ProjectionVersion: domain.MemoryProjectionVersion,
			GraftSeq:          1,
		},
	}
	if _, err := svc.PutMemoryDigest(ctx, repo, digest); err != nil {
		t.Fatalf("explicit incomplete coverage was rejected: %v", err)
	}
}

func TestMemoryDigestRejectsInvalidGraftCoverageBeforeStorage(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("invalid-memory-coverage-repo")
	snapshotID := hh("invalid-memory-coverage-snapshot")
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: snapshotID, RepoID: repo, DocHash: snapshotID}); err != nil {
		t.Fatal(err)
	}
	for name, coverage := range map[string]*domain.MemoryGraftCoverage{
		"missing version":       {LineageFingerprint: hh("lineage"), GraftSeq: 1},
		"missing fingerprint":   {ProjectionVersion: 1, ProjectionComplete: true, GraftSeq: 1},
		"sequence overflow":     {ProjectionVersion: 1, LineageFingerprint: hh("lineage"), GraftSeq: domain.MaxGraftSeq + 1},
		"invalid parent":        {ProjectionVersion: 1, LineageFingerprint: hh("lineage"), GraftParents: []domain.ContentHash{"not-a-hash"}},
		"invalid pinned source": {ProjectionVersion: 1, LineageFingerprint: hh("lineage"), PinnedSources: []domain.ContentHash{"not-a-hash"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.PutMemoryDigest(ctx, repo, domain.MemoryDigest{
				SnapshotID: snapshotID, Summary: "invalid", Provider: domain.ProviderCodex, GraftCoverage: coverage,
			})
			if err == nil {
				t.Fatal("invalid graft coverage was accepted")
			}
		})
	}
}

func TestMemoryDigestFallsBackOnlyForPointerlessLegacySnapshot(t *testing.T) {
	ctx := context.Background()
	repo := hh("legacy-memory-repo")
	snapshotID := hh("legacy-memory-snapshot")
	base := store.NewFSStore(t.TempDir())
	if err := base.PutSnapshot(ctx, domain.Snapshot{ID: snapshotID, RepoID: repo, DocHash: snapshotID}); err != nil {
		t.Fatal(err)
	}
	legacy := domain.MemoryDigest{SnapshotID: snapshotID, Summary: "legacy metadata", Provider: domain.ProviderClaude}
	if err := base.PutMemoryMeta(ctx, repo, legacy); err != nil {
		t.Fatal(err)
	}
	svc := NewService(base, base, auth.NewTeamTokenAuth(), gitengine.NewEngine(base), base)
	got, err := svc.GetMemoryDigest(ctx, repo, snapshotID)
	if err != nil || got.Summary != legacy.Summary {
		t.Fatalf("pointerless fallback: %+v err=%v", got, err)
	}

	hash, err := base.PutMemory(ctx, repo, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := base.SetSnapshotMemory(ctx, repo, snapshotID, hash); err != nil {
		t.Fatal(err)
	}
	failing := &missingMemoryBlob{FSStore: base}
	svc = NewService(base, failing, auth.NewTeamTokenAuth(), gitengine.NewEngine(base), base)
	if _, err := svc.GetMemoryDigest(ctx, repo, snapshotID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("pointer-backed corruption was hidden by legacy fallback: %v", err)
	}
}

type missingMemoryBlob struct {
	*store.FSStore
}

func (s *missingMemoryBlob) GetMemory(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error) {
	return domain.MemoryDigest{}, domain.ErrNotFound
}
