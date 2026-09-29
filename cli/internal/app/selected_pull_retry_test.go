package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type pullQueryFunc func(context.Context, string, domain.ContextSelection) (domain.ContextQueryView, error)

func (f pullQueryFunc) QueryContext(ctx context.Context, repo string, in domain.ContextSelection) (domain.ContextQueryView, error) {
	return f(ctx, repo, in)
}

func TestSelectedPullCurrentRetriesOnlyRevisionWithOriginalSelection(t *testing.T) {
	for _, at := range []string{"first memory", "context recheck", "memory recheck", "apply start", "apply recheck"} {
		t.Run(at, func(t *testing.T) {
			svc, st, r, m, g, original := selectedPullFixture(t)
			qc, mc := 0, 0
			advance := func() { r.view.Revision.Evidence++; m.rev = r.view.Revision }
			svc.query = pullQueryFunc(func(ctx context.Context, repo string, in domain.ContextSelection) (domain.ContextQueryView, error) {
				qc++
				if at == "context recheck" && qc == 2 || at == "apply start" && qc == 3 || at == "apply recheck" && qc == 4 {
					advance()
				}
				return r.QueryContext(ctx, repo, in)
			})
			svc.memory = promptReadFunc(func(ctx context.Context, repo string, in domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				mc++
				if at == "first memory" && mc == 1 || at == "memory recheck" && mc == 2 {
					advance()
				}
				page, err := m.QueryEffectiveMemory(ctx, repo, in)
				page.StateHash = agentHash(fmt.Sprint("memory revision ", page.Revision.Evidence))
				return page, err
			})
			got, err := svc.ApplyCurrent(context.Background(), SelectedPullInput{Cwd: g.repo.LocalPath})
			if err != nil {
				t.Fatal(err)
			}
			if got.Plan.Context.Revision.Evidence != 1 || !reflect.DeepEqual(got.Plan.Expected, original.Expected) || got.Plan.Selection != original.Selection {
				t.Fatalf("moved selection or stale receipt: %+v", got.Plan)
			}
			if qc < 5 || qc > 8 || mc < 5 || mc > 8 {
				t.Fatalf("missing fresh reads: query=%d memory=%d", qc, mc)
			}
			stored, err := st.ReadAppliedPull(context.Background(), original.RepoID, original.Remote)
			if err != nil || stored.Plan.ID != got.Plan.ID {
				t.Fatalf("receipt not committed: %v", err)
			}
		})
	}
}

func TestSelectedPullExplicitApplyDoesNotReplacePreviewRevision(t *testing.T) {
	svc, st, r, m, g, plan := selectedPullFixture(t)
	r.view.Revision.Evidence++
	m.rev = r.view.Revision
	_, err := svc.Apply(context.Background(), g.repo.LocalPath, plan)
	if !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatalf("replaced explicit plan: %v", err)
	}
	if _, err := st.ReadAppliedPull(context.Background(), plan.RepoID, plan.Remote); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("wrote stale receipt: %v", err)
	}
}

func TestSelectedPullRetryStopsOnChangedOrInvalidSource(t *testing.T) {
	for _, kind := range []string{"context", "memory", "metadata", "code", "branch", "checkout", "malformed", "context denied", "memory denied", "cancel", "continuous"} {
		t.Run(kind, func(t *testing.T) {
			svc, st, r, m, g, plan := selectedPullFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			denied := errors.New("permission revoked")
			qc, mc := 0, 0
			// The first memory read encounters only a newer server revision.
			svc.query = pullQueryFunc(func(ctx context.Context, repo string, in domain.ContextSelection) (domain.ContextQueryView, error) {
				qc++
				if qc == 2 {
					switch kind {
					case "context":
						r.view.StateHash = agentHash("new content")
					case "metadata":
						r.view.Snapshots[0].Message = "changed under same hash"
					case "context denied":
						return domain.ContextQueryView{}, denied
					}
				}
				return r.QueryContext(ctx, repo, in)
			})
			svc.memory = promptReadFunc(func(ctx context.Context, repo string, in domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				mc++
				if mc == 1 || kind == "continuous" {
					r.view.Revision.Evidence++
					m.rev = r.view.Revision
				}
				if mc == 1 {
					switch kind {
					case "code":
						g.sha = strings.Repeat("b", 40)
					case "branch":
						g.branch = "other"
					case "checkout":
						p := *plan.Expected.Position
						p.Rewound = true
						if err := st.PutWorkingPosition(ctx, p); err != nil {
							t.Fatal(err)
						}
					case "cancel":
						cancel()
					}
				}
				if mc == 2 && kind == "memory denied" {
					return domain.EffectiveMemoryPage{}, denied
				}
				page, err := m.QueryEffectiveMemory(ctx, repo, in)
				if mc == 1 && kind == "malformed" {
					page.Total = 1
				} // claims empty and no cursor, even with revision drift
				if mc == 2 && kind == "memory" {
					page.LineageHash = agentHash("new memory")
				}
				return page, err
			})
			_, err := svc.ApplyCurrent(ctx, SelectedPullInput{Cwd: g.repo.LocalPath})
			want := domain.ErrSelectionChanged
			switch kind {
			case "malformed":
				want = domain.ErrHashMismatch
			case "context denied", "memory denied":
				want = denied
			case "cancel":
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("%s: got %v, want %v", kind, err, want)
			}
			if qc > 3 || mc > 3 {
				t.Fatalf("unbounded reads %d/%d", qc, mc)
			}
			if kind == "malformed" && qc != 1 {
				t.Fatal("retried malformed page")
			}
			if _, err := st.ReadAppliedPull(context.Background(), plan.RepoID, plan.Remote); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("published rejected receipt: %v", err)
			}
		})
	}
}

