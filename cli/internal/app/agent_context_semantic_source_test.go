package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func semanticAgentFixture(t *testing.T) (*AgentContextService, inbound.PrepareAgentContextInput, *agentHistoryFixture, *agentMemoryFixture) {
	t.Helper()
	s, in, h, m, _ := agentServiceFixture(t)
	in.LatestMain = true
	in.ArtifactOnly = true
	in.WorktreeStateHash = agentHash("working selection")
	in.WorkingPosition = &domain.AgentWorkingPosition{Branch: "feature", CodeCommit: strings.Repeat("b", 40)}
	h.view.DeliveryStateHash = agentHash("context semantic")
	s.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
		p, err := m.QueryEffectiveMemory(ctx, repo, req)
		p.DeliveryStateHash = agentHash("memory semantic")
		return p, err
	})
	return s, in, h, m
}

func TestAgentSemanticSourceAcceptsPriorUnrelatedGeneration(t *testing.T) {
	for _, change := range []string{"graph", "evidence", "branch membership", "pending"} {
		t.Run(change, func(t *testing.T) {
			s, in, h, m := semanticAgentFixture(t)
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if p.Content.Selection.ContextDeliveryHash == "" || p.Content.Selection.MemoryDeliveryHash == "" || p.ValidateIdentity() != nil {
				t.Fatal("proof pair not bound")
			}
			rev := *h.view.Revision
			switch change {
			case "graph", "branch membership":
				rev.Graph++
			case "evidence":
				rev.Evidence++
			case "pending":
				rev.Pending++
			}
			h.view.Revision, m.rev = &rev, rev
			if change == "branch membership" {
				// Display-only backend memberships are not part of the CLI's
				// Snapshot DTO, but do change the legacy context state hash.
				h.view.StateHash = agentHash("new display membership")
			}
			memory := s.memory
			s.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				v, e := memory.QueryEffectiveMemory(ctx, repo, req)
				if change != "pending" {
					v.StateHash = agentHash("new pagination generation")
				}
				return v, e
			})
			hc, mc := h.calls, m.calls
			if err := s.ValidateLatestMain(context.Background(), in.Cwd, p.Content.Selection); err != nil {
				t.Fatal(err)
			}
			if h.calls-hc != 2 || m.calls-mc != 1 {
				t.Fatal("did not reauthorize history-memory-history")
			}
		})
	}
}

func TestHistoryQueryPropagatesDeliveryProof(t *testing.T) {
	repo := string(agentHash("repository"))
	git := historyServerTipGit{historyGit{repo}}
	remote := &historyServerTipRemote{historyRemote: historyRemote{view: historyServerTipView(repo, "main")}}
	remote.view.DeliveryStateHash = agentHash("full context proof")
	s := NewHistoryQueryService(git, nil, nil, remote)
	v, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Server: true, ServerTip: true, Branch: "main"})
	if err != nil || v.DeliveryStateHash != remote.view.DeliveryStateHash {
		t.Fatal("server proof lost", err)
	}
}

func TestAgentSemanticSourceRejectsDriftAndProofDowngrade(t *testing.T) {
	for _, change := range []string{"main snapshot", "main code", "main branch", "context proof", "context missing", "context malformed", "memory proof", "memory missing", "memory malformed", "memory selection", "revoked history", "revoked memory", "partial package", "malformed package", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			s, in, h, _ := semanticAgentFixture(t)
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			secret := "private-source-body"
			memory := s.memory
			s.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				v, e := memory.QueryEffectiveMemory(ctx, repo, req)
				switch change {
				case "memory proof":
					v.DeliveryStateHash = agentHash(secret)
				case "memory missing":
					v.DeliveryStateHash = ""
				case "memory malformed":
					v.DeliveryStateHash = domain.ContentHash(secret)
				case "memory selection":
					v.Selection.CodeCommit = strings.Repeat("c", 40)
				case "revoked memory":
					return v, errors.New("permission revoked")
				}
				return v, e
			})
			switch change {
			case "main snapshot":
				h.view.Position = agentHash(secret)
			case "main code":
				h.view.Selection.CodeCommit = strings.Repeat("c", 40)
			case "main branch":
				h.view.Selection.Branch = "other"
			case "context proof":
				h.view.DeliveryStateHash = agentHash(secret)
			case "context missing":
				h.view.DeliveryStateHash = ""
			case "context malformed":
				h.view.DeliveryStateHash = domain.ContentHash(secret)
			case "revoked history":
				s.history = retryHistoryFunc(func(context.Context, inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
					return domain.HistoryQueryResult{}, errors.New("permission revoked")
				})
			case "partial package":
				p.Content.Selection.MemoryDeliveryHash = ""
			case "malformed package":
				p.Content.Selection.ContextDeliveryHash = domain.ContentHash(secret)
			case "cancelled":
				cancel()
			}
			err = s.ValidateLatestMain(ctx, in.Cwd, p.Content.Selection)
			if err == nil {
				t.Fatal("changed or unproven source accepted")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), string(agentHash(secret))) {
				t.Fatal("private proof leaked")
			}
		})
	}
}

