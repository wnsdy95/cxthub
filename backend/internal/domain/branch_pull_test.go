package domain

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type branchPlanFixture struct {
	repo      Repo
	request   BranchPullRequest
	refs      []Ref
	history   []HistoryEvent
	snapshots []Snapshot
}

func bpHash(s string) ContentHash { return HashContent([]byte(s)) }
func bpEvent(n int, repo ContentHash, identity, branch, kind string, target ContentHash) HistoryEvent {
	return HistoryEvent{ID: fmt.Sprintf("%032x", n+1), RepoID: string(repo), BranchID: identity, Branch: branch, Kind: kind, Target: target, CreatedAt: time.Unix(int64(n+1), 0)}
}
func bpFixture() branchPlanFixture {
	r := Repo{ID: bpHash("plan repo")}
	a, b, c, u := bpHash("a"), bpHash("b"), bpHash("c"), bpHash("unrelated")
	return branchPlanFixture{repo: r, request: BranchPullRequest{Version: 1, Branch: "feature"}, refs: []Ref{{RepoID: r.ID, Kind: RefBranch, Name: "feature", BranchID: "selected", Target: b}, {RepoID: r.ID, Kind: RefBranch, Name: "other", Target: u}}, history: []HistoryEvent{bpEvent(0, r.ID, "selected", "feature", "birth", b)}, snapshots: []Snapshot{
		{RepoID: r.ID, ID: a, DocHash: a, Branch: "old-name"}, {RepoID: r.ID, ID: b, DocHash: b, Parents: []ContentHash{a}, GraftParents: []ContentHash{c}, GraftSeq: 2}, {RepoID: r.ID, ID: c, DocHash: c}, {RepoID: r.ID, ID: u, DocHash: u}}}
}
func (f branchPlanFixture) plan(ctx context.Context) (BranchPullPlan, error) {
	return SelectBranchPullDependencies(ctx, f.repo, f.request, f.refs, f.history, f.snapshots)
}
func TestBranchPullClosure(t *testing.T) {
	f := bpFixture()
	candidate, absent := bpHash("candidate"), bpHash("absent")
	f.snapshots = append(f.snapshots, Snapshot{RepoID: f.repo.ID, ID: candidate, DocHash: candidate, ClaudeSettings: bpHash("settings")})
	f.request.ObservationRoots = []ContentHash{candidate, absent, candidate}
	p, err := f.plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []ContentHash{bpHash("a"), bpHash("b"), bpHash("c"), candidate}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if !reflect.DeepEqual(p.SnapshotIndex, want) || !reflect.DeepEqual(p.AbsentRoots, []ContentHash{absent}) || len(p.Refs) != 1 || len(p.History) != 1 {
		t.Fatalf("wrong scope: %+v", p)
	}
	if !reflect.DeepEqual(p.SettingsObjects, []BranchPullSettings{{Kind: "claude", Hash: bpHash("settings")}}) {
		t.Fatalf("settings: %+v", p.SettingsObjects)
	}
	for _, snap := range f.snapshots {
		if token, ok := p.SnapshotStates[snap.ID]; ok {
			want, _ := SnapshotStateHash(snap)
			if token != want {
				t.Fatal("state token changed")
			}
		}
	}
	for i, j := 0, len(f.snapshots)-1; i < j; i, j = i+1, j-1 {
		f.snapshots[i], f.snapshots[j] = f.snapshots[j], f.snapshots[i]
	}
	again, err := f.plan(context.Background())
	if err != nil || !reflect.DeepEqual(again, p) {
		t.Fatalf("nondeterministic plan: %v", err)
	}
}
func TestBranchPullNameReuseAndExplicitDependencies(t *testing.T) {
	f := bpFixture()
	old := bpEvent(1, f.repo.ID, "old", "feature", "birth", bpHash("unrelated"))
	archived := bpEvent(2, f.repo.ID, "old", "feature", "archive", bpHash("unrelated"))
	archived.BindingParent = old.ID
	f.history[0].BindingParent = archived.ID
	f.history = append(f.history, archived, old)
	p, err := f.plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	projection, err := ProjectContextBranches(p.History)
	if err != nil || len(p.History) != 3 || projection.Released["feature"] != archived.ID || projection.Active["feature"].ID != "selected" {
		t.Fatalf("projection: %+v %v", projection, err)
	}
}
func TestBranchPullCompletedPRWitness(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "witness_only", true: "explicit_dependency"}[explicit], func(t *testing.T) {
			f := bpFixture()
			birth := bpEvent(1, f.repo.ID, "source", "other", "birth", bpHash("unrelated"))
			pin := bpEvent(2, f.repo.ID, "source", "other", "position", bpHash("unrelated"))
			pin.GitAfter = strings.Repeat("1", 40)
			pin.MemoryPinned = true
			if explicit {
				pin.BindingParent = birth.ID
			}
			future := bpEvent(3, f.repo.ID, "source", "other", "archive", bpHash("unrelated"))
			future.BindingParent = birth.ID
			future.MemoryPinned = true
			future.GitAfter = pin.GitAfter
			receipt := bpEvent(4, f.repo.ID, "selected", "feature", "pr-merge", bpHash("b"))
			receipt.PRCompleted = true
			receipt.Source = pin.Target
			receipt.SourceBranchID = "source"
			receipt.PR = &PullRequestMerge{Number: 1, BaseBranch: "feature", HeadBranch: "other", HeadSHA: pin.GitAfter, MergeSHA: strings.Repeat("2", 40)}
			f.history = append(f.history, birth, pin, future, receipt)
			p, err := f.plan(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			ids := map[string]bool{}
			for _, e := range p.History {
				ids[e.ID] = true
			}
			if !ids[pin.ID] || ids[future.ID] || ids[birth.ID] != explicit {
				t.Fatalf("foreign lifecycle leak or missing witness: %+v", ids)
			}
			f.history = []HistoryEvent{f.history[0], birth, future, receipt}
			if _, err = f.plan(context.Background()); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("lifecycle substituted for ordinary proof: %v", err)
			}
		})
	}
}
func TestBranchPullHistoryBehindHeadAndInheritedMemoryRoots(t *testing.T) {
	f := bpFixture()
	older := bpEvent(1, f.repo.ID, "selected", "feature", "position", bpHash("unrelated"))
	older.GitAfter = strings.Repeat("3", 40)
	older.MemoryPinned = true
	older.MemoryHash = bpHash("historical memory")
	older.MemorySource = bpHash("owner")
	f.history = append(f.history, older)
	f.snapshots = append(f.snapshots, Snapshot{RepoID: f.repo.ID, ID: older.MemorySource, DocHash: older.MemorySource})
	p, err := f.plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.SnapshotIndex) != 5 || len(p.History) != 2 {
		t.Fatalf("lost older code/owner: %+v", p)
	}
}
func TestBranchPullRejectsIncompleteEvidence(t *testing.T) {
	for _, name := range []string{"missing_parent", "cycle", "missing_event_dependency", "conflicting_event", "identity", "owner_root", "bad_doc", "bad_settings", "missing_branch", "unsupported", "too_many_roots"} {
		t.Run(name, func(t *testing.T) {
			f := bpFixture()
			want := ErrIntegrity
			switch name {
			case "missing_parent":
				f.snapshots[1].Parents = []ContentHash{bpHash("missing")}
			case "cycle":
				f.snapshots[0].Parents = []ContentHash{bpHash("b")}
			case "missing_event_dependency":
				f.history[0].BindingParent = strings.Repeat("f", 32)
			case "conflicting_event":
				e := f.history[0]
				e.GitAfter = strings.Repeat("1", 40)
				f.history = append(f.history, e)
			case "identity":
				f.refs[0].BranchID = "different"
				want = ErrRefConflict
			case "owner_root":
				f.history[0].MemorySource = bpHash("missing")
			case "bad_doc":
				f.snapshots[0].DocHash = bpHash("different")
			case "bad_settings":
				f.snapshots[0].AgentsSettings = "invalid"
			case "missing_branch":
				f.request.Branch = "absent"
				want = ErrNotFound
			case "unsupported":
				f.request.Version = 2
				want = ErrValidation
			case "too_many_roots":
				f.request.ObservationRoots = make([]ContentHash, MaxBranchPullRoots+1)
				want = ErrValidation
			}
			p, err := f.plan(context.Background())
			if !errors.Is(err, want) || p.SelectedRef.Target != "" {
				t.Fatalf("got %+v %v, want %v", p, err, want)
			}
		})
	}
}
func TestBranchPullLegacyArchiveAndCancellation(t *testing.T) {
	f := bpFixture()
	f.history = nil
	f.refs[0].BranchID = ""
	archived, err := NewBranchLifecycleRef(f.repo.ID, "feature", bpHash("b"), 1, BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	f.refs = append(f.refs, archived)
	if _, err = f.plan(context.Background()); !errors.Is(err, ErrBranchArchived) {
		t.Fatalf("archive accepted: %v", err)
	}
	f = bpFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = f.plan(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestBranchPullRetainedLifecycleTargetsAreRequired(t *testing.T) {
	f := bpFixture()
	tag, err := NewBranchLifecycleRef(f.repo.ID, "feature", bpHash("unrelated"), 1, BranchActive)
	if err != nil {
		t.Fatal(err)
	}
	f.refs = append(f.refs, tag)
	p, err := f.plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.SnapshotIndex) != 4 || len(p.Refs) != 2 {
		t.Fatalf("disconnected lifecycle root omitted: %+v", p)
	}
	f.snapshots = f.snapshots[:3]
	f.request.ObservationRoots = []ContentHash{tag.Target}
	if _, err = f.plan(context.Background()); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("required lifecycle root treated as optional absence: %v", err)
	}
}

func TestBranchPullTwoNameDependencyChains(t *testing.T) {
	f := bpFixture()
	own := f.history[0]
	own.Branch = "old"
	foreign := bpEvent(1, f.repo.ID, "foreign", "feature", "birth", bpHash("a"))
	release := bpEvent(2, f.repo.ID, "foreign", "elsewhere", "rename", bpHash("a"))
	release.PreviousBranch, release.BindingParent = "feature", foreign.ID
	rename := bpEvent(3, f.repo.ID, "selected", "feature", "rename", bpHash("b"))
	rename.PreviousBranch, rename.BindingParent, rename.NameParent = "old", own.ID, release.ID
	// Input order and timestamps cannot stand in for either causal chain.
	rename.CreatedAt = own.CreatedAt.Add(-time.Hour)
	f.history = []HistoryEvent{rename, release, own, foreign}
	p, err := f.plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	full, err := ProjectContextBranches(f.history)
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := ProjectContextBranches(p.History)
	if err != nil || !reflect.DeepEqual(full, scoped) || len(p.History) != 4 {
		t.Fatalf("lost two-name dependencies: %+v %v", scoped, err)
	}
	for _, missing := range []string{own.ID, foreign.ID} {
		broken := f
		broken.history = nil
		for _, e := range f.history {
			if e.ID != missing {
				broken.history = append(broken.history, e)
			}
		}
		if _, err := broken.plan(context.Background()); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("missing %s accepted: %v", missing, err)
		}
	}
}

