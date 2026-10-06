package domain

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type publicationFixture struct {
	in   PublicationPlanInput
	next int
	a, b ContentHash
}

func newPublicationFixture() *publicationFixture {
	f := &publicationFixture{next: 10, a: HashContent([]byte("A")), b: HashContent([]byte("B"))}
	f.in = PublicationPlanInput{RepoID: string(HashContent([]byte("repo"))), ContextProtocol: 1, Scope: PublicationScope{Branches: []PublicationBranch{{"feature", "R"}}}}
	f.in.History = []HistoryEvent{f.event("birth", "R", "feature", f.a)}
	f.in.Refs = []Ref{{RepoID: f.in.RepoID, Kind: RefBranch, Name: "feature", BranchID: "R", Target: f.a}}
	f.in.Snapshots = []Snapshot{{ID: f.a, DocHash: f.a, RepoID: f.in.RepoID}, {ID: f.b, DocHash: f.b, RepoID: f.in.RepoID, Parents: []ContentHash{f.a}}}
	return f
}
func (f *publicationFixture) event(kind, id, name string, target ContentHash) HistoryEvent {
	f.next++
	e := HistoryEvent{ID: fmt.Sprintf("%032x", f.next), RepoID: f.in.RepoID, BranchID: id, Branch: name, Kind: kind, Target: target, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(int64(f.next), 0).UTC()}
	if kind == "publish" {
		e.Source = target
	}
	return e
}
func (f *publicationFixture) origin(branchID, name string) {
	e := &f.in.History[0]
	e.Creation = &GitCreation{Evidence: "process-argv", Command: []string{"git", "branch", e.Branch, name}, StartRef: name, StartCommit: e.GitAfter, OriginBranch: name, OriginBranchID: branchID}
}
func (f *publicationFixture) publication(id, name, alias, worktree string, target ContentHash) (HistoryEvent, HistoryEvent) {
	proof := f.event("position", id, name, target)
	proof.LocalBranch, proof.WorktreeID = alias, worktree
	p := f.event("publish", id, name, target)
	p.LocalBranch, p.WorktreeID = alias, worktree
	f.in.History = append(f.in.History, proof, p)
	return proof, p
}
func planMust(t *testing.T, in PublicationPlanInput) PublicationPlan {
	t.Helper()
	p, err := PlanPublication(in)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func planMustFail(t *testing.T, in PublicationPlanInput, want error) {
	t.Helper()
	p, err := PlanPublication(in)
	if !errors.Is(err, want) || !reflect.DeepEqual(p, PublicationPlan{}) {
		t.Fatalf("plan=%+v err=%v want=%v", p, err, want)
	}
}
func sentIDs(p PublicationPlan) []string {
	out := []string{}
	for _, e := range p.HistoryToSend {
		out = append(out, e.ID)
	}
	return out
}

func TestPublicationPlanSelectionContract(t *testing.T) {
	f := newPublicationFixture()
	auto := planMust(t, f.in)
	manual := f.in
	manual.Scope.Branches = nil
	manual.Ref = "feature"
	if got := planMust(t, manual); !reflect.DeepEqual(auto, got) {
		t.Fatalf("manual/identity plans differ: %+v %+v", auto, got)
	}
	for _, tc := range []struct {
		name string
		edit func(*PublicationPlanInput)
		want error
	}{
		{"empty scoped selection", func(in *PublicationPlanInput) { in.Scope.Branches = nil }, ErrInvalidRef},
		{"protocol zero defers", func(in *PublicationPlanInput) { in.ContextProtocol = 0 }, ErrContextProtocolRequired},
		{"two selectors", func(in *PublicationPlanInput) { in.Ref = "feature" }, ErrInvalidRef},
		{"wrong frozen identity", func(in *PublicationPlanInput) { in.Scope.Branches = []PublicationBranch{{"feature", "other"}} }, ErrSyncConflict},
		{"unknown explicit name", func(in *PublicationPlanInput) { in.Scope.Branches = nil; in.Ref = "absent" }, ErrNotFound},
		{"conflicting selected names", func(in *PublicationPlanInput) {
			in.Scope.Branches = append(in.Scope.Branches, PublicationBranch{"other", "R"})
		}, ErrSyncConflict},
	} {
		t.Run(tc.name, func(t *testing.T) { f := newPublicationFixture(); tc.edit(&f.in); planMustFail(t, f.in, tc.want) })
	}
}

func TestPublicationPlanImmutableCollisions(t *testing.T) {
	for _, where := range []string{"local-local", "accepted-accepted", "accepted-local", "filtered foreign"} {
		t.Run(where, func(t *testing.T) {
			f := newPublicationFixture()
			e := f.in.History[0]
			bad := e
			bad.Target = f.b
			switch where {
			case "local-local":
				f.in.History = append(f.in.History, bad)
			case "accepted-accepted":
				f.in.Accepted = []HistoryEvent{e, bad}
			case "accepted-local":
				f.in.Accepted = []HistoryEvent{bad}
			case "filtered foreign":
				bad.BranchID, bad.Branch = "X", "unselected"
				f.in.History = append(f.in.History, bad)
			}
			planMustFail(t, f.in, ErrHashMismatch)
		})
	}
	f := newPublicationFixture()
	e := f.in.History[0]
	f.in.History = append(f.in.History, e)
	f.in.Accepted = []HistoryEvent{e, e}
	p := planMust(t, f.in)
	if len(p.HistoryToSend) != 0 || len(p.RefsToPush) != 1 {
		t.Fatalf("exact duplicates not acknowledged: %+v", p)
	}
}

func TestPublicationPlanPreservesExactEmptyCreationPayload(t *testing.T) {
	f := newPublicationFixture()
	f.in.History[0].Creation = &GitCreation{Evidence: "unavailable", Command: []string{}}
	p := planMust(t, f.in)
	if !reflect.DeepEqual(p.HistoryToSend, f.in.History) {
		t.Fatal("plan changed immutable empty creation payload")
	}
}

func TestPublicationPlanForeignWitnessCausalOrder(t *testing.T) {
	for _, kind := range []string{"position", "attach"} {
		t.Run(kind, func(t *testing.T) {
			f := newPublicationFixture()
			f.origin("X", "source")
			w := f.event(kind, "X", "source", f.b)
			w.CreatedAt = time.Unix(9999, 0).UTC()
			w.MemorySource = HashContent([]byte("owner"))
			w.MemoryHash = HashContent([]byte("memory"))
			w.MemoryPinned = true
			f.in.History = append(f.in.History, w)
			p := planMust(t, f.in)
			if got := sentIDs(p); !reflect.DeepEqual(got, []string{w.ID, f.in.History[0].ID}) {
				t.Fatalf("witness must precede reversed-clock birth: %v", got)
			}
			if !reflect.DeepEqual(p.Authority, []PublicationBranch{{"feature", "R"}}) || len(p.RefsToPush) != 1 {
				t.Fatalf("foreign authority leaked: %+v", p)
			}
			if !slices.Contains(p.SnapshotRoots, w.MemorySource) || slices.Contains(p.SnapshotRoots, w.MemoryHash) {
				t.Fatalf("snapshot roots confuse owner with memory object: %v", p.SnapshotRoots)
			}
			// Output owns nested immutable payloads instead of borrowing mutable caller buffers.
			f.in.History[0].Creation.Command[0] = "changed"
			if p.HistoryToSend[1].Creation.Command[0] != "git" {
				t.Fatal("plan aliases input payload")
			}
		})
	}
}

func TestPublicationPlanAcceptedWitnessAndNoForeignWrites(t *testing.T) {
	for _, kind := range []string{"birth", "position", "publish"} {
		t.Run("accepted "+kind, func(t *testing.T) {
			f := newPublicationFixture()
			f.origin("X", "source")
			w := f.event(kind, "X", "source", f.b)
			f.in.Accepted = []HistoryEvent{w}
			p := planMust(t, f.in)
			if got := sentIDs(p); !reflect.DeepEqual(got, []string{f.in.History[0].ID}) {
				t.Fatalf("accepted witness replayed: %v", got)
			}
		})
	}
	for _, kind := range []string{"birth", "publish"} {
		t.Run("unaccepted "+kind, func(t *testing.T) {
			f := newPublicationFixture()
			f.origin("X", "source")
			f.in.History = append(f.in.History, f.event(kind, "X", "source", f.b))
			planMustFail(t, f.in, ErrSyncConflict)
		})
	}
	f := newPublicationFixture()
	f.origin("X", "source")
	birth := f.event("birth", "X", "source", f.b)
	w := f.event("position", "X", "source", f.b)
	w.BindingParent = birth.ID
	f.in.History = append(f.in.History, birth, w)
	planMustFail(t, f.in, ErrSyncConflict)
	// A rejected optional witness must not prevent using another closed witness.
	good := f.event("position", "X", "source", f.b)
	f.in.History = append(f.in.History, good)
	p := planMust(t, f.in)
	if slices.Contains(sentIDs(p), w.ID) || slices.Contains(sentIDs(p), birth.ID) || !slices.Contains(sentIDs(p), good.ID) {
		t.Fatalf("failed witness closure leaked: %v", sentIDs(p))
	}
}

func TestPublicationPlanGlobalAmbiguityBeforePartition(t *testing.T) {
	f := newPublicationFixture()
	x := f.event("birth", "X", "other", f.b)
	f.in.History = append(f.in.History, x)
	f.in.Refs = append(f.in.Refs, Ref{RepoID: f.in.RepoID, Kind: RefBranch, Name: "other", BranchID: "X", Target: f.b})
	f.publication("R", "feature", "shared", strings.Repeat("1", 32), f.a)
	f.publication("X", "other", "shared", strings.Repeat("2", 32), f.b)
	planMustFail(t, f.in, ErrSyncConflict)
	f.in.Scope.Branches = []PublicationBranch{{"other", "X"}}
	planMustFail(t, f.in, ErrSyncConflict)
	// A previously accepted barrier is terminal, not a competing new capture.
	f.in.Scope.Branches = []PublicationBranch{{"feature", "R"}}
	for _, e := range f.in.History {
		if e.BranchID == "X" {
			f.in.Accepted = append(f.in.Accepted, e)
		}
	}
	planMust(t, f.in)
}

func TestPublicationPlanCompleteAliasGroupAndOrdering(t *testing.T) {
	for _, covers := range []bool{false, true} {
		t.Run(fmt.Sprint(covers), func(t *testing.T) {
			f := newPublicationFixture()
			a := f.event("attach", "R", "feature", f.a)
			a.LocalBranch = "alias"
			a.WorktreeID = strings.Repeat("1", 32)
			f.in.History = append(f.in.History, a)
			_, pa := f.publication("R", "feature", "alias", strings.Repeat("2", 32), f.a)
			alias := ""
			if covers {
				alias = "alias"
			}
			_, pb := f.publication("R", "feature", alias, strings.Repeat("3", 32), f.b)
			if !covers {
				planMustFail(t, f.in, ErrSyncConflict)
				return
			}
			p := planMust(t, f.in)
			ids := sentIDs(p)
			if slices.Index(ids, pb.ID) >= slices.Index(ids, pa.ID) {
				t.Fatalf("ancestor preceded maximal completion: %v", ids)
			}
			// Permuting the same frozen catalog must not change authority or payload order.
			slices.Reverse(f.in.History)
			if other := planMust(t, f.in); !reflect.DeepEqual(other, p) {
				t.Fatalf("input order changed plan: %+v %+v", p, other)
			}
		})
	}
}

func TestPublicationPlanDoesNotDrainUnrelatedIdentity(t *testing.T) {
	f := newPublicationFixture()
	f.in.History = append(f.in.History, f.event("birth", "X", "other", f.b))
	adv := f.event("advance", "X", "other", f.a)
	adv.Source = f.b
	f.in.History = append(f.in.History, adv)
	f.publication("X", "other", "", "", f.a)
	f.publication("X", "other", "", "", HashContent([]byte("missing foreign graph")))
	p := planMust(t, f.in)
	if len(p.HistoryToSend) != 1 || p.HistoryToSend[0].BranchID != "R" {
		t.Fatalf("unselected group leaked: %+v", p)
	}
}

func TestPublicationPlanMissingAndCyclicDependencies(t *testing.T) {
	for _, cycle := range []bool{false, true} {
		t.Run(fmt.Sprint(cycle), func(t *testing.T) {
			f := newPublicationFixture()
			a := f.event("position", "R", "feature", f.a)
			b := f.event("position", "R", "feature", f.b)
			a.BindingParent = b.ID
			f.in.History = append(f.in.History, a)
			if cycle {
				b.BindingParent = a.ID
				f.in.History = append(f.in.History, b)
			}
			planMustFail(t, f.in, ErrSyncConflict)
		})
	}
	f := newPublicationFixture()
	f.origin("X", "source")
	x := f.event("birth", "X", "source", f.b)
	x.Creation = &GitCreation{Evidence: "process-argv", Command: []string{"git", "branch", "source", "feature"}, StartRef: "feature", StartCommit: x.GitAfter, OriginBranch: "feature", OriginBranchID: "R"}
	f.in.History = append(f.in.History, x)
	f.in.Scope.HistoryOnly = true
	f.in.Scope.Branches = append(f.in.Scope.Branches, PublicationBranch{"source", "X"})
	planMustFail(t, f.in, ErrSyncConflict)
}

func TestPublicationPlanHistoryOnlyAndLostAcknowledgment(t *testing.T) {
	f := newPublicationFixture()
	archive := f.event("archive", "R", "feature", "")
	archive.Source = f.a
	archive.BindingParent = f.in.History[0].ID
	f.in.History = append(f.in.History, archive)
	f.in.Scope.HistoryOnly = true
	f.in.Refs = nil
	first := planMust(t, f.in)
	if len(first.RefsToPush) != 0 || len(first.HistoryToSend) != 2 {
		t.Fatalf("history-only reconciled tips: %+v", first)
	}
	f.in.Accepted = first.HistoryToSend[:1]
	retry := planMust(t, f.in)
	if !reflect.DeepEqual(retry.Authority, first.Authority) || !reflect.DeepEqual(retry.HistoryToSend, first.HistoryToSend[1:]) {
		t.Fatalf("retry changed authority/payload: %+v", retry)
	}
	f.in.Accepted = first.HistoryToSend
	if p := planMust(t, f.in); len(p.HistoryToSend) != 0 || len(p.RefsToPush) != 0 {
		t.Fatalf("empty drain broadened: %+v", p)
	}
}

func TestPublicationPlanRejectsIncompleteOrInvalidGraph(t *testing.T) {
	for _, scenario := range []string{"missing", "cycle", "repo", "duplicate", "incomparable"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPublicationFixture()
			f.publication("R", "feature", "", "", f.a)
			f.publication("R", "feature", "", "", f.b)
			want := ErrSyncConflict
			switch scenario {
			case "missing":
				f.in.Snapshots = f.in.Snapshots[:1]
				want = ErrNotFound
			case "cycle":
				f.in.Snapshots[0].Parents = []ContentHash{f.b}
			case "repo":
				f.in.Snapshots[0].RepoID = "other"
				want = ErrHashMismatch
			case "duplicate":
				bad := f.in.Snapshots[0]
				bad.Message = "changed"
				f.in.Snapshots = append(f.in.Snapshots, bad)
				want = ErrHashMismatch
			case "incomparable":
				f.in.Snapshots[1].Parents = nil
			}
			planMustFail(t, f.in, want)
		})
	}
}