func TestSelectedPullRetryRereadsAllPagesAndReauthorizes(t *testing.T) {
	for _, denyRead := range []int{0, 1, 2, 3, 4} {
		t.Run(fmt.Sprint(denyRead), func(t *testing.T) {
			svc, st, r, m, g, plan := selectedPullFixture(t)
			items := make([]domain.EffectiveMemoryItem, 51)
			for i := range items {
				items[i] = domain.EffectiveMemoryItem{ID: agentHash(fmt.Sprint(i)), SourceSnapshot: r.view.Position, Kind: "decision", Text: fmt.Sprintf("condition %d", i), State: "retained", Reason: "project_decision"}
			}
			reads, firstPages, attempt := 0, 0, 1
			denied := errors.New("revoked on retry")
			svc.memory = promptReadFunc(func(ctx context.Context, repo string, in domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				if attempt == 2 {
					reads++
					if reads == denyRead {
						return domain.EffectiveMemoryPage{}, denied
					}
				}
				page, err := m.QueryEffectiveMemory(ctx, repo, in)
				page.Total = len(items)
				page.StateHash = agentHash(fmt.Sprint("memory revision ", page.Revision.Evidence))
				if in.Cursor == "" {
					firstPages++
					page.Items = items[:50]
					page.NextCursor = fmt.Sprint("page2-", r.view.Revision.Evidence)
				} else {
					expected := fmt.Sprint("page2-", r.view.Revision.Evidence)
					if in.Cursor != expected {
						t.Fatalf("reused old cursor %q, want %q", in.Cursor, expected)
					}
					page.Items = items[50:]
					page.NextCursor = ""
				}
				if attempt == 1 && firstPages == 2 && in.Cursor == "" {
					r.view.Revision.Evidence++
					m.rev = r.view.Revision
					page.Revision.Evidence = m.rev.Evidence
					page.StateHash = agentHash(fmt.Sprint("memory revision ", page.Revision.Evidence))
					page.NextCursor = fmt.Sprint("page2-", r.view.Revision.Evidence)
					attempt = 2
				}
				return page, err
			})
			got, err := svc.ApplyCurrent(context.Background(), SelectedPullInput{Cwd: g.repo.LocalPath})
			if denyRead > 0 {
				if !errors.Is(err, denied) {
					t.Fatalf("revocation %d: %v", denyRead, err)
				}
				if _, err := st.ReadAppliedPull(context.Background(), plan.RepoID, plan.Remote); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("wrote unauthorized receipt")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Plan.Memory) != 2 || len(got.Plan.Memory[0].Items) != 50 || len(got.Plan.Memory[1].Items) != 1 || got.Plan.Memory[0].Revision.Evidence != 1 {
				t.Fatal("mixed pages or revisions")
			}
			if reads != 6 {
				t.Fatalf("did not reread every page and recheck first page in Preview and Apply: %d", reads)
			}
		})
	}
}

// These failures originate outside the server read loop and must never be
// mistaken for evidence-revision contention, even with the same public code.
type pullCommitFailure struct {
	outbound.SelectedPullStore
	calls int
}

func (s *pullCommitFailure) ApplySelectedPull(context.Context, outbound.SelectedPullPlan) (outbound.SelectedPullReceipt, error) {
	s.calls++
	return outbound.SelectedPullReceipt{}, fmt.Errorf("lost local CAS: %w", domain.ErrSelectionChanged)
}
func TestSelectedPullCurrentDoesNotRetryStorageCAS(t *testing.T) {
	svc, st, _, _, g, plan := selectedPullFixture(t)
	failing := &pullCommitFailure{SelectedPullStore: st}
	svc.store = failing
	_, err := svc.ApplyCurrent(context.Background(), SelectedPullInput{Cwd: g.repo.LocalPath})
	if !errors.Is(err, domain.ErrSelectionChanged) || failing.calls != 1 {
		t.Fatalf("retried failed local apply %d: %v", failing.calls, err)
	}
	if _, err := st.ReadAppliedPull(context.Background(), plan.RepoID, plan.Remote); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("receipt published")
	}
}
func TestSelectedPullCurrentKeepsOriginalReceiptAndIndex(t *testing.T) {
	for _, kind := range []string{"receipt", "index"} {
		t.Run(kind, func(t *testing.T) {
			svc, st, r, m, g, plan := selectedPullFixture(t)
			calls := 0
			svc.memory = promptReadFunc(func(ctx context.Context, repo string, in domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				calls++
				if calls == 1 {
					r.view.Revision.Evidence++
					m.rev = r.view.Revision
					if kind == "receipt" {
						if _, err := st.ApplySelectedPull(ctx, plan); err != nil {
							t.Fatal(err)
						}
					} else {
						index, position, err := st.ReadStaging(ctx, plan.RepoID)
						if err != nil {
							t.Fatal(err)
						}
						before := index.Revision
						index.Sequence++
						index = index.WithRevision()
						if err := st.CompareAndSwapStaging(ctx, before, index, position); err != nil {
							t.Fatal(err)
						}
					}
				}
				return m.QueryEffectiveMemory(ctx, repo, in)
			})
			_, err := svc.ApplyCurrent(context.Background(), SelectedPullInput{Cwd: g.repo.LocalPath})
			if !errors.Is(err, domain.ErrSelectionChanged) {
				t.Fatalf("adopted different local anchor: %v", err)
			}
			receipt, err := st.ReadAppliedPull(context.Background(), plan.RepoID, plan.Remote)
			if kind == "receipt" {
				if err != nil || receipt.Plan.ID != plan.ID {
					t.Fatal("overwrote competing receipt")
				}
			} else if !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("wrote receipt for changed index")
			}
		})
	}
}

