package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type baselineRefQueries struct {
	inbound.LocalRefQueries
	state *promotionBriefingState
	err   error
	calls int
	cwd   string
}

func (queries *baselineRefQueries) Refs(ctx context.Context, cwd string) ([]domain.Ref, error) {
	queries.calls++
	queries.cwd = cwd
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if queries.err != nil {
		return nil, queries.err
	}
	return []domain.Ref{{Kind: domain.RefBranch, Name: "main", Target: queries.state.local}}, nil
}

type baselinePullSync struct {
	*promotionBriefingState
	onPull func() error
}

func (syncer *baselinePullSync) Pull(ctx context.Context, in inbound.SyncInput) (inbound.SyncOutput, error) {
	if syncer.onPull != nil {
		if err := syncer.onPull(); err != nil {
			return inbound.SyncOutput{}, err
		}
	}
	return syncer.promotionBriefingState.Pull(ctx, in)
}

type baselineQueueSync struct {
	*baselinePullSync
	onQueue func()
	queued  int
}

func (syncer *baselineQueueSync) QueuePullRequest(context.Context, inbound.SyncInput, outbound.MergedPullRequest) error {
	syncer.queued++
	if syncer.onQueue != nil {
		syncer.onQueue()
	}
	return nil
}

func newBaselineState() *promotionBriefingState {
	baseline := briefingHash("early baseline")
	return &promotionBriefingState{
		baseline: baseline,
		local:    baseline,
		remote:   baseline,
		promoted: briefingHash("early promoted"),
	}
}

func baselineResolver() *fakePRMergeResolver {
	return &fakePRMergeResolver{pulls: []outbound.MergedPullRequest{{Number: 491, BaseBranch: "main", HeadBranch: "feature/topic"}}}
}

func TestIncomingBaselineUsesRefQueryDespiteUnrelatedCatalogFailure(t *testing.T) {
	root := newPostMergeBriefingRepo(t)
	state := newBaselineState()
	state.listErr = errors.New("unrelated snapshot metadata is unavailable")
	queries := &baselineRefQueries{state: state}
	handleIncomingContexts(context.Background(), &Container{Queries: queries, List: state, Sync: state, PRMerges: baselineResolver()}, root)
	if queries.calls != 1 || queries.cwd != root || state.appends != 1 {
		t.Fatalf("ref calls=%d cwd=%q appends=%d", queries.calls, queries.cwd, state.appends)
	}
	if cursor, ok := capture.ReadPullBriefingCursor(root, "main"); !ok || cursor != state.baseline {
		t.Fatalf("baseline cursor=%s present=%v", cursor, ok)
	}
}

func TestIncomingBaselineSurvivesFetchCancellation(t *testing.T) {
	root := newPostMergeBriefingRepo(t)
	state := newBaselineState()
	queries := &baselineRefQueries{state: state}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var beforeFetch domain.ContentHash
	syncer := &baselinePullSync{promotionBriefingState: state, onPull: func() error {
		beforeFetch, _ = capture.ReadPullBriefingCursor(root, "main")
		cancel()
		return ctx.Err()
	}}
	handleIncomingContexts(ctx, &Container{Queries: queries, List: state, Sync: syncer}, root)
	if beforeFetch != state.baseline {
		t.Fatalf("baseline before fetch=%s, want %s", beforeFetch, state.baseline)
	}
	if cursor, ok := capture.ReadPullBriefingCursor(root, "main"); !ok || cursor != state.baseline {
		t.Fatalf("cancelled fetch lost baseline: %s %v", cursor, ok)
	}
}

