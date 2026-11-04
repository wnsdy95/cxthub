package app

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPRPromotionProjectsMergedMemoryBeforeAnyMemorize(t *testing.T) {
	ctx := systemTestContext()
	svc, st := newFsckSvc(t)
	repo := hh("projection promotion")
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/a/b"}); err != nil {
		t.Fatal(err)
	}
	recordPRMergeChain(t, svc, "https://github.com/a/b", strings.Repeat("3", 40), strings.Repeat("4", 40))
	origin := prSnapshot(t, st, repo, "origin")
	base := prSnapshot(t, st, repo, "base", origin)
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutMemoryDigest(ctx, repo, domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: base, Summary: "MAIN DECISION"})); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		source := prSnapshot(t, st, repo, fmt.Sprint("feature", i), origin)
		d := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: source, Summary: fmt.Sprintf("FEATURE %d DECISION", i)})
		hash, err := svc.PutMemoryDigest(ctx, repo, d)
		if err != nil {
			t.Fatal(err)
		}
		pr := domain.PullRequestMerge{Number: i, BaseBranch: "main", HeadBranch: fmt.Sprintf("feature/%d", i), HeadSHA: strings.Repeat(fmt.Sprint(i), 40), MergeSHA: strings.Repeat(fmt.Sprint(i+2), 40)}
		birth := domain.HistoryEvent{ID: strings.Repeat(fmt.Sprint(i), 32), RepoID: string(repo), BranchID: fmt.Sprintf("feature-%d", i), Branch: pr.HeadBranch, Kind: "birth", Source: origin, Target: source, GitAfter: pr.HeadSHA, CreatedAt: time.Now().UTC()}
		if err := svc.RecordHistory(ctx, birth); err != nil {
			t.Fatal(err)
		}
		publishPRSource(t, svc, birth)
		if _, err := svc.PromoteRepositoryPR(ctx, repo, pr); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.PromoteRepositoryPR(ctx, repo, pr); err != nil {
			t.Fatal(err)
		}
		got, err := svc.GetMemoryProjection(ctx, repo, source)
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j <= i; j++ {
			want := "MAIN DECISION"
			if j > 0 {
				want = fmt.Sprintf("FEATURE %d DECISION", j)
			}
			if strings.Count(got.Digest.Summary, want) != 1 {
				t.Fatalf("after promotion %d omitted or duplicated %q: %s", i, want, got.Digest.Summary)
			}
		}
		stored, err := svc.GetSnapshot(ctx, repo, source)
		if err != nil {
			t.Fatal(err)
		}
		if stored.MemoryHash != hash {
			t.Fatal("read projection rewrote original memory")
		}
	}
}
