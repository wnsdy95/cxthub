package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestContextSegmentCacheFencesStagingAndEpochReset(t *testing.T) {
	svc, storage := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh(t.Name())
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	segmentCapture(t, storage, repo, []string{"first"})
	if err := storage.AdvanceRepositoryRevision(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	spy := &segmentReadSpy{FSStore: storage}
	metadata := &projectionCacheMetadataStore{MetadataStore: spy, HistoryStore: storage, RepositoryRevisions: storage, generation: testProjectionGeneration(repo), readOnly: true}
	svc.meta = metadata
	in := domain.ContextSelection{SegmentLimit: 1}
	first, err := svc.QueryContext(ctx, repo, in)
	if err != nil || svc.segmentCache.Stats().Entries == 0 {
		t.Fatalf("warm source: %+v %v", first, err)
	}
	reads := spy.catalogReads
	if _, err := svc.QueryContext(ctx, repo, in); err != nil || spy.catalogReads != reads {
		t.Fatalf("same source reread catalog: %d -> %d %v", reads, spy.catalogReads, err)
	}
	for _, reset := range []bool{false, true} {
		previous := first
		segmentCapture(t, storage, repo, []string{"first", fmt.Sprint(reset)})
		if reset {
			metadata.generation.Epoch = "22345678-abcd-4321-9876-123456789abc"
		} else {
			metadata.generation.Sequence++
		}
		in.SegmentStateHash = previous.StateHash
		if _, err := svc.QueryContext(ctx, repo, in); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("stale continuation accepted after reset=%v: %v", reset, err)
		}
		in.SegmentStateHash = ""
		first, err = svc.QueryContext(ctx, repo, in)
		if err != nil || first.StateHash == previous.StateHash || first.Revision != previous.Revision {
			t.Fatalf("source-only mutation missed after reset=%v: %+v %v", reset, first, err)
		}
	}
}

func TestProjectionCachesRejectWarmHitsWithoutCurrentProof(t *testing.T) {
	service, storage := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh(t.Name())
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	root := segmentCapture(t, storage, repo, []string{"first"})
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: root.ID}
	if err := storage.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	if err := storage.AdvanceRepositoryRevision(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	metadata := &projectionCacheMetadataStore{MetadataStore: storage, HistoryStore: storage, RepositoryRevisions: storage, generation: testProjectionGeneration(repo), readOnly: true}
	service.meta = metadata
	view, err := service.loadRepositoryView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	reads := []struct {
		name string
		run  func(context.Context) error
	}{
		{"outer graph", func(bound context.Context) error {
			_, err := service.cachedGraphState(bound, repo, view, "")
			return err
		}},
		{"branch", func(bound context.Context) error { _, err := service.projectGraphState(bound, view, ""); return err }},
		{"segments", func(bound context.Context) error {
			_, err := service.QueryContext(bound, repo, domain.ContextSelection{Scope: "current", Position: "main", SegmentLimit: 1})
			return err
		}},
	}
	for _, read := range reads {
		if err := read.run(ctx); err != nil {
			t.Fatalf("warm %s: %v", read.name, err)
		}
	}
	if service.graphCache.Stats().Entries == 0 || service.branchCache.Stats().Entries == 0 || service.segmentCache.Stats().Entries == 0 {
		t.Fatal("regression did not warm all three caches")
	}
	for _, read := range reads {
		t.Run(read.name, func(t *testing.T) {
			for _, failure := range []error{domain.ErrIntegrity, context.Canceled, context.DeadlineExceeded} {
				metadata.err = failure
				if err := read.run(ctx); !errors.Is(err, failure) {
					t.Fatalf("warm hit ignored generation failure %v: %v", failure, err)
				}
			}
			metadata.err = nil
			metadata.generation.Scope = domain.RepositoryViewSourceScope
			if err := read.run(ctx); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatalf("view-only source certified cached Git inputs: %v", err)
			}
			metadata.generation.Scope = domain.RepositoryGraphSourceScope
			canceled, cancel := context.WithCancel(ctx)
			metadata.read = cancel
			if err := read.run(canceled); !errors.Is(err, context.Canceled) {
				t.Fatalf("point-read cancellation accepted warm hit: %v", err)
			}
			metadata.read = nil
		})
	}
}
