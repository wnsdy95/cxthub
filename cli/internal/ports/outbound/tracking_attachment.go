package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// TrackingAttachmentCommit applies one frozen tracking operation. Nil expected
// values mean absence, never an unchecked overwrite. A nil Position leaves all
// worktree selections unchanged.
type TrackingAttachmentCommit struct {
	// RequirePristine is setup-only authority; the final store check and acceptance share one lock.
	RequirePristine   bool                      `json:"require_pristine,omitempty"`
	ObservedSnapshots []domain.Snapshot         `json:"observed_snapshots,omitempty"`
	Attachment        domain.TrackingAttachment `json:"attachment"`
	ExpectedRef       *domain.Ref               `json:"expected_ref,omitempty"`
	Position          *TrackingPositionCAS      `json:"position,omitempty"`
}

type TrackingPositionCAS struct {
	Expected *domain.WorkingPosition `json:"expected,omitempty"`
	Next     domain.WorkingPosition  `json:"next"`
}

type TrackingAttachmentStore interface {
	CommitTrackingAttachment(context.Context, TrackingAttachmentCommit) error
}

// TrackingAttachmentPins is a passive GC check. An invalid pending journal
// returns an error; collection must not guess which unfinished objects it owns.
type TrackingAttachmentPins interface {
	HasTrackingAttachmentPin(context.Context, domain.ContentHash) (bool, error)
}

// CaptureTrackingGate prevents a first setup attachment from crossing a capture's
// ancestry read and durable writes. The callback must use the supplied context.
type CaptureTrackingGate interface {
	WithCaptureTrackingGate(context.Context, func(context.Context) error) error
}

// PristineTrackingStore is a preflight only. RequirePristine repeats this check
// under writer exclusion immediately before the existing journal's acceptance.
type PristineTrackingStore interface {
	TrackingPristine(context.Context, string) (bool, error)
}

// SetupHeadInitializer preserves an existing replica atomically on setup retry.
type SetupHeadInitializer interface {
	InitializeHeadIfAbsent(context.Context, domain.Ref) error
}

// InitialCapturePositionStore changes only an exactly empty init position under
// first-setup admission. It creates no context identity proof or server authority.
type InitialCapturePositionStore interface {
	InitializeCapturePosition(context.Context, *domain.WorkingPosition, domain.WorkingPosition) error
}
