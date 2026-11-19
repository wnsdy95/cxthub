package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type segmentUploadStatus int

func (s segmentUploadStatus) Error() string   { return fmt.Sprintf("synthetic status %d", int(s)) }
func (s segmentUploadStatus) StatusCode() int { return int(s) }

type segmentRemoteFake struct {
	repo    string
	uploads []domain.AnchoredSessionUpload
	answer  func(domain.AnchoredSessionUpload) (domain.AnchoredSessionReceipt, error)
}

func (f *segmentRemoteFake) PutAnchoredSession(_ context.Context, repo string, upload domain.AnchoredSessionUpload) (domain.AnchoredSessionReceipt, error) {
	f.repo = repo
	f.uploads = append(f.uploads, upload)
	if f.answer == nil {
		return domain.AnchoredSessionReceipt{Revision: 1, Changed: true}, nil
	}
	return f.answer(upload)
}

func segmentUploadFixture(remote *segmentRemoteFake) *SegmentUploadService {
	sha := strings.Repeat("a", 40)
	git := &segmentGitFake{
		records: domain.GitRecords{Root: "/work/app", Worktrees: []domain.RecordedWorktree{{Root: "/work/app", Main: true, Current: "main",
			Made: []domain.MadeCommit{{SHA: sha, AtSec: 2}}}},
			Commits: []domain.RecordedCommit{{SHA: sha, CreatedSec: 2, AuthoredSec: 2, AuthorEmail: "dev@example.test", Subject: "feat: synthetic", OnBranch: true}}},
		unread: []domain.UnreadSessionRecord{{Source: "git", Reason: "synthetic"}},
	}
	item := func(id string, atMs int64, commands ...domain.RecordedCommand) domain.RecordedItem {
		return domain.RecordedItem{ID: id, Turn: "t1", AtMs: atMs, Folder: "/work/app", Branch: "main", Commands: commands}
	}
	claude := &segmentSourceFake{batch: domain.SessionRecordBatch{Sessions: []domain.RecordedSession{
		{Provider: domain.ProviderClaude, ID: "with-commit", Scope: domain.SessionScopeFolder, Items: []domain.RecordedItem{
			item("m1", 1000), item("m2", 2500, domain.RecordedCommand{StartedMs: 1500, EndedMs: 2500, Folder: "/work/app", Tokens: []string{sha[:7]}}), item("m3", 3000)}},
		{Provider: domain.ProviderClaude, ID: "not/a key", Scope: domain.SessionScopeFolder, Items: []domain.RecordedItem{item("n1", 5000)}},
		{Provider: domain.ProviderClaude, ID: "without-commit", Scope: domain.SessionScopeFolder, Items: []domain.RecordedItem{item("w1", 6000)}},
	}}}
	repo := func(context.Context, string) (domain.Repo, error) {
		return domain.Repo{ID: domain.HashContent([]byte("synthetic repository"))}, nil
	}
	return NewSegmentUploadService(NewSegmentReportService(git, claude), remote, repo, "laptop")
}

func TestSegmentUploadSendsOneReportPerSession(t *testing.T) {
	remote := &segmentRemoteFake{answer: func(upload domain.AnchoredSessionUpload) (domain.AnchoredSessionReceipt, error) {
		return domain.AnchoredSessionReceipt{Revision: 3, Changed: upload.SessionID == "with-commit"}, nil
	}}
	result, err := segmentUploadFixture(remote).PushSegments(context.Background(), "/work/app")
	if err != nil {
		t.Fatal(err)
	}
	if remote.repo != string(domain.HashContent([]byte("synthetic repository"))) || len(remote.uploads) != 2 {
		t.Fatalf("repo=%s uploads=%d", remote.repo, len(remote.uploads))
	}
	first := remote.uploads[0]
	if first.SessionID != "with-commit" || first.Provider != domain.ProviderClaude || first.Body.Machine != "laptop" || first.Body.Records != 3 ||
		len(first.Body.Segments) != 1 || first.Body.Segments[0].Commits[0].Subject != "feat: synthetic" || first.Body.Segments[0].LastOrdinal != 2 {
		t.Fatalf("first upload: %+v", first)
	}
	want := []domain.SegmentPushSession{
		{Provider: domain.ProviderClaude, ID: "with-commit", State: domain.SegmentPushStored, Revision: 3},
		// An identifier the server cannot key by is listed, not sent and not rewritten.
		{Provider: domain.ProviderClaude, ID: "not/a key", State: domain.SegmentPushRefused, Reason: "the tool's session identifier cannot be used as a key on the server"},
		{Provider: domain.ProviderClaude, ID: "without-commit", State: domain.SegmentPushUnchanged, Revision: 3},
	}
	if !reflect.DeepEqual(result.Sessions, want) || result.Stored != 1 || result.Unchanged != 1 || result.Refused != 1 {
		t.Fatalf("result: %+v", result)
	}
	// What the report could not read is carried into the result.
	if len(result.Unread) != 1 || result.Unread[0].Source != "git" {
		t.Fatalf("unread: %+v", result.Unread)
	}
}

