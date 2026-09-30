package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// Deliberately expose the optional capability on the same reader. A wrapper
// around only SnapshotListReader would discard branch/position capabilities.
type unorderedHistoryReader struct {
	*historyReader
	onRead     func(int)
	err        error
	aliasReads int
	position   domain.WorkingPosition
}

func (r *unorderedHistoryReader) ListSnapshots(context.Context, string, string) ([]domain.Snapshot, error) {
	panic("history requested presentation ordering despite catalog capability")
}
func (r *unorderedHistoryReader) ListSnapshotCatalog(ctx context.Context, repo string) ([]domain.Snapshot, error) {
	if r.onRead != nil {
		r.onRead(r.reads + 1)
	}
	if r.err != nil {
		return nil, r.err
	}
	out, err := r.historyReader.ListSnapshots(ctx, repo, "")
	if r.reads%2 == 0 {
		slices.Reverse(out)
	}
	return out, err
}
func (r *unorderedHistoryReader) ResolveLocalBranch(_ context.Context, _, name string) (domain.LocalBranchBinding, error) {
	r.aliasReads++
	if name == "alias" {
		return domain.LocalBranchBinding{Branch: "main"}, nil
	}
	return domain.LocalBranchBinding{}, domain.ErrNotFound
}
func (r *unorderedHistoryReader) ReadWorkingPosition(context.Context, string) (domain.WorkingPosition, error) {
	return r.position, nil
}

func TestHistoryCatalogMatchesFallback(t *testing.T) {
	ctx := context.Background()
	for _, scope := range []string{"head", "branch", "hash", "position", "all", "retained", "missing", "ambiguous"} {
		t.Run(scope, func(t *testing.T) {
			legacy, old := makeHistoryQuery()
			current, fresh := makeHistoryQuery()
			reader := &unorderedHistoryReader{historyReader: fresh}
			current.local = reader
			in := inbound.HistoryQueryInput{}
			switch scope {
			case "branch":
				in.Branch = "main"
			case "hash":
				in.Ref = string(old.snaps[1].ID)
			case "position":
				in.Position = old.snaps[0].ID
			case "all":
				in.All = true
			case "retained":
				in.Retained = true
			case "missing":
				old.snaps[0].Parents = []domain.ContentHash{domain.HashContent([]byte("missing"))}
				fresh.snaps[0].Parents = slices.Clone(old.snaps[0].Parents)
			case "ambiguous":
				ref := domain.Ref{RepoID: old.refs[0].RepoID, Name: "main", Kind: domain.RefTag, Target: old.snaps[0].ID}
				old.refs = append(old.refs, ref)
				fresh.refs = append(fresh.refs, ref)
				in.Ref = "main"
			}
			want, oldErr := legacy.QueryHistory(ctx, in)
			got, err := current.QueryHistory(ctx, in)
			if fmt.Sprint(err) != fmt.Sprint(oldErr) || !reflect.DeepEqual(want, got) {
				t.Fatalf("query changed (including StateHash): old=%+v/%v new=%+v/%v", want, oldErr, got, err)
			}
			if old.reads != 2 || fresh.reads != 2 {
				t.Fatalf("fresh observations: old=%d new=%d", old.reads, fresh.reads)
			}
		})
	}
}

func TestHistoryCatalogKeepsOptionalCapabilitiesAndServerScope(t *testing.T) {
	s, base := makeHistoryQuery()
	r := &unorderedHistoryReader{historyReader: base, position: domain.WorkingPosition{Snapshot: base.refs[0].Target, Branch: "main"}}
	s.local = r
	got, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Ref: "alias"})
	if err != nil || got.Position != base.refs[0].Target || r.aliasReads != 1 {
		t.Fatal(got, err, r.aliasReads)
	}
	r.reads = 0
	base.refs[0].Symbolic = ""
	remote := &historyRemote{err: errors.New("stop after request")}
	s.remote = remote
	_, _ = s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Server: true})
	if r.reads != 0 || remote.input.Branch != "main" || remote.input.Position != string(r.position.Snapshot) {
		t.Fatal("catalog scan or lost pinned branch on server path", r.reads, remote.input)
	}
}

