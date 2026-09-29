package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// ReadAgentHistoryPage returns contiguous complete turns, newest first. Like
// events, HTTP authorization is checked on every request; both document indexes
// and chunks independently enforce repository ownership. No page is cached.
func (s *Service) ReadAgentHistoryPage(ctx context.Context, repo, hash domain.ContentHash, req domain.AgentHistoryPageRequest) (domain.AgentHistoryPage, error) {
	if err := ctx.Err(); err != nil {
		return domain.AgentHistoryPage{}, err
	}
	if err := validateHashes(repo, hash); err != nil {
		return domain.AgentHistoryPage{}, err
	}
	if req.Before < -1 || req.Limit < 1 || req.Limit > domain.MaxAgentHistoryPageTurns || req.MaxBytes < 1 || req.MaxBytes > domain.MaxAgentHistoryPageBytes || domain.ValidateOptionalContentHash(req.CoveredBy) != nil {
		return domain.AgentHistoryPage{}, fmt.Errorf("%w: invalid history page range or byte budget", domain.ErrValidation)
	}
	return repositoryRead(outbound.WithDocReadOnly(ctx), s, func(ctx context.Context) (domain.AgentHistoryPage, error) {
		return s.readAgentHistoryPage(ctx, repo, hash, req)
	})
}

