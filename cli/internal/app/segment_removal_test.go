package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type removalRemoteFake struct {
	listed  []domain.AnchoredSessionRemoval
	listErr error
	lists   int
	removed []domain.SessionUploadKey
	allowed []domain.SessionUploadKey
	answer  []domain.AnchoredSessionRemoval
	err     error
	repo    string
	// What the diagnosis reads.
	sessions       []domain.AnchoredSessionListed
	serverErr      error
	summaries      map[string][]domain.AnchoredSummary
	summariesAsked []string
	syncedAt       string
	github         domain.GitHubRepositoryConnection
	githubErr      error
	refreshes      int
	archives       []string
}

func (f *removalRemoteFake) ListRemovedAnchoredSessions(_ context.Context, repo string) ([]domain.AnchoredSessionRemoval, error) {
	f.lists++
	f.repo = repo
	return f.listed, f.listErr
}

func (f *removalRemoteFake) RemoveAnchoredSession(_ context.Context, repo string, provider domain.ProviderKind, session string) ([]domain.AnchoredSessionRemoval, error) {
	f.repo = repo
	f.removed = append(f.removed, domain.SessionUploadKey{Provider: provider, SessionID: session})
	return f.answer, f.err
}

func (f *removalRemoteFake) ListAnchoredSessions(context.Context, string) ([]domain.AnchoredSessionListed, error) {
	return f.sessions, f.serverErr
}

func (f *removalRemoteFake) ListAnchoredCommitSummaries(_ context.Context, _ string, commit string) ([]domain.AnchoredSummary, error) {
	f.summariesAsked = append(f.summariesAsked, commit)
	return f.summaries[commit], nil
}

func (f *removalRemoteFake) ReadRemoteGitSyncedAt(context.Context, string) (string, error) {
	return f.syncedAt, nil
}

func (f *removalRemoteFake) ReadGitHubConnection(context.Context, string) (domain.GitHubRepositoryConnection, error) {
	return f.github, f.githubErr
}

func (f *removalRemoteFake) RequestRemoteGitRefresh(context.Context, string) error {
	f.refreshes++
	return f.err
}

func (f *removalRemoteFake) SetAnchoredSessionArchived(_ context.Context, _ string, provider domain.ProviderKind, session string, archived bool) error {
	state := "restored"
	if archived {
		state = "archived"
	}
	f.archives = append(f.archives, string(provider)+"/"+session+" "+state)
	return f.err
}

func (f *removalRemoteFake) AllowAnchoredSession(_ context.Context, repo string, provider domain.ProviderKind, session string) error {
	f.repo = repo
	f.allowed = append(f.allowed, domain.SessionUploadKey{Provider: provider, SessionID: session})
	return f.err
}

type removalRecordsFake struct {
	uploadRecordsFake
	actions []string
}

func (f *removalRecordsFake) LogSessionAction(_ context.Context, _ string, key domain.SessionUploadKey, entry domain.SessionUploadLogEntry) error {
	run := "requested"
	if entry.Automatic {
		run = "automatic"
	}
	f.actions = append(f.actions, string(key.Provider)+"/"+key.SessionID+" "+run+" "+entry.Action)
	return nil
}

