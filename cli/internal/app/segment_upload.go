package app

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	// branchIDs, branchRemote and newBranchID are set together by WithBranchIDs.
	branchIDs    outbound.BranchIDStore
	branchRemote outbound.BranchLedgerRemote
	newBranchID  func() (string, error)
	// worktreeRemote is set by WithWorktreeList.
	worktreeRemote outbound.WorktreeListRemote
	// state, now and contentEpoch are set together by WithCollectorState.
	state        outbound.CollectorStateStore
	now          func() time.Time
	contentEpoch func(ctx context.Context, cwd string) string
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

// WithBranchIDs also gives every branch of the repository an ID in Git's own
// branch configuration, and registers with the server the IDs of branches
// Git's records show on origin. This is the one thing a run writes on this
// computer.
func (s *SegmentUploadService) WithBranchIDs(store outbound.BranchIDStore, remote outbound.BranchLedgerRemote, newID func() (string, error)) *SegmentUploadService {
	s.branchIDs, s.branchRemote, s.newBranchID = store, remote, newID
	return s
}

// WithWorktreeList also tells the server which worktrees the repository has
// on this computer: each folder's name and the branch checked out in it.
func (s *SegmentUploadService) WithWorktreeList(remote outbound.WorktreeListRemote) *SegmentUploadService {
	s.worktreeRemote = remote
	return s
}

// pushWorktrees reports the worktree list. It does not stop the run: what
// did not happen is said in the result and the next run tries again.
func (s *SegmentUploadService) pushWorktrees(ctx context.Context, repoID string, worktrees []domain.RecordedWorktree, auto *collectorRun) (*domain.SegmentPushWorktrees, error) {
	registrations := domain.WorktreeRegistrations(worktrees)
	out := &domain.SegmentPushWorktrees{Listed: len(registrations)}
	sent := ""
	if auto != nil {
		// An automatic run reports again only when the list differs from
		// what it last reported.
		if sent = domain.Fingerprint(struct {
			Machine   string                        `json:"machine"`
			Worktrees []domain.WorktreeRegistration `json:"worktrees"`
		}{s.machine, registrations}); sent == auto.state.Worktrees {
			out.Reported = true
			return out, nil
		}
	}
	err := s.worktreeRemote.ReportWorktrees(ctx, repoID, s.machine, registrations)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return out, ctxErr
	}
	if err != nil {
		out.NotReported = err.Error()
		return out, nil
	}
	out.Reported = true
	if auto != nil {
		auto.state.Worktrees = sent
	}
	return out, nil
}

// pushBranchIDs assigns and registers branch IDs. Neither step stops the
// run: what did not happen is said in the result and the next run fills it.
func (s *SegmentUploadService) pushBranchIDs(ctx context.Context, cwd, repoID string, auto *collectorRun) (*domain.SegmentPushBranches, error) {
	out := &domain.SegmentPushBranches{}
	branches, assigned, err := EnsureBranchIDs(ctx, s.branchIDs, cwd, s.newBranchID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return out, ctxErr
	}
	out.Assigned = assigned
	if err != nil {
		out.NotAssigned = err.Error()
		// What Git holds is still worth registering.
		err = s.branchIDs.WithBranchIDLock(ctx, cwd, func() error {
			var readErr error
			branches, readErr = s.branchIDs.LocalBranches(ctx, cwd)
			return readErr
		})
		if err != nil {
			out.NotRegistered = "the repository's branches could not be read"
			return out, nil
		}
	}
	for _, branch := range branches {
		if domain.ValidBranchID(branch.ID) {
			out.WithID++
		}
	}
	registrations := domain.BranchRegistrations(branches)
	out.OnOrigin = len(registrations)
	if len(registrations) == 0 {
		return out, nil
	}
	sent := ""
	if auto != nil {
		// An automatic run registers again only when the branches or their
		// IDs differ from what it last registered.
		if sent = domain.Fingerprint(registrations); sent == auto.state.Branches {
			out.Registered = len(registrations)
			return out, nil
		}
	}
	results, err := s.branchRemote.RegisterBranchIDs(ctx, repoID, registrations)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return out, ctxErr
	}
	if err != nil {
		out.NotRegistered = err.Error()
		return out, nil
	}
	out.Registered = len(results)
	if auto != nil {
		auto.state.Branches = sent
	}
	return out, nil
}

// WithCollectorState enables CollectSegments. state is the memory of what
// was sent; contentEpoch names the masking policy and the document identity
// content is built with, so that a change of either rebuilds it.
func (s *SegmentUploadService) WithCollectorState(state outbound.CollectorStateStore, now func() time.Time, contentEpoch func(ctx context.Context, cwd string) string) *SegmentUploadService {
	s.state, s.now, s.contentEpoch = state, now, contentEpoch
	return s
}

