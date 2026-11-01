//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type pendingProjectionPGSpy struct {
	*store.PostgresStore
	scans, evidence int
}

func (storage *pendingProjectionPGSpy) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	storage.scans++
	return storage.PostgresStore.ListSnapshots(ctx, repo, branch)
}

func (storage *pendingProjectionPGSpy) ListPRContributions(context.Context, domain.ContentHash) ([]domain.PRContribution, error) {
	storage.evidence++
	return nil, pendingEvidenceFailure
}

func TestPGPendingProjectionWorkerMatchesDirectClassificationAcrossMutations(t *testing.T) {
	service, storage, repo := collaborationPG(t)
	ctx, cancel := context.WithTimeout(systemTestContext(), 90*time.Second)
	defer cancel()
	t.Cleanup(func() {
		if err := storage.ConfigurePendingProjection(systemTestContext(), repo, false); err != nil {
			t.Error(err)
		}
	})
	base := collaborationSnapshot(t, storage, repo, "base")
	live := collaborationSnapshot(t, storage, repo, "live", base)
	held := collaborationSnapshot(t, storage, repo, "held", base)
	if err := storage.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
		t.Fatal(err)
	}
	pending := domain.Pending{RepoID: repo, SessionID: "live", Provider: domain.ProviderCodex, Target: live, Branch: "main", UpdatedAt: time.Now().UTC()}
	if err := storage.PutPending(ctx, repo, pending); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutUnsync(ctx, repo, domain.Unsync{RepoID: repo, User: "author", Branch: "topic", Target: held, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigurePendingProjections(ctx, []domain.ContentHash{repo}); err != nil {
		t.Fatal(err)
	}
	direct := NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	spy := &pendingProjectionPGSpy{PostgresStore: storage}
	service.meta = spy
	assertParity := func(label string) {
		t.Helper()
		want, err := direct.GetPendingState(ctx, repo)
		if err != nil {
			t.Fatal(label, err)
		}
		for attempt := 0; attempt < 10; attempt++ {
			if err := service.ProcessPendingProjections(ctx, 16); err != nil {
				t.Fatal(label, err)
			}
			got, err := service.GetPendingState(ctx, repo)
			if errors.Is(err, domain.ErrPendingProjectionNotReady) {
				continue
			}
			if err != nil {
				t.Fatal(label, err)
			}
			if got.ReadGeneration == nil || !got.ReadGeneration.Complete {
				t.Fatal(label, "missing complete generation")
			}
			got.ReadGeneration = nil
			if !reflect.DeepEqual(got, want) {
				actual, _ := json.Marshal(got)
				expected, _ := json.Marshal(want)
				t.Fatalf("%s parity mismatch\ngot: %s\nwant: %s", label, actual, expected)
			}
			if spy.scans != 0 || spy.evidence != 0 {
				t.Fatal("projection used original graph reads", spy.scans, spy.evidence)
			}
			return
		}
		t.Fatal(label, "projection did not converge")
	}
	assertParity("pending-and-unsync")
	if err := service.SetSessionArchived(ctx, repo, live, true); err != nil {
		t.Fatal(err)
	}
	assertParity("archive")
	if err := service.SetSessionArchived(ctx, repo, live, false); err != nil {
		t.Fatal(err)
	}
	assertParity("restore")
	if err := storage.AddGraftParents(ctx, repo, live, []domain.ContentHash{held}); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: live}, base); err != nil {
		t.Fatal(err)
	}
	assertParity("graft-published")
	if err := storage.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base}, live); err != nil {
		t.Fatal(err)
	}
	assertParity("reset-preserves-reflog-publication")
	if err := storage.DeletePending(ctx, repo, pending.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteUnsync(ctx, repo, "author", "topic"); err != nil {
		t.Fatal(err)
	}
	assertParity("deletion-tombstones")
	pr, _ := collaborationPR(t, direct, storage, repo, base, 1)
	assertParity("branch-birth-and-publication")
	evidenceStore := &collaborationGitEvidencePG{PostgresStore: storage, repo: repo}
	evidenceService := NewService(evidenceStore, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	recordPRMergeChain(t, evidenceService, "https://github.com/acme/proj.git", fmt.Sprintf("%040x", 100), pr.MergeSHA)
	if _, err := direct.PromoteRepositoryPR(ctx, repo, pr); err != nil {
		t.Fatal(err)
	}
	assertParity("completed-pr")
	if err := storage.EnableContextProtocol(ctx, repo); err != nil {
		t.Fatal(err)
	}
	assertParity("protocol-migration")
}

func TestPGPendingProjectionWorkerResumesCapturedCursorAfterRestart(t *testing.T) {
	service, storage, repo := collaborationPG(t)
	ctx, cancel := context.WithTimeout(systemTestContext(), 90*time.Second)
	defer cancel()
	t.Cleanup(func() {
		if err := storage.ConfigurePendingProjection(systemTestContext(), repo, false); err != nil {
			t.Error(err)
		}
	})
	for index := 0; index < 130; index++ {
		collaborationSnapshot(t, storage, repo, fmt.Sprintf("snapshot-%03d", index))
	}
	if err := service.ConfigurePendingProjections(ctx, []domain.ContentHash{repo}); err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessPendingProjections(ctx, 1); err != nil {
		t.Fatal(err)
	}
	read, err := storage.ReadPendingProjection(ctx, repo)
	if err != nil || read.Ready {
		t.Fatal("partial capture published a complete view", read, err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	restarted := NewService(peer, peer, auth.NewTeamTokenAuth(), gitengine.NewEngine(peer), peer)
	if err := restarted.ConfigurePendingProjections(ctx, []domain.ContentHash{repo}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ProcessPendingProjections(ctx, 4); err != nil {
		t.Fatal(err)
	}
	got, err := service.GetPendingState(ctx, repo)
	if err != nil || got.ReadGeneration == nil {
		t.Fatal("restart did not resume durable work", got, err)
	}
	before, err := storage.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ProcessPendingProjections(ctx, 4); err != nil {
		t.Fatal(err)
	}
	after, err := storage.RepositoryRevision(ctx, repo)
	if err != nil || before != after {
		t.Fatal("idle derived work changed source revision", before, after, err)
	}
}
