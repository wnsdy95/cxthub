package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// Embedding the port, rather than the concrete service, deliberately hides the
// optional observer and exercises the independently fenced legacy query path.
type workingHistoryQueryOnly struct{ inbound.HistoryQuery }

func newWorkingHistoryObservationFixture(t *testing.T) (*WorkingStateService, *workingReadFixture, *HistoryQueryService, *observedHistoryCatalogReader) {
	t.Helper()
	work, f := newContextDiffFixture(t)
	f.index.Entries = []domain.StagedSession{f.entry(t, 2)}
	f.index = f.index.WithRevision()
	f.pending = []domain.Pending{{RepoID: f.repo, Provider: domain.ProviderCodex, SessionID: "session", Target: workingDoc(t, "session", 3).Hash}}
	_, base := makeHistoryQuery()
	head := f.snapshots[f.position.Snapshot]
	head.Parents = base.snaps[2].Parents
	head.GraftParents = base.snaps[2].GraftParents
	base.snaps[2] = head
	for i := range base.refs {
		base.refs[i].Target = head.ID
	}
	f.snapshots[head.ID] = head
	reader := &observedHistoryCatalogReader{unorderedHistoryReader: &unorderedHistoryReader{historyReader: base}}
	history := NewHistoryQueryService(f, f, reader, nil)
	work.history = history
	return work, f, history, reader
}

func queryWorkingHistoryObservation(ctx context.Context, s *WorkingStateService, op, cwd string) (any, error) {
	if op == "status" {
		return s.Status(ctx, cwd)
	}
	return s.Diff(ctx, inbound.ContextDiffInput{Cwd: cwd, Staged: op == "diff_staged"})
}

func workingHistoryObservationReads(op string) int {
	if op == "status" {
		return 2
	}
	return 3
}

func assertWorkingHistoryObservationPublicEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatal("working DTO or public revision differs")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE_DO_NOT_PRINT") || strings.Contains(string(raw), "CatalogRevision") || strings.Contains(string(raw), "catalog_revision") {
		t.Fatal("private metadata or catalog fence exposed in diagnostics")
	}
}

func TestWorkingHistoryObservationFreshCountsAndFallbackDTO(t *testing.T) {
	for _, op := range []string{"status", "diff", "diff_staged"} {
		t.Run(op, func(t *testing.T) {
			s, _, history, r := newWorkingHistoryObservationFixture(t)
			wantReads := workingHistoryObservationReads(op)
			got, err := queryWorkingHistoryObservation(context.Background(), s, op, "fixture-cwd")
			if err != nil || r.reads != wantReads || r.refReads != wantReads {
				t.Fatalf("observer scans: snapshots=%d refs=%d want=%d err=%v", r.reads, r.refReads, wantReads, err)
			}
			// A second top-level call must obtain a new catalog even when unchanged.
			again, err := queryWorkingHistoryObservation(context.Background(), s, op, "fixture-cwd")
			if err != nil || r.reads != 2*wantReads || r.refReads != 2*wantReads {
				t.Fatalf("new call reused observations: snapshots=%d refs=%d err=%v", r.reads, r.refReads, err)
			}
			assertWorkingHistoryObservationPublicEqual(t, again, got)
			r.reads, r.refReads = 0, 0
			s.history = workingHistoryQueryOnly{HistoryQuery: history}
			if _, ok := s.history.(inbound.LocalHistoryObserver); ok {
				t.Fatal("fallback wrapper still exposes observer")
			}
			fallback, err := queryWorkingHistoryObservation(context.Background(), s, op, "fixture-cwd")
			if err != nil || r.reads != 2*wantReads || r.refReads != 2*wantReads {
				t.Fatalf("fallback scans: snapshots=%d refs=%d want=%d err=%v", r.reads, r.refReads, 2*wantReads, err)
			}
			assertWorkingHistoryObservationPublicEqual(t, got, fallback)
		})
	}
}

