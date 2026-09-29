package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type retryHistoryFunc func(context.Context, inbound.HistoryQueryInput) (domain.HistoryQueryResult, error)

func (f retryHistoryFunc) QueryHistory(ctx context.Context, in inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
	return f(ctx, in)
}

func TestAgentPreparationRetriesRevisionWithOriginalSelection(t *testing.T) {
	for _, at := range []string{"first memory page", "context revalidation", "memory revalidation"} {
		t.Run(at, func(t *testing.T) {
			s, in, h, m, _ := agentServiceFixture(t)
			calls, memoryCalls := 0, 0
			advance := func() {
				rev := *h.view.Revision
				rev.Evidence++
				h.view.Revision = &rev
				m.rev = rev
			}
			s.history = retryHistoryFunc(func(ctx context.Context, _ inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
				calls++
				if at == "context revalidation" && calls == 2 {
					advance()
				}
				return h.view, ctx.Err()
			})
			s.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				memoryCalls++
				if at == "first memory page" && memoryCalls == 1 || at == "memory revalidation" && memoryCalls == 2 {
					advance()
				}
				return m.QueryEffectiveMemory(ctx, repo, req)
			})
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if p.Content.Selection.SnapshotID != in.SnapshotID || p.Content.Selection.EvidenceRevision != 5 || p.Content.Selection.CodeCommit != h.view.Selection.CodeCommit {
				t.Fatalf("wrong pinned selection: %+v", p.Content.Selection)
			}
			if calls < 3 || memoryCalls < 3 || calls > 4 || memoryCalls > 4 {
				t.Fatalf("preparation did not reread and reauthorize: history=%d memory=%d", calls, memoryCalls)
			}
		})
	}
}

func TestAgentPreparationRetryNeverFollowsChangedSelection(t *testing.T) {
	for _, field := range []string{"position", "branch", "code", "content"} {
		t.Run(field, func(t *testing.T) {
			s, in, h, m, _ := agentServiceFixture(t)
			m.rev.Evidence++ // Force revision contention on the first attempt.
			calls := 0
			s.history = retryHistoryFunc(func(ctx context.Context, _ inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
				calls++
				v := h.view
				if calls == 2 {
					switch field {
					case "position":
						v.Position = agentHash("other source")
					case "branch":
						v.Selection.Branch = "other"
					case "code":
						v.Selection.CodeCommit = strings.Repeat("b", 40)
					case "content":
						v.StateHash = agentHash("changed content")
					}
				}
				return v, ctx.Err()
			})
			p, err := s.PrepareAgentContext(context.Background(), in)
			if !errors.Is(err, domain.ErrSelectionChanged) || p.ID != "" || calls != 2 || m.calls != 1 {
				t.Fatalf("followed changed selection: %v, history=%d memory=%d", err, calls, m.calls)
			}
		})
	}
}

func TestAgentPreparationRevisionRetryIsBoundedAndReauthorizes(t *testing.T) {
	denied := errors.New("permission revoked")
	for _, stop := range []string{"continuous contention", "revoked", "cancelled", "corrupt"} {
		t.Run(stop, func(t *testing.T) {
			s, in, h, m, _ := agentServiceFixture(t)
			m.rev.Evidence++
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			s.history = retryHistoryFunc(func(ctx context.Context, _ inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
				calls++
				if calls == 2 && stop == "revoked" {
					return domain.HistoryQueryResult{}, denied
				}
				if calls == 2 && stop == "corrupt" {
					return domain.HistoryQueryResult{}, domain.ErrHashMismatch
				}
				return h.view, ctx.Err()
			})
			if stop == "cancelled" {
				s.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
					page, err := m.QueryEffectiveMemory(ctx, repo, req)
					cancel()
					return page, err
				})
			}
			p, err := s.PrepareAgentContext(ctx, in)
			wantErr, wantCalls := domain.ErrSelectionChanged, 3
			switch stop {
			case "revoked":
				wantErr, wantCalls = denied, 2
			case "cancelled":
				wantErr, wantCalls = context.Canceled, 1
			case "corrupt":
				wantErr, wantCalls = domain.ErrHashMismatch, 2
			}
			if !errors.Is(err, wantErr) || p.ID != "" || calls != wantCalls {
				t.Fatalf("unsafe retry: %v, history=%d", err, calls)
			}
		})
	}
}

