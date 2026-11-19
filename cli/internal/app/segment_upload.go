package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// SegmentUploadService uploads the report SegmentReportService computes, one
// report per session. Like the report it reads everything again on each call
// and keeps nothing on this computer.
type SegmentUploadService struct {
	reports *SegmentReportService
	remote  outbound.AnchoredSessionRemote
	repo    func(ctx context.Context, cwd string) (domain.Repo, error)
	machine string
	// contentRemote and content are set together by WithSessionContent.
	contentRemote outbound.SessionContentRemote
	content       map[domain.ProviderKind]outbound.SessionContentSource
}

// NewSegmentUploadService creates the service. machine is the label the
// uploader's own computers are told apart by; it may be empty.
func NewSegmentUploadService(reports *SegmentReportService, remote outbound.AnchoredSessionRemote, repo func(context.Context, string) (domain.Repo, error), machine string) *SegmentUploadService {
	return &SegmentUploadService{reports: reports, remote: remote, repo: repo, machine: machine}
}

// WithSessionContent also uploads the conversation of each session's own
// records, read back by the given tool's source, and names it in the report.
// A tool without a source gets its report without content, and says so.
func (s *SegmentUploadService) WithSessionContent(remote outbound.SessionContentRemote, sources map[domain.ProviderKind]outbound.SessionContentSource) *SegmentUploadService {
	s.contentRemote, s.content = remote, sources
	return s
}

var _ inbound.SegmentUploader = (*SegmentUploadService)(nil)

// refusedSessionReport reports whether the server refused this one report
// rather than the caller: the identifier belongs to another account, or the
// report is too large or not well formed. Other sessions can still be stored.
func refusedSessionReport(err error) bool {
	var status statusCodeError
	if !errors.As(err, &status) {
		return false
	}
	code := status.StatusCode()
	return code == 409 || code == 413 || code == 422
}

// PushSegments uploads every session of the repository holding cwd.
func (s *SegmentUploadService) PushSegments(ctx context.Context, cwd string) (domain.SegmentPushResult, error) {
	result := domain.SegmentPushResult{Version: 1, Sessions: []domain.SegmentPushSession{}, Unread: []domain.UnreadSessionRecord{}}
	if s.reports == nil || s.remote == nil || s.repo == nil {
		return result, fmt.Errorf("session report upload unavailable")
	}
	repo, err := s.repo(ctx, cwd)
	if err != nil {
		return result, err
	}
	inputs, err := s.reports.read(ctx, cwd)
	if err != nil {
		return result, err
	}
	report, records := domain.BuildSegmentReportWithRecords(inputs)
	plan := domain.BuildAnchoredUploadPlan(report, inputs.Git.Commits, s.machine)
	result.Unread = append(result.Unread, report.Unread...)
	result.AgentCallsWithoutSender = plan.CallsWithoutSender
	stopped := func(err error) error {
		return fmt.Errorf("stopped after %d of %d session reports: %w", len(result.Sessions), len(plan.Uploads), err)
	}
	// The plan holds one upload per session of the report, in the same order.
	for index, upload := range plan.Uploads {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		session := domain.SegmentPushSession{Provider: upload.Provider, ID: upload.SessionID}
		if !domain.ValidAnchoredSessionID(upload.SessionID) {
			session.State, session.Reason = domain.SegmentPushRefused, "the tool's session identifier cannot be used as a key on the server"
			result.Refused++
			result.Sessions = append(result.Sessions, session)
			continue
		}
		if s.contentRemote != nil {
			state, reason, err := s.attachContent(ctx, string(repo.ID), inputs.Git.Root, records[index], &upload.Body)
			if err != nil {
				return result, stopped(err)
			}
			session.Content, session.ContentReason = state, reason
			if state == domain.SegmentContentNotUploaded {
				result.ContentNotUploaded++
			}
		}
		receipt, err := s.remote.PutAnchoredSession(ctx, string(repo.ID), upload)
		switch {
		case err == nil && receipt.Changed:
			session.State, session.Revision = domain.SegmentPushStored, receipt.Revision
			result.Stored++
		case err == nil:
			session.State, session.Revision = domain.SegmentPushUnchanged, receipt.Revision
			result.Unchanged++
		case refusedSessionReport(err):
			session.State, session.Reason = domain.SegmentPushRefused, err.Error()
			result.Refused++
		default:
			// Not signed in, not a member, or the server cannot be reached:
			// the remaining sessions would fail the same way.
			return result, stopped(err)
		}
		result.Sessions = append(result.Sessions, session)
	}
	return result, nil
}

// attachContent uploads the conversation of one session's own records and
// names it in body. A session whose content cannot be read back or is refused
// keeps its report without content and a reason; an error is returned only
// for what would stop every session.
func (s *SegmentUploadService) attachContent(ctx context.Context, repoID, repoRoot string, records domain.SessionRecordRanges, body *domain.AnchoredSessionBody) (domain.SegmentContentState, string, error) {
	if body.Records == 0 {
		return domain.SegmentContentNone, "", nil
	}
	source := s.content[records.Provider]
	if source == nil {
		return domain.SegmentContentNotUploaded, "the conversation of this tool's records is not read yet", nil
	}
	content, err := source.ReadSessionContent(ctx, repoRoot, records)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", ctxErr
	}
	if err != nil {
		return domain.SegmentContentNotUploaded, err.Error(), nil
	}
	if len(content.Missing) > 0 {
		return domain.SegmentContentNotUploaded, fmt.Sprintf("%d records changed while they were read; the next run uploads the conversation", len(content.Missing)), nil
	}
	doc, first, count := content.Document()
	if len(doc.Events) == 0 {
		return domain.SegmentContentNone, "", nil
	}
	ref, err := s.contentRemote.PutSessionContent(ctx, repoID, doc)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", ctxErr
	}
	switch {
	case err == nil:
	case refusedSessionReport(err), errors.Is(err, domain.ErrInvalidCIR), errors.Is(err, domain.ErrConversationManifest), errors.Is(err, domain.ErrSessionContentNotAccepted):
		// This one document was refused or cannot be built; others can be.
		return domain.SegmentContentNotUploaded, err.Error(), nil
	default:
		return "", "", err
	}
	if !body.AttachContent(ref, len(doc.Events), first, count) {
		return domain.SegmentContentNotUploaded, "the conversation was read in other ranges than the report has", nil
	}
	return domain.SegmentContentStored, "", nil
}