func TestWorkingHistoryObservationRetriesFullCatalogChanges(t *testing.T) {
	for _, op := range []string{"status", "diff", "diff_staged"} {
		for _, change := range historyCatalogChanges() {
			t.Run(op+"/"+change.name, func(t *testing.T) {
				s, _, history, r := newWorkingHistoryObservationFixture(t)
				before, err := queryWorkingHistoryObservation(context.Background(), s, op, "")
				if err != nil {
					t.Fatal(err)
				}
				r.reads, r.refReads = 0, 0
				r.onRead = func(n int) {
					if n == 2 {
						change.apply(r.historyReader)
					}
				}
				got, err := queryWorkingHistoryObservation(context.Background(), s, op, "")
				wantReads := workingHistoryObservationReads(op) + 1
				if err != nil || r.reads != wantReads || r.refReads != wantReads {
					t.Fatalf("catalog change escaped outer fence: snapshots=%d refs=%d want=%d err=%v", r.reads, r.refReads, wantReads, err)
				}
				if !change.visible {
					assertWorkingHistoryObservationPublicEqual(t, got, before)
				} else if reflect.DeepEqual(got, before) {
					t.Fatal("reachable metadata change did not update public revision")
				}
				// The settled observer result must preserve the legacy revision hash.
				r.onRead = nil
				s.history = workingHistoryQueryOnly{HistoryQuery: history}
				want, err := queryWorkingHistoryObservation(context.Background(), s, op, "")
				if err != nil {
					t.Fatal(err)
				}
				assertWorkingHistoryObservationPublicEqual(t, got, want)
			})
		}
	}
}

func TestWorkingHistoryObservationRejectsContinuousHiddenChanges(t *testing.T) {
	for _, op := range []string{"status", "diff", "diff_staged"} {
		for _, change := range historyCatalogChanges() {
			if change.visible {
				continue
			}
			t.Run(op+"/"+change.name, func(t *testing.T) {
				s, _, _, r := newWorkingHistoryObservationFixture(t)
				originalSnapshots, originalRefs := slices.Clone(r.snaps), slices.Clone(r.refs)
				r.onRead = func(n int) {
					r.snaps, r.refs = slices.Clone(originalSnapshots), slices.Clone(originalRefs)
					if n%2 == 0 {
						change.apply(r.historyReader)
					}
				}
				if _, err := queryWorkingHistoryObservation(context.Background(), s, op, ""); !errors.Is(err, domain.ErrSelectionChanged) || r.reads != 4 || r.refReads != 4 {
					t.Fatalf("continuous hidden changes accepted or retries unbounded: snapshots=%d refs=%d err=%v", r.reads, r.refReads, err)
				}
			})
		}
	}
}

func TestWorkingHistoryObservationStillFencesStateWithUnchangedCatalog(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		for _, op := range []string{"status", "diff", "diff_staged"} {
			t.Run(fmt.Sprintf("%s/fallback=%t", op, fallback), func(t *testing.T) {
				s, f, history, r := newWorkingHistoryObservationFixture(t)
				scans := 1
				if fallback {
					s.history = workingHistoryQueryOnly{HistoryQuery: history}
					scans = 2
				}
				// Mutation after one complete observation, without changing history.
				r.onRead = func(n int) {
					if n == scans+1 {
						f.pending[0].Target = workingDoc(t, "session", 4).Hash
					}
				}
				got, err := queryWorkingHistoryObservation(context.Background(), s, op, "")
				wantReads := scans * (workingHistoryObservationReads(op) + 1)
				if err != nil || r.reads != wantReads || r.refReads != wantReads {
					t.Fatalf("working revision change escaped fence: snapshots=%d refs=%d want=%d err=%v", r.reads, r.refReads, wantReads, err)
				}
				r.onRead = nil
				want, err := queryWorkingHistoryObservation(context.Background(), s, op, "")
				if err != nil {
					t.Fatal(err)
				}
				assertWorkingHistoryObservationPublicEqual(t, got, want)
				if op == "diff" {
					diff := got.(domain.ContextDiff)
					if len(diff.Changes) != 1 || diff.Changes[0].After != f.pending[0].Target || diff.Changes[0].Added.End != 4 {
						t.Fatal("diff returned the prior pending capture")
					}
				}
			})
		}
	}
}