// A session that was removed from the server is not sent: neither its
// report nor its conversation, and no summary is written for it.
func TestSegmentUploadSendsNothingOfARemovedSession(t *testing.T) {
	remote, source, content := &segmentRemoteFake{}, &segmentContentSourceFake{}, &segmentContentRemoteFake{}
	summarizer, summaries := &segmentSummarizerFake{}, &segmentSummaryRemoteFake{}
	removals := &removalRemoteFake{listed: []domain.AnchoredSessionRemoval{{Provider: domain.ProviderClaude, SessionID: "with-commit", RemovedAt: "2031-01-02T03:04:05Z", ByYou: true}}}
	records := &removalRecordsFake{}
	service := segmentUploadFixture(remote).WithSessionContent(content, map[domain.ProviderKind]outbound.SessionContentSource{domain.ProviderClaude: source}).
		WithSummaries(summaries, summarizer).WithRemovals(removals).WithUploadRecords(records, time.Now)
	result, err := service.PushSegments(context.Background(), "/work/app")
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 || result.Stored != 1 || result.Refused != 1 || removals.lists != 1 {
		t.Fatalf("result: %+v lists=%d", result, removals.lists)
	}
	first := result.Sessions[0]
	if first.ID != "with-commit" || first.State != domain.SegmentPushRemoved || first.Content != "" ||
		first.Reason != "you removed it from the server on 2031-01-02T03:04:05Z; cxt segments allow claude with-commit lets it be uploaded again" {
		t.Fatalf("the removed session: %+v", first)
	}
	for _, upload := range remote.uploads {
		if upload.SessionID == "with-commit" {
			t.Fatal("the report of a removed session was sent")
		}
	}
	for _, read := range source.read {
		if read.ID == "with-commit" {
			t.Fatal("the conversation of a removed session was read back")
		}
	}
	if len(content.docs) != 1 || len(summaries.stored) != 0 {
		t.Fatalf("documents sent: %d summaries: %v", len(content.docs), summaries.stored)
	}
	// It is said in the session's log, and no copy of a report that was not
	// sent is kept.
	if !reflect.DeepEqual(records.actions, []string{"claude/with-commit requested not sent"}) {
		t.Fatalf("actions: %v", records.actions)
	}
	for _, kept := range records.kept {
		if kept.session == "claude/with-commit" {
			t.Fatal("a copy of a report that was not sent was kept")
		}
	}

	// A removal by someone who manages the repository says so.
	removals.listed[0].ByYou = false
	result, err = service.PushSegments(context.Background(), "/work/app")
	if err != nil || !strings.HasPrefix(result.Sessions[0].Reason, "someone who manages the repository removed it from the server") {
		t.Fatalf("removed by a manager: %+v %v", result.Sessions[0], err)
	}

	// The list cannot be read: nothing is sent, because the server would
	// refuse the next request the same way.
	removals.listErr = segmentUploadStatus(403)
	remote.uploads = nil
	if _, err := service.PushSegments(context.Background(), "/work/app"); err == nil || len(remote.uploads) != 0 || !strings.Contains(err.Error(), "stopped after 0 of 3 session reports") {
		t.Fatalf("an unreadable list: %v uploads=%d", err, len(remote.uploads))
	}
}

