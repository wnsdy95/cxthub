package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// SegmentUploadService uploads the report SegmentReportService computes, one
// report per session. Like the report it reads everything again on each call.
// What it writes on this computer is said by its options: branch IDs in Git's
// configuration, the collector's memory, and a copy of what it sent.
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
	// summaryRemote and summarizer are set together by WithSummaries.
	summaryRemote outbound.AnchoredSummaryRemote
	summarizer    outbound.SessionSummarizer
	// removalRemote is set by WithRemovals.
	removalRemote outbound.AnchoredRemovalRemote
	// uploadRecords and clock are set by WithUploadRecords.
	uploadRecords outbound.SessionUploadRecords
	clock         func() time.Time
	// originBranches and requestRemoteRead are set together by
	// WithRemoteReadRequests.
	originBranches    func(ctx context.Context, cwd string) (string, error)
	requestRemoteRead func(ctx context.Context, repoID string) error
	// collectorRemote, originPushes and version are set together by
	// WithCollectorReports.
	collectorRemote outbound.CollectorReportRemote
	originPushes    func(ctx context.Context, cwd string) ([]domain.RecordedPush, error)
	version         string
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

// WithSummaries also writes each session down at the commits it made and
// stores those summaries on the server, after the session's report. A summary
// is made from the conversation up to the commit, so it needs WithSessionContent.
func (s *SegmentUploadService) WithSummaries(remote outbound.AnchoredSummaryRemote, summarizer outbound.SessionSummarizer) *SegmentUploadService {
	s.summaryRemote, s.summarizer = remote, summarizer
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

// WithRemovals makes every run ask the server which of the caller's sessions
// were removed from it, and send nothing of those: neither the report nor the
// conversation. A removed session is uploaded again only after it is allowed
// again.
func (s *SegmentUploadService) WithRemovals(remote outbound.AnchoredRemovalRemote) *SegmentUploadService {
	s.removalRemote = remote
	return s
}

// WithUploadRecords also keeps, in the repository's .cxt folder, a copy of
// what is sent for each session and a log of the uploads. The copies are a
// record to compare the server with; no run reads them.
func (s *SegmentUploadService) WithUploadRecords(records outbound.SessionUploadRecords, clock func() time.Time) *SegmentUploadService {
	s.uploadRecords, s.clock = records, clock
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

// WithRemoteReadRequests makes an automatic run ask the server to read the
// repository from its Git host when origin's branches, as this clone holds
// them, are not what they were when it last asked: this computer pushed or
// fetched. The server opens a part of a session once it has seen its commit
// on the host, and without being asked it reads the host only every few
// hours. branches reads Git's own records and asks no remote.
func (s *SegmentUploadService) WithRemoteReadRequests(branches func(ctx context.Context, cwd string) (string, error), request func(ctx context.Context, repoID string) error) *SegmentUploadService {
	s.originBranches, s.requestRemoteRead = branches, request
	return s
}

// askForRemoteRead asks the server to read the remote when origin's branches
// changed in this clone since it was last asked. It never fails the run. The
// branches are remembered once the server answered: it took the request, or
// it said that it knows no Git host for the repository, which it would say
// again about the same branches. A request that got no such answer is made
// again by the next run. A clone that holds no branch of origin has nothing
// the server could read, and asks nothing.
func (s *SegmentUploadService) askForRemoteRead(ctx context.Context, cwd, repoID string, state *domain.CollectorState) {
	if s.originBranches == nil || s.requestRemoteRead == nil {
		return
	}
	branches, err := s.originBranches(ctx, cwd)
	if err != nil || strings.TrimSpace(branches) == "" {
		return
	}
	held := domain.Fingerprint(branches)
	if held == state.OriginBranches {
		return
	}
	if err := s.requestRemoteRead(ctx, repoID); err != nil && !noGitHostToRead(err) {
		return
	}
	state.OriginBranches = held
}

// WithCollectorReports makes an automatic run tell the server about the
// collector itself: the pushes this clone made, as Git recorded them, and
// what went wrong in the run. The server uses it to say of a push that came
// without a session whether it was made where cxt runs. pushes reads Git's
// own records and asks no remote; version is the version of this program.
func (s *SegmentUploadService) WithCollectorReports(remote outbound.CollectorReportRemote, pushes func(ctx context.Context, cwd string) ([]domain.RecordedPush, error), version string) *SegmentUploadService {
	s.collectorRemote, s.originPushes, s.version = remote, pushes, version
	return s
}

// reportCollector tells the server about the collector when there is
// something to say that the server was not told yet: this clone pushed or
// fetched, so origin's branches as it holds them are other ones, or its run
// ended otherwise than the last one it reported. A run with nothing new
// sends nothing. It never fails the run; what the server did not take is
// sent again by the next run.
func (s *SegmentUploadService) reportCollector(ctx context.Context, cwd, repoID string, state *domain.CollectorState, runErr error) {
	if s.collectorRemote == nil || s.originPushes == nil || ctx.Err() != nil {
		return
	}
	report := domain.CollectorReport{Machine: s.machine, Version: domain.CollectorLine(s.version, domain.CollectorVersionLimit), RunError: domain.CollectorRunError(runErr)}
	branches := ""
	if s.originBranches != nil {
		branches, _ = s.originBranches(ctx, cwd)
	}
	about := domain.Fingerprint(struct {
		Report   domain.CollectorReport `json:"report"`
		Branches string                 `json:"branches"`
	}{report, branches})
	if about == state.Reported {
		return
	}
	pushes, err := s.originPushes(ctx, cwd)
	if err != nil {
		return
	}
	report.Pushes = pushes
	if s.collectorRemote.ReportCollector(ctx, repoID, report) == nil {
		state.Reported = about
	}
}

// noGitHostToRead reports the server's answer that the repository has no Git
// origin it could read.
func noGitHostToRead(err error) bool {
	var status statusCodeError
	return errors.As(err, &status) && status.StatusCode() == 409
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
	return s.push(ctx, cwd, nil, nil)
}

// PushSegmentsOf uploads only the named sessions, by the same rules as
// PushSegments: everything is read again, and the report, the conversation
// and the summaries of each named session are sent. A session the tools'
// records on this computer do not hold is named in the result.
func (s *SegmentUploadService) PushSegmentsOf(ctx context.Context, cwd string, keys []domain.SessionUploadKey) (domain.SegmentPushResult, error) {
	only := make(map[string]bool, len(keys))
	for _, key := range keys {
		only[domain.CollectorSessionKey(key.Provider, key.SessionID)] = true
	}
	result, err := s.push(ctx, cwd, nil, only)
	if err != nil {
		return result, err
	}
	handled := map[string]bool{}
	for _, session := range result.Sessions {
		handled[domain.CollectorSessionKey(session.Provider, session.ID)] = true
	}
	for _, key := range keys {
		if !handled[domain.CollectorSessionKey(key.Provider, key.SessionID)] {
			result.NotFound = append(result.NotFound, key)
		}
	}
	return result, nil
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
		state.Target, state.Sessions, state.Branches, state.Worktrees, state.OriginBranches, state.Reported = target, nil, "", "", "", ""
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
	result, runErr := s.push(ctx, cwd, run, nil)
	if runErr == nil {
		// After the uploads: the server reads the host with the reports it
		// was just given.
		s.askForRemoteRead(ctx, cwd, string(repo.ID), &state)
	}
	// Whether the run ended well or not: the server is told which pushes
	// this clone made, and what went wrong.
	s.reportCollector(ctx, cwd, string(repo.ID), &state, runErr)
	state.LastRun = &domain.CollectorRun{At: now.Format(time.RFC3339Nano), Millis: s.now().UTC().Sub(now).Milliseconds(), Stored: result.Stored, Unchanged: result.Unchanged, Refused: result.Refused}
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

// push is one run. auto carries the memory of an automatic run and is nil for
// a run a person asked for; only, when it is not nil, names the sessions the
// run is for.
func (s *SegmentUploadService) push(ctx context.Context, cwd string, auto *collectorRun, only map[string]bool) (domain.SegmentPushResult, error) {
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
	// The tool's own memory is read for the folder a session started in.
	folders := make(map[string]string, len(inputs.Sessions))
	for _, recorded := range inputs.Sessions {
		folders[domain.CollectorSessionKey(recorded.Provider, recorded.ID)] = recorded.StartFolder
	}
	plan := domain.BuildAnchoredUploadPlan(report, inputs.Git.Commits, s.machine)
	result.Unread = append(result.Unread, report.Unread...)
	result.AgentCallsWithoutSender = plan.CallsWithoutSender
	stopped := func(err error) error {
		return fmt.Errorf("stopped after %d of %d session reports: %w", len(result.Sessions), len(plan.Uploads), err)
	}
	// Sessions that were removed from the server are not sent. The server
	// would refuse their report; asking first keeps their conversation from
	// being uploaded before that refusal. The server is asked once, and only
	// when the run is about to send something: a run with nothing new makes
	// no request at all.
	var removed map[string]domain.AnchoredSessionRemoval
	removalOf := func(key string) (domain.AnchoredSessionRemoval, bool, error) {
		if s.removalRemote == nil {
			return domain.AnchoredSessionRemoval{}, false, nil
		}
		if removed == nil {
			listed, err := s.removalRemote.ListRemovedAnchoredSessions(ctx, string(repo.ID))
			if err != nil {
				return domain.AnchoredSessionRemoval{}, false, err
			}
			removed = make(map[string]domain.AnchoredSessionRemoval, len(listed))
			for _, removal := range listed {
				removed[domain.CollectorSessionKey(removal.Provider, removal.SessionID)] = removal
			}
		}
		removal, gone := removed[key]
		return removal, gone, nil
	}
	// notSent records a removed session in the result, in the collector's
	// memory and in the session's log.
	notSent := func(session domain.SegmentPushSession, key string, removal domain.AnchoredSessionRemoval) {
		session.State, session.Reason = domain.SegmentPushRemoved, removedSessionReason(removal)
		session.Content, session.ContentReason = "", ""
		result.Removed++
		if auto != nil {
			// What was sent is no longer on the server.
			delete(auto.state.Sessions, key)
		}
		if s.uploadRecords != nil && s.clock != nil {
			_ = s.uploadRecords.LogSessionAction(context.WithoutCancel(ctx), cwd, domain.SessionUploadKey{Provider: session.Provider, SessionID: session.ID},
				domain.SessionUploadLogEntry{At: s.clock().UTC().Format(time.RFC3339Nano), Automatic: auto != nil, Action: "not sent", Reason: session.Reason})
		}
		result.Sessions = append(result.Sessions, session)
	}
	// The plan holds one upload per session of the report, in the same order.
	for index, upload := range plan.Uploads {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		key := domain.CollectorSessionKey(upload.Provider, upload.SessionID)
		if only != nil && !only[key] {
			continue
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
		var remembered domain.CollectorSessionState
		if auto != nil {
			remembered = auto.state.Sessions[key]
		}
		// The conversation as it was read in this run; nil when it was not read.
		var conversation *domain.CIRDocument
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
				// The conversation is about to be read back and sent.
				removal, gone, removalErr := removalOf(key)
				if removalErr != nil {
					return result, stopped(removalErr)
				}
				if gone {
					notSent(session, key, removal)
					continue
				}
				var first, count []int
				state, reason, first, count, conversation, err = s.attachContent(ctx, string(repo.ID), inputs.Git.Root, records[index], &upload.Body)
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
		known := auto != nil && sent != "" && remembered.Sent == sent
		var receipt domain.AnchoredSessionReceipt
		if !known {
			// The report is about to be sent.
			removal, gone, removalErr := removalOf(key)
			if removalErr != nil {
				return result, stopped(removalErr)
			}
			if gone {
				if session.Content == domain.SegmentContentNotUploaded {
					result.ContentNotUploaded--
				}
				notSent(session, key, removal)
				continue
			}
			receipt, err = s.remote.PutAnchoredSession(ctx, string(repo.ID), upload)
			remembered.Sent = ""
		}
		switch {
		case known:
			// The server already stored exactly this report.
			session.State = domain.SegmentPushUnchanged
			result.Unchanged++
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
		// The report is on the server: its summaries can be stored. They are
		// made from the conversation read in this run.
		if s.summaryRemote != nil && s.summarizer != nil && conversation != nil && session.State != domain.SegmentPushRefused {
			folder := folders[key]
			if folder == "" {
				folder = inputs.Git.Root
			}
			written, reason, err := s.pushSummaries(ctx, string(repo.ID), folder, upload, *conversation, &remembered, auto != nil)
			session.Summaries, session.SummaryReason = written, reason
			result.Summaries += written
			if err != nil {
				if auto != nil {
					auto.state.Sessions[key] = remembered
				}
				return result, stopped(err)
			}
		}
		if auto != nil {
			auto.state.Sessions[key] = remembered
		}
		if s.uploadRecords != nil && s.clock != nil {
			entry := domain.SessionUploadLogEntry{
				At: s.clock().UTC().Format(time.RFC3339Nano), Automatic: auto != nil,
				Report: session.State, Revision: session.Revision, ReportHash: domain.Fingerprint(upload.Body), ServerHash: receipt.BodyHash, Reason: session.Reason,
				Content: session.Content, ContentReason: session.ContentReason, Summaries: session.Summaries, SummaryReason: session.SummaryReason,
			}
			if content := upload.Body.Content; content != nil {
				entry.Doc, entry.Events = content.Doc, content.Events
			}
			if result.LocalRecords == nil {
				result.LocalRecords = &domain.SegmentPushLocalRecords{}
			}
			if err := s.uploadRecords.KeepSessionUpload(context.WithoutCancel(ctx), cwd, upload, conversation, entry); err != nil {
				if result.LocalRecords.NotKept == "" {
					result.LocalRecords.NotKept = err.Error()
				}
			} else {
				result.LocalRecords.Kept++
			}
		}
		result.Sessions = append(result.Sessions, session)
	}
	return result, nil
}

// attachContent uploads the conversation of one session's own records and
// names it in body. A session whose content cannot be read back or is refused
// keeps its report without content and a reason; an error is returned only
// for what would stop every session.
func (s *SegmentUploadService) attachContent(ctx context.Context, repoID, repoRoot string, records domain.SessionRecordRanges, body *domain.AnchoredSessionBody) (state domain.SegmentContentState, reason string, first, count []int, conversation *domain.CIRDocument, err error) {
	if body.Records == 0 {
		return domain.SegmentContentNone, "", nil, nil, nil, nil
	}
	source := s.content[records.Provider]
	if source == nil {
		return domain.SegmentContentNotUploaded, "the conversation of this tool's records is not read yet", nil, nil, nil, nil
	}
	content, err := source.ReadSessionContent(ctx, repoRoot, records)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", nil, nil, nil, ctxErr
	}
	if err != nil {
		return domain.SegmentContentNotUploaded, err.Error(), nil, nil, nil, nil
	}
	if len(content.Missing) > 0 {
		return domain.SegmentContentNotUploaded, fmt.Sprintf("%d records changed while they were read; the next run uploads the conversation", len(content.Missing)), nil, nil, nil, nil
	}
	doc, first, count := content.Document()
	if len(doc.Events) == 0 {
		return domain.SegmentContentNone, "", nil, nil, nil, nil
	}
	ref, err := s.contentRemote.PutSessionContent(ctx, repoID, doc)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", nil, nil, nil, ctxErr
	}
	switch {
	case err == nil:
	case refusedSessionReport(err), errors.Is(err, domain.ErrInvalidCIR), errors.Is(err, domain.ErrConversationManifest), errors.Is(err, domain.ErrSessionContentNotAccepted):
		// This one document was refused or cannot be built; others can be.
		return domain.SegmentContentNotUploaded, err.Error(), nil, nil, nil, nil
	default:
		return "", "", nil, nil, nil, err
	}
	if !body.AttachContent(ref, len(doc.Events), first, count) {
		return domain.SegmentContentNotUploaded, "the conversation was read in other ranges than the report has", nil, nil, nil, nil
	}
	return domain.SegmentContentStored, "", first, count, &doc, nil
}

// pushSummaries writes the session down at each commit it made and stores the
// summaries. An automatic run leaves out the commits it already summarized; a
// run a person asked for writes every one again. A summary that could not be
// made or stored is said in reason and tried again by the next run; an error
// is returned only when the run was cancelled.
func (s *SegmentUploadService) pushSummaries(ctx context.Context, repoID, folder string, upload domain.AnchoredSessionUpload, conversation domain.CIRDocument, remembered *domain.CollectorSessionState, automatic bool) (written int, reason string, err error) {
	points := domain.AnchoredSummaryPoints(upload.Body)
	done := map[string]struct{}{}
	if automatic {
		for _, commit := range remembered.Summarized {
			done[commit] = struct{}{}
		}
	}
	var wanted []domain.AnchoredSummaryPoint
	var ends []int
	for _, point := range points {
		for _, commit := range point.Commits {
			if _, ok := done[commit]; !ok {
				wanted, ends = append(wanted, point), append(ends, point.End)
				break
			}
		}
	}
	// What failed is read again by the next automatic run.
	failed := func(why string) {
		reason = why
		remembered.ContentKey = ""
	}
	if len(wanted) > 0 {
		digests, err := s.summarizer.Summarize(ctx, folder, upload.Provider, upload.SessionID, conversation, ends)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return written, reason, ctxErr
		}
		switch {
		case err != nil:
			failed(err.Error())
		case len(digests) != len(wanted):
			failed("the summaries do not match the commits they were asked for")
		default:
			for index, point := range wanted {
				body, said := domain.AnchoredSummaryOf(digests[index])
				for _, commit := range point.Commits {
					if _, ok := done[commit]; ok {
						continue
					}
					if said {
						err := s.summaryRemote.PutAnchoredSummary(ctx, repoID, upload.Provider, upload.SessionID, commit, body)
						if ctxErr := ctx.Err(); ctxErr != nil {
							return written, reason, ctxErr
						}
						if err != nil {
							failed(err.Error())
							continue
						}
						written++
					}
					// A conversation that gives nothing to write down is not asked again.
					done[commit] = struct{}{}
				}
			}
		}
	}
	remembered.Summarized = domain.AnchoredSummaryRemembered(points, done)
	return written, reason, nil
}

// ResendSession sends again what was last sent for a session, from the copy
// kept in the repository's .cxt folder: the conversation document and the
// report, and the summaries written anew from that conversation. It is for a
// session whose records the tool no longer holds; nothing is read from the
// tool, and nothing is computed anew but the summaries.
func (s *SegmentUploadService) ResendSession(ctx context.Context, cwd string, key domain.SessionUploadKey) (domain.SegmentPushResult, error) {
	result := domain.SegmentPushResult{Version: 1, Sessions: []domain.SegmentPushSession{}, Unread: []domain.UnreadSessionRecord{}}
	if s.remote == nil || s.repo == nil || s.uploadRecords == nil || s.clock == nil {
		return result, fmt.Errorf("session report upload unavailable")
	}
	name := string(key.Provider) + " " + key.SessionID
	record, err := s.uploadRecords.ReadSessionUpload(ctx, cwd, key, true)
	if err != nil {
		return result, fmt.Errorf("no copy of %s is kept under .cxt/sessions: %w", name, err)
	}
	if record.Report == nil {
		return result, fmt.Errorf("no copy of the report of %s is kept under .cxt/sessions", name)
	}
	repo, err := s.repo(ctx, cwd)
	if err != nil {
		return result, err
	}
	upload := domain.AnchoredSessionUpload{Provider: key.Provider, SessionID: key.SessionID, Body: *record.Report}
	session := domain.SegmentPushSession{Provider: key.Provider, ID: key.SessionID}
	if s.removalRemote != nil {
		removals, err := s.removalRemote.ListRemovedAnchoredSessions(ctx, string(repo.ID))
		if err != nil {
			return result, err
		}
		for _, removal := range removals {
			if removal.Provider == key.Provider && removal.SessionID == key.SessionID {
				session.State, session.Reason = domain.SegmentPushRemoved, removedSessionReason(removal)
				result.Removed, result.Sessions = 1, append(result.Sessions, session)
				return result, nil
			}
		}
	}
	// The conversation first, as in every upload: the report may only name
	// a document the repository holds.
	if content := upload.Body.Content; content != nil {
		if record.Conversation == nil || s.contentRemote == nil {
			return result, fmt.Errorf("the report of %s names a conversation, and no copy of it is kept under .cxt/sessions", name)
		}
		ref, err := s.contentRemote.PutSessionContent(ctx, string(repo.ID), *record.Conversation)
		if err != nil {
			return result, fmt.Errorf("the copy of the conversation of %s was not taken: %w", name, err)
		}
		if ref.Hash != content.Doc {
			return result, fmt.Errorf("the copy of the conversation of %s is not the document its report names; nothing was sent of the report", name)
		}
		session.Content = domain.SegmentContentStored
	}
	receipt, err := s.remote.PutAnchoredSession(ctx, string(repo.ID), upload)
	switch {
	case err == nil && receipt.Changed:
		session.State, session.Revision, result.Stored = domain.SegmentPushStored, receipt.Revision, 1
	case err == nil:
		session.State, session.Revision, result.Unchanged = domain.SegmentPushUnchanged, receipt.Revision, 1
	case refusedSessionReport(err):
		session.State, session.Reason, result.Refused = domain.SegmentPushRefused, err.Error(), 1
	default:
		return result, err
	}
	if s.summaryRemote != nil && s.summarizer != nil && record.Conversation != nil && session.State != domain.SegmentPushRefused {
		remembered := domain.CollectorSessionState{}
		written, reason, err := s.pushSummaries(ctx, string(repo.ID), cwd, upload, *record.Conversation, &remembered, false)
		session.Summaries, session.SummaryReason, result.Summaries = written, reason, written
		if err != nil {
			return result, err
		}
	}
	entry := domain.SessionUploadLogEntry{At: s.clock().UTC().Format(time.RFC3339Nano), Report: session.State, Revision: session.Revision,
		ReportHash: domain.Fingerprint(upload.Body), ServerHash: receipt.BodyHash, Reason: session.Reason, Content: session.Content,
		Summaries: session.Summaries, SummaryReason: session.SummaryReason, Action: "sent again from the copy"}
	if content := upload.Body.Content; content != nil {
		entry.Doc, entry.Events = content.Doc, content.Events
	}
	_ = s.uploadRecords.LogSessionAction(context.WithoutCancel(ctx), cwd, key, entry)
	result.Sessions = append(result.Sessions, session)
	return result, nil
}

// removedSessionReason says why a removed session is not sent and what
// brings it back.
func removedSessionReason(removal domain.AnchoredSessionRemoval) string {
	who := "you removed it from the server"
	if !removal.ByYou {
		who = "someone who manages the repository removed it from the server"
	}
	when := ""
	if removal.RemovedAt != "" {
		when = " on " + removal.RemovedAt
	}
	return fmt.Sprintf("%s%s; cxt segments allow %s %s lets it be uploaded again", who, when, removal.Provider, removal.SessionID)
}
