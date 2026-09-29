package domain

import (
	"bytes"
	"fmt"
)

// EventRange uses canonical event offsets, not transcript byte offsets. Kind
// counts include archival boundary events; replacements remain part of that event.
type EventRange struct {
	Start int            `json:"start"`
	End   int            `json:"end"`
	Kinds map[string]int `json:"kinds"`
}

type ContextChange struct {
	CountsKnown  bool         `json:"counts_known"`
	Provider     ProviderKind `json:"provider"`
	SessionID    string       `json:"session_id"`
	SourceID     ContentHash  `json:"source_id,omitempty"`
	Generation   ContentHash  `json:"generation,omitempty"`
	Before       ContentHash  `json:"before,omitempty"`
	After        ContentHash  `json:"after"`
	State        string       `json:"state"`
	Baseline     string       `json:"baseline"`
	Reason       string       `json:"reason,omitempty"`
	BeforeEvents int          `json:"before_events"`
	AfterEvents  int          `json:"after_events"`
	Added        EventRange   `json:"added"`
	Removed      EventRange   `json:"removed"`
}

type ContextDiff struct {
	Version        int                  `json:"version"`
	Revision       ContentHash          `json:"revision"`
	Mode           string               `json:"mode"`
	Selection      WorkingSelection     `json:"selection"`
	Freshness      ObservationFreshness `json:"freshness"`
	BeforeRevision ContentHash          `json:"before_revision"`
	AfterRevision  ContentHash          `json:"after_revision"`
	Coverage       string               `json:"coverage"`
	Gaps           []string             `json:"gaps"`
	Changes        []ContextChange      `json:"changes"`
}

// CompareContextDocuments never coalesces equal text across source identities.
// Canonical full events must match (including tool calls, locked state and IDs)
// before a suffix can be called an extension. Divergence is a replacement, not
// a bag-of-text diff. The caller supplies provenance and baseline selection.
func CompareContextDocuments(before *SessionDoc, after SessionDoc) (ContextChange, error) {
	out := ContextChange{Provider: after.CIR.Envelope.SourceProvider, SessionID: after.CIR.Envelope.SessionOriginID, After: after.Hash}
	if err := ValidateSessionDocHash(after); err != nil {
		return out, err
	}
	if out.SessionID == "" {
		return out, fmt.Errorf("%w: missing source session identity", ErrHashMismatch)
	}
	next := canonicalEvents(after.CIR.Events)
	out.CountsKnown = true
	out.AfterEvents = len(next)
	out.Added, out.Removed = eventRange(next, 0, 0), eventRange(nil, 0, 0)
	if before == nil {
		out.State = "new_source"
		out.Added = eventRange(next, 0, len(next))
		return out, nil
	}
	if err := ValidateSessionDocHash(*before); err != nil {
		return out, err
	}
	if before.CIR.Envelope.SourceProvider != out.Provider || before.CIR.Envelope.SessionOriginID != out.SessionID {
		return out, fmt.Errorf("%w: different source sessions cannot share coverage", ErrHashMismatch)
	}
	prior := canonicalEvents(before.CIR.Events)
	out.Before, out.BeforeEvents = before.Hash, len(prior)
	common := 0
	for common < len(prior) && common < len(next) {
		a, err := canonicalJSON(prior[common])
		if err != nil {
			return out, err
		}
		b, err := canonicalJSON(next[common])
		if err != nil {
			return out, err
		}
		if !bytes.Equal(a, b) {
			break
		}
		common++
	}
	switch {
	case common == len(prior) && common == len(next):
		out.State = "unchanged"
	case common == len(prior):
		out.State = "extended"
	case common == len(next):
		out.State = "older_observation"
	default:
		out.State = "replacement"
	}
	out.Added = eventRange(next, common, len(next))
	out.Removed = eventRange(prior, common, len(prior))
	return out, nil
}

func eventRange(events []Event, start, end int) EventRange {
	out := EventRange{Start: start, End: end, Kinds: map[string]int{}}
	for _, e := range events[start:end] {
		out.Kinds[e.Kind]++
	}
	return out
}