func TestAgentSemanticSourceRejectsWritesBetweenCurrentReads(t *testing.T) {
	for _, stage := range []string{"memory", "last history"} {
		for _, counter := range []string{"graph", "evidence"} {
			t.Run(stage+"/"+counter, func(t *testing.T) {
				s, in, h, m := semanticAgentFixture(t)
				p, err := s.PrepareAgentContext(context.Background(), in)
				if err != nil {
					t.Fatal(err)
				}
				// Continuous writes must exhaust the bounded read retry; no
				// mixed-generation set becomes valid just because proofs match.
				rev := *h.view.Revision
				rev.Graph += 10
				h.view.Revision, m.rev = &rev, rev
				advance := func() {
					next := *h.view.Revision
					if counter == "graph" {
						next.Graph++
					} else {
						next.Evidence++
					}
					h.view.Revision, m.rev = &next, next
				}
				historyCalls := 0
				s.history = retryHistoryFunc(func(ctx context.Context, in inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
					historyCalls++
					if stage == "last history" && historyCalls%2 == 0 {
						advance()
					}
					return h.QueryHistory(ctx, in)
				})
				memory := s.memory
				s.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
					if stage == "memory" {
						advance()
					}
					return memory.QueryEffectiveMemory(ctx, repo, req)
				})
				err = s.ValidateLatestMain(context.Background(), in.Cwd, p.Content.Selection)
				if !errors.Is(err, domain.ErrSelectionChanged) || !strings.Contains(err.Error(), "current source reads changed") || !strings.Contains(err.Error(), counter+"_revision=") {
					t.Fatal("mixed current generation accepted or opaque", err)
				}
				want := 3
				if stage == "last history" {
					want = 6
				}
				if historyCalls != want {
					t.Fatalf("read retry not bounded: %d calls, want %d", historyCalls, want)
				}
			})
		}
	}
}

func TestAgentSemanticSourceRetriesConcurrentUnrelatedWrite(t *testing.T) {
	for _, stage := range []string{"memory", "last history"} {
		for _, change := range []string{"unrelated", "history changed", "memory changed", "permission revoked", "cancelled"} {
			t.Run(stage+"/"+change, func(t *testing.T) {
				s, in, h, m := semanticAgentFixture(t)
				p, err := s.PrepareAgentContext(context.Background(), in)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				advanced := false
				advance := func() {
					rev := *h.view.Revision
					rev.Evidence++
					h.view.Revision, m.rev = &rev, rev
					advanced = true
				}
				hc, mc := 0, 0
				s.history = retryHistoryFunc(func(ctx context.Context, query inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
					hc++
					if advanced {
						switch change {
						case "history changed":
							h.view.DeliveryStateHash = agentHash("changed selected content")
						case "permission revoked":
							return domain.HistoryQueryResult{}, errors.New("permission revoked")
						case "cancelled":
							cancel()
						}
					}
					if stage == "last history" && hc == 2 {
						advance()
					}
					return h.QueryHistory(ctx, query)
				})
				memory := s.memory
				s.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
					mc++
					if stage == "memory" && mc == 1 {
						advance()
					}
					v, err := memory.QueryEffectiveMemory(ctx, repo, req)
					if mc > 1 && change == "memory changed" {
						v.DeliveryStateHash = agentHash("changed selected memory")
					}
					return v, err
				})
				err = s.ValidateLatestMain(ctx, in.Cwd, p.Content.Selection)
				if change == "unrelated" {
					if err != nil || mc != 2 {
						t.Fatal("stable authorized retry failed", err)
					}
				} else if err == nil {
					t.Fatal("retry accepted source drift or lost authorization")
				}
				if hc > 4 || mc > 2 {
					t.Fatal("retried a changed source or failed authorization")
				}
			})
		}
	}
}

