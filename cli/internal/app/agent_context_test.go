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

type agentHistoryFixture struct {
	view  domain.HistoryQueryResult
	calls int
	after func(*domain.HistoryQueryResult)
}

func (f *agentHistoryFixture) QueryHistory(ctx context.Context, in inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
	if !in.Server {
		return domain.HistoryQueryResult{}, fmt.Errorf("server query required")
	}
	f.calls++
	out := f.view
	if f.calls > 1 && f.after != nil {
		f.after(&out)
	}
	return out, ctx.Err()
}

type agentMemoryFixture struct {
	items     []domain.EffectiveMemoryItem
	calls     int
	rev       domain.RepositoryRevision
	failAfter int
}

func (f *agentMemoryFixture) QueryEffectiveMemory(ctx context.Context, repo string, in domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	f.calls++
	if f.failAfter > 0 && f.calls > f.failAfter {
		return domain.EffectiveMemoryPage{}, fmt.Errorf("permission denied")
	}
	p := domain.EffectiveMemoryPage{Content: in.Content, Selection: in.Selection, StateHash: agentHash("memory"), LineageHash: agentHash("lineage"), Items: f.items, Total: len(f.items)}
	p.Revision.Graph = f.rev.Graph
	p.Revision.Evidence = f.rev.Evidence
	return p, ctx.Err()
}

type agentDocFixture struct {
	docs  map[domain.ContentHash]domain.SessionDoc
	calls int
}

func (f *agentDocFixture) GetDoc(ctx context.Context, h domain.ContentHash) (domain.SessionDoc, error) {
	f.calls++
	d, ok := f.docs[h]
	if !ok {
		return d, domain.ErrNotFound
	}
	return d, ctx.Err()
}

type agentWorkFixture struct{ state domain.PersonalWorkState }

func (f agentWorkFixture) ReadPersonalWork(context.Context, string, domain.PersonalWorkScope) (domain.PersonalWorkState, error) {
	return f.state, nil
}

type agentTokenFixture struct{}

func (agentTokenFixture) CountAgentTokens(ctx context.Context, p domain.ProviderKind, model, prompt string) (domain.AgentTokenUsage, error) {
	return domain.AgentTokenUsage{Tokens: len(prompt), Exact: true, Tokenizer: "fixture-byte-counter"}, ctx.Err()
}

type agentCapabilityFixture struct{ window int }

func (f agentCapabilityFixture) AgentCapability(ctx context.Context, p domain.ProviderKind, model string) (domain.AgentHostCapability, error) {
	return domain.AgentHostCapability{Provider: p, Model: model, HostVersion: "test", Verified: true, HostInputKnown: true, AutoCompactKnown: true, Evidence: "synthetic fixture; not real host support", ContextWindow: f.window, ReservedTokens: 100, FramingTokens: 10, Tokenizer: "fixture-byte-counter"}, ctx.Err()
}
func agentHash(s string) domain.ContentHash { return domain.HashContent([]byte(s)) }
func agentDocument(t testing.TB, session string, events ...domain.Event) domain.SessionDoc {
	t.Helper()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: session, Fidelity: domain.FidelityFull}, Events: events}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	return doc
}
func agentMessage(role, text string, seq int) domain.Event {
	return domain.Event{Kind: domain.EventMessage, Role: role, Seq: seq, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}
}
func agentServiceFixture(t testing.TB) (*AgentContextService, inbound.PrepareAgentContextInput, *agentHistoryFixture, *agentMemoryFixture, *agentDocFixture) {
	t.Helper()
	doc := agentDocument(t, "shared-session", agentMessage("user", "teammate raw command must stay out of B", 0), agentMessage("assistant", "archived response", 1))
	snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: "repo", MemoryHash: agentHash("memory")}
	h := &agentHistoryFixture{view: domain.HistoryQueryResult{Version: 1, ServerChecked: true, Complete: true, StateHash: agentHash("context"), Position: snap.ID, Snapshots: []domain.Snapshot{snap}, Revision: &domain.RepositoryRevision{Graph: 5, Evidence: 4}, Selection: domain.HistorySelection{CodeCommit: strings.Repeat("a", 40), Branch: "main", Source: "server"}}}
	m := &agentMemoryFixture{rev: *h.view.Revision, items: []domain.EffectiveMemoryItem{{ID: agentHash("claim"), SourceSnapshot: snap.ID, Kind: "decision", Text: "Use server-authoritative memory", State: "retained", Reason: "project_decision"}}}
	d := &agentDocFixture{docs: map[domain.ContentHash]domain.SessionDoc{doc.Hash: doc}}
	service := NewAgentContextService(h, m, d, nil, nil, nil)
	in := inbound.PrepareAgentContextInput{RepoID: "repo", Cwd: "/fixture", Provider: domain.ProviderCodex, Model: "fixture", SnapshotID: snap.ID}
	return service, in, h, m, d
}

