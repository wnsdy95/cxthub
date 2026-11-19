package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type segmentGitFake struct {
	records domain.GitRecords
	unread  []domain.UnreadSessionRecord
	err     error
	cwd     string
}

func (f *segmentGitFake) ReadGitRecords(_ context.Context, cwd string) (domain.GitRecords, []domain.UnreadSessionRecord, error) {
	f.cwd = cwd
	return f.records, f.unread, f.err
}

type segmentSourceFake struct {
	batch domain.SessionRecordBatch
	err   error
	scope *domain.SessionRecordScope
}

func (f *segmentSourceFake) ReadSessionRecords(_ context.Context, scope domain.SessionRecordScope) (domain.SessionRecordBatch, error) {
	f.scope = &scope
	return f.batch, f.err
}

func TestSegmentReportCombinesGitAndEveryToolsRecords(t *testing.T) {
	git := &segmentGitFake{
		records: domain.GitRecords{Root: "/work/app", Origin: "example.test/team/app", Worktrees: []domain.RecordedWorktree{{Root: "/work/app", Main: true, Current: "main", Aliases: []string{"/private/work/app"}}}},
		unread:  []domain.UnreadSessionRecord{{Source: "git", Reason: "synthetic"}},
	}
	claude := &segmentSourceFake{batch: domain.SessionRecordBatch{
		Sessions: []domain.RecordedSession{{Provider: domain.ProviderClaude, ID: "c1", Items: []domain.RecordedItem{{ID: "m1", AtMs: 1000, Folder: "/work/app", Branch: "main"}}}},
		Unread:   []domain.UnreadSessionRecord{{Provider: domain.ProviderClaude, Source: "claude", Reason: "synthetic"}},
	}}
	codex := &segmentSourceFake{batch: domain.SessionRecordBatch{
		Sessions: []domain.RecordedSession{{Provider: domain.ProviderCodex, ID: "x1"}},
		Calls:    []domain.RecordedAgentCall{{Provider: domain.ProviderCodex, Tool: "sendInput", From: "x1", To: []string{"x2"}, AtMs: 2000}},
	}}
	report, err := NewSegmentReportService(git, claude, codex).SegmentReport(context.Background(), "/work/app/pkg")
	if err != nil {
		t.Fatal(err)
	}
	wantScope := domain.SessionRecordScope{Folders: []string{"/work/app", "/private/work/app"}, Origin: "example.test/team/app"}
	if git.cwd != "/work/app/pkg" || !reflect.DeepEqual(*claude.scope, wantScope) || !reflect.DeepEqual(*codex.scope, wantScope) {
		t.Fatalf("cwd=%q claude scope=%+v codex scope=%+v", git.cwd, claude.scope, codex.scope)
	}
	if report.Summary.Sessions.Total != 2 || report.Summary.UnreadRecords != 2 || report.Summary.AgentCalls != 1 || report.Repository.Root != "/work/app" {
		t.Fatalf("report = %+v", report)
	}
}

func TestSegmentReportStopsOnAReadError(t *testing.T) {
	failure := errors.New("synthetic failure")
	if _, err := NewSegmentReportService(&segmentGitFake{err: domain.ErrNotGitRepo}).SegmentReport(context.Background(), "/tmp"); !errors.Is(err, domain.ErrNotGitRepo) {
		t.Fatalf("git error: %v", err)
	}
	later := &segmentSourceFake{}
	if _, err := NewSegmentReportService(&segmentGitFake{}, &segmentSourceFake{err: failure}, later).SegmentReport(context.Background(), "/tmp"); !errors.Is(err, failure) || later.scope != nil {
		t.Fatalf("source error: %v, later source read: %v", err, later.scope != nil)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSegmentReportService(&segmentGitFake{}).SegmentReport(ctx, "/tmp"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}