func TestHistoryCatalogFreshMetadataFence(t *testing.T) {
	for _, change := range []string{"memory", "graft", "message", "retained", "ref"} {
		t.Run(change, func(t *testing.T) {
			s, base := makeHistoryQuery()
			r := &unorderedHistoryReader{historyReader: base}
			s.local = r
			r.onRead = func(n int) {
				if n != 2 {
					return
				}
				switch change {
				case "memory":
					base.snaps[2].MemoryHash = domain.HashContent([]byte("new memory"))
				case "graft":
					base.snaps[2].GraftParents = nil
				case "message":
					base.snaps[3].Message = "unreachable metadata still participates"
				case "retained":
					base.snaps = base.snaps[:3]
				case "ref":
					base.refs[0].Target = base.snaps[1].ID
				}
			}
			got, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{})
			if err != nil || r.reads != 3 {
				t.Fatal("did not revalidate changed catalog", r.reads, err)
			}
			legacy := NewHistoryQueryService(s.git, s.code, base, nil)
			want, err := legacy.QueryHistory(context.Background(), inbound.HistoryQueryInput{})
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatal("mixed catalog", got, want, err)
			}
		})
	}
	s, base := makeHistoryQuery()
	s.local = &unorderedHistoryReader{historyReader: base}
	base.unstable = true
	if _, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{}); !errors.Is(err, domain.ErrSelectionChanged) || base.reads != 4 {
		t.Fatal("continuous change accepted", base.reads, err)
	}
}