func TestAgentContextMemoryDefaultDoesNotReadOrInjectTeammateConversation(t *testing.T) {
	s, in, _, _, docs := agentServiceFixture(t)
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	prompt, _ := p.Prompt()
	if docs.calls != 0 || len(p.Content.History) != 0 || strings.Contains(prompt, "teammate raw command") {
		t.Fatal("default B replayed raw shared context")
	}
	if !strings.Contains(prompt, "server-authoritative memory") || p.Usage.Exact || p.Delivery != "prepared" {
		t.Fatalf("incorrect receipt %+v", p)
	}
	if err := p.ValidateIdentity(); err != nil {
		t.Fatal(err)
	}
}
func TestAgentContextPersonalStateScopeAndExactConditions(t *testing.T) {
	s, in, _, _, _ := agentServiceFixture(t)
	in.PersonalScope = domain.PersonalWorkScope{ActorID: "alice", SessionID: "session-a", WorktreeID: "tree-a"}
	state := domain.PersonalWorkState{Scope: in.PersonalScope, Goal: "Resume my work", Sources: []domain.AgentSourcePointer{{SnapshotID: in.SnapshotID}}, Constraints: []domain.ExactUserConstraint{{Text: "Do not deploy.\nKeep original IDs.", Source: domain.AgentSourcePointer{SnapshotID: in.SnapshotID}}}}
	s.work = agentWorkFixture{state: state}
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Content.PersonalWork.Constraints, state.Constraints) {
		t.Fatal("changed exact user conditions")
	}
	state.Scope.ActorID = "bob"
	s.work = agentWorkFixture{state: state}
	if _, err = s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal(err)
	}
}
func TestAgentContextRequiredConditionsNeverSilentlyTruncate(t *testing.T) {
	s, in, _, _, _ := agentServiceFixture(t)
	in.PersonalScope = domain.PersonalWorkScope{ActorID: "alice", SessionID: "s", WorktreeID: "w"}
	s.work = agentWorkFixture{state: domain.PersonalWorkState{Scope: in.PersonalScope, Goal: strings.Repeat("essential ", 1000), Sources: []domain.AgentSourcePointer{{SnapshotID: in.SnapshotID}}}}
	if _, err := s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrContextBudgetExceeded) {
		t.Fatal(err)
	}
}
func TestAgentContextHistoryRequiresKnownTokenizerAndCapability(t *testing.T) {
	s, in, h, _, docs := agentServiceFixture(t)
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 200000, Source: "explicit"}
	if _, err := s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrProviderCapabilityUnknown) {
		t.Fatal(err)
	}
	if docs.calls != 0 || h.calls != 0 {
		t.Fatal("read history before capability preflight")
	}
}
func TestAgentContextHistoryChronologyToolsAndCapacity(t *testing.T) {
	s, in, h, _, docs := agentServiceFixture(t)
	events := []domain.Event{agentMessage("user", "first", 0), {Kind: domain.EventToolCall, CallID: "call-1", ToolName: "fixture", Input: map[string]any{"q": "\ud55c\uae00"}, Seq: 1}, {Kind: domain.EventToolResult, CallID: "call-1", Output: "result", Seq: 2}, agentMessage("user", "latest", 3), agentMessage("assistant", "answer", 4)}
	doc := agentDocument(t, "shared-session", events...)
	snap := h.view.Snapshots[0]
	snap.ID = doc.Hash
	snap.DocHash = doc.Hash
	h.view.Snapshots = []domain.Snapshot{snap}
	h.view.Position = doc.Hash
	in.SnapshotID = doc.Hash
	docs.docs[doc.Hash] = doc
	s.tokens = agentTokenFixture{}
	s.capabilities = agentCapabilityFixture{window: 900000}
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 200000, Source: "explicit"}
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Content.History) != 2 || p.Content.History[0].Events[0].Blocks[0].Text != "first" || len(p.Content.History[0].Events) != 3 || p.Content.History[1].Events[0].Blocks[0].Text != "latest" {
		t.Fatal("history order or tool pair broken")
	}
	if p.Delivery != "prepared" || p.Capability != "verified_for_preparation" {
		t.Fatal("claimed provider acceptance")
	}
	s.capabilities = agentCapabilityFixture{window: p.Usage.Tokens}
	if _, err = s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrContextBudgetExceeded) {
		t.Fatal(err)
	}
}
func TestAgentContextRechecksRevisionAndAuthorization(t *testing.T) {
	for _, kind := range []string{"revision", "permission"} {
		t.Run(kind, func(t *testing.T) {
			s, in, h, m, _ := agentServiceFixture(t)
			if kind == "revision" {
				h.after = func(v *domain.HistoryQueryResult) { v.StateHash = agentHash("changed") }
			} else {
				m.failAfter = 1
			}
			if _, err := s.PrepareAgentContext(context.Background(), in); err == nil {
				t.Fatal("accepted changed/revoked source")
			}
		})
	}
}
func TestAgentContextNativeResumeIsNotPrepared(t *testing.T) {
	s, in, h, _, _ := agentServiceFixture(t)
	in.NativeResume = true
	if _, err := s.PrepareAgentContext(context.Background(), in); err == nil {
		t.Fatal("injected into native resume")
	}
	if h.calls != 0 {
		t.Fatal("queried native resume")
	}
}
func TestAgentContextUnpairedToolFailsWithoutInventingReplay(t *testing.T) {
	doc := agentDocument(t, "s", agentMessage("user", "go", 0), domain.Event{Kind: domain.EventToolCall, CallID: "c", ToolName: "test", Input: map[string]any{}, Seq: 1})
	if _, err := agentHistoryTurns(doc.CIR); !errors.Is(err, domain.ErrInvalidCIR) {
		t.Fatal(err)
	}
}
func TestAgentContextCorruptHistoryFails(t *testing.T) {
	s, in, _, _, docs := agentServiceFixture(t)
	s.tokens = agentTokenFixture{}
	s.capabilities = agentCapabilityFixture{window: 900000}
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 200000, Source: "explicit"}
	doc := docs.docs[in.SnapshotID]
	doc.CIR.Events[0].Blocks[0].Text = "tampered"
	docs.docs[in.SnapshotID] = doc
	if _, err := s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal(err)
	}
}