var _ inbound.SegmentUploader = (*SegmentUploadService)(nil)
var _ inbound.SegmentCollector = (*SegmentUploadService)(nil)

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

// PushSegments uploads every session of the repository holding cwd. It is
// the run a person asks for: it reads everything, sends every report and lets
// the server say what changed.
func (s *SegmentUploadService) PushSegments(ctx context.Context, cwd string) (domain.SegmentPushResult, error) {
	return s.push(ctx, cwd, nil)
}

// CollectSegments is the run that needs nobody to ask for it. It reads the
// same records by the same rules, and differs from PushSegments in two ways
// only: it leaves out sessions whose records all lie before automatic reading
// began in this repository, and it remembers what it sent so that it sends
// only what changed since.
func (s *SegmentUploadService) CollectSegments(ctx context.Context, cwd string) (domain.SegmentPushResult, error) {
	if s.state == nil || s.now == nil {
		return domain.SegmentPushResult{}, fmt.Errorf("automatic session report upload unavailable")
	}
	repo, err := s.repo(ctx, cwd)
	if err != nil {
		return domain.SegmentPushResult{}, err
	}
	state, err := s.state.LoadCollectorState(ctx, cwd)
	if err != nil {
		return domain.SegmentPushResult{}, err
	}
	now := s.now().UTC()
	if state.Version != domain.CollectorStateVersion || state.EnabledAt == "" {
		// Automatic reading begins now. Earlier sessions wait for a request.
		state = domain.CollectorState{Version: domain.CollectorStateVersion, EnabledAt: now.Format(time.RFC3339Nano)}
	}
	// What was sent was sent to one repository on one server.
	if target := string(repo.ID) + "\n" + repo.RemoteURL; state.Target != target {
		state.Target, state.Sessions, state.Branches, state.Worktrees = target, nil, "", ""
	}
	if state.Sessions == nil {
		state.Sessions = map[string]domain.CollectorSessionState{}
	}
	enabledAt, err := time.Parse(time.RFC3339Nano, state.EnabledAt)
	if err != nil {
		enabledAt = now
		state.EnabledAt = now.Format(time.RFC3339Nano)
	}
	run := &collectorRun{state: &state, enabledAt: enabledAt}
	if s.contentEpoch != nil {
		run.epoch = s.contentEpoch(ctx, cwd)
	}
	result, runErr := s.push(ctx, cwd, run)
	state.LastRun = &domain.CollectorRun{At: now.Format(time.RFC3339Nano), Stored: result.Stored, Unchanged: result.Unchanged, Refused: result.Refused}
	if runErr != nil {
		state.LastRun.Error = runErr.Error()
	}
	// What was sent before a failure was still sent.
	if err := s.state.SaveCollectorState(context.WithoutCancel(ctx), cwd, state); err != nil && runErr == nil {
		runErr = err
	}
	return result, runErr
}

// collectorRun carries the memory of an automatic run.
type collectorRun struct {
	state     *domain.CollectorState
	enabledAt time.Time
	epoch     string
}

