package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
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
