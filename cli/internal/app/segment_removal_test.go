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
