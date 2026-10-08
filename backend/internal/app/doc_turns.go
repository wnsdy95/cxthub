package app

import (
	"context"
	"encoding/json"
	"errors"
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
	if err := domain.ValidateAgentHistoryPageRequest(req); err != nil {
		return domain.AgentHistoryPage{}, err
	}

	return repositoryReadForRepo(outbound.WithDocReadOnly(ctx), s, repo, func(ctx context.Context) (domain.AgentHistoryPage, error) {
		return s.readAgentHistoryPage(ctx, repo, hash, req)
	})
}

func (s *Service) readAgentHistoryPage(ctx context.Context, repo, hash domain.ContentHash, req domain.AgentHistoryPageRequest) (domain.AgentHistoryPage, error) {
	out := domain.AgentHistoryPage{Version: domain.AgentHistoryPageVersion, Hash: hash, DocIdentity: req.DocIdentity, NextBefore: -1, Turns: []domain.AgentHistoryTurn{}}
	projection := req.IncompleteTail == "omit"
	if projection {
		out.Version = domain.AgentHistoryProjectionVersion
	}
	// Cache only within this coherent read and key by the complete reference.
	sources := map[domain.DocumentRef]docReadSource{}
	sourceFor := func(ref domain.DocumentRef) (docReadSource, error) {
		if source, ok := sources[ref]; ok {
			return source, nil
		}
		source, err := s.agentHistoryReadSource(ctx, repo, ref)
		if err == nil {
			sources[ref] = source
		}
		return source, err
	}
	index := func(ref domain.DocumentRef) (domain.DocReadIndex, error) {
		source, err := sourceFor(ref)
		if err != nil {
			return domain.DocReadIndex{}, err
		}
		idx := source.index
		hash := ref.Hash
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
			// V2 must reject malformed ordering across turn boundaries too:
			// neither an omitted tail nor a metadata/prefix proof may hide it.
			if projection && (ev.Seq < 0 || i > 0 && ev.Seq <= idx.Events[i-1].Seq) {
				return idx, fmt.Errorf("%w: invalid historical event sequence", domain.ErrIntegrity)
			}
			offset += ev.Length + 1
		}
		return idx, nil
	}
	ref := domain.DocumentRef{Hash: hash, Identity: req.DocIdentity}
	idx, err := index(ref)
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
		other, err := index(domain.DocumentRef{Hash: req.CoveredBy, Identity: req.CoveredByIdentity})
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
	read := sources[ref].read
	// V2 may classify a tail whose body will not be returned. Bound that work
	// separately from MaxBytes, sharing one allowance across every inspected
	// event (including marker look-behind) and all selected turns. Chunks may
	// overfetch at their fixed storage boundaries; this counts indexed bodies.
	readRemaining := domain.MaxAgentHistoryPageBytes
	classified := map[int]domain.CIREvent{}
	readEvent := func(item domain.DocEventIndex) (domain.CIREvent, error) {
		if projection {
			if ev, ok := classified[item.Index]; ok {
				return ev, nil
			}
			if item.Length > readRemaining {
				return domain.CIREvent{}, historyClassificationBudgetError()
			}
			readRemaining -= item.Length
		}
		raw, err := read(item.Offset, item.Length)
		if err != nil {
			return domain.CIREvent{}, err
		}
		ev, err := domain.DecodeIndexedEvent(raw, item)
		if err != nil {
			return ev, err
		}
		if string(ev.Role) != item.Role || ev.Seq != item.Seq {
			return ev, domain.ErrIntegrity
		}
		if projection {
			classified[item.Index] = ev
		}
		return ev, nil
	}
	// CIR can place a user-turn marker immediately before its prompt. V1 keeps
	// its small-marker look-behind; v2 must classify any adjacent user event
	// within the shared allowance so an oversized marker is never left behind.
	withMarker := func(start int) (int, error) {
		if start > 0 && idx.Events[start-1].Role == "user" && (projection || idx.Events[start-1].Length <= 1024) {
			item := idx.Events[start-1]
			ev, err := readEvent(item)
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
			if projection && errors.Is(err, domain.ErrContextBudgetExceeded) && (len(out.Turns) > 0 || out.OmittedTail != nil) {
				break
			}
			return out, err
		}
		canOmit := projection && out.Before == out.Total && end == out.Total
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
		if size > remaining && !canOmit {
			if len(out.Turns) == 0 && out.OmittedTail == nil {
				return out, historyTurnBudgetError()
			}
			break
		}
		if projection {
			needed := 0
			for _, item := range idx.Events[start:end] {
				if _, ok := classified[item.Index]; ok {
					continue
				}
				if item.Length > readRemaining-needed {
					needed = readRemaining + 1
					break
				}
				needed += item.Length
			}
			if needed > readRemaining {
				if len(out.Turns) == 0 && out.OmittedTail == nil {
					return out, historyClassificationBudgetError()
				}
				break
			}
		}
		turn := domain.AgentHistoryTurn{Start: start, End: end, Events: make([]domain.CIREvent, 0, end-start)}
		for _, item := range idx.Events[start:end] {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			ev, err := readEvent(item)
			if err != nil {
				return out, err
			}
			turn.Events = append(turn.Events, ev)
		}
		incomplete, err := classifyHistoryTurn(turn.Events, projection)
		if err != nil {
			return out, err
		}
		if incomplete {
			if !canOmit {
				return out, incompleteHistoryTurnError()
			}
			out.OmittedTail = &domain.AgentHistoryTail{Start: start, End: end, Reason: "incomplete_tool_pair"}
			end = start
			continue
		}
		raw, err := json.Marshal(turn.Events)
		if err != nil {
			return out, err
		}
		if len(raw) > remaining {
			if len(out.Turns) == 0 && out.OmittedTail == nil {
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

func historyClassificationBudgetError() error {
	return fmt.Errorf("%w: history classification exceeds the 4 MiB indexed-body read allowance; unclassified turns are not omitted (this is not a token limit)", domain.ErrContextBudgetExceeded)
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

func incompleteHistoryTurnError() error {
	return fmt.Errorf("%w: incomplete historical tool pair", domain.ErrIntegrity)
}

// A pending call is a tail candidate only after all events pass validation.
// An earlier outstanding call must never hide a later duplicate or orphan.
func classifyHistoryTurn(events []domain.CIREvent, projection bool) (bool, error) {
	calls := map[string]bool{}
	for i, ev := range events {
		if projection {
			if ev.Seq < 0 || i > 0 && ev.Seq <= events[i-1].Seq {
				return false, fmt.Errorf("%w: invalid historical event sequence", domain.ErrIntegrity)
			}
			switch ev.Kind {
			case domain.EventTurn, domain.EventMessage, domain.EventToolCall, domain.EventToolResult, domain.EventReasoning, domain.EventCompaction:
			default:
				return false, fmt.Errorf("%w: unknown historical event kind", domain.ErrIntegrity)
			}
			// Omitted events never reach the final turn marshal or the client's
			// wire validator. Check the CIR union here as well, without exposing
			// private event data in an error. Strict v1 keeps its old checks.
			if _, err := json.Marshal(ev); err != nil {
				return false, fmt.Errorf("%w: invalid historical event", domain.ErrIntegrity)
			}
		}
		if ev.Role == "user" && (ev.Kind != domain.EventMessage && ev.Kind != domain.EventTurn || i > 0 && !(i == 1 && events[0].Kind == domain.EventTurn && ev.Kind == domain.EventMessage)) {
			return false, fmt.Errorf("%w: invalid user turn boundary", domain.ErrIntegrity)
		}
		switch ev.Kind {
		case domain.EventToolCall:
			if _, found := calls[ev.CallID]; found || ev.CallID == "" {
				return false, fmt.Errorf("%w: duplicate or missing tool call identity", domain.ErrIntegrity)
			}
			calls[ev.CallID] = false
		case domain.EventToolResult:
			done, found := calls[ev.CallID]
			if !found || done {
				return false, fmt.Errorf("%w: unmatched tool result", domain.ErrIntegrity)
			}
			calls[ev.CallID] = true
		}
	}
	for _, done := range calls {
		if !done {
			return true, nil
		}
	}
	return false, nil
}