func TestAgentContextArtifactAllowsInspectionWithoutClaimingHostAcceptance(t *testing.T) {
	s, in, h, _, docs := agentServiceFixture(t)
	events := make([]domain.Event, 0, 320)
	for i := 0; i < 160; i++ {
		events = append(events, agentMessage("user", fmt.Sprintf("turn-%03d %s", i, strings.Repeat("\ud55c\uae00x", 1000)), i*2), agentMessage("assistant", "response", i*2+1))
	}
	doc := agentDocument(t, "large-fixture", events...)
	snap := h.view.Snapshots[0]
	snap.ID, snap.DocHash = doc.Hash, doc.Hash
	h.view.Position = doc.Hash
	h.view.Snapshots = []domain.Snapshot{snap}
	docs.docs[doc.Hash] = doc
	in.SnapshotID = doc.Hash
	in.ArtifactOnly = true
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 800000, Source: "explicit"}
	s.tokens = ConservativeAgentTokenCounter{}
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !p.ArtifactOnly || p.Capability != "unverified_artifact_only" || p.Usage.Exact || p.Usage.Tokens > 800000 || p.Usage.Tokens < 750000 {
		t.Fatalf("wrong bounded artifact receipt: %+v", p.Usage)
	}
	if len(p.Content.History) == 0 || len(p.Content.History) == 160 {
		t.Fatal("did not choose a bounded latest suffix")
	}
	latest := p.Content.History[len(p.Content.History)-1].Events[0].Blocks[0].Text
	if !strings.HasPrefix(latest, "turn-159") {
		t.Fatal("did not retain latest complete turn")
	}
	raw, err := p.Artifact()
	if err != nil || len(raw) == 0 {
		t.Fatal(err)
	}
	in.ArtifactOnly = false
	if _, err = s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrProviderCapabilityUnknown) {
		t.Fatal("launched unknown capability", err)
	}
}

