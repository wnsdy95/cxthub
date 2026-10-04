package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type agentPageFixture struct {
	docs              map[domain.ContentHash]domain.SessionDoc
	calls, readEvents int
	requests          []domain.AgentHistoryPageRequest
	requestedHashes   []domain.ContentHash
	mutate            func(*domain.AgentHistoryPage)
	failAt            int
}

func (*agentPageFixture) GetDoc(context.Context, domain.ContentHash) (domain.SessionDoc, error) {
	return domain.SessionDoc{}, fmt.Errorf("full document body must not be requested")
}

func (f *agentPageFixture) ReadAgentHistoryPage(ctx context.Context, hash domain.ContentHash, req domain.AgentHistoryPageRequest) (domain.AgentHistoryPage, error) {
	f.calls++
	f.requests = append(f.requests, req)
	f.requestedHashes = append(f.requestedHashes, hash)
	if f.failAt > 0 && f.calls >= f.failAt {
		return domain.AgentHistoryPage{}, errors.New("permission revoked")
	}
	doc := f.docs[hash]
	before := req.Before
	if before == -1 {
		before = len(doc.CIR.Events)
	}
	p := domain.AgentHistoryPage{Version: 1, Hash: hash, Provider: doc.CIR.Envelope.SourceProvider, SessionID: doc.CIR.Envelope.SessionOriginID, Total: len(doc.CIR.Events), Before: before, NextBefore: -1, Turns: []domain.AgentHistoryTurn{}}
	if req.IncompleteTail == "omit" {
		p.Version = domain.AgentHistoryProjectionVersion
	}
	if prior, ok := f.docs[req.CoveredBy]; ok && prior.CIR.Envelope.SourceProvider == p.Provider && prior.CIR.Envelope.SessionOriginID == p.SessionID && p.SessionID != "" && len(prior.CIR.Events) >= len(doc.CIR.Events) {
		left, _ := json.Marshal(doc.CIR.Events)
		right, _ := json.Marshal(prior.CIR.Events[:len(doc.CIR.Events)])
		if string(left) == string(right) {
			p.Covered = true
			return p, ctx.Err()
		}
	}
	end, bytes := before, 0
	if req.IncompleteTail == "omit" && before == len(doc.CIR.Events) && before > 0 {
		_, tail, err := projectAgentHistoryTurns(doc.CIR, true)
		if err != nil {
			return p, err
		}
		if tail != nil {
			p.OmittedTail = tail
			end = tail.Start
		}
	}

	for i := end - 1; i >= 0; i-- {
		if doc.CIR.Events[i].Role != "user" || doc.CIR.Events[i].Kind != domain.EventMessage {
			continue
		}
		events := doc.CIR.Events[i:end]
		raw, _ := json.Marshal(events)
		if len(raw)+bytes > req.MaxBytes {
			if len(p.Turns) == 0 {
				return p, domain.ErrContextBudgetExceeded
			}
			p.NextBefore = end
			break
		}
		p.Turns = append(p.Turns, domain.AgentHistoryTurn{Start: i, End: end, Hash: domain.HashContent(raw), Events: events})
		f.readEvents += len(events)
		bytes += len(raw)
		end = i
		if len(p.Turns) == req.Limit && i > 0 {
			p.NextBefore = i
			break
		}
	}
	if f.mutate != nil {
		f.mutate(&p)
	}
	return p, ctx.Err()
}

func TestAgentContextPagedReadsOnlyLatestTurnsThatFit(t *testing.T) {
	s, in, h, _, _ := agentServiceFixture(t)
	var events []domain.Event
	for i := 0; i < 1000; i++ {
		events = append(events, agentMessage("user", fmt.Sprintf("turn-%04d %s", i, strings.Repeat("\ud55c\uae00", 600)), len(events)), agentMessage("assistant", "done", len(events)+1))
	}
	doc := agentDocument(t, "paged", events...)
	pages := &agentPageFixture{docs: map[domain.ContentHash]domain.SessionDoc{doc.Hash: doc}}
	s.documents = pages
	h.view.Snapshots = []domain.Snapshot{{ID: doc.Hash, DocHash: doc.Hash, RepoID: in.RepoID}}
	h.view.Position, in.SnapshotID = doc.Hash, doc.Hash
	in.ArtifactOnly = true
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 12000, Source: "explicit"}
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if pages.calls != 1 || pages.readEvents != 32 || len(p.Content.History) == 0 || len(p.Content.History) >= 16 {
		t.Fatalf("unbounded read: calls=%d events=%d selected=%d", pages.calls, pages.readEvents, len(p.Content.History))
	}
	latest := p.Content.History[len(p.Content.History)-1]
	if !strings.HasPrefix(latest.Events[0].Blocks[0].Text, "turn-0999") || latest.Source.StartEvent != 1998 || latest.Source.EndEvent != 2000 {
		t.Fatalf("wrong tail %+v", latest.Source)
	}
	if p.Usage.Tokens > 12000 {
		t.Fatal("budget exceeded")
	}
}