func TestSegmentUploadKeepsGoingAfterARefusedReportOnly(t *testing.T) {
	for _, status := range []int{409, 413, 422} {
		remote := &segmentRemoteFake{answer: func(upload domain.AnchoredSessionUpload) (domain.AnchoredSessionReceipt, error) {
			if upload.SessionID == "with-commit" {
				return domain.AnchoredSessionReceipt{}, fmt.Errorf("wrapped: %w", segmentUploadStatus(status))
			}
			return domain.AnchoredSessionReceipt{Revision: 1, Changed: true}, nil
		}}
		result, err := segmentUploadFixture(remote).PushSegments(context.Background(), "/work/app")
		if err != nil || len(remote.uploads) != 2 || result.Refused != 2 || result.Stored != 1 || !strings.Contains(result.Sessions[0].Reason, fmt.Sprint(status)) {
			t.Fatalf("status %d: %+v %v", status, result, err)
		}
	}
	// Anything else would fail the same way for every session: stop at once.
	for _, failure := range []error{segmentUploadStatus(401), segmentUploadStatus(403), segmentUploadStatus(404), segmentUploadStatus(503), errors.New("connection refused")} {
		remote := &segmentRemoteFake{answer: func(domain.AnchoredSessionUpload) (domain.AnchoredSessionReceipt, error) {
			return domain.AnchoredSessionReceipt{}, failure
		}}
		result, err := segmentUploadFixture(remote).PushSegments(context.Background(), "/work/app")
		if !errors.Is(err, failure) || len(remote.uploads) != 1 || len(result.Sessions) != 0 || !strings.Contains(err.Error(), "stopped after 0 of 3 session reports") {
			t.Fatalf("%v: uploads=%d result=%+v err=%v", failure, len(remote.uploads), result, err)
		}
	}
}

func TestSegmentUploadStopsBeforeUploadingWhenItCannotRead(t *testing.T) {
	remote := &segmentRemoteFake{}
	failure := errors.New("synthetic failure")
	noRepo := func(context.Context, string) (domain.Repo, error) { return domain.Repo{}, failure }
	if _, err := NewSegmentUploadService(NewSegmentReportService(&segmentGitFake{}), remote, noRepo, "").PushSegments(context.Background(), "/tmp"); !errors.Is(err, failure) {
		t.Fatalf("repository error: %v", err)
	}
	repo := func(context.Context, string) (domain.Repo, error) {
		return domain.Repo{ID: domain.HashContent(nil)}, nil
	}
	if _, err := NewSegmentUploadService(NewSegmentReportService(&segmentGitFake{err: domain.ErrNotGitRepo}), remote, repo, "").PushSegments(context.Background(), "/tmp"); !errors.Is(err, domain.ErrNotGitRepo) {
		t.Fatalf("read error: %v", err)
	}
	if _, err := (&SegmentUploadService{}).PushSegments(context.Background(), "/tmp"); err == nil {
		t.Fatal("an unwired service must fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := segmentUploadFixture(remote)
	remote.answer = func(domain.AnchoredSessionUpload) (domain.AnchoredSessionReceipt, error) {
		cancel()
		return domain.AnchoredSessionReceipt{Revision: 1, Changed: true}, nil
	}
	if result, err := service.PushSegments(ctx, "/work/app"); !errors.Is(err, context.Canceled) || len(remote.uploads) != 1 || result.Stored != 1 {
		t.Fatalf("cancelled: %+v %v uploads=%d", result, err, len(remote.uploads))
	}
}

type segmentContentSourceFake struct {
	read    []domain.SessionRecordRanges
	roots   []string
	answer  func(domain.SessionRecordRanges) (domain.SessionContent, error)
	missing map[string][]string
}

func (f *segmentContentSourceFake) ReadSessionContent(_ context.Context, root string, records domain.SessionRecordRanges) (domain.SessionContent, error) {
	f.read = append(f.read, records)
	f.roots = append(f.roots, root)
	if f.answer != nil {
		return f.answer(records)
	}
	// Two events for every record, so ranges and events never coincide.
	content := domain.SessionContent{Envelope: domain.Envelope{CIRVersion: domain.CIRVersionV1, SourceProvider: records.Provider, SessionOriginID: records.ID}, Missing: f.missing[records.ID]}
	seq := 0
	for _, ids := range records.Ranges {
		var events []domain.Event
		for _, id := range ids {
			for _, half := range []string{"a", "b"} {
				events = append(events, domain.Event{Kind: domain.EventMessage, Seq: seq, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: id + half}}})
				seq++
			}
		}
		content.Ranges = append(content.Ranges, events)
	}
	return content, nil
}