// The collector forgets what it sent of a removed session, so that the
// session is sent whole again once it is allowed again.
func TestCollectorForgetsWhatItSentOfARemovedSession(t *testing.T) {
	f := newCollectorFixture(t)
	removals := &removalRemoteFake{}
	f.service.WithRemovals(removals)
	f.began(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
	key := domain.CollectorSessionKey(domain.ProviderClaude, "with-commit")
	if first := f.run(t); first.Stored != 2 || f.state.state.Sessions[key].Sent == "" {
		t.Fatalf("first run: %+v", first)
	}
	// A run with nothing new asks the server nothing, also not which sessions
	// were removed.
	asked := removals.lists
	if quiet := f.run(t); quiet.Unchanged != 2 || removals.lists != asked || len(f.remote.uploads) != 0 {
		t.Fatalf("a run with nothing new: %+v lists=%d (was %d)", quiet, removals.lists, asked)
	}
	// The session goes on after it was removed: the run that would send it
	// asks, sends nothing of it, and forgets what was sent.
	removals.listed = []domain.AnchoredSessionRemoval{{Provider: domain.ProviderClaude, SessionID: "with-commit", ByYou: true}}
	session := &f.claude.batch.Sessions[0]
	session.Items = append(session.Items, domain.RecordedItem{ID: "m4", Turn: "t2", AtMs: collectorDay(20), Folder: "/work/app", Branch: "main"})
	second := f.run(t)
	if _, remembered := f.state.state.Sessions[key]; remembered || second.Removed != 1 || len(f.remote.uploads) != 0 || len(f.source.read) != 0 || removals.lists != asked+1 {
		t.Fatalf("after the removal: %+v remembered=%v uploads=%d read=%d lists=%d", second, remembered, len(f.remote.uploads), len(f.source.read), removals.lists)
	}
	// Allowed again: the next run sends it whole.
	removals.listed = nil
	if third := f.run(t); third.Stored != 1 || len(f.remote.uploads) != 1 || f.remote.uploads[0].SessionID != "with-commit" || len(f.source.read) != 1 {
		t.Fatalf("after it was allowed again: %+v uploads=%+v", third, f.remote.uploads)
	}
}

// A run for named sessions sends those alone and names what it did not find.
func TestSegmentUploadOfNamedSessionsOnly(t *testing.T) {
	remote, source, content := &segmentRemoteFake{}, &segmentContentSourceFake{}, &segmentContentRemoteFake{}
	service := segmentUploadFixture(remote).WithSessionContent(content, map[domain.ProviderKind]outbound.SessionContentSource{domain.ProviderClaude: source})
	named := []domain.SessionUploadKey{{Provider: domain.ProviderClaude, SessionID: "without-commit"}, {Provider: domain.ProviderCodex, SessionID: "without-commit"}, {Provider: domain.ProviderClaude, SessionID: "absent"}}
	result, err := service.PushSegmentsOf(context.Background(), "/work/app", named)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != 1 || result.Sessions[0].ID != "without-commit" || result.Stored != 1 || len(remote.uploads) != 1 || len(source.read) != 1 || source.read[0].ID != "without-commit" {
		t.Fatalf("result: %+v uploads=%d read=%+v", result, len(remote.uploads), source.read)
	}
	if !reflect.DeepEqual(result.NotFound, named[1:]) {
		t.Fatalf("not found: %+v", result.NotFound)
	}
}

func TestRemoveAndAllowASessionThroughTheService(t *testing.T) {
	key := domain.SessionUploadKey{Provider: domain.ProviderClaude, SessionID: "without-commit"}
	sub := domain.SessionUploadKey{Provider: domain.ProviderClaude, SessionID: "sub-1"}
	removals := &removalRemoteFake{answer: []domain.AnchoredSessionRemoval{
		{Provider: key.Provider, SessionID: key.SessionID, RemovedAt: "2031-01-02T03:04:05Z", ByYou: true}, {Provider: sub.Provider, SessionID: sub.SessionID, RemovedAt: "2031-01-02T03:04:05Z", ByYou: true}}}
	records := &removalRecordsFake{}
	remote := &segmentRemoteFake{}
	uploader := segmentUploadFixture(remote)
	repoID := domain.HashContent([]byte("synthetic repository"))
	repo := func(context.Context, string) (domain.Repo, error) { return domain.Repo{ID: repoID}, nil }
	care := NewSegmentCareService(records).WithServer(removals, repo, uploader, func() time.Time { return time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC) })

	removed, err := care.RemoveSession(context.Background(), "/work/app", key)
	if err != nil || len(removed.Removed) != 2 || !reflect.DeepEqual(removals.removed, []domain.SessionUploadKey{key}) || removals.repo != string(repoID) {
		t.Fatalf("remove: %+v %v asked=%+v", removed, err, removals.removed)
	}
	// Every removed session has the removal in its own log.
	if !reflect.DeepEqual(records.actions, []string{"claude/without-commit requested removed from the server", "claude/sub-1 requested removed from the server"}) {
		t.Fatalf("actions: %v", records.actions)
	}

	// Allowing takes the removal back and sends the session again, alone.
	records.actions = nil
	allowed, err := care.AllowSession(context.Background(), "/work/app", key)
	if err != nil || !reflect.DeepEqual(removals.allowed, []domain.SessionUploadKey{key}) || allowed.Allowed != key {
		t.Fatalf("allow: %+v %v", allowed, err)
	}
	if allowed.Upload.Stored != 1 || len(allowed.Upload.Sessions) != 1 || len(remote.uploads) != 1 || remote.uploads[0].SessionID != "without-commit" || len(allowed.Upload.NotFound) != 0 {
		t.Fatalf("the upload after allowing: %+v uploads=%+v", allowed.Upload, remote.uploads)
	}
	if !reflect.DeepEqual(records.actions, []string{"claude/without-commit requested allowed again on the server"}) {
		t.Fatalf("actions: %v", records.actions)
	}
	// A session whose records the tool no longer holds is allowed, and said
	// not to be found.
	gone := domain.SessionUploadKey{Provider: domain.ProviderClaude, SessionID: "records-gone"}
	if allowed, err := care.AllowSession(context.Background(), "/work/app", gone); err != nil || !reflect.DeepEqual(allowed.Upload.NotFound, []domain.SessionUploadKey{gone}) {
		t.Fatalf("records gone: %+v %v", allowed, err)
	}

	// What the server refuses is not done and not logged.
	records.actions = nil
	removals.err = errors.New("synthetic refusal")
	if _, err := care.RemoveSession(context.Background(), "/work/app", key); err == nil || !strings.Contains(err.Error(), "claude without-commit was not removed") {
		t.Fatalf("a refused removal: %v", err)
	}
	if _, err := care.AllowSession(context.Background(), "/work/app", key); err == nil || !strings.Contains(err.Error(), "was not allowed again") || len(records.actions) != 0 {
		t.Fatalf("a refused allow: %v actions=%v", err, records.actions)
	}
	// Without a server neither can be done.
	if _, err := NewSegmentCareService(records).RemoveSession(context.Background(), "/work/app", key); err == nil {
		t.Fatal("a removal without a server succeeded")
	}
	if _, err := NewSegmentCareService(records).AllowSession(context.Background(), "/work/app", key); err == nil {
		t.Fatal("an allow without a server succeeded")
	}
}