func TestPublicationPlanForeignEffectClassesStayPending(t *testing.T) {
	for _, kind := range []string{"birth", "orphan", "rename", "archive", "advance", "publish"} {
		t.Run(kind, func(t *testing.T) {
			f := newPublicationFixture()
			f.origin("X", "source")
			birth := f.event("birth", "X", "source", f.b)
			effect := birth
			if kind != "birth" {
				effect = f.event(kind, "X", "source", f.b)
				switch kind {
				case "orphan":
					effect.Target = ""
				case "rename":
					effect.PreviousBranch, effect.Branch, effect.Source = "source", "renamed", f.b
					effect.BindingParent = birth.ID
					f.in.History = append(f.in.History, birth)
				case "archive":
					effect.Source, effect.Target, effect.BindingParent = f.b, "", birth.ID
					f.in.History = append(f.in.History, birth)
				case "advance":
					effect.Source, effect.BindingParent = f.b, birth.ID
					f.in.History = append(f.in.History, birth)
				}
			}
			w := f.event("position", "X", "source", f.b)
			w.BindingParent = effect.ID
			f.in.History = append(f.in.History, effect, w)
			planMustFail(t, f.in, ErrSyncConflict)
		})
	}
	f := newPublicationFixture()
	f.origin("X", "source")
	witness := f.event("position", "X", "source", f.b)
	completion := f.event("publish", "X", "source", f.b)
	f.in.History = append(f.in.History, witness, completion)
	p := planMust(t, f.in)
	if !slices.Contains(sentIDs(p), witness.ID) || slices.Contains(sentIDs(p), completion.ID) {
		t.Fatalf("ordinary witness activated foreign completion: %v", sentIDs(p))
	}
}

