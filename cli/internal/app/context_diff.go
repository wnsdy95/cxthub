package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// Diff compares authenticated stored captures. A fence after reading immutable
// bodies prevents a late pending replacement from producing mixed revisions.
func (s *WorkingStateService) Diff(ctx context.Context, in inbound.ContextDiffInput) (domain.ContextDiff, error) {
	if s.docs == nil {
		return domain.ContextDiff{}, fmt.Errorf("context_documents_unavailable")
	}
	for attempt := 0; attempt < 3; attempt++ {
		observation, err := s.stableWorkingObservation(ctx, in.Cwd)
		if err != nil {
			return domain.ContextDiff{}, err
		}
		result, diffErr := s.diffObservation(ctx, observation, in.Staged)
		after, err := s.readWorkingObservation(ctx, in.Cwd)
		if err != nil {
			return domain.ContextDiff{}, err
		}
		if after.State.Revision != observation.State.Revision {
			continue
		}
		return result, diffErr
	}
	return domain.ContextDiff{}, domain.ErrSelectionChanged
}

type contextDiffCandidate struct {
	Hash       domain.ContentHash
	SourceID   domain.ContentHash
	Generation domain.ContentHash
	Kind       string
	Rank       int
}

type diffDocuments struct {
	service *WorkingStateService
	docs    map[domain.ContentHash]domain.SessionDoc
}

func (d *diffDocuments) read(ctx context.Context, hash domain.ContentHash, provider domain.ProviderKind, session string) (domain.SessionDoc, error) {
	if err := ctx.Err(); err != nil {
		return domain.SessionDoc{}, err
	}
	doc, ok := d.docs[hash]
	if !ok {
		var err error
		doc, err = d.service.docs.GetDoc(ctx, hash)
		if err != nil {
			return doc, err
		}
		if doc.Hash != hash {
			return doc, domain.ErrHashMismatch
		}
		if err = domain.ValidateSessionDocHash(doc); err != nil {
			return doc, err
		}
		d.docs[hash] = doc
	}
	if doc.CIR.Envelope.SourceProvider != provider || doc.CIR.Envelope.SessionOriginID != session {
		return doc, domain.ErrHashMismatch
	}
	return doc, nil
}

func (s *WorkingStateService) diffObservation(ctx context.Context, o workingObservation, staged bool) (domain.ContextDiff, error) {
	out := domain.ContextDiff{Version: domain.WorkingStateVersion, Mode: "observed", Selection: o.State.Selection, Freshness: o.State.Freshness, BeforeRevision: o.State.HistoryRevision, AfterRevision: o.State.Revision, Coverage: o.State.Coverage, Gaps: append([]string{}, o.State.Gaps...), Changes: []domain.ContextChange{}}
	if staged {
		out.Mode = "staged"
		out.AfterRevision = o.Index.Revision
	} else if len(o.Index.Entries) > 0 {
		out.BeforeRevision = o.Index.Revision
	}
	// This revision fences the entire comparison, including receipt changes that
	// can alter baseline selection without changing the HEAD or index hashes.
	out.Revision = o.State.Revision
	docs := diffDocuments{service: s, docs: map[domain.ContentHash]domain.SessionDoc{}}
	if staged {
		for _, e := range o.Index.Entries {
			candidates := finalizedCandidates(o, e.Provider, e.SessionID, e.SourceID, e.Key)
			change, err := docs.compare(ctx, o, e.DocHash, e.Provider, e.SessionID, candidates, e.SourceID, e.Generation)
			if err != nil {
				return out, err
			}
			if doc, ok := docs.docs[e.DocHash]; ok && len(doc.CIR.Events) != e.Events {
				return out, domain.ErrHashMismatch
			}
			out.Changes = append(out.Changes, change)
		}
	} else {
		for _, p := range o.Pending {
			candidates := []contextDiffCandidate{}
			for _, e := range o.Index.Entries {
				if e.Provider == p.Provider && e.SessionID == p.SessionID {
					candidates = append(candidates, contextDiffCandidate{Hash: e.DocHash, SourceID: e.SourceID, Generation: e.Generation, Kind: "staged", Rank: 0})
				}
			}
			if len(candidates) == 0 {
				candidates = finalizedCandidates(o, p.Provider, p.SessionID, "", "")
			}
			change, err := docs.compare(ctx, o, p.Target, p.Provider, p.SessionID, candidates, "", "")
			if err != nil {
				return out, err
			}
			out.Changes = append(out.Changes, change)
		}
		if len(o.Pending) == 0 {
			out.Gaps = append(out.Gaps, "no_stored_pending_observation")
		}
	}
	for _, c := range out.Changes {
		if c.State == "unavailable" || c.State == "unknown" {
			out.Gaps = append(out.Gaps, c.Reason)
		}
	}
	sort.Strings(out.Gaps)
	// Stable comparison receipt does not contain raw provider text.
	raw, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	out.Revision = domain.HashContent(raw)
	return out, nil
}