func TestAgentContextHistoryPrefixProofIsSessionScoped(t *testing.T) {
	s, in, h, _, docs := agentServiceFixture(t)
	first := []domain.Event{agentMessage("user", "same words", 0), agentMessage("assistant", "same answer", 1)}
	latest := agentDocument(t, "alice", append(append([]domain.Event{}, first...), agentMessage("user", "latest", 2))...)
	prior := agentDocument(t, "alice", first...)
	other := agentDocument(t, "bob", first...)
	h.view.Snapshots = nil
	for _, doc := range []domain.SessionDoc{latest, prior, other} {
		docs.docs[doc.Hash] = doc
		h.view.Snapshots = append(h.view.Snapshots, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: in.RepoID})
	}
	in.SnapshotID = latest.Hash
	h.view.Position = latest.Hash
	in.ArtifactOnly = true
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 200000, Source: "explicit"}
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Content.History) != 3 {
		t.Fatalf("expected alice latest+prefix and independent bob, got %d", len(p.Content.History))
	}
	if p.Content.History[0].SessionID != "bob" || p.Content.History[1].SessionID != "alice" {
		t.Fatal("mixed source identity/order")
	}
}

func TestAgentContextSyntheticPackageNotReplayedAtNewBranch(t *testing.T) {
	s, in, _, _, _ := agentServiceFixture(t)
	for i := 0; i < 10; i++ {
		p, err := s.PrepareAgentContext(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		cir, err := agentPackageCIR(p, in.Provider, in.Cwd)
		if err != nil {
			t.Fatal(err)
		}
		if got := seedConversationContext(cir); len(got.Events) != 0 {
			t.Fatal("synthetic package became a new user conversation")
		}
	}
}

func BenchmarkAgentContextArtifact800K(b *testing.B) {
	s, in, h, _, docs := agentServiceFixture(b)
	events := make([]domain.Event, 0, 320)
	for i := 0; i < 160; i++ {
		events = append(events, agentMessage("user", fmt.Sprintf("turn-%03d %s", i, strings.Repeat("\ud55c\uae00x", 1000)), 2*i), agentMessage("assistant", "response", 2*i+1))
	}
	doc := agentDocument(b, "large-benchmark", events...)
	snap := h.view.Snapshots[0]
	snap.ID, snap.DocHash = doc.Hash, doc.Hash
	h.view.Position = doc.Hash
	h.view.Snapshots = []domain.Snapshot{snap}
	docs.docs[doc.Hash] = doc
	in.SnapshotID = doc.Hash
	in.ArtifactOnly = true
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 800000, Source: "explicit"}
	b.ReportAllocs()
	b.SetBytes(800000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.PrepareAgentContext(context.Background(), in); err != nil {
			b.Fatal(err)
		}
	}
}