func TestWorkingHistoryObservationDiffRechecksHiddenMetadataAfterBodies(t *testing.T) {
	for _, op := range []string{"diff", "diff_staged"} {
		for _, change := range historyCatalogChanges() {
			if change.visible {
				continue
			}
			for _, continuous := range []bool{false, true} {
				if continuous && change.name != "unreachable_message" {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/continuous=%t", op, change.name, continuous), func(t *testing.T) {
					s, f, _, r := newWorkingHistoryObservationFixture(t)
					want, err := queryWorkingHistoryObservation(context.Background(), s, op, "")
					if err != nil {
						t.Fatal(err)
					}
					r.reads, r.refReads = 0, 0
					docReads := 0
					f.onDoc = func() {
						if r.reads < 2 {
							t.Fatal("documents opened before two matching observations")
						}
						docReads++
						if continuous {
							r.snaps[3].Message = fmt.Sprintf("PRIVATE_DO_NOT_PRINT_%d", docReads)
						} else {
							change.apply(r.historyReader)
							// Change only hidden retained metadata, leaving public Revision
							// and all document hashes unchanged.
							f.onDoc = func() { docReads++ }
						}
					}
					got, err := queryWorkingHistoryObservation(context.Background(), s, op, "")
					if continuous {
						if !errors.Is(err, domain.ErrSelectionChanged) || r.reads != 9 || r.refReads != 9 || docReads != 6 {
							t.Fatalf("post-body instability escaped bounded retries: snapshots=%d refs=%d documents=%d err=%v", r.reads, r.refReads, docReads, err)
						}
					} else {
						if err != nil || r.reads != 6 || r.refReads != 6 || docReads != 4 {
							t.Fatalf("hidden post-body mutation did not rebuild diff: snapshots=%d refs=%d documents=%d err=%v", r.reads, r.refReads, docReads, err)
						}
						assertWorkingHistoryObservationPublicEqual(t, got, want)
					}
				})
			}
		}
	}
}

func TestWorkingHistoryObservationFallbackDiffPostBodyFence(t *testing.T) {
	for _, op := range []string{"diff", "diff_staged"} {
		t.Run(op, func(t *testing.T) {
			s, f, history, r := newWorkingHistoryObservationFixture(t)
			s.history = workingHistoryQueryOnly{HistoryQuery: history}
			f.onDoc = func() {
				f.onDoc = nil
				f.pending[0].Target = workingDoc(t, "session", 4).Hash
			}
			got, err := queryWorkingHistoryObservation(context.Background(), s, op, "")
			if err != nil || r.reads != 12 || r.refReads != 12 {
				t.Fatalf("fallback lost post-body state fence: snapshots=%d refs=%d err=%v", r.reads, r.refReads, err)
			}
			want, err := queryWorkingHistoryObservation(context.Background(), s, op, "")
			if err != nil {
				t.Fatal(err)
			}
			assertWorkingHistoryObservationPublicEqual(t, got, want)
		})
	}
}

type workingHistoryObserverStub struct {
	query   func(context.Context, inbound.HistoryQueryInput) (domain.HistoryQueryResult, error)
	observe func(context.Context, string) (domain.HistoryQueryResult, domain.ContentHash, error)
}

func (r workingHistoryObserverStub) QueryHistory(ctx context.Context, in inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
	return r.query(ctx, in)
}

func (r workingHistoryObserverStub) ObserveLocalHistory(ctx context.Context, cwd string) (domain.HistoryQueryResult, domain.ContentHash, error) {
	return r.observe(ctx, cwd)
}

