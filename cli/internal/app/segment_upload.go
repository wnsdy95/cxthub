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
}

// NewSegmentUploadService creates the service. machine is the label the
// uploader's own computers are told apart by; it may be empty.
func NewSegmentUploadService(reports *SegmentReportService, remote outbound.AnchoredSessionRemote, repo func(context.Context, string) (domain.Repo, error), machine string) *SegmentUploadService {
	return &SegmentUploadService{reports: reports, remote: remote, repo: repo, machine: machine}
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
	report := domain.BuildSegmentReport(inputs)
	plan := domain.BuildAnchoredUploadPlan(report, inputs.Git.Commits, s.machine)
	result.Unread = append(result.Unread, report.Unread...)
	result.AgentCallsWithoutSender = plan.CallsWithoutSender
	for _, upload := range plan.Uploads {
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
			return result, fmt.Errorf("stopped after %d of %d session reports: %w", len(result.Sessions), len(plan.Uploads), err)
		}
		result.Sessions = append(result.Sessions, session)
	}
	return result, nil
}