func TestAgentContextPagedPreservesSessionPrefixAndToolPairs(t *testing.T) {
	s, in, h, _, _ := agentServiceFixture(t)
	first := []domain.Event{agentMessage("user", "do work", 0), {Kind: domain.EventToolCall, CallID: "c", ToolName: "test", Seq: 1, Input: map[string]any{}}, {Kind: domain.EventToolResult, CallID: "c", Seq: 2, Output: "ok"}}
	latest := agentDocument(t, "alice", append(append([]domain.Event{}, first...), agentMessage("user", "latest", 3))...)
	prior := agentDocument(t, "alice", first...)
	other := agentDocument(t, "bob", first...)
	pages := &agentPageFixture{docs: map[domain.ContentHash]domain.SessionDoc{latest.Hash: latest, prior.Hash: prior, other.Hash: other}}
	s.documents = pages
	h.view.Snapshots = nil
	for _, d := range []domain.SessionDoc{latest, prior, other} {
		h.view.Snapshots = append(h.view.Snapshots, domain.Snapshot{ID: d.Hash, DocHash: d.Hash, RepoID: in.RepoID})
	}
	h.view.Position, in.SnapshotID = latest.Hash, latest.Hash
	in.ArtifactOnly = true
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 200000, Source: "explicit"}
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Content.History) != 3 || p.Content.History[0].SessionID != "bob" || len(p.Content.History[1].Events) != 3 {
		t.Fatalf("bad prefix/tool history %+v", p.Content.History)
	}
	proof := false
	for _, req := range pages.requests {
		proof = proof || req.CoveredBy == latest.Hash
	}
	if !proof {
		t.Fatal("no server prefix proof requested")
	}
}

func TestAgentContextPagedRejectsCorruptionAndRevocation(t *testing.T) {
	for _, kind := range []string{"hash", "cursor", "revocation"} {
		t.Run(kind, func(t *testing.T) {
			s, in, h, _, docs := agentServiceFixture(t)
			doc := docs.docs[in.SnapshotID]
			for i := 2; i < 40; i++ {
				doc.CIR.Events = append(doc.CIR.Events, agentMessage("user", fmt.Sprint(i), i))
			}
			doc = agentDocument(t, "paged", doc.CIR.Events...)
			h.view.Position, in.SnapshotID = doc.Hash, doc.Hash
			h.view.Snapshots = []domain.Snapshot{{ID: doc.Hash, DocHash: doc.Hash, RepoID: in.RepoID}}
			pages := &agentPageFixture{docs: map[domain.ContentHash]domain.SessionDoc{doc.Hash: doc}}
			if kind == "revocation" {
				pages.failAt = 2
			} else {
				pages.mutate = func(p *domain.AgentHistoryPage) {
					if kind == "hash" {
						p.Turns[0].Hash = agentHash("corrupt")
					} else {
						p.NextBefore = p.Before
					}
				}
			}
			s.documents = pages
			in.ArtifactOnly = true
			in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 200000, Source: "explicit"}
			if _, err := s.PrepareAgentContext(context.Background(), in); err == nil {
				t.Fatal("accepted bad/revoked page")
			}
		})
	}
}

func TestAgentContextSyntheticTurnDoesNotAttachToPreviousUser(t *testing.T) {
	turns, err := agentHistoryTurns(domain.CIRDocument{Events: []domain.Event{agentMessage("user", "real", 0), agentMessage("assistant", "answer", 1), agentMessage("user", "[cxt context package v1]\n{}", 2), agentMessage("assistant", "synthetic answer", 3), agentMessage("user", "new real", 4)}})
	if err != nil || len(turns) != 2 || len(turns[0].Events) != 2 {
		t.Fatalf("seed reply contaminated old turn: %+v %v", turns, err)
	}
}
