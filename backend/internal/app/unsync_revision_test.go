package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type unsyncRevisionStore interface {
	outbound.MetadataStore
	outbound.BlobStore
	outbound.RepositoryRevisions
	outbound.EvidenceRevisions
}

func prepareUnsyncRevisionFixture(t *testing.T, storage unsyncRevisionStore) (domain.ContentHash, domain.ContentHash, domain.ContentHash) {
	t.Helper()
	ctx := systemTestContext()
	repo := hh(t.Name() + time.Now().String())
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	var targets []domain.ContentHash
	for _, message := range []string{"target", "descendant"} {
		cir := pendingGCCIR(domain.ProviderCodex, string(repo)+message)
		raw, err := domain.CanonicalBytes(cir)
		if err != nil {
			t.Fatal(err)
		}
		target := domain.HashContent(raw)
		if _, err := storage.PutDoc(ctx, repo, domain.SessionDoc{Hash: target, CIR: cir}); err != nil {
			t.Fatal(err)
		}
		snapshot := domain.Snapshot{RepoID: repo, ID: target, DocHash: target, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}
		if len(targets) != 0 {
			snapshot.Parents = []domain.ContentHash{targets[0]}
		}
		if err := storage.PutSnapshot(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, target)
	}
	return repo, targets[0], targets[1]
}

func assertUnsyncRevision(t *testing.T, ctx context.Context, storage unsyncRevisionStore, repo domain.ContentHash, want domain.RepositoryRevision) {
	t.Helper()
	got, err := storage.RepositoryRevision(ctx, repo)
	if err != nil || got != want {
		t.Fatalf("revision=%+v want=%+v err=%v", got, want, err)
	}
}

func exerciseUnsyncDeletionRevisions(t *testing.T, service *Service, storage unsyncRevisionStore) {
	t.Helper()
	ctx := systemTestContext()
	repo, target, descendant := prepareUnsyncRevisionFixture(t, storage)
	if err := storage.AdvanceRepositoryRevision(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	if err := storage.AdvanceEvidenceRevision(ctx, repo); err != nil {
		t.Fatal(err)
	}
	want, err := storage.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, pointer := range []domain.Unsync{
		{RepoID: repo, User: "other", Branch: "main", Target: target},
		{RepoID: repo, User: "alice", Branch: "other", Target: target},
	} {
		if err := storage.PutUnsync(ctx, repo, pointer); err != nil {
			t.Fatal(err)
		}
	}
	assertOthers := func() {
		t.Helper()
		pointers, err := storage.ListUnsyncs(ctx, repo)
		if err != nil || len(pointers) != 2 {
			t.Fatalf("unrelated pointers changed: %+v %v", pointers, err)
		}
		for _, pointer := range pointers {
			if pointer.User == "alice" && pointer.Branch == "main" {
				t.Fatal("resolved pointer remains")
			}
		}
	}
	for replay := 0; replay < 3; replay++ {
		if err := service.DeleteUnsync(ctx, repo, "alice", "main"); err != nil {
			t.Fatal(err)
		}
		assertUnsyncRevision(t, ctx, storage, repo, want)
		assertOthers()
	}
	if err := service.PutUnsync(ctx, repo, "alice", "main", domain.Unsync{Target: target}); err != nil {
		t.Fatal(err)
	}
	want.Graph++
	assertUnsyncRevision(t, ctx, storage, repo, want)
	if err := service.DeleteUnsync(ctx, repo, "alice", "main"); err != nil {
		t.Fatal(err)
	}
	want.Graph++
	assertUnsyncRevision(t, ctx, storage, repo, want)
	assertOthers()
	if err := service.DeleteUnsync(ctx, repo, "alice", "main"); err != nil {
		t.Fatal(err)
	}
	assertUnsyncRevision(t, ctx, storage, repo, want)
	var previous domain.ContentHash
	for _, shared := range []domain.ContentHash{target, descendant} {
		if err := storage.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: shared}, previous); err != nil {
			t.Fatal(err)
		}
		previous = shared
		for replay := 0; replay < 3; replay++ {
			if err := service.PutUnsync(ctx, repo, "alice", "main", domain.Unsync{Target: target}); err != nil {
				t.Fatal(err)
			}
			assertUnsyncRevision(t, ctx, storage, repo, want)
			assertOthers()
		}
		if err := storage.PutUnsync(ctx, repo, domain.Unsync{RepoID: repo, User: "alice", Branch: "main", Target: target}); err != nil {
			t.Fatal(err)
		}
		if err := service.PutUnsync(ctx, repo, "alice", "main", domain.Unsync{Target: target}); err != nil {
			t.Fatal(err)
		}
		want.Graph++
		assertUnsyncRevision(t, ctx, storage, repo, want)
		assertOthers()
	}
	for _, user := range []string{"", strings.Repeat("x", 129)} {
		if err := service.DeleteUnsync(ctx, repo, user, "main"); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid user delete: %v", err)
		}
		if err := service.PutUnsync(ctx, repo, user, "main", domain.Unsync{Target: target}); err == nil {
			t.Fatal("invalid user accepted on shared-target path")
		}
		assertUnsyncRevision(t, ctx, storage, repo, want)
	}
}

func TestUnsyncDeletionRevisions(t *testing.T) {
	service, storage := newFsckSvc(t)
	exerciseUnsyncDeletionRevisions(t, service, storage)
}
