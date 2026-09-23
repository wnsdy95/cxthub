package app

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func prPublication(observation domain.HistoryEvent) domain.HistoryEvent {
	key := sha256.Sum256([]byte(observation.ID + ":publish"))
	return domain.HistoryEvent{ID: fmt.Sprintf("%x", key[:16]), RepoID: observation.RepoID,
		BranchID: observation.BranchID, Branch: observation.Branch, LocalBranch: observation.LocalBranch,
		WorktreeID: observation.WorktreeID, Kind: "publish", Source: observation.Target, Target: observation.Target,
		GitAfter: observation.GitAfter, MemoryPinned: true, CreatedAt: observation.CreatedAt.Add(time.Second)}
}

func publishPRSource(t *testing.T, svc *Service, observation domain.HistoryEvent) {
	t.Helper()
	if err := svc.RecordHistory(systemTestContext(), prPublication(observation)); err != nil {
		t.Fatal(err)
	}
}

func TestPRSourceWaitsForFinalizedPublication(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		t.Run(fmt.Sprintf("preexisting-raw-ancestor=%t", preexisting), func(t *testing.T) {
			ctx := systemTestContext()
			svc, st := newFsckSvc(t)
			repo := hh(t.Name())
			if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/publication"}); err != nil {
				t.Fatal(err)
			}
			base := prSnapshot(t, st, repo, "base")
			first := prSnapshot(t, st, repo, "first provider", base)
			complete := prSnapshot(t, st, repo, "complete capture", first)
			if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
				t.Fatal(err)
			}
			pr := domain.PullRequestMerge{Number: 175, BaseBranch: "main", HeadBranch: "feature/x", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
			birth := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "feature", Branch: pr.HeadBranch, Kind: "birth", Source: base, Target: first, GitAfter: pr.HeadSHA, WorktreeID: strings.Repeat("5", 32), CreatedAt: time.Now().UTC()}
			if preexisting {
				if err := svc.RecordHistory(ctx, birth); err != nil {
					t.Fatal(err)
				}
			}
			job, err := svc.SubmitPRPromotion(ctx, repo, pr)
			if err != nil {
				t.Fatal(err)
			}
			if !preexisting {
				if err := svc.RecordHistory(ctx, birth); err != nil {
					t.Fatal(err)
				}
			}
			assertWaiting := func() {
				t.Helper()
				if err := svc.RetryPRPromotion(ctx, repo, job.ID); err != nil {
					t.Fatal(err)
				}
				if err := svc.ProcessPRPromotions(ctx, 1); err != nil {
					t.Fatal(err)
				}
				got, err := st.GetPRJob(ctx, repo, job.ID)
				if err != nil || got.State != "waiting" || got.Reason != "source_context_pending" {
					t.Fatalf("partial publication was not kept waiting: %+v %v", got, err)
				}
				rows, err := svc.ListHistory(ctx, repo)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range rows {
					if e.Kind == "pr-merge" {
						t.Fatal("raw history froze a PR receipt")
					}
				}
				ref, err := st.GetRef(ctx, repo, domain.RefBranch, "main")
				if err != nil || ref.Target != base {
					t.Fatalf("partial publication moved main: %+v %v", ref, err)
				}
			}
			assertWaiting()
			last := birth
			last.ID, last.Kind = strings.Repeat("2", 32), "position"
			last.Source, last.Target = complete, complete
			if err := svc.RecordHistory(ctx, last); err != nil {
				t.Fatal(err)
			}
			assertWaiting()
			publishPRSource(t, svc, last)
			ref, err := st.GetRef(ctx, repo, domain.RefBranch, "main")
			if err != nil || ref.Target != base {
				t.Fatalf("publish changed the branch before promotion: %+v %v", ref, err)
			}
			if err := svc.RetryPRPromotion(ctx, repo, job.ID); err != nil {
				t.Fatal(err)
			}
			if err := svc.ProcessPRPromotions(ctx, 1); err != nil {
				t.Fatal(err)
			}
			got, err := st.GetPRJob(ctx, repo, job.ID)
			if err != nil || got.State != "completed" {
				t.Fatalf("finalized source did not complete: %+v %v", got, err)
			}
			ref, err = st.GetRef(ctx, repo, domain.RefBranch, "main")
			if err != nil || ref.Target != complete {
				t.Fatalf("promotion missed the complete capture: %+v %v", ref, err)
			}
			rows, err := svc.ListHistory(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range rows {
				if e.Kind == "pr-merge" && e.Source != complete {
					t.Fatalf("wrong frozen source: %+v", e)
				}
			}
		})
	}
}