func (s *Service) readAgentHistoryPage(ctx context.Context, repo, hash domain.ContentHash, req domain.AgentHistoryPageRequest) (domain.AgentHistoryPage, error) {
	out := domain.AgentHistoryPage{Version: domain.AgentHistoryPageVersion, Hash: hash, NextBefore: -1, Turns: []domain.AgentHistoryTurn{}}
	store, ok := s.blobs.(outbound.DocReadStore)
	if !ok {
		return out, domain.ErrAgentHistoryUnavailable
	}
	index := func(hash domain.ContentHash) (domain.DocReadIndex, error) {
		idx, err := store.DocReadIndex(ctx, repo, hash)
		if err != nil {
			return idx, err
		}
		if idx.Version != 1 || idx.Hash != hash {
			return idx, domain.ErrIntegrity
		}
		offset := 0
		for i, ev := range idx.Events {
			if i%1024 == 0 {
				if err := ctx.Err(); err != nil {
					return idx, err
				}
			}
			if ev.Index != i || ev.Offset != offset || ev.Length <= 0 || domain.ValidateContentHash(ev.Hash) != nil || ev.Length >= int(^uint(0)>>1)-offset {
				return idx, domain.ErrIntegrity
			}
			offset += ev.Length + 1
		}
		return idx, nil
	}
	idx, err := index(hash)
	if err != nil {
		return out, err
	}
	out.Provider, out.SessionID, out.Total = idx.Envelope.SourceProvider, idx.Envelope.SessionOriginID, len(idx.Events)
	out.Before = req.Before
	if out.Before == -1 {
		out.Before = out.Total
	}
	if out.Before > out.Total || (out.Before > 0 && out.Before < out.Total && idx.Events[out.Before].Role != "user") {
		return out, fmt.Errorf("%w: before must be a complete turn boundary", domain.ErrValidation)
	}
	if req.CoveredBy != "" {
		other, err := index(req.CoveredBy)
		if err != nil {
			return out, err
		}
		if historyIndexCovered(ctx, idx, other) {
			out.Covered = true
			return out, nil
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
	}
	previousUser := func(end int) int {
		for i := end - 1; i >= 0; i-- {
			if idx.Events[i].Role == "user" {
				return i
			}
		}
		return -1
	}
	if previousUser(out.Before) < 0 {
		return out, nil
	}
	read, err := s.eventRangeReader(ctx, repo, hash)
	if err != nil {
		return out, err
	}
	// CIR can place a small user-turn marker immediately before its prompt.
	// Inspect only that bounded metadata, never the body of an earlier prompt.
	withMarker := func(start int) (int, error) {
		if start > 0 && idx.Events[start-1].Role == "user" && idx.Events[start-1].Length <= 1024 {
			item := idx.Events[start-1]
			raw, err := read(item.Offset, item.Length)
			if err != nil {
				return start, err
			}
			ev, err := domain.DecodeIndexedEvent(raw, item)
			if err != nil {
				return start, err
			}
			if ev.Kind == domain.EventTurn {
				return start - 1, nil
			}
		}
		return start, nil
	}
	if out.Before < out.Total && out.Before > 0 {
		start, err := withMarker(out.Before)
		if err != nil {
			return out, err
		}
		if start != out.Before {
			return out, fmt.Errorf("%w: before splits a prompt from its turn marker", domain.ErrValidation)
		}
	}
	remaining, end := req.MaxBytes, out.Before
	for len(out.Turns) < req.Limit {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		start := previousUser(end)
		if start < 0 {
			break
		}
		start, err = withMarker(start)
		if err != nil {
			return out, err
		}
		// Canonical bytes are a lower bound on the JSON wire body. Preflight
		// prevents any chunk fetch for a huge candidate that cannot fit.
		size := 1
		for _, item := range idx.Events[start:end] {
			if item.Length >= remaining-size {
				size = remaining + 1
				break
			}
			size += item.Length + 1
		}
		if size > remaining {
			if len(out.Turns) == 0 {
				return out, historyTurnBudgetError()
			}
			break
		}
		turn := domain.AgentHistoryTurn{Start: start, End: end, Events: make([]domain.CIREvent, 0, end-start)}
		for _, item := range idx.Events[start:end] {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			raw, err := read(item.Offset, item.Length)
			if err != nil {
				return out, err
			}
			ev, err := domain.DecodeIndexedEvent(raw, item)
			if err != nil {
				return out, err
			}
			if string(ev.Role) != item.Role || ev.Seq != item.Seq {
				return out, domain.ErrIntegrity
			}
			turn.Events = append(turn.Events, ev)
		}
		if err := completeHistoryTurn(turn.Events); err != nil {
			return out, err
		}
		raw, err := json.Marshal(turn.Events)
		if err != nil {
			return out, err
		}
		if len(raw) > remaining {
			if len(out.Turns) == 0 {
				return out, historyTurnBudgetError()
			}
			break
		}
		turn.Hash = domain.HashContent(raw)
		out.Turns = append(out.Turns, turn)
		remaining -= len(raw)
		end = start
	}
	if previousUser(end) >= 0 {
		out.NextBefore = end
	}
	return out, nil
}

func historyTurnBudgetError() error {
	return fmt.Errorf("%w: newest complete turn exceeds the requested history byte range; no turn was skipped (this is not a token limit)", domain.ErrContextBudgetExceeded)
}

func historyIndexCovered(ctx context.Context, older, newer domain.DocReadIndex) bool {
	provider, session := older.Envelope.SourceProvider, older.Envelope.SessionOriginID
	if (provider != domain.ProviderClaude && provider != domain.ProviderCodex) || session == "" || newer.Envelope.SourceProvider != provider || newer.Envelope.SessionOriginID != session || len(older.Events) > len(newer.Events) || len(older.Events) > domain.MaxContextSegmentPrefixEvents {
		return false
	}
	for i, ev := range older.Events {
		if i%1024 == 0 && ctx.Err() != nil {
			return false
		}
		if ev.Hash != newer.Events[i].Hash {
			return false
		}
	}
	return true
}

func completeHistoryTurn(events []domain.CIREvent) error {
	calls := map[string]bool{}
	for i, ev := range events {
		if ev.Role == "user" && (ev.Kind != domain.EventMessage && ev.Kind != domain.EventTurn || i > 0 && !(i == 1 && events[0].Kind == domain.EventTurn && ev.Kind == domain.EventMessage)) {
			return fmt.Errorf("%w: invalid user turn boundary", domain.ErrIntegrity)
		}
		switch ev.Kind {
		case domain.EventToolCall:
			if _, found := calls[ev.CallID]; found || ev.CallID == "" {
				return fmt.Errorf("%w: duplicate or missing tool call identity", domain.ErrIntegrity)
			}
			calls[ev.CallID] = false
		case domain.EventToolResult:
			done, found := calls[ev.CallID]
			if !found || done {
				return fmt.Errorf("%w: unmatched tool result", domain.ErrIntegrity)
			}
			calls[ev.CallID] = true
		}
	}
	for _, done := range calls {
		if !done {
			return fmt.Errorf("%w: incomplete historical tool pair", domain.ErrIntegrity)
		}
	}
	return nil
}
