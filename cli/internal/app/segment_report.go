package app

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// SegmentReportService reads everything again on each call and computes the
// report from those records alone. It keeps nothing between calls and has no
// store, so it cannot write.
type SegmentReportService struct {
	git     outbound.GitRecordSource
	sources []outbound.SessionRecordSource
}

// NewSegmentReportService creates the service over Git and the given tools.
func NewSegmentReportService(git outbound.GitRecordSource, sources ...outbound.SessionRecordSource) *SegmentReportService {
	return &SegmentReportService{git: git, sources: sources}
}

var _ inbound.SegmentReporter = (*SegmentReportService)(nil)

// SegmentReport reports the repository holding cwd.
func (s *SegmentReportService) SegmentReport(ctx context.Context, cwd string) (domain.SegmentReport, error) {
	git, unread, err := s.git.ReadGitRecords(ctx, cwd)
	if err != nil {
		return domain.SegmentReport{}, err
	}
	inputs := domain.SegmentInputs{Git: git, Unread: unread}
	scope := git.ScopeOf()
	for _, source := range s.sources {
		batch, err := source.ReadSessionRecords(ctx, scope)
		if err != nil {
			return domain.SegmentReport{}, err
		}
		inputs.Sessions = append(inputs.Sessions, batch.Sessions...)
		inputs.Calls = append(inputs.Calls, batch.Calls...)
		inputs.Unread = append(inputs.Unread, batch.Unread...)
	}
	if err := ctx.Err(); err != nil {
		return domain.SegmentReport{}, err
	}
	return domain.BuildSegmentReport(inputs), nil
}
