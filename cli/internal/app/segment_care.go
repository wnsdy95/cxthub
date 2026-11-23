package app

import (
	"context"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// SegmentCareService finds and puts right what is wrong between the sessions
// on the server and the records on this computer. Every method says what it
// found or did; none of them decides anything from a hook having run.
type SegmentCareService struct {
	records outbound.SessionUploadRecords
	// The rest is set by WithServer.
	removals outbound.AnchoredRemovalRemote
	repo     func(ctx context.Context, cwd string) (domain.Repo, error)
	uploader inbound.SegmentUploader
	clock    func() time.Time
}

// NewSegmentCareService creates the service over the records kept in the
// repository's .cxt folder.
func NewSegmentCareService(records outbound.SessionUploadRecords) *SegmentCareService {
	return &SegmentCareService{records: records}
}

// WithServer adds the repository's server, for what changes a session there.
// repo names the server repository of the folder; uploader sends a session
// again after it was allowed again.
func (s *SegmentCareService) WithServer(removals outbound.AnchoredRemovalRemote, repo func(context.Context, string) (domain.Repo, error), uploader inbound.SegmentUploader, clock func() time.Time) *SegmentCareService {
	s.removals, s.repo, s.uploader, s.clock = removals, repo, uploader, clock
	return s
}

var _ inbound.SegmentCare = (*SegmentCareService)(nil)

// logAction writes what was done to a session into its log on this computer.
// The log is a record; failing to write it does not undo what was done.
func (s *SegmentCareService) logAction(ctx context.Context, cwd string, key domain.SessionUploadKey, action string) {
	if s.records == nil || s.clock == nil {
		return
	}
	_ = s.records.LogSessionAction(context.WithoutCancel(ctx), cwd, key, domain.SessionUploadLogEntry{At: s.clock().UTC().Format(time.RFC3339Nano), Action: action})
}

// RemoveSession removes a session from the server for every reader, with
// the sessions it spawned. Nothing on this computer is removed: the tool's
// records and the copies in .cxt stay.
func (s *SegmentCareService) RemoveSession(ctx context.Context, cwd string, key domain.SessionUploadKey) (domain.SegmentRemoveResult, error) {
	result := domain.SegmentRemoveResult{Version: 1, Removed: []domain.AnchoredSessionRemoval{}}
	if s.removals == nil || s.repo == nil {
		return result, fmt.Errorf("the repository's server is unavailable")
	}
	repo, err := s.repo(ctx, cwd)
	if err != nil {
		return result, err
	}
	removed, err := s.removals.RemoveAnchoredSession(ctx, string(repo.ID), key.Provider, key.SessionID)
	if err != nil {
		return result, fmt.Errorf("%s %s was not removed: %w", key.Provider, key.SessionID, err)
	}
	for _, each := range removed {
		s.logAction(ctx, cwd, domain.SessionUploadKey{Provider: each.Provider, SessionID: each.SessionID}, "removed from the server")
	}
	result.Removed = append(result.Removed, removed...)
	return result, nil
}

// AllowSession takes back the removal of one session and uploads it again.
// A session that was removed together with the one that spawned it has a
// removal of its own and is allowed by its own name.
func (s *SegmentCareService) AllowSession(ctx context.Context, cwd string, key domain.SessionUploadKey) (domain.SegmentAllowResult, error) {
	result := domain.SegmentAllowResult{Version: 1, Allowed: key}
	if s.removals == nil || s.repo == nil || s.uploader == nil {
		return result, fmt.Errorf("the repository's server is unavailable")
	}
	repo, err := s.repo(ctx, cwd)
	if err != nil {
		return result, err
	}
	if err := s.removals.AllowAnchoredSession(ctx, string(repo.ID), key.Provider, key.SessionID); err != nil {
		return result, fmt.Errorf("%s %s was not allowed again: %w", key.Provider, key.SessionID, err)
	}
	s.logAction(ctx, cwd, key, "allowed again on the server")
	// Nothing comes back by itself; the session is sent again now.
	result.Upload, err = s.uploader.PushSegmentsOf(ctx, cwd, []domain.SessionUploadKey{key})
	if err != nil {
		return result, fmt.Errorf("%s %s is allowed again, but uploading it failed: %w", key.Provider, key.SessionID, err)
	}
	return result, nil
}

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
