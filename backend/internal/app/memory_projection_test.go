package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type projectionTestSource struct {
	snapshots    []domain.Snapshot
	memories     map[domain.ContentHash]domain.MemoryDigest
	onMemory     func()
	lists, reads int
}

func (s *projectionTestSource) ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error) {
	s.lists++
	return append([]domain.Snapshot(nil), s.snapshots...), nil
}
func (s *projectionTestSource) GetMemory(_ context.Context, _, hash domain.ContentHash) (domain.MemoryDigest, error) {
	s.reads++
	if s.onMemory != nil {
		s.onMemory()
	}
	if d, ok := s.memories[hash]; ok {
		return d, nil
	}
	return domain.MemoryDigest{}, domain.ErrNotFound
}
func projectionSource() (*projectionTestSource, domain.ContentHash, domain.ContentHash) {
	repo, base, tip := hh("repo"), hh("base"), hh("tip")
	s := &projectionTestSource{memories: map[domain.ContentHash]domain.MemoryDigest{}}
	for _, entry := range []struct {
		id   domain.ContentHash
		text string
	}{{base, "BASE DECISION"}, {tip, "TIP DECISION"}} {
		d := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: entry.id, Summary: entry.text})
		hash, _ := domain.MemoryDigestHash(d)
		s.memories[hash] = d
		s.snapshots = append(s.snapshots, domain.Snapshot{ID: entry.id, RepoID: repo, MemoryHash: hash})
	}
	return s, repo, tip
}
func TestProjectMemoryRetriesConcurrentGraftChange(t *testing.T) {
	s, repo, tip := projectionSource()
	s.onMemory = func() {
		s.onMemory = nil
		s.snapshots[1].GraftParents = []domain.ContentHash{s.snapshots[0].ID}
		s.snapshots[1].GraftSeq++
	}
	got, err := ProjectMemory(context.Background(), s, repo, tip)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Digest.Summary, "BASE DECISION") || s.lists != 4 {
		t.Fatalf("inconsistent projection: %+v lists=%d", got, s.lists)
	}
	if s.reads != 2 {
		t.Fatalf("immutable memory loaded repeatedly: %d", s.reads)
	}
}
func TestProjectMemoryRejectsBrokenLineage(t *testing.T) {
	for _, kind := range []string{"missing snapshot", "cycle", "foreign repository", "missing memory", "corrupt memory"} {
		t.Run(kind, func(t *testing.T) {
			s, repo, tip := projectionSource()
			switch kind {
			case "missing snapshot":
				s.snapshots[1].GraftParents = []domain.ContentHash{hh("absent")}
			case "cycle":
				s.snapshots[1].GraftParents = []domain.ContentHash{tip}
			case "foreign repository":
				s.snapshots[1].RepoID = hh("foreign")
			case "missing memory":
				delete(s.memories, s.snapshots[1].MemoryHash)
			case "corrupt memory":
				s.memories[s.snapshots[1].MemoryHash] = domain.MemoryDigest{SnapshotID: tip, Summary: "tampered"}
			}
			if _, err := ProjectMemory(context.Background(), s, repo, tip); err == nil {
				t.Fatal("invalid lineage presented as complete")
			}
		})
	}
}
func TestProjectMemoryInvalidatesTransitiveCoverage(t *testing.T) {
	s, repo, tip := projectionSource()
	s.snapshots[1].Parents = []domain.ContentHash{s.snapshots[0].ID}
	reader := &projectionReader{source: s, repoID: repo, memories: map[domain.ContentHash]domain.MemoryDigest{}}
	if _, err := reader.readState(context.Background(), tip); err != nil {
		t.Fatal(err)
	}
	fp, ok := newMemoryProjectionFingerprinter(context.Background(), reader).root(s.snapshots[1])
	if !ok {
		t.Fatal("incomplete fixture")
	}
	digest := domain.MergeDigests(s.memories[s.snapshots[0].MemoryHash], s.memories[s.snapshots[1].MemoryHash])
	digest.GraftCoverage = &domain.MemoryGraftCoverage{ProjectionVersion: domain.MemoryProjectionVersion, ProjectionComplete: true, LineageFingerprint: fp}
	hash, _ := domain.MemoryDigestHash(digest)
	s.memories[hash] = digest
	s.snapshots[1].MemoryHash = hash
	first, err := ProjectMemory(context.Background(), s, repo, tip)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first.Digest.Summary, "LATE") {
		t.Fatal("invalid fixture")
	}
	late := hh("late")
	d := domain.MemoryDigest{SnapshotID: late, Summary: "LATE DECISION"}
	dh, _ := domain.MemoryDigestHash(d)
	s.memories[dh] = d
	s.snapshots = append(s.snapshots, domain.Snapshot{ID: late, RepoID: repo, MemoryHash: dh})
	s.snapshots[0].GraftParents = []domain.ContentHash{late}
	s.snapshots[0].GraftSeq++
	next, err := ProjectMemory(context.Background(), s, repo, tip)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"BASE DECISION", "TIP DECISION", "LATE DECISION"} {
		if !strings.Contains(next.Digest.Summary, want) {
			t.Fatalf("stale transitive projection omitted %s", want)
		}
	}
	if first.StateHash == next.StateHash {
		t.Fatal("dependency change did not invalidate cursor state")
	}
}
func TestProjectMemoryBoundsAndCancellation(t *testing.T) {
	s, repo, _ := projectionSource()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ProjectMemory(ctx, s, repo, s.snapshots[0].ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if s.lists != 0 {
		t.Fatal("cancelled read touched storage")
	}
	var prev domain.ContentHash
	for i := 0; i <= maxProjectionSnapshots; i++ {
		id := hh(fmt.Sprint("depth", i))
		snap := domain.Snapshot{ID: id, RepoID: repo}
		if prev != "" {
			snap.Parents = []domain.ContentHash{prev}
		}
		s.snapshots = append(s.snapshots, snap)
		prev = id
	}
	if _, err := ProjectMemory(context.Background(), s, repo, prev); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unbounded graph: %v", err)
	}
	reader := &projectionReader{source: s, repoID: repo, bytes: maxProjectionMemoryBytes, memories: map[domain.ContentHash]domain.MemoryDigest{}}
	if _, err := reader.GetMemory(context.Background(), s.snapshots[0].MemoryHash); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unbounded memory: %v", err)
	}
}

func TestPRPromotionProjectsMergedMemoryBeforeAnyMemorize(t *testing.T) {
	ctx := context.Background()
	svc, st := newFsckSvc(t)
	repo := hh("projection promotion")
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/a/b"}); err != nil {
		t.Fatal(err)
	}
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