type segmentContentRemoteFake struct {
	docs   []domain.CIRDocument
	answer func(domain.CIRDocument) (domain.DocumentRef, error)
}

func (f *segmentContentRemoteFake) PutSessionContent(_ context.Context, _ string, doc domain.CIRDocument) (domain.DocumentRef, error) {
	f.docs = append(f.docs, doc)
	if f.answer != nil {
		return f.answer(doc)
	}
	return domain.DocumentRef{Hash: domain.HashContent([]byte(doc.Envelope.SessionOriginID))}, nil
}

func TestSegmentUploadNamesTheConversationInTheReport(t *testing.T) {
	remote, source, content := &segmentRemoteFake{}, &segmentContentSourceFake{}, &segmentContentRemoteFake{}
	service := segmentUploadFixture(remote).WithSessionContent(content, map[domain.ProviderKind]outbound.SessionContentSource{domain.ProviderClaude: source})
	result, err := service.PushSegments(context.Background(), "/work/app")
	if err != nil {
		t.Fatal(err)
	}
	// The document goes up before the report that names it.
	if len(content.docs) != 2 || len(remote.uploads) != 2 || len(source.read) != 2 || source.roots[0] != "/work/app" {
		t.Fatalf("documents=%d reports=%d reads=%d roots=%v", len(content.docs), len(remote.uploads), len(source.read), source.roots)
	}
	first := remote.uploads[0].Body
	want := &domain.AnchoredContent{Doc: domain.HashContent([]byte("with-commit")), Events: 6}
	if first.Content == nil || *first.Content != *want || len(content.docs[0].Events) != 6 {
		t.Fatalf("content = %+v, document events = %d", first.Content, len(content.docs[0].Events))
	}
	// with-commit: one segment of three records that keeps its whole turn.
	if segment := first.Segments[0]; segment.FirstEvent != 0 || segment.Events != 6 || first.Uncommitted != nil {
		t.Fatalf("segment = %+v", segment)
	}
	if !reflect.DeepEqual(source.read[0].Ranges, [][]string{{"m1", "m2", "m3"}}) {
		t.Fatalf("records read back: %v", source.read[0].Ranges)
	}
	// without-commit: only an uncommitted part.
	second := remote.uploads[1].Body
	if second.Content == nil || second.Uncommitted.FirstEvent != 0 || second.Uncommitted.Events != 2 {
		t.Fatalf("second report = %+v %+v", second.Content, second.Uncommitted)
	}
	for _, session := range result.Sessions {
		if session.State == domain.SegmentPushStored && session.Content != domain.SegmentContentStored {
			t.Fatalf("session = %+v", session)
		}
	}
	if result.ContentNotUploaded != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestSegmentUploadKeepsAReportWhoseConversationCannotGoUp(t *testing.T) {
	failure := errors.New("synthetic read failure")
	for name, test := range map[string]struct {
		source  *segmentContentSourceFake
		content *segmentContentRemoteFake
		sources bool
		reason  string
	}{
		"no reader for the tool":  {source: &segmentContentSourceFake{}, content: &segmentContentRemoteFake{}, reason: "is not read yet"},
		"records gone since read": {source: &segmentContentSourceFake{missing: map[string][]string{"with-commit": {"m2"}}}, content: &segmentContentRemoteFake{}, sources: true, reason: "1 records changed while they were read"},
		"the source cannot be read": {source: &segmentContentSourceFake{answer: func(domain.SessionRecordRanges) (domain.SessionContent, error) {
			return domain.SessionContent{}, failure
		}}, content: &segmentContentRemoteFake{}, sources: true, reason: "synthetic read failure"},
		"the document is refused": {source: &segmentContentSourceFake{}, sources: true, reason: "synthetic status 413", content: &segmentContentRemoteFake{answer: func(domain.CIRDocument) (domain.DocumentRef, error) {
			return domain.DocumentRef{}, segmentUploadStatus(413)
		}}},
		"read in other ranges": {sources: true, reason: "other ranges", content: &segmentContentRemoteFake{}, source: &segmentContentSourceFake{answer: func(records domain.SessionRecordRanges) (domain.SessionContent, error) {
			event := domain.Event{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "one"}}}
			return domain.SessionContent{Envelope: domain.Envelope{CIRVersion: domain.CIRVersionV1}, Ranges: [][]domain.Event{{event}, {event}, {event}}}, nil
		}}},
	} {
		remote := &segmentRemoteFake{}
		sources := map[domain.ProviderKind]outbound.SessionContentSource{}
		if test.sources {
			sources[domain.ProviderClaude] = test.source
		}
		result, err := segmentUploadFixture(remote).WithSessionContent(test.content, sources).PushSegments(context.Background(), "/work/app")
		if err != nil || len(remote.uploads) != 2 || result.Stored != 2 {
			t.Fatalf("%s: uploads=%d result=%+v err=%v", name, len(remote.uploads), result, err)
		}
		first := result.Sessions[0]
		if first.Content != domain.SegmentContentNotUploaded || !strings.Contains(first.ContentReason, test.reason) || result.ContentNotUploaded == 0 {
			t.Fatalf("%s: session=%+v result=%+v", name, first, result)
		}
		// The report went up as it would have without content: no range names an event.
		body := remote.uploads[0].Body
		if body.Content != nil || body.Segments[0].FirstEvent != 0 || body.Segments[0].Events != 0 {
			t.Fatalf("%s: report = %+v", name, body)
		}
	}
	// A session whose records hold no conversation has none, and that is not a gap.
	remote := &segmentRemoteFake{}
	empty := &segmentContentSourceFake{answer: func(records domain.SessionRecordRanges) (domain.SessionContent, error) {
		return domain.SessionContent{Ranges: make([][]domain.Event, len(records.Ranges))}, nil
	}}
	content := &segmentContentRemoteFake{}
	result, err := segmentUploadFixture(remote).WithSessionContent(content, map[domain.ProviderKind]outbound.SessionContentSource{domain.ProviderClaude: empty}).PushSegments(context.Background(), "/work/app")
	if err != nil || len(content.docs) != 0 || result.ContentNotUploaded != 0 || result.Sessions[0].Content != domain.SegmentContentNone {
		t.Fatalf("no conversation: docs=%d result=%+v err=%v", len(content.docs), result, err)
	}
	// What would fail for every session stops the run before any report.
	for _, failure := range []error{segmentUploadStatus(401), segmentUploadStatus(503), errors.New("connection refused")} {
		remote := &segmentRemoteFake{}
		refusing := &segmentContentRemoteFake{answer: func(domain.CIRDocument) (domain.DocumentRef, error) { return domain.DocumentRef{}, failure }}
		_, err := segmentUploadFixture(remote).WithSessionContent(refusing, map[domain.ProviderKind]outbound.SessionContentSource{domain.ProviderClaude: &segmentContentSourceFake{}}).PushSegments(context.Background(), "/work/app")
		if !errors.Is(err, failure) || len(remote.uploads) != 0 || !strings.Contains(err.Error(), "stopped after 0 of 3 session reports") {
			t.Fatalf("%v: uploads=%d err=%v", failure, len(remote.uploads), err)
		}
	}
}