func TestAgentPreparationRetryRereadsAllPagedHistoryWithoutLeakingState(t *testing.T) {
	var events []domain.Event
	for i := 0; i < 40; i++ {
		event := agentMessage("user", fmt.Sprintf("turn-%d", i), i)
		event.ID = fmt.Sprintf("event-%d", i)
		events = append(events, event)
	}
	// Exercise body cursors, overlapping native event IDs, and prefix proofs.
	latest := agentDocument(t, "native", events[10:]...)
	older := agentDocument(t, "native", events[:20]...)
	prefix := agentDocument(t, "native", events[:10]...)
	s, in, pages, history := preparePagedHistoryFixture(t, latest, older, prefix)
	memory := s.memory.(*agentMemoryFixture)
	historyCalls, memoryCalls := 0, 0
	var requests []domain.EffectiveMemoryRequest
	s.history = retryHistoryFunc(func(ctx context.Context, query inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
		historyCalls++
		if query.Position != in.SnapshotID || query.Branch != in.Branch || !query.Server {
			t.Fatalf("retry changed the input selection: %+v", query)
		}
		if historyCalls == 2 {
			// Copy the revision: mutating the shared pointer would also rewrite
			// the first read and hide the very race this test exercises.
			revision := *history.view.Revision
			revision.Evidence++
			history.view.Revision = &revision
			memory.rev = revision
		}
		return history.view, ctx.Err()
	})
	s.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
		memoryCalls++
		requests = append(requests, req)
		page, err := memory.QueryEffectiveMemory(ctx, repo, req)
		page.Items = append([]domain.EffectiveMemoryItem(nil), page.Items...)
		page.Total = 2
		if req.Cursor == "" {
			page.NextCursor = "second-page"
		} else if req.Cursor == "second-page" {
			page.Items[0].ID = agentHash("second memory item")
			page.Items[0].Text = "Keep the second constraint"
		} else {
			t.Fatalf("unexpected memory cursor: %q", req.Cursor)
		}
		return page, err
	})
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if historyCalls != 4 || memoryCalls != 5 || pages.calls != 14 || pages.readEvents != 100 {
		t.Fatalf("incomplete reread: history=%d memory=%d pages=%d events=%d", historyCalls, memoryCalls, pages.calls, pages.readEvents)
	}
	if !reflect.DeepEqual(pages.requests[:7], pages.requests[7:]) || !reflect.DeepEqual(pages.requestedHashes[:7], pages.requestedHashes[7:]) {
		t.Fatal("retry skipped or reused a history cursor, metadata read, or coverage proof")
	}
	for i, cursor := range []string{"", "second-page", "", "second-page", ""} {
		if requests[i].Cursor != cursor || requests[i].Selection != requests[0].Selection {
			t.Fatalf("memory cursor or selection leaked into request %d: %+v", i, requests[i])
		}
	}
	if len(p.Content.History) != 40 || len(p.Content.ProjectMemory) != 2 {
		t.Fatalf("retry duplicated or omitted content: history=%d memory=%d", len(p.Content.History), len(p.Content.ProjectMemory))
	}
	for i, segment := range p.Content.History {
		if len(segment.Events) != 1 || segment.Events[0].ID != events[i].ID {
			t.Fatalf("retry lost chronological event %d: %+v", i, segment)
		}
	}
	// A stable preparation at the final revision must produce identical bytes,
	// including coverage gaps, source pointers, token usage, and package digest.
	want, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil || !reflect.DeepEqual(p, want) {
		t.Fatalf("retry differs from a fresh stable preparation: err=%v got=%s want=%s", err, p.ID, want.ID)
	}
	if err := p.ValidateIdentity(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentPreparationRetryExcludesExplicitPersonalWork(t *testing.T) {
	for _, kind := range []string{"complete scope", "work state path", "both"} {
		t.Run(kind, func(t *testing.T) {
			s, in, h, memory, _ := agentServiceFixture(t)
			if kind != "work state path" {
				in.PersonalScope = domain.PersonalWorkScope{ActorID: "alice", SessionID: "session", WorktreeID: "worktree"}
				s.work = agentWorkFixture{state: domain.PersonalWorkState{Scope: in.PersonalScope, Sources: []domain.AgentSourcePointer{{SnapshotID: in.SnapshotID}}}}
			}
			if kind != "complete scope" {
				in.WorkStatePath = "/fixture/work-state.json"
			}
			calls := 0
			s.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				calls++
				page, err := memory.QueryEffectiveMemory(ctx, repo, req)
				if calls == 1 {
					page.Revision.Evidence++ // A second attempt would succeed.
				}
				return page, err
			})
			p, err := s.PrepareAgentContext(context.Background(), in)
			if !errors.Is(err, domain.ErrSelectionChanged) || !reflect.DeepEqual(p, domain.AgentContextPackage{}) || h.calls != 1 || calls != 1 {
				t.Fatalf("retried cached personal authorization: err=%v history=%d memory=%d", err, h.calls, calls)
			}
		})
	}
}

