package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type sourceJobStore interface {
	protocolStore
	outbound.PRJobStore
}

func sourceJobFixture(t *testing.T, st sourceJobStore) (domain.ContentHash, domain.HistoryEvent, time.Time) {
	t.Helper()
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name()))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := st.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	proof := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "feature", Branch: "feature", LocalBranch: "native-alias", WorktreeID: strings.Repeat("5", 32), Kind: "position", Source: doc.Hash, Target: doc.Hash, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(110, 0).UTC()}
	attach := proof
	attach.ID, attach.Kind, attach.GitAfter, attach.CreatedAt = strings.Repeat("2", 32), "attach", "", time.Unix(100, 0).UTC()
	for _, e := range []domain.HistoryEvent{attach, proof} {
		if err := st.ApplyHistoryEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	return repo, proof, time.Unix(1000, 0).UTC()
}

func sourceJob(repo domain.ContentHash, number int, now time.Time) domain.PRPromotionJob {
	pr := domain.PullRequestMerge{Number: number, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	return domain.PRPromotionJob{ID: domain.PRPromotionID(repo, number), RepoID: repo, PR: pr, State: "attention", Reason: "source_finalization_required", Attempts: 8, CreatedAt: now, UpdatedAt: now, NextAttempt: now}
}

func storeSourcePublication(t *testing.T, st sourceJobStore, proof domain.HistoryEvent) {
	t.Helper()
	proof.ID, proof.Kind, proof.CreatedAt = strings.Repeat("3", 32), "publish", time.Unix(50, 0).UTC()
	if err := st.ApplyHistoryEvent(context.Background(), proof); err != nil {
		t.Fatal(err)
	}
}

func checkPRSourceWake(t *testing.T, st sourceJobStore) {
	t.Helper()
	ctx := context.Background()
	repo, proof, now := sourceJobFixture(t, st)
	var jobs []domain.PRPromotionJob
	for i := 1; i <= 6; i++ {
		j := sourceJob(repo, i, now.Add(time.Duration(i)*time.Second))
		switch i {
		case 2:
			j.PR.HeadBranch = "native-alias"
		case 3:
			j.PR.HeadSHA = strings.Repeat("c", 40)
		case 4:
			j.PR.HeadBranch = "other"
		case 5:
			j.Reason = "integrity_check_failed"
		case 6:
			j.State, j.Reason = "completed", ""
		}
		if _, err := st.EnqueuePRJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	// The eligible jobs fall outside the UI's latest-100 window.
	for i := 10; i < 110; i++ {
		j := sourceJob(repo, i, now.Add(time.Duration(i)*time.Second))
		j.State, j.Reason = "completed", ""
		if _, err := st.EnqueuePRJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := st.ListPRJobs(ctx, repo)
	if err != nil || len(listed) != 100 {
		t.Fatalf("UI listing setup: %d %v", len(listed), err)
	}
	for _, j := range listed {
		if j.ID == jobs[0].ID {
			t.Fatal("wake candidate accidentally visible in UI page")
		}
	}
	if err := st.WakePRSourceJobs(ctx, repo, now); err != nil {
		t.Fatal(err)
	}
	before, _ := st.GetPRJob(ctx, repo, jobs[0].ID)
	if before.State != "attention" {
		t.Fatal("ordinary proof inferred legacy finalization")
	}
	storeSourcePublication(t, st, proof)
	// Another repository's wake never releases these jobs.
	if err := st.WakePRSourceJobs(ctx, domain.HashContent([]byte("other repo")), now); err != nil {
		t.Fatal(err)
	}
	before, _ = st.GetPRJob(ctx, repo, jobs[0].ID)
	if before.State != "attention" {
		t.Fatal("cross-repository wake")
	}
	for pass := 0; pass < 2; pass++ {
		if err := st.WakePRSourceJobs(ctx, "", now); err != nil {
			t.Fatal(err)
		}
		for i, j := range jobs {
			got, err := st.GetPRJob(ctx, repo, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if i < 2 {
				if got.State != "waiting" || got.Reason != "" || got.Attempts != 0 || got.Version != j.Version+1 || !got.CreatedAt.Equal(j.CreatedAt) {
					t.Fatalf("exact source wake %d: %+v", i, got)
				}
			} else if got.State != j.State || got.Reason != j.Reason || got.Version != j.Version {
				t.Fatalf("unrelated/terminal job changed %d: %+v", i, got)
			}
		}
	}
}

func checkPRReawakenedSerialization(t *testing.T, st sourceJobStore) {
	t.Helper()
	ctx := context.Background()
	repo, proof, now := sourceJobFixture(t, st)
	old := sourceJob(repo, 1, now)
	young := sourceJob(repo, 2, now.Add(time.Second))
	young.State, young.Reason, young.Attempts = "waiting", "", 0
	third := sourceJob(repo, 3, now.Add(2*time.Second))
	third.State, third.Reason, third.Attempts = "waiting", "", 0
	for _, j := range []domain.PRPromotionJob{old, young, third} {
		if _, err := st.EnqueuePRJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Minute)
	running, err := st.ClaimPRJob(ctx, repo, "", now, time.Minute)
	if err != nil || running.ID != young.ID {
		t.Fatalf("younger ready job: %+v %v", running, err)
	}
	storeSourcePublication(t, st, proof)
	if err := st.WakePRSourceJobs(ctx, repo, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimPRJob(ctx, repo, old.ID, now, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("woken older job overlapped running successor: %v", err)
	}
	// An expired running job recovers before the older waiting job, keeping one
	// running row and fencing completion from the expired worker.
	now = now.Add(2 * time.Minute)
	recovered, err := st.ClaimPRJob(ctx, repo, "", now, time.Minute)
	if err != nil || recovered.ID != young.ID {
		t.Fatalf("expired successor recovery: %+v %v", recovered, err)
	}
	running.State = "completed"
	if err := st.FinishPRJob(ctx, running); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale completion not fenced: %v", err)
	}
	recovered.State = "completed"
	if err := st.FinishPRJob(ctx, recovered); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{old.ID, third.ID} {
		got, err := st.ClaimPRJob(ctx, repo, "", now, time.Minute)
		if err != nil || got.ID != want {
			t.Fatalf("FIFO after running successor: %+v %v", got, err)
		}
		got.State = "completed"
		if err := st.FinishPRJob(ctx, got); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFSPRSourceWake(t *testing.T) { checkPRSourceWake(t, NewFSStore(t.TempDir())) }
func TestFSPRReawakenedSerialization(t *testing.T) {
	checkPRReawakenedSerialization(t, NewFSStore(t.TempDir()))
}
