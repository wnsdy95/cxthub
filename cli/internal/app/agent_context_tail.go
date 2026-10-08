package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Exclusion proofs are distinct from selected-history deduplication. A later
// divergent snapshot must never overwrite the evidence that an unfinished turn
// was omitted, then resurrect a balanced earlier prefix of that same turn.
const (
	maxAgentTailAnchors    = 64
	maxAgentTailProofs     = 512
	maxAgentTailHashEvents = 1000000
	// Matches the server's whole-prefix proof limit. Beyond it, absence of a
	// proof is not evidence that a same-session partial capture is independent.
	maxAgentHistoryPrefixEvents = 250000
)

type agentTailAnchor struct {
	hash     domain.ContentHash
	identity domain.DocumentIdentity
	total    int
	events   []domain.ContentHash // only the full-document fallback needs hashes
}

type agentTailExclusions struct {
	bySession               map[string][]agentTailAnchor
	anchors, proofs, events int
}

func (e *agentTailExclusions) remember(session string, anchor agentTailAnchor) error {
	if e.anchors >= maxAgentTailAnchors || len(anchor.events) > maxAgentTailHashEvents-e.events {
		return agentTailProofLimit()
	}
	if e.bySession == nil {
		e.bySession = make(map[string][]agentTailAnchor)
	}
	e.bySession[session] = append(e.bySession[session], anchor)
	e.anchors++
	e.events += len(anchor.events)
	return nil
}

func (e *agentTailExclusions) proof(total int) error {
	if total > maxAgentHistoryPrefixEvents || e.proofs >= maxAgentTailProofs {
		return agentTailProofLimit()
	}
	e.proofs++
	return nil
}

func agentTailProofLimit() error {
	return fmt.Errorf("%w: incomplete-history exclusion proof limit reached; use source-specific MCP retrieval", domain.ErrAgentContextUnavailable)
}

func (e *agentTailExclusions) contains(ctx context.Context, session string, hashes []domain.ContentHash) (bool, error) {
	for _, anchor := range e.bySession[session] {
		if len(hashes) > anchor.total {
			continue
		}
		if err := e.proof(len(hashes)); err != nil {
			return false, err
		}
		covered := true
		for i, h := range hashes {
			if i%1024 == 0 {
				if err := ctx.Err(); err != nil {
					return false, err
				}
			}
			if anchor.events[i] != h {
				covered = false
				break
			}
		}
		if covered {
			return true, nil
		}
	}
	return false, nil
}

func markAgentIncompleteTail(p *domain.AgentContextPackage, snapshot domain.Snapshot, tail *domain.AgentHistoryTail) {
	p.Content.Gaps = append(append([]domain.AgentCoverageGap(nil), p.Content.Gaps...), domain.AgentCoverageGap{
		Reason: "history_incomplete_tool_pair",
		Source: &domain.AgentSourcePointer{SnapshotID: snapshot.ID, DocHash: snapshot.DocHash, DocIdentity: snapshot.DocIdentity,
			StartEvent: tail.Start, EndEvent: tail.End, Tool: "context_fetch"},
	})
}
