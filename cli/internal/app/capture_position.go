package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// EnsureCapturePosition runs before capture takes its shared admission gate.
// The caller resolves repo from its connected GitContext and checks it again
// inside that gate before writing capture bytes.
func EnsureCapturePosition(ctx context.Context, store outbound.SessionStore, repo string) error {
	admission, ok := store.(outbound.CapturePositionStore)
	if !ok {
		return fmt.Errorf("capture position admission unavailable")
	}
	return admission.EnsureCapturePosition(ctx, repo)
}

func (s *ContextHistoryService) EnsureCapturePosition(ctx context.Context, repo string) error {
	return EnsureCapturePosition(ctx, s.store, repo)
}

func (s *ContextHistoryService) WithCaptureTrackingGate(ctx context.Context, fn func(context.Context) error) error {
	gate, ok := s.store.(outbound.CaptureTrackingGate)
	if !ok {
		return fmt.Errorf("capture tracking gate unavailable")
	}
	return gate.WithCaptureTrackingGate(ctx, fn)
}

var _ inbound.ContextCaptureAdmission = (*ContextHistoryService)(nil)
var _ inbound.ContextCaptureGate = (*ContextHistoryService)(nil)