func TestPublicationPlanRejectsMissingProofAndSelfOrigin(t *testing.T) {
	f := newPublicationFixture()
	completion := f.event("publish", "R", "feature", f.a)
	completion.GitAfter = strings.Repeat("b", 40) // birth is valid proof only at its exact code association
	f.in.History = append(f.in.History, completion)
	planMustFail(t, f.in, ErrSyncConflict)
	f = newPublicationFixture()
	f.origin("R", "feature")
	f.in.History = append(f.in.History, f.event("position", "R", "feature", f.a))
	planMustFail(t, f.in, ErrSyncConflict)
}

func TestPublicationAncestryRootsPreserveExistingReadScope(t *testing.T) {
	f := newPublicationFixture()
	_, a := f.publication("R", "feature", "", "", f.a)
	roots, err := PublicationAncestryRoots(f.in.RepoID, f.in.History, nil)
	if err != nil || len(roots) != 0 {
		t.Fatalf("single-source group requested graph: %v %v", roots, err)
	}
	f.publication("R", "feature", "", "", f.b)
	roots, err = PublicationAncestryRoots(f.in.RepoID, f.in.History, nil)
	if err != nil || len(roots) != 2 {
		t.Fatalf("competing targets missing: %v %v", roots, err)
	}
	roots, err = PublicationAncestryRoots(f.in.RepoID, f.in.History, []HistoryEvent{a})
	if err != nil || len(roots) != 0 {
		t.Fatalf("accepted barrier still competes: %v %v", roots, err)
	}
	bad := a
	bad.Target = f.b
	bad.Source = f.b
	if _, err = PublicationAncestryRoots(f.in.RepoID, f.in.History, []HistoryEvent{a, bad}); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("collision hidden before graph loads: %v", err)
	}
}