func TestHistoryCatalogRejectsErrorsAndForeignRetainedRecords(t *testing.T) {
	s, base := makeHistoryQuery()
	r := &unorderedHistoryReader{historyReader: base, err: errors.New("unreadable catalog")}
	s.local = r
	if _, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{}); !errors.Is(err, r.err) {
		t.Fatal(err)
	}
	r.err = nil
	base.snaps[3].RepoID = string(domain.HashContent([]byte("another repo")))
	if _, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{}); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal("hidden corrupt scope", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.onRead = func(int) { t.Fatal("read after cancellation") }
	if _, err := s.QueryHistory(ctx, inbound.HistoryQueryInput{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestHistoryCatalogWorkingQueriesKeepAllFences(t *testing.T) {
	ctx := context.Background()
	for _, op := range []string{"status", "diff", "diff_staged"} {
		t.Run(op, func(t *testing.T) {
			work, f := newWorkingReadFixture(t)
			f.index.Entries = []domain.StagedSession{f.entry(t, 2)}
			f.index = f.index.WithRevision()
			f.pending = []domain.Pending{{RepoID: f.repo, Provider: domain.ProviderCodex, SessionID: "session", Target: workingDoc(t, "session", 3).Hash}}
			base := &historyReader{snaps: []domain.Snapshot{f.snapshots[f.position.Snapshot]}, refs: []domain.Ref{{RepoID: f.repo, Kind: domain.RefHEAD, Name: "HEAD", Symbolic: "main", Target: f.position.Snapshot}}}
			history := NewHistoryQueryService(f, f, base, nil)
			work.history = history
			query := func() (any, error) {
				if op == "status" {
					return work.Status(ctx, "")
				}
				return work.Diff(ctx, inbound.ContextDiffInput{Staged: op == "diff_staged"})
			}
			want, err := query()
			if err != nil {
				t.Fatal(err)
			}
			wantReads := 3
			if op == "status" {
				wantReads = 2
			}
			if base.reads != wantReads {
				t.Fatal("unexpected baseline fence", base.reads)
			}
			base.reads = 0
			history.local = &unorderedHistoryReader{historyReader: base}
			got, err := query()
			if err != nil || !reflect.DeepEqual(want, got) || base.reads != wantReads {
				t.Fatal("changed working result or fence", got, want, err, base.reads)
			}
			base.reads = 0
			work.history = workingHistoryQueryOnly{HistoryQuery: history}
			fallback, err := query()
			if err != nil || !reflect.DeepEqual(want, fallback) || base.reads != 2*wantReads {
				t.Fatalf("fallback changed public result/revision or fence: reads=%d want=%d; err=%v", base.reads, 2*wantReads, err)
			}
		})
	}
}

// Count snapshot and ref scans separately: avoiding a second snapshot scan must
// not hide a second ref scan or a cached ref observation.
type observedHistoryCatalogReader struct {
	*unorderedHistoryReader
	refReads int
	onRefs   func()
	refErr   error
}

func (r *observedHistoryCatalogReader) ListRefs(ctx context.Context, repo string) ([]domain.Ref, error) {
	r.refReads++
	if r.onRefs != nil {
		r.onRefs()
	}
	if r.refErr != nil {
		return nil, r.refErr
	}
	return r.historyReader.ListRefs(ctx, repo)
}

type historyCatalogChange struct {
	name    string
	visible bool
	apply   func(*historyReader)
}

func historyCatalogChanges() []historyCatalogChange {
	return []historyCatalogChange{
		{"reachable_memory", true, func(r *historyReader) { r.snaps[0].MemoryHash = domain.HashContent([]byte("changed attachment")) }},
		{"reachable_graft", true, func(r *historyReader) { r.snaps[2].GraftParents = nil }},
		{"reachable_message", true, func(r *historyReader) { r.snaps[0].Message = "PRIVATE_DO_NOT_PRINT" }},
		{"reachable_settings", true, func(r *historyReader) { r.snaps[0].CodexSettings = domain.HashContent([]byte("changed settings")) }},
		{"unreachable_memory", false, func(r *historyReader) { r.snaps[3].MemoryHash = domain.HashContent([]byte("hidden attachment")) }},
		{"unreachable_claude_settings", false, func(r *historyReader) {
			r.snaps[3].ClaudeSettings = domain.HashContent([]byte("hidden claude settings"))
		}},
		{"unreachable_agents_settings", false, func(r *historyReader) {
			r.snaps[3].AgentsSettings = domain.HashContent([]byte("hidden agents settings"))
		}},
		{"unreachable_codex_settings", false, func(r *historyReader) { r.snaps[3].CodexSettings = domain.HashContent([]byte("hidden codex settings")) }},
		{"unreachable_graft", false, func(r *historyReader) { r.snaps[3].GraftParents = []domain.ContentHash{r.snaps[0].ID} }},
		{"unreachable_message", false, func(r *historyReader) { r.snaps[3].Message = "PRIVATE_DO_NOT_PRINT" }},
		{"retained_remove", false, func(r *historyReader) { r.snaps = r.snaps[:3] }},
		{"retained_add", false, func(r *historyReader) {
			r.snaps = append(r.snaps, domain.Snapshot{ID: domain.HashContent([]byte("new retained snapshot")), RepoID: r.refs[0].RepoID})
		}},
		{"unselected_ref", false, func(r *historyReader) { r.refs[1].Target = r.snaps[3].ID }},
		{"new_ref", false, func(r *historyReader) {
			r.refs = append(r.refs, domain.Ref{RepoID: r.refs[0].RepoID, Kind: domain.RefTag, Name: "retained", Target: r.snaps[3].ID})
		}},
	}
}

func TestHistoryCatalogObservationSingleFreshScanMatchesPublicQuery(t *testing.T) {
	for _, kind := range []string{"ancestry", "missing_parent", "empty"} {
		t.Run(kind, func(t *testing.T) {
			s, base := makeHistoryQuery()
			switch kind {
			case "missing_parent":
				base.snaps[0].Parents = []domain.ContentHash{domain.HashContent([]byte("missing parent"))}
			case "empty":
				base.snaps, base.refs = nil, nil
			}
			r := &observedHistoryCatalogReader{unorderedHistoryReader: &unorderedHistoryReader{historyReader: base}}
			s.local = r
			var observer inbound.LocalHistoryObserver = s
			got, hash, err := observer.ObserveLocalHistory(context.Background(), "fixture-cwd")
			if err != nil || domain.ValidateContentHash(hash) != nil || base.reads != 1 || r.refReads != 1 {
				t.Fatalf("single observation: snapshots=%d refs=%d hash-valid=%t err=%v", base.reads, r.refReads, domain.ValidateContentHash(hash) == nil, err)
			}
			want, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Cwd: "fixture-cwd"})
			if err != nil || !reflect.DeepEqual(got, want) || base.reads != 3 || r.refReads != 3 {
				t.Fatalf("public query lost its two scans or changed DTO/StateHash: snapshots=%d refs=%d err=%v", base.reads, r.refReads, err)
			}
			again, againHash, err := observer.ObserveLocalHistory(context.Background(), "fixture-cwd")
			if err != nil || !reflect.DeepEqual(got, again) || hash != againHash || base.reads != 4 || r.refReads != 4 {
				t.Fatalf("observation cached data or depends on catalog ordering: snapshots=%d refs=%d err=%v", base.reads, r.refReads, err)
			}
		})
	}
}

func TestHistoryCatalogObservationHashIncludesHiddenMetadata(t *testing.T) {
	for _, change := range historyCatalogChanges() {
		t.Run(change.name, func(t *testing.T) {
			s, base := makeHistoryQuery()
			r := &observedHistoryCatalogReader{unorderedHistoryReader: &unorderedHistoryReader{historyReader: base}}
			s.local = r
			before, beforeHash, err := s.ObserveLocalHistory(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			change.apply(base)
			after, afterHash, err := s.ObserveLocalHistory(context.Background(), "")
			if err != nil || domain.ValidateContentHash(afterHash) != nil || beforeHash == afterHash || base.reads != 2 || r.refReads != 2 {
				t.Fatalf("new call missed catalog change: snapshots=%d refs=%d hash-changed=%t err=%v", base.reads, r.refReads, beforeHash != afterHash, err)
			}
			if (before.StateHash != after.StateHash) != change.visible || (!change.visible && !reflect.DeepEqual(before, after)) {
				t.Fatal("full catalog fingerprint changed public HEAD query semantics")
			}
			want, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{})
			if err != nil || !reflect.DeepEqual(after, want) || base.reads != 4 || r.refReads != 4 {
				t.Fatalf("fresh observer DTO differs from fenced query: snapshots=%d refs=%d err=%v", base.reads, r.refReads, err)
			}
		})
	}
}

func TestHistoryCatalogObservationLeavesStabilityToCaller(t *testing.T) {
	s, base := makeHistoryQuery()
	base.unstable = true
	r := &observedHistoryCatalogReader{unorderedHistoryReader: &unorderedHistoryReader{historyReader: base}}
	s.local = r
	_, first, err := s.ObserveLocalHistory(context.Background(), "")
	if err != nil || base.reads != 1 || r.refReads != 1 {
		t.Fatalf("observer attempted inner stabilization: snapshots=%d refs=%d err=%v", base.reads, r.refReads, err)
	}
	_, second, err := s.ObserveLocalHistory(context.Background(), "")
	if err != nil || first == second || base.reads != 2 || r.refReads != 2 {
		t.Fatal("observer concealed fresh unstable metadata", err)
	}
	base.reads, r.refReads = 0, 0
	if _, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{}); !errors.Is(err, domain.ErrSelectionChanged) || base.reads != 4 || r.refReads != 4 {
		t.Fatalf("public query accepted continuous changes: snapshots=%d refs=%d err=%v", base.reads, r.refReads, err)
	}
}

