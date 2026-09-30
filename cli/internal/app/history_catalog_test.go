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
			wantReads := 6
			if op == "status" {
				wantReads = 4
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
		})
	}
}