func TestAgentSemanticPreparationPartialSupportRemainsLegacy(t *testing.T) {
	for _, support := range []string{"neither", "context only", "memory only"} {
		t.Run(support, func(t *testing.T) {
			s, in, h, m := semanticAgentFixture(t)
			if support != "context only" {
				h.view.DeliveryStateHash = ""
			}
			if support != "memory only" {
				s.memory = m
			}
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if p.Content.Selection.ContextDeliveryHash != "" || p.Content.Selection.MemoryDeliveryHash != "" {
				t.Fatal("partial evidence escaped")
			}
			if err = s.ValidateLatestMain(context.Background(), in.Cwd, p.Content.Selection); err != nil {
				t.Fatal(err)
			}
			rev := *h.view.Revision
			rev.Graph++
			h.view.Revision, m.rev = &rev, rev
			if err = s.ValidateLatestMain(context.Background(), in.Cwd, p.Content.Selection); !errors.Is(err, domain.ErrSelectionChanged) {
				t.Fatal("legacy package upgraded itself", err)
			}
		})
	}
}

func TestAgentSemanticPreparationRetriesWithoutMixingPages(t *testing.T) {
	for _, failure := range []string{"none", "context changed", "context missing", "context malformed", "memory changed", "memory missing", "memory malformed", "second page proof"} {
		t.Run(failure, func(t *testing.T) {
			s, in, h, m := semanticAgentFixture(t)
			calls := 0
			s.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				calls++
				if calls == 2 && failure != "second page proof" {
					if req.Cursor != "cursor-4" {
						t.Fatal("wrong first continuation")
					}
					rev := *h.view.Revision
					rev.Evidence++
					h.view.Revision, m.rev = &rev, rev
					h.view.StateHash = agentHash("new branch membership generation")
					switch failure {
					case "context changed":
						h.view.DeliveryStateHash = agentHash("different context")
					case "context missing":
						h.view.DeliveryStateHash = ""
					case "context malformed":
						h.view.DeliveryStateHash = "invalid"
					}
					return domain.EffectiveMemoryPage{}, domain.ErrEffectiveMemoryCursorStale
				}
				v, err := m.QueryEffectiveMemory(ctx, repo, req)
				v.DeliveryStateHash = agentHash("memory semantic")
				v.StateHash = agentHash(fmt.Sprint("generation", m.rev.Evidence))
				v.Total = 2
				index := 0
				if req.Cursor == "" {
					v.NextCursor = fmt.Sprint("cursor-", m.rev.Evidence)
				} else {
					if req.Cursor != fmt.Sprint("cursor-", m.rev.Evidence) {
						t.Fatal("stale cursor reused")
					}
					index = 1
				}
				v.Items = []domain.EffectiveMemoryItem{{ID: agentHash(fmt.Sprint("item", index)), SourceSnapshot: h.view.Position, Kind: "decision", Text: fmt.Sprint("selected decision ", index), State: "retained", Reason: "project_decision"}}
				if calls >= 3 || failure == "second page proof" && calls == 2 {
					switch failure {
					case "memory changed", "second page proof":
						v.DeliveryStateHash = agentHash("changed omitted content")
					case "memory missing":
						v.DeliveryStateHash = ""
					case "memory malformed":
						v.DeliveryStateHash = "invalid"
					}
				}
				return v, err
			})
			p, err := s.PrepareAgentContext(context.Background(), in)
			if failure != "none" {
				if err == nil || p.ID != "" {
					t.Fatal("proof drift accepted")
				}
				if calls > 3 {
					t.Fatal("retried observed semantic drift", calls)
				}
				return
			}
			if err != nil || p.Content.Selection.EvidenceRevision != 5 || len(p.Content.ProjectMemory) != 2 || calls != 5 || h.calls != 3 {
				t.Fatalf("retry mixed generations: %v history=%d memory=%d", err, h.calls, calls)
			}
			if p.Content.Selection.ContextDeliveryHash != h.view.DeliveryStateHash || p.Content.Selection.MemoryDeliveryHash != agentHash("memory semantic") {
				t.Fatal("proofs not retained")
			}
		})
	}
}

