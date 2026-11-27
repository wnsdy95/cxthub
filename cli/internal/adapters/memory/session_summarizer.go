package memory

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// SessionSummarizer writes a session down at points of its conversation with
// the distiller and the memory sources a capture uses.
type SessionSummarizer struct {
	sources   map[domain.ProviderKind]outbound.MemorySource
	distiller outbound.MemoryDistiller
	mask      func(folder string, native domain.NativeMemory) domain.NativeMemory
}

// NewSessionSummarizer creates the summarizer. mask hides secret values in
// the tool's own memory the way the conversation was masked.
func NewSessionSummarizer(sources map[domain.ProviderKind]outbound.MemorySource, distiller outbound.MemoryDistiller, mask func(folder string, native domain.NativeMemory) domain.NativeMemory) *SessionSummarizer {
	return &SessionSummarizer{sources: sources, distiller: distiller, mask: mask}
}

// Summarize distills the conversation up to each end. The tool's own memory
// is read once, as it is now: cxt may run long after the commit, and the tool
// keeps no earlier state of it. When it cannot be read, the conversation
// alone is distilled, as a capture does.
func (s *SessionSummarizer) Summarize(ctx context.Context, folder string, provider domain.ProviderKind, sessionID string, conversation domain.CIRDocument, ends []int) ([]domain.MemoryDigest, error) {
	if s.distiller == nil || s.mask == nil {
		return nil, fmt.Errorf("session summaries unavailable")
	}
	var native *domain.NativeMemory
	if source := s.sources[provider]; source != nil && folder != "" {
		if read, found, err := source.ReadNative(ctx, folder, sessionID); err == nil && found {
			masked := s.mask(folder, read)
			native = &masked
		}
	}
	digests := make([]domain.MemoryDigest, 0, len(ends))
	for _, end := range ends {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if end < 1 || end > len(conversation.Events) {
			return nil, fmt.Errorf("session %s: a summary point lies outside the conversation", sessionID)
		}
		upTo := conversation
		upTo.Events = conversation.Events[:end:end]
		digest, err := s.distiller.Distill(ctx, upTo.EffectiveContext(), native)
		if err != nil {
			return nil, fmt.Errorf("session %s: %w", sessionID, err)
		}
		digests = append(digests, digest)
	}
	return digests, nil
}

var _ outbound.SessionSummarizer = (*SessionSummarizer)(nil)