func TestWorkingHistoryObservationRejectsInvalidHashAndObserverErrors(t *testing.T) {
	for _, op := range []string{"status", "diff", "diff_staged"} {
		for _, kind := range []string{"empty_hash", "malformed_hash", "error", "canceled", "server", "wrong_version", "foreign_repo", "position_changed"} {
			t.Run(op+"/"+kind, func(t *testing.T) {
				s, f := newWorkingReadFixture(t)
				calls := 0
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				readErr := errors.New("observer failed")
				var wantErr error
				s.history = workingHistoryObserverStub{
					query: func(context.Context, inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
						t.Fatal("observer rejection silently fell back to public history")
						return domain.HistoryQueryResult{}, nil
					},
					observe: func(gotCtx context.Context, cwd string) (domain.HistoryQueryResult, domain.ContentHash, error) {
						calls++
						if gotCtx != ctx || cwd != "fixture-cwd" {
							t.Fatal("observer lost context or cwd scope")
						}
						out, hash := f.history, domain.HashContent([]byte("full catalog"))
						switch kind {
						case "empty_hash":
							hash = ""
							wantErr = domain.ErrHashMismatch
						case "malformed_hash":
							hash = "sha256:malformed"
							wantErr = domain.ErrHashMismatch
						case "error":
							wantErr = readErr
							return out, hash, readErr
						case "canceled":
							cancel()
							wantErr = context.Canceled
							return out, hash, gotCtx.Err()
						case "server":
							out.ServerChecked = true
						case "wrong_version":
							out.Version++
						case "foreign_repo":
							out.Snapshots = slices.Clone(out.Snapshots)
							out.Snapshots[0].RepoID = string(domain.HashContent([]byte("foreign repo")))
							wantErr = domain.ErrHashMismatch
						case "position_changed":
							out.Position = domain.HashContent([]byte("other selection"))
							wantErr = domain.ErrSelectionChanged
						}
						return out, hash, nil
					},
				}
				if _, err := queryWorkingHistoryObservation(ctx, s, op, "fixture-cwd"); err == nil || (wantErr != nil && !errors.Is(err, wantErr)) || calls != 1 {
					t.Fatalf("invalid observer result accepted/retried: calls=%d err=%v want=%v", calls, err, wantErr)
				}
			})
		}
	}
}

func TestWorkingHistoryObservationCancellationBeforeRead(t *testing.T) {
	for _, op := range []string{"status", "diff", "diff_staged"} {
		t.Run(op, func(t *testing.T) {
			s, _, _, r := newWorkingHistoryObservationFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := queryWorkingHistoryObservation(ctx, s, op, ""); !errors.Is(err, context.Canceled) || r.reads != 0 || r.refReads != 0 {
				t.Fatalf("canceled working query scanned history: snapshots=%d refs=%d err=%v", r.reads, r.refReads, err)
			}
		})
	}
}

func TestWorkingHistoryObservationSelectedHeadAndFallbackParity(t *testing.T) {
	for _, kind := range []string{"detached", "rewound"} {
		for _, op := range []string{"status", "diff", "diff_staged"} {
			t.Run(kind+"/"+op, func(t *testing.T) {
				s, f, history, r := newWorkingHistoryObservationFixture(t)
				selected := f.position.Snapshot
				// A shared branch tip may differ from the worktree's selected HEAD.
				future := r.snaps[3].ID
				r.refs[1].Target = future
				r.refs[0].Symbolic = ""
				f.position.SharedTarget = future
				wantMode := "named"
				if kind == "detached" {
					f.position.Branch, f.position.BranchID = "", ""
					f.position.LocalBranch = "main"
					wantMode = "detached"
				} else {
					f.position.Rewound = true
				}
				got, err := queryWorkingHistoryObservation(context.Background(), s, op, "")
				wantReads := workingHistoryObservationReads(op)
				if err != nil || r.reads != wantReads || r.refReads != wantReads {
					t.Fatalf("selected HEAD observer scans: snapshots=%d refs=%d err=%v", r.reads, r.refReads, err)
				}
				var selection domain.WorkingSelection
				switch value := got.(type) {
				case domain.WorkingState:
					selection = value.Selection
				case domain.ContextDiff:
					selection = value.Selection
					if len(value.Changes) != 1 || value.Changes[0].State != "extended" {
						t.Fatal("selected HEAD comparison lost its stored baseline")
					}
				}
				if selection.ContextSnapshot != selected || selection.ContextSnapshot == future || selection.ContextMode != wantMode || selection.ContextBranchID != f.position.BranchID || !selection.CodeMatchesSelection {
					t.Fatal("working query followed shared branch tip or lost selection identity")
				}
				r.reads, r.refReads = 0, 0
				s.history = workingHistoryQueryOnly{HistoryQuery: history}
				want, err := queryWorkingHistoryObservation(context.Background(), s, op, "")
				if err != nil || r.reads != 2*wantReads || r.refReads != 2*wantReads {
					t.Fatalf("selected HEAD fallback scans: snapshots=%d refs=%d err=%v", r.reads, r.refReads, err)
				}
				assertWorkingHistoryObservationPublicEqual(t, got, want)
			})
		}
	}
}