func TestHistoryCatalogObservationRejectsErrorsCancellationAndForeignScope(t *testing.T) {
	for _, kind := range []string{"snapshot_error", "ref_error", "empty_ref_error", "foreign_retained", "foreign_ref", "canceled_before", "canceled_during", "empty_canceled_during"} {
		t.Run(kind, func(t *testing.T) {
			s, base := makeHistoryQuery()
			r := &observedHistoryCatalogReader{unorderedHistoryReader: &unorderedHistoryReader{historyReader: base}}
			s.local = r
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := domain.ErrHashMismatch
			switch kind {
			case "snapshot_error":
				want = errors.New("catalog unavailable")
				r.err = want
			case "ref_error", "empty_ref_error":
				want = errors.New("refs unavailable")
				r.refErr = want
				if kind == "empty_ref_error" {
					base.snaps, base.refs = nil, nil
				}
			case "foreign_retained":
				base.snaps[3].RepoID = string(domain.HashContent([]byte("foreign repo")))
			case "foreign_ref":
				base.refs[1].RepoID = string(domain.HashContent([]byte("foreign repo")))
			case "canceled_before":
				want = context.Canceled
				cancel()
				r.onRead = func(int) { t.Fatal("snapshot read after cancellation") }
				r.onRefs = func() { t.Fatal("ref read after cancellation") }
			case "canceled_during", "empty_canceled_during":
				want = context.Canceled
				r.onRefs = cancel
				if kind == "empty_canceled_during" {
					base.snaps, base.refs = nil, nil
				}
			}
			if _, _, err := s.ObserveLocalHistory(ctx, ""); !errors.Is(err, want) {
				t.Fatalf("observer error=%v want=%v", err, want)
			}
		})
	}
}