// The diagnosis reads the tools' records, what cxt keeps here and the server,
// and changes nothing anywhere.
func TestDiagnoseReadsThisComputerAndTheServer(t *testing.T) {
	sha := strings.Repeat("a", 40)
	onRemote := true
	listed := domain.AnchoredSessionListed{Provider: domain.ProviderClaude, SessionID: "with-commit", Revision: 1, BodyHash: "sha256:server", Records: 3, Segments: 1}
	listed.Commits = append(listed.Commits, struct {
		SHA      string   `json:"sha"`
		OnRemote *bool    `json:"on_remote,omitempty"`
		Copies   []string `json:"copies,omitempty"`
	}{SHA: sha, OnRemote: &onRemote})
	removals := &removalRemoteFake{sessions: []domain.AnchoredSessionListed{listed}, syncedAt: "2031-01-02T03:00:00Z",
		summaries: map[string][]domain.AnchoredSummary{sha: {{Provider: domain.ProviderClaude, SessionID: "with-commit", Commit: sha}}},
		listed:    []domain.AnchoredSessionRemoval{{Provider: domain.ProviderClaude, SessionID: "without-commit", RemovedAt: "2031-01-02T03:04:05Z", ByYou: true}}}
	body := domain.AnchoredSessionBody{Records: 3, Segments: []domain.AnchoredSegment{{}}}
	store := &uploadRecordsStore{records: map[domain.SessionUploadKey]domain.SessionUploadRecord{
		{Provider: domain.ProviderClaude, SessionID: "with-commit"}: {Provider: domain.ProviderClaude, SessionID: "with-commit", Report: &body,
			Log: []domain.SessionUploadLogEntry{{At: "2031-01-02T03:00:00Z", Report: domain.SegmentPushStored, ServerHash: "sha256:server"}, {At: "2031-01-02T03:01:00Z", Action: "archived on the server"}}},
	}}
	uploader := segmentUploadFixture(&segmentRemoteFake{})
	repo := func(context.Context, string) (domain.Repo, error) {
		return domain.Repo{ID: domain.HashContent([]byte("synthetic repository"))}, nil
	}
	facts := SegmentLocalFacts{SetUp: true, AutoOn: true, Connected: true}
	var originAsked []string
	care := NewSegmentCareService(store).WithServer(removals, repo, uploader, func() time.Time { return time.Date(2031, 1, 2, 4, 0, 0, 0, time.UTC) }).
		WithLocalFacts(uploader.reports, func(context.Context, string) SegmentLocalFacts { return facts },
			func(_ context.Context, _ string, commits []string) map[string]bool {
				originAsked = commits
				return map[string]bool{sha: true}
			})
	diagnosis, err := care.Diagnose(context.Background(), "/work/app")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]domain.SegmentFinding{}
	for _, finding := range diagnosis.Findings {
		found[finding.Code] = finding
	}
	// The session that is on the server as it was sent raises nothing; the
	// removed one is said with what brings it back; the unread record of the
	// fixture and the collector that never ran are said too.
	if removed := found["removed"]; removed.Fix != "cxt segments allow claude without-commit" || removed.Session == nil || removed.Session.SessionID != "without-commit" {
		t.Fatalf("the removed session: %+v (all: %+v)", removed, diagnosis.Findings)
	}
	for _, code := range []string{"unread-records", "collector-never-ran"} {
		if _, ok := found[code]; !ok {
			t.Fatalf("%s was not found: %+v", code, diagnosis.Findings)
		}
	}
	for _, code := range []string{"server-behind", "server-differs", "not-on-server", "push-not-seen", "summary-missing", "no-copy"} {
		if finding, ok := found[code]; ok {
			t.Fatalf("a finding that does not hold: %+v", finding)
		}
	}
	if !reflect.DeepEqual(originAsked, []string{sha}) || !reflect.DeepEqual(removals.summariesAsked, []string{sha}) {
		t.Fatalf("asked of Git: %v of the server: %v", originAsked, removals.summariesAsked)
	}

	// What the server says of the connection with GitHub reaches the
	// diagnosis; a server that fails to say is a server that was not read.
	if diagnosisCode(diagnosis, "github-not-connected") != nil {
		t.Fatalf("a server that says nothing of GitHub: %+v", diagnosis.Findings)
	}
	removals.github = domain.GitHubRepositoryConnection{State: domain.GitHubRepositoryNotConnected, ConnectURL: "https://example.test/connect/github"}
	if diagnosis, err = care.Diagnose(context.Background(), "/work/app"); err != nil || diagnosisCode(diagnosis, "github-not-connected") == nil ||
		diagnosisCode(diagnosis, "github-not-connected").Fix != "an owner of the repository connects it at https://example.test/connect/github" {
		t.Fatalf("a repository that is not connected to GitHub: %+v %v", diagnosis.Findings, err)
	}
	removals.githubErr = errors.New("synthetic status 500")
	if diagnosis, err = care.Diagnose(context.Background(), "/work/app"); err != nil || diagnosisCode(diagnosis, "server-not-read") == nil || diagnosisCode(diagnosis, "github-not-connected") != nil {
		t.Fatalf("a server that fails to say whether GitHub is connected: %+v %v", diagnosis.Findings, err)
	}
	removals.github, removals.githubErr = domain.GitHubRepositoryConnection{}, nil

	// The copy's log says the server took another report than it holds now.
	removals.sessions[0].BodyHash = "sha256:other"
	if diagnosis, err = care.Diagnose(context.Background(), "/work/app"); err != nil || diagnosisCode(diagnosis, "server-differs") == nil {
		t.Fatalf("a report changed on the server: %+v %v", diagnosis.Findings, err)
	}
	// A server that cannot be read is a finding, and the rest is still said.
	removals.serverErr = errors.New("server answered 401")
	diagnosis, err = care.Diagnose(context.Background(), "/work/app")
	if err != nil || diagnosisCode(diagnosis, "server-not-read") == nil || diagnosisCode(diagnosis, "server-not-read").Fix != "cxt login" || diagnosisCode(diagnosis, "unread-records") == nil {
		t.Fatalf("an unreadable server: %+v %v", diagnosis.Findings, err)
	}
	// Without a server configured nothing is asked of one.
	facts.Connected = false
	removals.serverErr = nil
	removals.summariesAsked = nil
	if diagnosis, err = care.Diagnose(context.Background(), "/work/app"); err != nil || diagnosisCode(diagnosis, "not-connected") == nil || len(removals.summariesAsked) != 0 {
		t.Fatalf("not connected: %+v %v", diagnosis.Findings, err)
	}
	if _, err := NewSegmentCareService(store).Diagnose(context.Background(), "/work/app"); err == nil {
		t.Fatal("a diagnosis without readers succeeded")
	}
}