func TestBranchPullPRUnionPreservesConflictingExactPins(t *testing.T) {
	f := bpFixture()
	wanted := map[string]bool{f.history[0].ID: true}
	var first HistoryEvent
	for i, target := range []ContentHash{bpHash("a"), bpHash("c")} {
		pin := bpEvent(1+2*i, f.repo.ID, fmt.Sprint("source-", i), fmt.Sprint("name-", i), "position", target)
		pin.MemoryPinned, pin.GitAfter = true, strings.Repeat(fmt.Sprint(i+1), 40)
		receipt := bpEvent(2+2*i, f.repo.ID, "selected", "feature", "pr-merge", bpHash("b"))
		receipt.PRCompleted, receipt.Source, receipt.SourceBranchID = true, pin.Target, pin.BranchID
		receipt.PR = &PullRequestMerge{Number: i + 1, BaseBranch: "feature", HeadBranch: pin.Branch, HeadSHA: pin.GitAfter, MergeSHA: strings.Repeat(fmt.Sprint(i+3), 40)}
		f.history = append(f.history, receipt, pin)
		wanted[pin.ID], wanted[receipt.ID] = true, true
		if i == 0 {
			first = pin
		}
	}
	divergent := first
	divergent.ID, divergent.MemoryHash = fmt.Sprintf("%032x", 6), bpHash("divergent pin")
	wanted[divergent.ID] = true
	wrongIdentity, wrongTarget := first, first
	wrongIdentity.ID, wrongIdentity.BranchID = fmt.Sprintf("%032x", 7), "reused-source-name"
	wrongTarget.ID, wrongTarget.Target = fmt.Sprintf("%032x", 8), bpHash("b")
	f.history = append(f.history, divergent, wrongIdentity, wrongTarget)
	p, err := f.plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range p.History {
		got[e.ID] = true
	}
	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("PR union or competing raw pins changed: got %v want %v", got, wanted)
	}
}