func finalizedCandidates(o workingObservation, provider domain.ProviderKind, session string, source, key domain.ContentHash) []contextDiffCandidate {
	ranks := map[domain.ContentHash]int{}
	for i, s := range o.History.Snapshots {
		ranks[s.ID] = i
	}
	exact, related := []contextDiffCandidate{}, []contextDiffCandidate{}
	for _, op := range o.Commits {
		rank, selected := ranks[op.Position.Snapshot]
		if !op.LocalFinalized || !selected {
			continue
		}
		for _, e := range op.Index.Entries {
			if e.Provider != provider || e.SessionID != session || (source != "" && e.SourceID != source) {
				continue
			}
			c := contextDiffCandidate{Hash: e.DocHash, SourceID: e.SourceID, Generation: e.Generation, Kind: "finalized_index", Rank: rank}
			if key != "" && e.Key == key {
				exact = append(exact, c)
			} else {
				related = append(related, c)
			}
		}
	}
	if len(exact) > 0 {
		return latestDiffCandidates(exact)
	}
	if len(related) > 0 {
		return latestDiffCandidates(related)
	}
	// Legacy captures have provider/session identity but not a provider file
	// fingerprint. They can prove an exact prefix, never a cross-source rewrite.
	for i, s := range o.History.Snapshots {
		if s.Provider == provider && s.SessionID == session {
			related = append(related, contextDiffCandidate{Hash: s.DocHash, Kind: "selected_context", Rank: i})
			// Compare with the latest selected capture, not an older matching
			// prefix that could conceal a subsequent rewrite. This also avoids
			// opening every cumulative version of a long session.
			break
		}
	}
	return related
}

func latestDiffCandidates(in []contextDiffCandidate) []contextDiffCandidate {
	// Keep the most recent selected operation for each exact source generation;
	// separate sources/generations must remain distinguishable for ambiguity checks.
	sort.SliceStable(in, func(i, j int) bool { return in[i].Rank < in[j].Rank })
	seen := map[string]bool{}
	out := []contextDiffCandidate{}
	for _, v := range in {
		key := string(v.SourceID) + "\x00" + string(v.Generation)
		if !seen[key] {
			out = append(out, v)
			seen[key] = true
		}
	}
	return out
}

func (d *diffDocuments) compare(ctx context.Context, o workingObservation, hash domain.ContentHash, provider domain.ProviderKind, session string, candidates []contextDiffCandidate, source, generation domain.ContentHash) (domain.ContextChange, error) {
	unknown := domain.ContextChange{Provider: provider, SessionID: session, SourceID: source, Generation: generation, After: hash, State: "unavailable", Baseline: "unknown", Reason: "stored_document_missing", Added: domain.EventRange{Kinds: map[string]int{}}, Removed: domain.EventRange{Kinds: map[string]int{}}}
	after, err := d.read(ctx, hash, provider, session)
	if errors.Is(err, domain.ErrNotFound) {
		return unknown, nil
	}
	if err != nil {
		return unknown, err
	}
	unknown.AfterEvents = len(after.CIR.Events)
	best := -1
	bestCount := -1
	var bestChange domain.ContextChange
	distinctSources := map[domain.ContentHash]bool{}
	missing := false
	for _, c := range candidates {
		if c.SourceID != "" {
			distinctSources[c.SourceID] = true
		}
	}
	// Pending records do not attest which provider file supplied a reused session
	// ID. Even identical text from distinct staged files cannot disambiguate it.
	if len(distinctSources) > 1 {
		unknown.State = "unknown"
		unknown.Reason = "ambiguous_capture_source"
		return unknown, nil
	}
	for i, c := range candidates {
		before, err := d.read(ctx, c.Hash, provider, session)
		if errors.Is(err, domain.ErrNotFound) {
			missing = true
			continue
		}
		if err != nil {
			return unknown, err
		}
		change, err := domain.CompareContextDocuments(&before, after)
		if err != nil {
			return unknown, err
		}
		if (change.State == "unchanged" || change.State == "extended") && change.BeforeEvents > bestCount {
			best = i
			bestCount = change.BeforeEvents
			bestChange = change
		}
	}
	if missing {
		unknown.Reason = "baseline_document_missing"
		return unknown, nil
	}
	if best < 0 && len(candidates) == 1 {
		before, err := d.read(ctx, candidates[0].Hash, provider, session)
		if err != nil {
			return unknown, err
		}
		bestChange, err = domain.CompareContextDocuments(&before, after)
		if err != nil {
			return unknown, err
		}
		best = 0
	}
	if best >= 0 {
		c := candidates[best]
		bestChange.Baseline = c.Kind
		bestChange.SourceID = source
		bestChange.Generation = generation
		if source == "" {
			bestChange.SourceID = c.SourceID
			bestChange.Generation = c.Generation
		}
		if c.Kind == "selected_context" && bestChange.State == "replacement" {
			bestChange.Reason = "source_generation_unrecorded"
		}
		return bestChange, nil
	}
	if len(candidates) > 0 {
		unknown.State = "unknown"
		unknown.Reason = "ambiguous_source_generation"
		return unknown, nil
	}
	if !o.History.Complete {
		unknown.State = "unknown"
		unknown.Reason = "history_incomplete"
		return unknown, nil
	}
	change, err := domain.CompareContextDocuments(nil, after)
	change.Baseline = "empty_selected_coverage"
	change.SourceID = source
	change.Generation = generation
	return change, err
}
