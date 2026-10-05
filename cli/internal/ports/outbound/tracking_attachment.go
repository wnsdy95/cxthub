package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// TrackingAttachmentCommit applies one frozen tracking operation. Nil expected
// values mean absence, never an unchecked overwrite. A nil Position leaves all
// worktree selections unchanged.
type TrackingAttachmentCommit struct {
	Attachment  domain.TrackingAttachment `json:"attachment"`
	ExpectedRef *domain.Ref               `json:"expected_ref,omitempty"`
	Position    *TrackingPositionCAS      `json:"position,omitempty"`
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
