package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// selectPagedHistory bounds body transfer and memory by complete turns. The
// archive is immutable; each page is separately authorized by the server and
// the enclosing package rechecks context/memory revisions before delivery.
func (s *AgentContextService) selectPagedHistory(ctx context.Context, in inbound.PrepareAgentContextInput, p *domain.AgentContextPackage, snapshots []domain.Snapshot, reader outbound.AgentHistoryPageReader) error {
	type nativeEventKey struct{ session, id string }
	seenDocs := map[domain.ContentHash]bool{}
	covered := map[string]domain.ContentHash{}
	var exclusions agentTailExclusions
	seenEvents := map[nativeEventKey]domain.ContentHash{}
	newest := []domain.AgentHistorySegment{}
snapshotLoop:
	for _, snapshot := range snapshots {
		if seenDocs[snapshot.DocHash] {
			continue
		}
		seenDocs[snapshot.DocHash] = true
		req := domain.AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: 4 << 20, IncompleteTail: "omit"}
		sessionKey := ""
		total := -1
		if len(covered) > 0 || exclusions.anchors > 0 {
			// An empty event range reveals the verified native identity without
			// reading the newest turn. An older capture may end mid-tool-pair;
			// reading its body first would fail before coverage can be proved.
			metadataReq := domain.AgentHistoryPageRequest{Before: 0, Limit: 1, MaxBytes: 1, IncompleteTail: "omit"}
			metadata, err := reader.ReadAgentHistoryPage(ctx, snapshot.DocHash, metadataReq)
			if err != nil {
				return err
			}
			if err := domain.ValidateAgentHistoryPage(snapshot.DocHash, metadataReq, metadata); err != nil {
				return err
			}
			total = metadata.Total
			sessionKey = string(metadata.Provider) + "\x00" + metadata.SessionID
			if metadata.SessionID != "" {
				for _, anchor := range exclusions.bySession[sessionKey] {
					if metadata.Total > anchor.total {
						continue
					}
					if err := exclusions.proof(metadata.Total); err != nil {
						return err
					}
					proofReq := metadataReq
					proofReq.CoveredBy = anchor.hash
					proof, err := reader.ReadAgentHistoryPage(ctx, snapshot.DocHash, proofReq)
					if err != nil {
						return err
					}
					if err := domain.ValidateAgentHistoryPage(snapshot.DocHash, proofReq, proof); err != nil {
						return err
					}
					if proof.Total != total || proof.SessionID != metadata.SessionID || proof.Provider != metadata.Provider {
						return domain.ErrHashMismatch
					}
					if proof.Covered {
						continue snapshotLoop
					}
				}
				// This hash selects only a proof candidate. The backend still
				// checks both native identities and the entire event-hash prefix.
				req.CoveredBy = covered[sessionKey]
			}
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			page, err := reader.ReadAgentHistoryPage(ctx, snapshot.DocHash, req)
			if err != nil {
				if errors.Is(err, domain.ErrContextBudgetExceeded) && len(newest) > 0 {
					// The omitted turn can exceed the server's byte bound even when
					// token capacity remains. Report that gap instead of claiming the
					// requested token budget caused the stop.
					p.Content.Gaps = append(p.Content.Gaps, domain.AgentCoverageGap{Reason: "older_turn_exceeds_transfer_bound", Source: &domain.AgentSourcePointer{SnapshotID: snapshot.ID, DocHash: snapshot.DocHash, Tool: "context_fetch"}})
					return s.fitAgentHistoryProjection(ctx, in, p)
				}
				return err
			}
			if err := domain.ValidateAgentHistoryPage(snapshot.DocHash, req, page); err != nil {
				return err
			}
			key := string(page.Provider) + "\x00" + page.SessionID
			if total >= 0 && (total != page.Total || sessionKey != key) {
				return domain.ErrHashMismatch
			}
			if total < 0 {
				total, sessionKey = page.Total, key
			}
			if page.Covered {
				break
			}
			if page.OmittedTail != nil {
				markAgentIncompleteTail(p, snapshot, page.OmittedTail)
				if page.SessionID != "" {
					if err := exclusions.remember(key, agentTailAnchor{hash: snapshot.DocHash, total: page.Total}); err != nil {
						return err
					}
				}
			}
			candidates := []domain.AgentHistorySegment{}
			pageEvents := map[nativeEventKey]domain.ContentHash{}
			for _, turn := range page.Turns {
				// Reuse the common replay filter/tool-pair validator. A synthetic
				// seed is not a new instruction, even after native re-capture.
				turns, err := agentHistoryTurns(domain.CIRDocument{Events: turn.Events})
				if err != nil {
					return err
				}
				if len(turns) == 0 {
					continue
				}
				if len(turns) != 1 {
					return fmt.Errorf("%w: server page split is not a complete user turn", domain.ErrHashMismatch)
				}
				filtered := turns[0]
				duplicate := page.SessionID != ""
				for _, ev := range filtered.Events {
					if ev.ID == "" {
						duplicate = false
						continue
					}
					raw, err := json.Marshal(ev)
					if err != nil {
						return err
					}
					eventKey := nativeEventKey{session: key, id: ev.ID}
					digest := domain.HashContent(raw)
					prior, ok := pageEvents[eventKey]
					if !ok {
						prior, ok = seenEvents[eventKey]
					}
					if !ok {
						duplicate = false
					} else if prior != digest {
						return fmt.Errorf("%w: native event identity reused with different content", domain.ErrHashMismatch)
					}
					if page.SessionID != "" {
						pageEvents[eventKey] = digest
					}
				}
				if !duplicate {
					candidates = append(candidates, domain.AgentHistorySegment{Source: domain.AgentSourcePointer{SnapshotID: snapshot.ID, DocHash: snapshot.DocHash, StartEvent: turn.Start + filtered.Start, EndEvent: turn.Start + filtered.End, Tool: "context_fetch"}, SessionID: page.SessionID, Events: filtered.Events})
				}
			}
			build := func(n int) domain.AgentContextPackage {
				candidate := *p
				candidate.Content.History = make([]domain.AgentHistorySegment, 0, n+len(newest))
				for i := n - 1; i >= 0; i-- {
					candidate.Content.History = append(candidate.Content.History, candidates[i])
				}
				for i := len(newest) - 1; i >= 0; i-- {
					candidate.Content.History = append(candidate.Content.History, newest[i])
				}
				return candidate
			}
			accepted := 0
			var rejected error
			for low, high := 1, len(candidates); low <= high; {
				middle := low + (high-low)/2
				_, err := s.measure(ctx, in, build(middle))
				if err == nil {
					accepted = middle
					low = middle + 1
				} else if agentCandidateLimit(err) {
					rejected = err
					high = middle - 1
				} else {
					return err
				}
			}
			if accepted > 0 {
				*p = build(accepted)
				newest = append(newest, candidates[:accepted]...)
			}
			if accepted < len(candidates) {
				if len(newest) == 0 {
					return fmt.Errorf("newest complete turn cannot be selected: %w", rejected)
				}
				return s.finishAgentHistorySelection(ctx, in, p, rejected)
			}
			for key, digest := range pageEvents {
				seenEvents[key] = digest
			}
			if page.NextBefore == -1 {
				if page.SessionID != "" {
					covered[key] = snapshot.DocHash
				}
				break
			}
			req.Before = page.NextBefore
		}
	}
	return s.fitAgentHistoryProjection(ctx, in, p)
}
