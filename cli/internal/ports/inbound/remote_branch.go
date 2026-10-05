package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// RemoteBranchObservation keeps a branch and its history from one verified
// fetch together. It does not grant permission to adopt either locally, and
// separate server reads are not a transactional revision.
type RemoteBranchObservation struct {
	Ref       domain.Ref
	History   []domain.HistoryEvent
	Snapshots []domain.Snapshot
}

// RemoteBranchObserver supplies the evidence required for tracking attachment.
// A ref-only resolver cannot prove the identity or historical memory selection.
type RemoteBranchObserver interface {
	ResolveRemoteBranchObservation(context.Context, SyncInput, string) (RemoteBranchObservation, error)
}

// TrackingAttachmentService separates verified preparation from a durable
// local command. The caller journals the prepared proof before applying it.
type TrackingAttachmentService interface {
	PrepareTrackingAttachment(context.Context, domain.HistoryEvent, RemoteBranchObservation, []string) (domain.TrackingAttachment, error)
	ApplyTrackingAttachment(context.Context, TrackingAttachmentInput) error
}
type TrackingAttachmentInput struct {
	Attachment       domain.TrackingAttachment
	ExpectedRef      *domain.Ref
	SelectPosition   bool
	ExpectedPosition *domain.WorkingPosition
}