func diagnosisCode(diagnosis domain.SegmentDiagnosis, code string) *domain.SegmentFinding {
	for index := range diagnosis.Findings {
		if diagnosis.Findings[index].Code == code {
			return &diagnosis.Findings[index]
		}
	}
	return nil
}

func TestRefreshAndArchiveThroughTheService(t *testing.T) {
	removals := &removalRemoteFake{}
	records := &removalRecordsFake{}
	repo := func(context.Context, string) (domain.Repo, error) {
		return domain.Repo{ID: domain.HashContent([]byte("synthetic repository"))}, nil
	}
	care := NewSegmentCareService(records).WithServer(removals, repo, segmentUploadFixture(&segmentRemoteFake{}), time.Now)
	key := domain.SessionUploadKey{Provider: domain.ProviderCodex, SessionID: "thread-1"}
	if err := care.RefreshRemote(context.Background(), "/work/app"); err != nil || removals.refreshes != 1 {
		t.Fatalf("refresh: %v %d", err, removals.refreshes)
	}
	for _, archived := range []bool{true, false} {
		if err := care.SetArchived(context.Background(), "/work/app", key, archived); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(removals.archives, []string{"codex/thread-1 archived", "codex/thread-1 restored"}) ||
		!reflect.DeepEqual(records.actions, []string{"codex/thread-1 requested archived on the server", "codex/thread-1 requested restored from the archive on the server"}) {
		t.Fatalf("archives: %v actions: %v", removals.archives, records.actions)
	}
	// What the server refuses is not logged as done.
	records.actions, removals.err = nil, errors.New("synthetic refusal")
	if err := care.SetArchived(context.Background(), "/work/app", key, true); err == nil || len(records.actions) != 0 {
		t.Fatalf("a refused archive: %v %v", err, records.actions)
	}
	if err := care.RefreshRemote(context.Background(), "/work/app"); err == nil {
		t.Fatal("a refused refresh succeeded")
	}
	if err := NewSegmentCareService(records).RefreshRemote(context.Background(), "/work/app"); err == nil {
		t.Fatal("a refresh without a server succeeded")
	}
}

type resendRecords struct {
	removalRecordsFake
	record  domain.SessionUploadRecord
	entries []domain.SessionUploadLogEntry
}

func (f *resendRecords) ReadSessionUpload(_ context.Context, _ string, key domain.SessionUploadKey, withConversation bool) (domain.SessionUploadRecord, error) {
	if key.SessionID != f.record.SessionID || !withConversation {
		return domain.SessionUploadRecord{}, domain.ErrNotFound
	}
	return f.record, nil
}

func (f *resendRecords) LogSessionAction(_ context.Context, _ string, _ domain.SessionUploadKey, entry domain.SessionUploadLogEntry) error {
	f.entries = append(f.entries, entry)
	return nil
}

// The copy kept under .cxt is sent again as it was: the conversation, the
// report, and the summaries written anew from that conversation. Nothing is
// read from the tool.
func TestResendSendsTheKeptCopyAndNothingFromTheTool(t *testing.T) {
	sha := strings.Repeat("a", 40)
	key := domain.SessionUploadKey{Provider: domain.ProviderClaude, SessionID: "records-gone"}
	conversation := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: domain.CIRVersionV1, SourceProvider: domain.ProviderClaude, SessionOriginID: key.SessionID},
		Events: []domain.Event{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "synthetic"}}}, {Seq: 1, Kind: domain.EventMessage, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: "done"}}}}}
	doc := domain.HashContent([]byte(key.SessionID)) // what the content fake answers for this conversation
	body := domain.AnchoredSessionBody{RuleVersion: 1, Scope: domain.SessionScopeFolder, Records: 2, Content: &domain.AnchoredContent{Events: 2, Parts: []domain.AnchoredContentPart{{Doc: doc, Events: 2}}},
		Segments: []domain.AnchoredSegment{{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", LastOrdinal: 1, Events: 2}, Commits: []domain.AnchoredCommit{{SHA: sha, Rule: domain.SegmentLinkPrintedNumber}}}}}
	records := &resendRecords{record: domain.SessionUploadRecord{Provider: key.Provider, SessionID: key.SessionID, Report: &body, ConversationKept: true, Conversation: &conversation}}
	remote, source, content := &segmentRemoteFake{}, &segmentContentSourceFake{}, &segmentContentRemoteFake{}
	summarizer, summaries := &segmentSummarizerFake{}, &segmentSummaryRemoteFake{}
	removals := &removalRemoteFake{}
	service := segmentUploadFixture(remote).WithSessionContent(content, map[domain.ProviderKind]outbound.SessionContentSource{domain.ProviderClaude: source}).
		WithSummaries(summaries, summarizer).WithRemovals(removals).WithUploadRecords(records, func() time.Time { return time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC) })

	result, err := service.ResendSession(context.Background(), "/work/app", key)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stored != 1 || len(result.Sessions) != 1 || result.Sessions[0].State != domain.SegmentPushStored || result.Sessions[0].Content != domain.SegmentContentStored || result.Sessions[0].Summaries != 1 {
		t.Fatalf("result: %+v", result)
	}
	if len(source.read) != 0 {
		t.Fatal("the tool's records were read")
	}
	if len(content.docs) != 1 || len(content.docs[0].Events) != 2 || len(remote.uploads) != 1 || !reflect.DeepEqual(remote.uploads[0].Body, body) || len(summaries.stored) != 1 {
		t.Fatalf("sent: documents=%d reports=%+v summaries=%v", len(content.docs), remote.uploads, summaries.stored)
	}
	if len(records.entries) != 1 || records.entries[0].Action != "sent again from the copy" || records.entries[0].Report != domain.SegmentPushStored || records.entries[0].Doc != doc ||
		records.entries[0].Parts != 1 || records.entries[0].Events != 2 {
		t.Fatalf("log: %+v", records.entries)
	}

	// A copy of the conversation that is not the document the report names
	// stops before the report.
	remote.uploads = nil
	content.answer = func(domain.CIRDocument) (domain.DocumentRef, error) {
		return domain.DocumentRef{Hash: domain.HashContent([]byte("another document"))}, nil
	}
	if _, err := service.ResendSession(context.Background(), "/work/app", key); err == nil || !strings.Contains(err.Error(), "is not the document its report names") || len(remote.uploads) != 0 {
		t.Fatalf("another document: %v uploads=%d", err, len(remote.uploads))
	}
	content.answer = nil
	// The copy is one document: a report that names two is not sent from it.
	two := body
	two.Content = &domain.AnchoredContent{Events: 2, Parts: []domain.AnchoredContentPart{{Doc: doc, Events: 1}, {Doc: domain.HashContent([]byte("a second document")), Events: 1}}}
	records.record.Report = &two
	if _, err := service.ResendSession(context.Background(), "/work/app", key); err == nil || !strings.Contains(err.Error(), "is not the document its report names") || len(remote.uploads) != 0 {
		t.Fatalf("a report of two documents: %v uploads=%d", err, len(remote.uploads))
	}
	records.record.Report = &body
	// A removed session is not sent from its copy either.
	removals.listed = []domain.AnchoredSessionRemoval{{Provider: key.Provider, SessionID: key.SessionID, ByYou: true}}
	content.docs = nil
	if result, err := service.ResendSession(context.Background(), "/work/app", key); err != nil || result.Removed != 1 || len(content.docs) != 0 || len(remote.uploads) != 0 {
		t.Fatalf("a removed session: %+v %v", result, err)
	}
	removals.listed = nil
	// No copy, a copy without its conversation, and a refusal by the server.
	if _, err := service.ResendSession(context.Background(), "/work/app", domain.SessionUploadKey{Provider: domain.ProviderClaude, SessionID: "never-kept"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("no copy: %v", err)
	}
	records.record.Conversation = nil
	if _, err := service.ResendSession(context.Background(), "/work/app", key); err == nil || !strings.Contains(err.Error(), "no copy of it is kept") {
		t.Fatalf("no conversation copy: %v", err)
	}
	records.record.Conversation = &conversation
	remote.answer = func(domain.AnchoredSessionUpload) (domain.AnchoredSessionReceipt, error) {
		return domain.AnchoredSessionReceipt{}, segmentUploadStatus(409)
	}
	if result, err := service.ResendSession(context.Background(), "/work/app", key); err != nil || result.Refused != 1 || result.Sessions[0].State != domain.SegmentPushRefused {
		t.Fatalf("a refused copy: %+v %v", result, err)
	}
}