func TestAgentPreparationRetryRejectsMalformedMemoryBeforeRevisionDrift(t *testing.T) {
	for _, at := range []string{"first page", "revalidation"} {
		for _, kind := range []string{"invalid state hash", "invalid item hash", "wrong pin echo", "wrong pin lineage", "items exceed total", "incomplete terminal page", "cursor past total"} {
			t.Run(at+"/"+kind, func(t *testing.T) {
				s, in, h, memory, _ := agentServiceFixture(t)
				in.MemoryPin = &domain.AgentMemoryPin{SnapshotID: in.SnapshotID, MemoryHash: agentHash("lineage")}
				failAt := 1
				if at == "revalidation" {
					failAt = 2
				}
				calls := 0
				s.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
					calls++
					page, err := memory.QueryEffectiveMemory(ctx, repo, req)
					page.Items = append([]domain.EffectiveMemoryItem(nil), page.Items...)
					if calls == failAt {
						page.Revision.Evidence++
						switch kind {
						case "invalid state hash":
							page.StateHash = "invalid"
						case "invalid item hash":
							page.Items[0].ID = "invalid"
						case "wrong pin echo":
							page.Selection.MemoryHash = agentHash("different pin")
						case "wrong pin lineage":
							page.LineageHash = agentHash("different pin")
						case "items exceed total":
							page.Total = 0
						case "incomplete terminal page":
							page.Total = 2
						case "cursor past total":
							page.NextCursor = "impossible-next-page"
						}
					}
					return page, err // Subsequent reads are valid, exposing an unsafe retry.
				})
				p, err := s.PrepareAgentContext(context.Background(), in)
				if err == nil || !reflect.DeepEqual(p, domain.AgentContextPackage{}) || calls != failAt || h.calls != failAt {
					t.Fatalf("malformed memory was retried or accepted: err=%v history=%d memory=%d want=%d", err, h.calls, calls, failAt)
				}
			})
		}
	}
}

func TestAgentPreparationRetryObservedSelectionABAIsTerminal(t *testing.T) {
	for _, field := range []string{"position", "branch", "code", "content"} {
		t.Run(field, func(t *testing.T) {
			s, in, h, memory, _ := agentServiceFixture(t)
			calls := 0
			s.history = retryHistoryFunc(func(ctx context.Context, _ inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
				calls++
				view := h.view
				if calls == 2 {
					revision := *view.Revision
					revision.Evidence++
					view.Revision = &revision
					switch field {
					case "position":
						view.Position = agentHash("other position")
					case "branch":
						view.Selection.Branch = "other"
					case "code":
						view.Selection.CodeCommit = strings.Repeat("b", 40)
					case "content":
						view.StateHash = agentHash("changed content")
					}
				}
				// Any retry would see A again and incorrectly erase the observed B.
				return view, ctx.Err()
			})
			p, err := s.PrepareAgentContext(context.Background(), in)
			if !errors.Is(err, domain.ErrSelectionChanged) || !reflect.DeepEqual(p, domain.AgentContextPackage{}) || calls != 2 || memory.calls != 1 {
				t.Fatalf("observed selection change was retried: err=%v history=%d memory=%d", err, calls, memory.calls)
			}
		})
	}
}