func TestAgentSemanticPreparationRetriesRevisionWithOriginalProofs(t *testing.T) {
	for _, at := range []string{"first memory page", "context revalidation", "memory revalidation"} {
		t.Run(at, func(t *testing.T) {
			s, in, h, m := semanticAgentFixture(t)
			advance := func() {
				rev := *h.view.Revision
				rev.Evidence++
				h.view.Revision, m.rev = &rev, rev
				h.view.StateHash = agentHash("new display membership generation")
			}
			historyCalls, memoryCalls := 0, 0
			s.history = retryHistoryFunc(func(ctx context.Context, in inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
				historyCalls++
				if at == "context revalidation" && historyCalls == 2 {
					advance()
				}
				return h.QueryHistory(ctx, in)
			})
			memory := s.memory
			s.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				memoryCalls++
				if at == "first memory page" && memoryCalls == 1 || at == "memory revalidation" && memoryCalls == 2 {
					advance()
				}
				v, err := memory.QueryEffectiveMemory(ctx, repo, req)
				v.StateHash = agentHash(fmt.Sprint("memory generation", m.rev.Evidence))
				return v, err
			})
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if p.Content.Selection.EvidenceRevision != 5 || p.Content.Selection.ContextStateHash != h.view.StateHash || p.Content.Selection.MemoryStateHash != agentHash("memory generation5") || p.Content.Selection.ContextDeliveryHash != h.view.DeliveryStateHash || p.Content.Selection.MemoryDeliveryHash != agentHash("memory semantic") || p.ValidateIdentity() != nil {
				t.Fatal("package combined old and new generations")
			}
			if historyCalls < 3 || historyCalls > 4 || memoryCalls < 3 || memoryCalls > 4 {
				t.Fatalf("did not bound and reauthorize retry: history=%d memory=%d", historyCalls, memoryCalls)
			}
		})
	}
}

func TestAgentSemanticPreparationSameRevisionStateDriftIsTerminal(t *testing.T) {
	s, in, h, m := semanticAgentFixture(t)
	h.after = func(v *domain.HistoryQueryResult) { v.StateHash = agentHash("different generation without revision") }
	p, err := s.PrepareAgentContext(context.Background(), in)
	if !errors.Is(err, domain.ErrSelectionChanged) || p.ID != "" || h.calls != 2 || m.calls != 1 {
		t.Fatalf("same revision state drift retried: %v history=%d memory=%d", err, h.calls, m.calls)
	}
}

func TestAgentSemanticSourceFinalReadStillChecksSourceAndCancellation(t *testing.T) {
	for _, change := range []string{"pending", "context proof", "main code", "missing proof", "revoked", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			s, in, h, _ := semanticAgentFixture(t)
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			s.history = retryHistoryFunc(func(ctx context.Context, in inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
				calls++
				v, err := h.QueryHistory(ctx, in)
				if calls == 2 {
					switch change {
					case "pending":
						rev := *v.Revision
						rev.Pending++
						v.Revision = &rev
					case "context proof":
						v.DeliveryStateHash = agentHash("changed context")
					case "main code":
						v.Selection.CodeCommit = strings.Repeat("c", 40)
					case "missing proof":
						v.DeliveryStateHash = ""
					case "revoked":
						return v, errors.New("permission revoked")
					case "cancelled":
						// Even a reader that returns success after cancellation
						// cannot authorize release.
						cancel()
					}
				}
				return v, err
			})
			err = s.ValidateLatestMain(ctx, in.Cwd, p.Content.Selection)
			if calls != 2 || (err == nil) != (change == "pending") {
				t.Fatal("wrong final-read result", calls, err)
			}
			if change == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("lost cancellation", err)
			}
		})
	}
}