func TestSelectedPullCursorIdentityCannotChangeWithinRevision(t *testing.T) {
	svc, st, r, m, g, plan := selectedPullFixture(t)
	reads := 0
	svc.memory = promptReadFunc(func(ctx context.Context, repo string, in domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
		reads++
		if reads == 1 {
			r.view.Revision.Evidence++
			m.rev = r.view.Revision
		}
		page, err := m.QueryEffectiveMemory(ctx, repo, in)
		page.Items = []domain.EffectiveMemoryItem{{ID: agentHash("first"), SourceSnapshot: r.view.Position, Kind: "decision", Text: "first page", State: "retained", Reason: "project_decision"}}
		page.Total = 2
		page.NextCursor = fmt.Sprint("cursor-", reads)
		return page, err
	})
	_, err := svc.ApplyCurrent(context.Background(), SelectedPullInput{Cwd: g.repo.LocalPath})
	if !errors.Is(err, domain.ErrSelectionChanged) || reads != 2 {
		t.Fatalf("accepted renamed cursor in same revision: reads=%d err=%v", reads, err)
	}
	if _, err := st.ReadAppliedPull(context.Background(), plan.RepoID, plan.Remote); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("receipt published")
	}
}

func TestSelectedPullStaleCursorRestartsOnlyContinuation(t *testing.T) {
	for _, kind := range []string{"stale", "generic", "initial"} {
		t.Run(kind, func(t *testing.T) {
			svc, _, r, m, g, _ := selectedPullFixture(t)
			qc, mc := 0, 0
			stale := false
			conflict := errors.New("generic conflict")
			svc.query = pullQueryFunc(func(ctx context.Context, repo string, in domain.ContextSelection) (domain.ContextQueryView, error) {
				qc++
				return r.QueryContext(ctx, repo, in)
			})
			svc.memory = promptReadFunc(func(ctx context.Context, repo string, in domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				mc++
				if kind == "initial" {
					return domain.EffectiveMemoryPage{}, domain.ErrEffectiveMemoryCursorStale
				}
				if in.Cursor != "" && !stale {
					if kind == "generic" {
						return domain.EffectiveMemoryPage{}, conflict
					}
					stale = true
					r.view.Revision.Evidence++
					m.rev = r.view.Revision
					return domain.EffectiveMemoryPage{}, domain.ErrEffectiveMemoryCursorStale
				}
				page, err := m.QueryEffectiveMemory(ctx, repo, in)
				index := 0
				if in.Cursor != "" {
					index = 1
				}
				page.Items = []domain.EffectiveMemoryItem{{ID: agentHash(fmt.Sprint(index)), SourceSnapshot: r.view.Position, Kind: "decision", Text: fmt.Sprint("page ", index), State: "retained", Reason: "project_decision"}}
				page.Total = 2
				page.StateHash = agentHash(fmt.Sprint("rev", page.Revision.Evidence))
				if in.Cursor == "" {
					page.NextCursor = fmt.Sprint("cursor-", page.Revision.Evidence)
				} else if in.Cursor != fmt.Sprint("cursor-", page.Revision.Evidence) {
					t.Fatal("reused expired cursor")
				}
				return page, err
			})
			got, err := svc.ApplyCurrent(context.Background(), SelectedPullInput{Cwd: g.repo.LocalPath})
			switch kind {
			case "stale":
				if err != nil || len(got.Plan.Memory) != 2 || got.Plan.Context.Revision.Evidence != 1 || qc != 5 || mc != 8 {
					t.Fatalf("did not fully restart: %v query=%d memory=%d", err, qc, mc)
				}
			case "generic":
				if !errors.Is(err, conflict) || qc != 1 || mc != 2 {
					t.Fatalf("retried generic conflict: %v %d/%d", err, qc, mc)
				}
			case "initial":
				if !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) || qc != 1 || mc != 1 {
					t.Fatalf("retried initial cursor error: %v %d/%d", err, qc, mc)
				}
			}
		})
	}
}