func TestAgentPreparationRetryWrappedCodeMismatchIsTerminal(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("attempt-%d", failAt), func(t *testing.T) {
			s, in, h, memory, _ := agentServiceFixture(t)
			memory.rev.Evidence++
			calls := 0
			s.history = retryHistoryFunc(func(ctx context.Context, _ inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
				calls++
				if calls == failAt {
					return domain.HistoryQueryResult{}, fmt.Errorf("wrapped read failure: %w", domain.ErrCodePositionMismatch)
				}
				return h.view, ctx.Err()
			})
			p, err := s.PrepareAgentContext(context.Background(), in)
			if !errors.Is(err, domain.ErrCodePositionMismatch) || !reflect.DeepEqual(p, domain.AgentContextPackage{}) || calls != failAt || memory.calls != failAt-1 {
				t.Fatalf("retried code mismatch: err=%v history=%d memory=%d", err, calls, memory.calls)
			}
		})
	}
}

type retryHistoryPageFunc func(context.Context, domain.ContentHash, domain.AgentHistoryPageRequest) (domain.AgentHistoryPage, error)

func (f retryHistoryPageFunc) GetDoc(context.Context, domain.ContentHash) (domain.SessionDoc, error) {
	return domain.SessionDoc{}, errors.New("retry must not fall back to full document reads")
}

func (f retryHistoryPageFunc) ReadAgentHistoryPage(ctx context.Context, hash domain.ContentHash, req domain.AgentHistoryPageRequest) (domain.AgentHistoryPage, error) {
	return f(ctx, hash, req)
}

func TestAgentPreparationRetryAttemptTwoReauthorizesEveryRead(t *testing.T) {
	for _, at := range []string{"history start", "context revalidation", "memory start", "memory revalidation", "history body", "history next page", "history metadata", "history proof"} {
		t.Run(at, func(t *testing.T) {
			var events []domain.Event
			for i := 0; i < 20; i++ {
				events = append(events, agentMessage("user", fmt.Sprintf("turn-%d", i), i))
			}
			latest := agentDocument(t, "native", events...)
			older := agentDocument(t, "native", events[:1]...)
			s, in, pages, h := preparePagedHistoryFixture(t, latest, older)
			memory := s.memory.(*agentMemoryFixture)
			denied := errors.New("attempt two permission revoked")
			historyCalls, memoryCalls, pageCalls := 0, 0, 0
			deniedReads := 0
			s.history = retryHistoryFunc(func(ctx context.Context, _ inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
				historyCalls++
				if deniedReads != 0 {
					t.Fatal("started another read after revoked authorization")
				}
				if at == "history start" && historyCalls == 3 || at == "context revalidation" && historyCalls == 4 {
					deniedReads++
					return domain.HistoryQueryResult{}, denied
				}
				if historyCalls == 2 {
					revision := *h.view.Revision
					revision.Evidence++
					h.view.Revision, memory.rev = &revision, revision
				}
				return h.view, ctx.Err()
			})
			s.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				memoryCalls++
				if deniedReads != 0 {
					t.Fatal("read memory after revoked authorization")
				}
				if at == "memory start" && memoryCalls == 2 || at == "memory revalidation" && memoryCalls == 3 {
					deniedReads++
					return domain.EffectiveMemoryPage{}, denied
				}
				return memory.QueryEffectiveMemory(ctx, repo, req)
			})
			s.documents = retryHistoryPageFunc(func(ctx context.Context, hash domain.ContentHash, req domain.AgentHistoryPageRequest) (domain.AgentHistoryPage, error) {
				pageCalls++
				if deniedReads != 0 {
					t.Fatal("read history after revoked authorization")
				}
				failAt := map[string]int{"history body": 5, "history next page": 6, "history metadata": 7, "history proof": 8}[at]
				if pageCalls == failAt {
					deniedReads++
					return domain.AgentHistoryPage{}, denied
				}
				return pages.ReadAgentHistoryPage(ctx, hash, req)
			})
			p, err := s.PrepareAgentContext(context.Background(), in)
			if !errors.Is(err, denied) || !reflect.DeepEqual(p, domain.AgentContextPackage{}) || deniedReads != 1 {
				t.Fatalf("did not fail closed at %s: err=%v history=%d memory=%d pages=%d denials=%d", at, err, historyCalls, memoryCalls, pageCalls, deniedReads)
			}
		})
	}
}
