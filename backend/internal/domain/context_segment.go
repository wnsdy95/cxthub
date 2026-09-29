package domain

import (
	"context"
	"encoding/json"
	"fmt"
)

const ContextSegmentVersion = 1
const MaxContextSegmentPage = 50
const MaxContextSegmentPrefixEvents = 250000

// ConversationSegment addresses a half-open range of canonical CIR events. Its
// identity includes repository and native stream provenance, never prose alone.
// Byte chunks remain an independent physical storage detail.
type ConversationSegment struct {
	ID         ContentHash  `json:"id"`
	RepoID     ContentHash  `json:"repo_id"`
	DocHash    ContentHash  `json:"doc_hash"`
	Provider   ProviderKind `json:"provider"`
	SessionID  string       `json:"session_id"`
	EventStart int          `json:"event_start"`
	EventEnd   int          `json:"event_end"`
}

// CommitContextBinding reuses an accepted immutable publication receipt. It
// attests to observed finalized captures, not every possible worker's activity.
type CommitContextBinding struct {
	EventID    string        `json:"event_id"`
	GitCommit  string        `json:"git_commit"`
	BranchID   string        `json:"branch_id"`
	WorktreeID string        `json:"worktree_id,omitempty"`
	SegmentIDs []ContentHash `json:"segment_ids"`
}

type ContextSegmentCoverage struct {
	SnapshotID         ContentHash            `json:"snapshot_id"`
	Segment            *ConversationSegment   `json:"segment,omitempty"`
	Bindings           []CommitContextBinding `json:"bindings"`
	Kind               string                 `json:"kind"` // verified_prefix, full_source, unavailable
	Reason             string                 `json:"reason"`
	TotalEvents        int                    `json:"total_events"`
	BaselineSnapshotID ContentHash            `json:"baseline_snapshot_id,omitempty"`
	BaselineDocHash    ContentHash            `json:"baseline_doc_hash,omitempty"`
}

// Pages follow the shared query's snapshot order, newest first. A verified
// prefix can refer to an older page; consumers must preserve that dependency.
// Complete means page exhaustion, not complete Git or capture coverage.
type ContextSegmentPage struct {
	Version   int                      `json:"version"`
	StateHash ContentHash              `json:"state_hash"`
	PageHash  ContentHash              `json:"page_hash"`
	Offset    int                      `json:"offset"`
	Next      int                      `json:"next"`
	Total     int                      `json:"total"`
	Complete  bool                     `json:"complete"`
	Entries   []ContextSegmentCoverage `json:"entries"`
}

func ValidateContextSegmentSelection(in ContextSelection) error {
	if in.SegmentLimit < 0 || in.SegmentLimit > MaxContextSegmentPage || in.SegmentOffset < 0 || (in.SegmentLimit == 0 && (in.SegmentOffset != 0 || in.SegmentStateHash != "")) {
		return fmt.Errorf("%w: segment_limit must be 1..%d for paged segments", ErrValidation, MaxContextSegmentPage)
	}
	if in.SegmentOffset > 0 && in.SegmentStateHash == "" {
		return fmt.Errorf("%w: segment continuation requires segment_state_hash", ErrValidation)
	}
	return ValidateOptionalContentHash(in.SegmentStateHash)
}

// ProjectConversationSegment requires indexes derived from owned verified
// documents. Only a complete prefix of the same native provider/session may be
// omitted. A matching message, mutable graft, or snapshot label is not proof.
func ProjectConversationSegment(ctx context.Context, snap Snapshot, idx DocReadIndex, bindings []CommitContextBinding, base *Snapshot, previous *DocReadIndex) (ContextSegmentCoverage, error) {
	out := ContextSegmentCoverage{SnapshotID: snap.ID, Kind: "full_source", Reason: "no_finalized_publication", TotalEvents: len(idx.Events), Bindings: append([]CommitContextBinding{}, bindings...)}
	if err := validateSegmentIndex(ctx, snap, idx); err != nil {
		return out, err
	}
	start := 0
	if len(bindings) != 0 {
		out.Reason = "no_verified_selected_baseline"
		if base != nil && previous != nil {
			if err := validateSegmentIndex(ctx, *base, *previous); err != nil {
				return out, err
			}
			natural := false
			for _, id := range snap.Parents {
				natural = natural || id == base.ID
			}
			sameStream := idx.Envelope.SourceProvider != "" && idx.Envelope.SessionOriginID != "" && idx.Envelope.SourceProvider == previous.Envelope.SourceProvider && idx.Envelope.SessionOriginID == previous.Envelope.SessionOriginID
			if natural && sameStream && base.RepoID == snap.RepoID && base.ID != snap.ID {
				out.Reason = "source_generation_changed"
				if len(previous.Events) <= len(idx.Events) && len(previous.Events) <= MaxContextSegmentPrefixEvents {
					equal := true
					for i, ev := range previous.Events {
						if i%1024 == 0 {
							if err := ctx.Err(); err != nil {
								return out, err
							}
						}
						if ev.Hash != idx.Events[i].Hash {
							equal = false
							break
						}
					}
					if equal {
						start = len(previous.Events)
						out.Kind, out.Reason = "verified_prefix", "finalized_natural_parent_prefix"
						out.BaselineSnapshotID, out.BaselineDocHash = base.ID, base.DocHash
					}
				} else if len(previous.Events) > MaxContextSegmentPrefixEvents {
					out.Reason = "prefix_verification_budget"
				}
			}
		}
	}
	segment := ConversationSegment{RepoID: snap.RepoID, DocHash: snap.DocHash, Provider: idx.Envelope.SourceProvider, SessionID: idx.Envelope.SessionOriginID, EventStart: start, EventEnd: len(idx.Events)}
	raw, err := json.Marshal(struct {
		Version int
		Segment ConversationSegment
	}{ContextSegmentVersion, segment})
	if err != nil {
		return out, err
	}
	segment.ID = HashContent(raw)
	out.Segment = &segment
	for i := range out.Bindings {
		out.Bindings[i].SegmentIDs = []ContentHash{segment.ID}
	}
	return out, nil
}

func validateSegmentIndex(ctx context.Context, snap Snapshot, idx DocReadIndex) error {
	if idx.Version != 1 || idx.Hash != snap.DocHash || ValidateContentHash(snap.ID) != nil || ValidateContentHash(snap.RepoID) != nil || ValidateContentHash(snap.DocHash) != nil {
		return ErrIntegrity
	}
	offset := 0
	for i, ev := range idx.Events {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if ev.Index != i || ev.Offset != offset || ev.Length < 1 || ValidateContentHash(ev.Hash) != nil {
			return ErrIntegrity
		}
		next := offset + ev.Length + 1
		if next <= offset {
			return ErrIntegrity
		}
		offset = next
	}
	return ctx.Err()
}