func TestIncomingBaselineSurvivesQueuedPromotionAndLaterGitRange(t *testing.T) {
	root := newPostMergeBriefingRepo(t)
	t.Setenv("TERM_SESSION_ID", "queued-baseline-retry")
	state := newBaselineState()
	queries := &baselineRefQueries{state: state}
	var beforeQueue domain.ContentHash
	syncer := &baselineQueueSync{baselinePullSync: &baselinePullSync{promotionBriefingState: state, onPull: func() error { return context.DeadlineExceeded }}}
	syncer.onQueue = func() {
		beforeQueue, _ = capture.ReadPullBriefingCursor(root, "main")
		state.local, state.remote = state.promoted, state.promoted
	}
	container := &Container{Queries: queries, List: state, Sync: syncer, PRMerges: baselineResolver()}
	handleIncomingContexts(context.Background(), container, root)
	if beforeQueue != state.baseline || syncer.queued != 1 {
		t.Fatalf("baseline before queue=%s queued=%d", beforeQueue, syncer.queued)
	}
	runGitForTest(t, root, "update-ref", "-d", "ORIG_HEAD")
	syncer.onPull = nil
	handleIncomingContexts(context.Background(), container, root)
	briefing, ok := capture.ConsumeBriefing(root)
	if !ok || !strings.Contains(briefing, string(state.promoted)) || strings.Contains(briefing, string(state.baseline)) {
		t.Fatalf("retry briefing=%q present=%v", briefing, ok)
	}
	if syncer.queued != 1 {
		t.Fatalf("completed discovery repeated: %d", syncer.queued)
	}
}

func TestIncomingBaselineRefFailureDoesNotFallbackOrBlockDurableHandoff(t *testing.T) {
	root := newPostMergeBriefingRepo(t)
	state := newBaselineState()
	queries := &baselineRefQueries{state: state, err: errors.New("ref read denied")}
	syncer := &baselineQueueSync{baselinePullSync: &baselinePullSync{promotionBriefingState: state}}
	handleIncomingContexts(context.Background(), &Container{Queries: queries, List: state, Sync: syncer, PRMerges: baselineResolver()}, root)
	if queries.calls != 1 || syncer.queued != 1 || !state.pullFetchOnly {
		t.Fatalf("ref calls=%d queued=%d fetched=%v", queries.calls, syncer.queued, state.pullFetchOnly)
	}
	if cursor, ok := capture.ReadPullBriefingCursor(root, "main"); ok || cursor != "" {
		t.Fatalf("failed ref query manufactured a baseline: %s", cursor)
	}
}

func TestLocalBranchTargetPreservesRefQueryErrors(t *testing.T) {
	for _, expected := range []error{context.DeadlineExceeded, domain.ErrHashMismatch, errors.New("ref read denied")} {
		t.Run(expected.Error(), func(t *testing.T) {
			state := newBaselineState()
			queries := &baselineRefQueries{state: state, err: expected}
			target, err := localBranchTarget(context.Background(), &Container{Queries: queries, List: state}, t.TempDir(), "main")
			if target != "" || !errors.Is(err, expected) || !strings.Contains(err.Error(), expected.Error()) {
				t.Fatalf("target=%s error=%v, want original cause %v", target, err, expected)
			}
		})
	}
}

func TestLocalBranchTargetDoesNotReadSnapshotFiles(t *testing.T) {
	root := newPostMergeBriefingRepo(t)
	repo := domain.Repo{ID: string(briefingHash("ref-only repository")), LocalPath: root}
	store := storage.NewFileStore(root)
	baseline := briefingHash("ref-only target")
	ctx := context.Background()
	if err := store.PutRef(ctx, domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: "main", Target: baseline}); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, ".cxt", "objects", "snapshots")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, strings.TrimPrefix(string(briefingHash("unrelated broken metadata")), "sha256:")), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	list := app.NewListSessionsService(store)
	if _, err := list.List(ctx, inbound.ListInput{}); err == nil {
		t.Fatal("fixture did not reject unrelated invalid snapshot metadata")
	}
	container := &Container{Queries: app.NewLocalRefQueryService(mergeObservationGit{repo}, store), List: list}
	if target, err := localBranchTarget(ctx, container, root, "main"); err != nil || target != baseline {
		t.Fatalf("target=%s error=%v", target, err)
	}
	if target, err := localBranchTarget(ctx, container, root, "new-branch"); err != nil || target != "" {
		t.Fatalf("absent branch target=%s error=%v", target, err)
	}
}