func (s *SegmentUploadService) push(ctx context.Context, cwd string, auto *collectorRun) (domain.SegmentPushResult, error) {
	result := domain.SegmentPushResult{Version: 1, Automatic: auto != nil, Sessions: []domain.SegmentPushSession{}, Unread: []domain.UnreadSessionRecord{}}
	if s.reports == nil || s.remote == nil || s.repo == nil {
		return result, fmt.Errorf("session report upload unavailable")
	}
	repo, err := s.repo(ctx, cwd)
	if err != nil {
		return result, err
	}
	if s.branchIDs != nil && s.branchRemote != nil {
		if result.Branches, err = s.pushBranchIDs(ctx, cwd, string(repo.ID), auto); err != nil {
			return result, err
		}
	}
	inputs, err := s.reports.read(ctx, cwd)
	if err != nil {
		return result, err
	}
	if s.worktreeRemote != nil {
		if result.Worktrees, err = s.pushWorktrees(ctx, string(repo.ID), inputs.Git.Worktrees, auto); err != nil {
			return result, err
		}
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
		if auto != nil && !domain.InAutomaticScope(report.Sessions[index], auto.enabledAt) {
			result.BeforeAutomatic++
			continue
		}
		session := domain.SegmentPushSession{Provider: upload.Provider, ID: upload.SessionID}
		if !domain.ValidAnchoredSessionID(upload.SessionID) {
			session.State, session.Reason = domain.SegmentPushRefused, "the tool's session identifier cannot be used as a key on the server"
			result.Refused++
			result.Sessions = append(result.Sessions, session)
			continue
		}
		key := domain.CollectorSessionKey(upload.Provider, upload.SessionID)
		var remembered domain.CollectorSessionState
		if auto != nil {
			remembered = auto.state.Sessions[key]
		}
		if s.contentRemote != nil {
			contentKey := ""
			if auto != nil {
				contentKey = domain.SessionContentKey(records[index], auto.epoch)
			}
			state, reason, reused := domain.SegmentContentState(""), "", false
			if auto != nil && remembered.ContentKey == contentKey {
				// The same records in the same epoch: the same document.
				switch {
				case remembered.Content == nil:
					state, reused = domain.SegmentContentNone, true
				case upload.Body.AttachContent(domain.DocumentRef{Hash: remembered.Content.Doc, Identity: remembered.Content.Identity}, remembered.Content.Events, remembered.FirstEvent, remembered.Events):
					state, reused = domain.SegmentContentStored, true
				}
			}
			if !reused {
				var first, count []int
				state, reason, first, count, err = s.attachContent(ctx, string(repo.ID), inputs.Git.Root, records[index], &upload.Body)
				if err != nil {
					return result, stopped(err)
				}
				remembered.ContentKey, remembered.Content, remembered.FirstEvent, remembered.Events = "", nil, nil, nil
				switch state {
				case domain.SegmentContentStored:
					remembered.ContentKey, remembered.Content, remembered.FirstEvent, remembered.Events = contentKey, upload.Body.Content, first, count
				case domain.SegmentContentNone:
					remembered.ContentKey = contentKey
				}
			}
			session.Content, session.ContentReason = state, reason
			if state == domain.SegmentContentNotUploaded {
				result.ContentNotUploaded++
			}
		}
		sent := ""
		if auto != nil {
			sent = domain.Fingerprint(upload.Body)
		}
		if auto != nil && sent != "" && remembered.Sent == sent {
			// The server already stored exactly this report.
			session.State = domain.SegmentPushUnchanged
			result.Unchanged++
			auto.state.Sessions[key] = remembered
			result.Sessions = append(result.Sessions, session)
			continue
		}
		receipt, err := s.remote.PutAnchoredSession(ctx, string(repo.ID), upload)
		remembered.Sent = ""
		switch {
		case err == nil && receipt.Changed:
			session.State, session.Revision = domain.SegmentPushStored, receipt.Revision
			result.Stored++
			remembered.Sent = sent
		case err == nil:
			session.State, session.Revision = domain.SegmentPushUnchanged, receipt.Revision
			result.Unchanged++
			remembered.Sent = sent
		case refusedSessionReport(err):
			session.State, session.Reason = domain.SegmentPushRefused, err.Error()
			result.Refused++
		default:
			// Not signed in, not a member, or the server cannot be reached:
			// the remaining sessions would fail the same way.
			if auto != nil {
				auto.state.Sessions[key] = remembered
			}
			return result, stopped(err)
		}
		if auto != nil {
			auto.state.Sessions[key] = remembered
		}
		result.Sessions = append(result.Sessions, session)
	}
	return result, nil
}

// attachContent uploads the conversation of one session's own records and
// names it in body. A session whose content cannot be read back or is refused
// keeps its report without content and a reason; an error is returned only
// for what would stop every session.
func (s *SegmentUploadService) attachContent(ctx context.Context, repoID, repoRoot string, records domain.SessionRecordRanges, body *domain.AnchoredSessionBody) (state domain.SegmentContentState, reason string, first, count []int, err error) {
	if body.Records == 0 {
		return domain.SegmentContentNone, "", nil, nil, nil
	}
	source := s.content[records.Provider]
	if source == nil {
		return domain.SegmentContentNotUploaded, "the conversation of this tool's records is not read yet", nil, nil, nil
	}
	content, err := source.ReadSessionContent(ctx, repoRoot, records)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", nil, nil, ctxErr
	}
	if err != nil {
		return domain.SegmentContentNotUploaded, err.Error(), nil, nil, nil
	}
	if len(content.Missing) > 0 {
		return domain.SegmentContentNotUploaded, fmt.Sprintf("%d records changed while they were read; the next run uploads the conversation", len(content.Missing)), nil, nil, nil
	}
	doc, first, count := content.Document()
	if len(doc.Events) == 0 {
		return domain.SegmentContentNone, "", nil, nil, nil
	}
	ref, err := s.contentRemote.PutSessionContent(ctx, repoID, doc)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", nil, nil, ctxErr
	}
	switch {
	case err == nil:
	case refusedSessionReport(err), errors.Is(err, domain.ErrInvalidCIR), errors.Is(err, domain.ErrConversationManifest), errors.Is(err, domain.ErrSessionContentNotAccepted):
		// This one document was refused or cannot be built; others can be.
		return domain.SegmentContentNotUploaded, err.Error(), nil, nil, nil
	default:
		return "", "", nil, nil, err
	}
	if !body.AttachContent(ref, len(doc.Events), first, count) {
		return domain.SegmentContentNotUploaded, "the conversation was read in other ranges than the report has", nil, nil, nil
	}
	return domain.SegmentContentStored, "", first, count, nil
}