func TestWorkingHistoryObservationFencesBranchIdentityAtSameSnapshot(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		for _, op := range []string{"status", "diff", "diff_staged"} {
			t.Run(fmt.Sprintf("%s/fallback=%t", op, fallback), func(t *testing.T) {
				s, f, history, r := newWorkingHistoryObservationFixture(t)
				before, err := queryWorkingHistoryObservation(context.Background(), s, op, "")
				if err != nil {
					t.Fatal(err)
				}
				selected := f.position.Snapshot
				r.reads, r.refReads = 0, 0
				scans := 1
				if fallback {
					s.history = workingHistoryQueryOnly{HistoryQuery: history}
					scans = 2
				}
				// Recreate the same logical branch at the same snapshot after the
				// first staging-position read. History and its catalog stay identical.
				r.onRead = func(n int) {
					if n == 1 {
						f.position.BranchID = "recreated-main"
					}
				}
				got, err := queryWorkingHistoryObservation(context.Background(), s, op, "")
				wantReads := scans * (workingHistoryObservationReads(op) + 1)
				if err != nil || r.reads != wantReads || r.refReads != wantReads || f.position.Snapshot != selected || reflect.DeepEqual(got, before) {
					t.Fatalf("same-snapshot branch identity escaped fence: snapshots=%d refs=%d want=%d err=%v", r.reads, r.refReads, wantReads, err)
				}
				r.onRead = nil
				want, err := queryWorkingHistoryObservation(context.Background(), s, op, "")
				if err != nil {
					t.Fatal(err)
				}
				assertWorkingHistoryObservationPublicEqual(t, got, want)
			})
		}
	}
}

func TestWorkingHistoryObservationPropagatesCatalogFailures(t *testing.T) {
	for _, op := range []string{"status", "diff", "diff_staged"} {
		for _, kind := range []string{"snapshot_error", "ref_error", "empty_ref_error", "foreign_retained", "foreign_ref", "empty_canceled_refs", "post_body_ref_error"} {
			t.Run(op+"/"+kind, func(t *testing.T) {
				if kind == "post_body_ref_error" && op == "status" {
					return
				}
				s, f, history, r := newWorkingHistoryObservationFixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				want := domain.ErrHashMismatch
				switch kind {
				case "snapshot_error":
					want = errors.New("snapshot catalog unavailable")
					r.err = want
				case "ref_error", "empty_ref_error":
					want = errors.New("ref catalog unavailable")
					r.refErr = want
					if kind == "empty_ref_error" {
						r.snaps, r.refs = nil, nil
					}
				case "foreign_retained":
					r.snaps[3].RepoID = string(domain.HashContent([]byte("foreign repo")))
				case "foreign_ref":
					r.refs[1].RepoID = string(domain.HashContent([]byte("foreign repo")))
				case "empty_canceled_refs":
					r.snaps, r.refs = nil, nil
					want = context.Canceled
					r.onRefs = cancel
				case "post_body_ref_error":
					want = errors.New("ref catalog became unavailable")
					f.onDoc = func() { r.refErr = want }
				}
				// Also prove the error cannot trigger a silent legacy-query fallback.
				s.history = workingHistoryObserverStub{
					query: func(context.Context, inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
						t.Fatal("failed observer silently fell back")
						return domain.HistoryQueryResult{}, nil
					},
					observe: history.ObserveLocalHistory,
				}
				if _, err := queryWorkingHistoryObservation(ctx, s, op, ""); !errors.Is(err, want) {
					t.Fatalf("working query swallowed catalog failure: err=%v want=%v", err, want)
				}
			})
		}
	}
}
