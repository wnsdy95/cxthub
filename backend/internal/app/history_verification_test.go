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

type countingHistoryBlobs struct {
	outbound.BlobStore
	reads   map[domain.ContentHash]int
	corrupt bool
}

func (b *countingHistoryBlobs) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	b.reads[hash]++
	doc, err := b.BlobStore.GetDoc(ctx, repo, hash)
	if b.corrupt {
		doc.CIR.Envelope.SourceProvider = "tampered"
	}
	return doc, err
}

func TestPRPromotionVerifiesEachImmutableArchiveOnce(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(map[bool]string{true: "same snapshot", false: "different source"}[same], func(t *testing.T) {
			svc, st := newFsckSvc(t)
			ctx := systemTestContext()
			repo := hh("verify-once")
			st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/a/b"})
			base := prSnapshot(t, st, repo, "base")
			source := base
			if !same {
				source = prSnapshot(t, st, repo, "feature", base)
			}
			st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base}, "")
			pr := domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature/x", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
			observation := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "feature", Branch: pr.HeadBranch, Kind: "birth", Source: base, Target: source, GitAfter: pr.HeadSHA, CreatedAt: time.Now().UTC()}
			if err := svc.RecordHistory(ctx, observation); err != nil {
				t.Fatal(err)
			}
			publishPRSource(t, svc, observation)
			blobs := &countingHistoryBlobs{BlobStore: st, reads: map[domain.ContentHash]int{}}
			svc.blobs = blobs
			if _, err := svc.PromoteRepositoryPR(ctx, repo, pr); err != nil {
				t.Fatal(err)
			}
			for _, id := range []domain.ContentHash{base, source} {
				if blobs.reads[id] != 1 {
					t.Fatalf("archive %s was reconstructed %d times, want once", id, blobs.reads[id])
				}
			}
			rows, _ := svc.ListHistory(ctx, repo)
			completed := 0
			for _, e := range rows {
				if e.PRCompleted {
					completed++
				}
			}
			if completed != 1 {
				t.Fatalf("completion count = %d", completed)
			}
		})
	}
}

func TestHistoryVerificationDoesNotTrustSeparateRequestsOrFailures(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh("verification-scope")
	st.PutRepo(ctx, domain.Repo{ID: repo})
	id := prSnapshot(t, st, repo, "original")
	blobs := &countingHistoryBlobs{BlobStore: st, reads: map[domain.ContentHash]int{}}
	svc.blobs = blobs
	e := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "main", Branch: "main", Kind: "position", Source: id, Target: id, SharedTarget: id, CreatedAt: time.Now().UTC()}
	if err := svc.RecordHistory(ctx, e); err != nil {
		t.Fatal(err)
	}
	if blobs.reads[id] != 1 {
		t.Fatalf("duplicate fields read archive %d times", blobs.reads[id])
	}
	e.ID = strings.Repeat("2", 32)
	blobs.corrupt = true
	if err := svc.RecordHistory(ctx, e); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("separate request trusted old verification: %v", err)
	}
	blobs.corrupt = false
	if err := svc.RecordHistory(ctx, e); err != nil {
		t.Fatal(err)
	}
	if blobs.reads[id] != 3 {
		t.Fatalf("failure/retry did not reverify: %d reads", blobs.reads[id])
	}
}
