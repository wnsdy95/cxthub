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
	// The next four are set by WithServer.
	removals outbound.SegmentCareRemote
	repo     func(ctx context.Context, cwd string) (domain.Repo, error)
	uploader inbound.SegmentUploader
	clock    func() time.Time
	// reports, facts and onOrigin are set by WithLocalFacts.
	reports  inbound.SegmentReporter
	facts    func(ctx context.Context, cwd string) SegmentLocalFacts
	onOrigin func(ctx context.Context, cwd string, commits []string) map[string]bool
}

// SegmentLocalFacts is what this computer says about uploading from the
// repository, read when the diagnosis runs.
type SegmentLocalFacts struct {
	// SetUp says cxt is set up in the repository; AutoOn that automatic
	// upload is not turned off; Connected that a server is configured.
	SetUp, AutoOn, Connected bool
	// Collector is the collector's memory; nil when it never ran here.
	Collector *domain.CollectorState
}

// NewSegmentCareService creates the service over the records kept in the
// repository's .cxt folder.
func NewSegmentCareService(records outbound.SessionUploadRecords) *SegmentCareService {
	return &SegmentCareService{records: records}
}

// WithServer adds the repository's server, for what changes a session there.
// repo names the server repository of the folder; uploader sends a session
// again after it was allowed again.
func (s *SegmentCareService) WithServer(removals outbound.SegmentCareRemote, repo func(context.Context, string) (domain.Repo, error), uploader inbound.SegmentUploader, clock func() time.Time) *SegmentCareService {
	s.removals, s.repo, s.uploader, s.clock = removals, repo, uploader, clock
	return s
}

// WithLocalFacts adds what the diagnosis reads on this computer: the tools'
// records and Git through reports, the state of uploading through facts, and
// which commits a remote-tracking branch of origin holds through onOrigin.
func (s *SegmentCareService) WithLocalFacts(reports inbound.SegmentReporter, facts func(context.Context, string) SegmentLocalFacts, onOrigin func(context.Context, string, []string) map[string]bool) *SegmentCareService {
	s.reports, s.facts, s.onOrigin = reports, facts, onOrigin
	return s
}

// segmentDiagnosisSummaryCommits bounds how many commits the diagnosis asks
// the server about summaries: one request each.
const segmentDiagnosisSummaryCommits = 20

// Diagnose reads everything again, on this computer and on the server, and
// says what does not fit, each with the command that puts it right. It
// changes nothing anywhere. A server that cannot be read is itself a finding;
// what this computer alone shows is still diagnosed.
func (s *SegmentCareService) Diagnose(ctx context.Context, cwd string) (domain.SegmentDiagnosis, error) {
	if s.reports == nil || s.facts == nil || s.clock == nil {
		return domain.SegmentDiagnosis{}, fmt.Errorf("diagnosis unavailable")
	}
	report, err := s.reports.SegmentReport(ctx, cwd)
	if err != nil {
		return domain.SegmentDiagnosis{}, err
	}
	facts := s.facts(ctx, cwd)
	in := domain.SegmentDiagnosisInput{Now: s.clock().UTC(), SetUp: facts.SetUp, AutoOn: facts.AutoOn, Connected: facts.Connected, Collector: facts.Collector, Report: report}
	if s.records != nil {
		keys, err := s.records.ListSessionUploads(ctx, cwd)
		if err != nil {
			return domain.SegmentDiagnosis{}, err
		}
		for _, key := range keys {
			record, err := s.records.ReadSessionUpload(ctx, cwd, key, false)
			if err != nil {
				return domain.SegmentDiagnosis{}, fmt.Errorf("%s %s: %w", key.Provider, key.SessionID, err)
			}
			in.Kept = append(in.Kept, domain.SessionUploadListingOf(record, false))
		}
	}
	if facts.Connected && s.removals != nil && s.repo != nil {
		server, err := s.readServer(ctx, cwd, report)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return domain.SegmentDiagnosis{}, ctxErr
		}
		if err != nil {
			in.ServerError = err.Error()
		} else {
			in.Server = server
			if s.onOrigin != nil {
				var commits []string
				for _, session := range server.Sessions {
					for _, commit := range session.Commits {
						commits = append(commits, commit.SHA)
					}
				}
				in.OnOrigin = s.onOrigin(ctx, cwd, commits)
			}
		}
	}
	return domain.DiagnoseSegments(in), nil
}

// readServer reads what the diagnosis compares with: the caller's sessions,
// their removals, when the server last read GitHub, and the summaries at the
// newest commits of this computer's sessions.
func (s *SegmentCareService) readServer(ctx context.Context, cwd string, report domain.SegmentReport) (*domain.SegmentDiagnosisServer, error) {
	repo, err := s.repo(ctx, cwd)
	if err != nil {
		return nil, err
	}
	id := string(repo.ID)
	server := &domain.SegmentDiagnosisServer{Summarized: map[string]map[string]bool{}}
	if server.Sessions, err = s.removals.ListAnchoredSessions(ctx, id); err != nil {
		return nil, err
	}
	if server.Removed, err = s.removals.ListRemovedAnchoredSessions(ctx, id); err != nil {
		return nil, err
	}
	if server.RemoteSyncedAt, err = s.removals.ReadRemoteGitSyncedAt(ctx, id); err != nil {
		return nil, err
	}
	// The newest commits first: the report lists sessions oldest first.
	for index := len(report.Sessions) - 1; index >= 0 && len(server.Summarized) < segmentDiagnosisSummaryCommits; index-- {
		segments := report.Sessions[index].Segments
		for at := len(segments) - 1; at >= 0 && len(server.Summarized) < segmentDiagnosisSummaryCommits; at-- {
			for _, commit := range segments[at].Commits {
				if _, asked := server.Summarized[commit.SHA]; asked {
					continue
				}
				summaries, err := s.removals.ListAnchoredCommitSummaries(ctx, id, commit.SHA)
				if err != nil {
					return nil, err
				}
				have := map[string]bool{}
				for _, summary := range summaries {
					have[domain.CollectorSessionKey(summary.Provider, summary.SessionID)] = true
				}
				server.Summarized[commit.SHA] = have
			}
		}
	}
	return server, nil
}

// RefreshRemote asks the server to read the repository from GitHub again
// soon, so that a push it has not seen opens what it explains.
func (s *SegmentCareService) RefreshRemote(ctx context.Context, cwd string) error {
	if s.removals == nil || s.repo == nil {
		return fmt.Errorf("the repository's server is unavailable")
	}
	repo, err := s.repo(ctx, cwd)
	if err != nil {
		return err
	}
	return s.removals.RequestRemoteGitRefresh(ctx, string(repo.ID))
}

// SetArchived archives a session on the server or restores it, as the
// Sessions tab does.
func (s *SegmentCareService) SetArchived(ctx context.Context, cwd string, key domain.SessionUploadKey, archived bool) error {
	if s.removals == nil || s.repo == nil {
		return fmt.Errorf("the repository's server is unavailable")
	}
	repo, err := s.repo(ctx, cwd)
	if err != nil {
		return err
	}
	if err := s.removals.SetAnchoredSessionArchived(ctx, string(repo.ID), key.Provider, key.SessionID, archived); err != nil {
		return err
	}
	action := "restored from the archive on the server"
	if archived {
		action = "archived on the server"
	}
	s.logAction(ctx, cwd, key, action)
	return nil
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