func TestPublishRequiresAcceptedExactObservation(t *testing.T) {
	for _, field := range []string{"missing", "branch", "identity", "local-branch", "worktree", "empty-worktree", "git", "target", "publish-only", "valid"} {
		t.Run(field, func(t *testing.T) {
			svc, st := newFsckSvc(t)
			ctx := systemTestContext()
			repo := hh(t.Name())
			if _, err := st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
				t.Fatal(err)
			}
			source := prSnapshot(t, st, repo, "source")
			other := prSnapshot(t, st, repo, "other")
			observation := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "feature", Branch: "feature/x", Kind: "position", Source: source, Target: source, GitAfter: strings.Repeat("a", 40), WorktreeID: strings.Repeat("2", 32), CreatedAt: time.Now().UTC()}
			publication := prPublication(observation)
			if field != "missing" && field != "publish-only" {
				if err := svc.RecordHistory(ctx, observation); err != nil {
					t.Fatal(err)
				}
			}
			switch field {
			case "branch":
				publication.Branch = "other"
			case "identity":
				publication.BranchID = "other"
			case "local-branch":
				publication.LocalBranch = "other"
			case "worktree":
				publication.WorktreeID = strings.Repeat("3", 32)
			case "empty-worktree":
				publication.WorktreeID = ""
			case "git":
				publication.GitAfter = strings.Repeat("b", 40)
			case "target":
				publication.Source, publication.Target = other, other
			case "publish-only":
				prior := publication
				prior.ID = strings.Repeat("4", 32)
				if err := st.ApplyHistoryEvent(ctx, prior); err != nil {
					t.Fatal(err)
				}
			}
			err := svc.RecordHistory(ctx, publication)
			if field != "valid" {
				if !errors.Is(err, domain.ErrConflict) {
					t.Fatalf("unproven publication accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.RecordHistory(ctx, publication); err != nil {
				t.Fatalf("acknowledged publication retry: %v", err)
			}
		})
	}
}

func TestHistoricalPRBindingRequiresPublishUntilCompleted(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprintf("completed=%t", completed), func(t *testing.T) {
			svc, st := newFsckSvc(t)
			ctx := systemTestContext()
			repo := hh(t.Name())
			origin := "https://github.com/acme/historical"
			if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: origin}); err != nil {
				t.Fatal(err)
			}
			base := prSnapshot(t, st, repo, "base")
			source := prSnapshot(t, st, repo, "historical source", base)
			ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base}
			if err := st.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
				t.Fatal(err)
			}
			pr := domain.PullRequestMerge{Number: 175, BaseBranch: "main", HeadBranch: "feature/x", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
			key := sha256.Sum256([]byte(string(repo) + "\x00" + normalizeGitURL(origin) + "\x00pr:175"))
			receipt := domain.HistoryEvent{ID: fmt.Sprintf("%x", key[:16]), RepoID: string(repo), BranchID: domain.LegacyContextBranchID(string(repo), "main"), Branch: "main", Kind: "pr-merge", Source: source, Target: source, SharedTarget: base, SourceBranchID: "feature", PR: &pr, CreatedAt: time.Now().UTC()}
			if err := svc.recordHistory(ctx, receipt, true, nil); err != nil {
				t.Fatal(err)
			}
			if completed {
				ref.Target = source
				if err := st.CompareAndSwapRef(ctx, repo, ref, base); err != nil {
					t.Fatal(err)
				}
				if _, err := svc.completePRPromotion(ctx, receipt, base, inbound.UpdateRefOutput{Ref: ref}, nil); err != nil {
					t.Fatal(err)
				}
			}
			_, err := svc.DeliverPRPromotion(ctx, repo, pr)
			if completed && err != nil {
				t.Fatalf("historical completion requires new evidence: %v", err)
			} else if !completed && !errors.Is(err, domain.ErrPRSourcePending) {
				t.Fatalf("historical pending binding bypassed finalization: %v", err)
			}
			got, err := st.GetRef(ctx, repo, domain.RefBranch, "main")
			if err != nil || got.Target != ref.Target {
				t.Fatalf("historical replay moved base: %+v %v", got, err)
			}
			if !completed {
				observation := domain.HistoryEvent{ID: strings.Repeat("3", 32), RepoID: string(repo), BranchID: receipt.SourceBranchID, Branch: pr.HeadBranch, Kind: "position", Source: source, Target: source, GitAfter: pr.HeadSHA, CreatedAt: time.Now().UTC()}
				if err := svc.RecordHistory(ctx, observation); err != nil {
					t.Fatal(err)
				}
				publishPRSource(t, svc, observation)
				if err := svc.RetryPRPromotion(ctx, repo, domain.PRPromotionID(repo, pr.Number)); err != nil {
					t.Fatal(err)
				}
				if _, err := svc.DeliverPRPromotion(ctx, repo, pr); err != nil {
					t.Fatalf("matching publication did not release historical binding: %v", err)
				}
			}
		})
	}
}

func TestPRLegacyGitMessageAndCurrentRefDoNotFinalizeSource(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh(t.Name())
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/legacy"}); err != nil {
		t.Fatal(err)
	}
	base := prSnapshot(t, st, repo, "base")
	doc := domain.SessionDoc{CIR: domain.CIRDocument{}}
	doc.CIR.Envelope.CIRVersion, doc.CIR.Envelope.SourceProvider = "1", "claude"
	doc.CIR.Events = []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "legacy source"}}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := st.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequestMerge{Number: 175, BaseBranch: "main", HeadBranch: "feature/x", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, Branch: pr.HeadBranch, Provider: "claude", Message: "legacy capture [git " + pr.HeadSHA + "]", Parents: []domain.ContentHash{base}}); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]domain.ContentHash{"main": base, pr.HeadBranch: doc.Hash} {
		if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: name, Target: target}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.DeliverPRPromotion(ctx, repo, pr); !errors.Is(err, domain.ErrPRSourcePending) {
		t.Fatalf("legacy metadata bypassed publication: %v", err)
	}
	rows, err := svc.ListHistory(ctx, repo)
	if err != nil || len(rows) != 0 {
		t.Fatalf("legacy metadata froze a receipt: %+v %v", rows, err)
	}
}
