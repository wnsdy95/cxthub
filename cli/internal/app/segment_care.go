package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// SegmentCareService finds and puts right what is wrong between the sessions
// on the server and the records on this computer. Every method says what it
// found or did; none of them decides anything from a hook having run.
type SegmentCareService struct {
	records outbound.SessionUploadRecords
}

// NewSegmentCareService creates the service over the records kept in the
// repository's .cxt folder.
func NewSegmentCareService(records outbound.SessionUploadRecords) *SegmentCareService {
	return &SegmentCareService{records: records}
}

var _ inbound.SegmentCare = (*SegmentCareService)(nil)

// UploadLog lists what is kept on this computer about the uploads of every
// session, each with its last log line, or of one session with every line.
func (s *SegmentCareService) UploadLog(ctx context.Context, cwd string, key *domain.SessionUploadKey) ([]domain.SessionUploadListing, error) {
	if s.records == nil {
		return nil, fmt.Errorf("upload records unavailable")
	}
	if key != nil {
		record, err := s.records.ReadSessionUpload(ctx, cwd, *key, false)
		if err != nil {
			return nil, fmt.Errorf("no upload record is kept for %s %s: %w", key.Provider, key.SessionID, err)
		}
		return []domain.SessionUploadListing{domain.SessionUploadListingOf(record, false)}, nil
	}
	keys, err := s.records.ListSessionUploads(ctx, cwd)
	if err != nil {
		return nil, err
	}
	out := make([]domain.SessionUploadListing, 0, len(keys))
	for _, each := range keys {
		record, err := s.records.ReadSessionUpload(ctx, cwd, each, false)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", each.Provider, each.SessionID, err)
		}
		out = append(out, domain.SessionUploadListingOf(record, true))
	}
	return out, nil
}
