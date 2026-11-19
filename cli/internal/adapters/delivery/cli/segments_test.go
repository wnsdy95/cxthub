package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type segmentReporterFunc func(context.Context, string) (domain.SegmentReport, error)

func (f segmentReporterFunc) SegmentReport(ctx context.Context, cwd string) (domain.SegmentReport, error) {
	return f(ctx, cwd)
}

// syntheticSegmentReport is built by the real rules from synthetic records, so
// the printed text is checked against a report the command can really produce.
func syntheticSegmentReport() domain.SegmentReport {
	const day = int64(1_800_000_000_000)
	sha := strings.Repeat("a", 40)
	worktree := domain.RecordedWorktree{Root: "/work/app", Main: true, Current: "main", Made: []domain.MadeCommit{{AtSec: day/1000 + 500, SHA: strings.Repeat("b", 40)}}}
	return domain.BuildSegmentReport(domain.SegmentInputs{
		Git: domain.GitRecords{Root: "/work/app", Origin: "example.test/team/app", Worktrees: []domain.RecordedWorktree{worktree},
			Commits: []domain.RecordedCommit{{SHA: sha, CreatedSec: day/1000 + 20, OnBranch: true}}},
		Sessions: []domain.RecordedSession{
			{Provider: domain.ProviderClaude, ID: "parent-session", Scope: domain.SessionScopeFolder, Items: []domain.RecordedItem{
				{ID: "m1", Turn: "t1", AtMs: day + 10_000, Folder: "/work/app", Branch: "main"},
				{ID: "m2", Turn: "t1", AtMs: day + 21_000, Folder: "/work/app", Branch: "main", Commands: []domain.RecordedCommand{
					{StartedMs: day + 19_000, EndedMs: day + 21_000, Folder: "/work/app", Tokens: []string{"aaaaaaa"}},
				}},
				{ID: "m3", Turn: "t2", AtMs: day + 30_000, Folder: "/work/app", Detached: true},
			}},
			{Provider: domain.ProviderClaude, ID: "sub-session", Sub: true, Parent: "parent-session", SpawnItem: "m1", Scope: domain.SessionScopeFolder, FileCopies: 1,
				Items: []domain.RecordedItem{{ID: "s1", AtMs: day + 15_000}}},
			{Provider: domain.ProviderCodex, ID: "lead-thread", Scope: domain.SessionScopeOriginURL},
		},
		Calls:  []domain.RecordedAgentCall{{Provider: domain.ProviderCodex, Tool: "sendInput", From: "lead-thread", To: []string{"worker-thread"}, Status: "completed", AtMs: day + 40_000, Item: "t1/i1"}},
		Unread: []domain.UnreadSessionRecord{{Provider: domain.ProviderCodex, Source: "/home/dev/.codex/state_9.sqlite", Reason: "thread database could not be read: synthetic"}},
	})
}

func TestRunSegmentsPrintsEveryLinkWithItsRule(t *testing.T) {
	report := syntheticSegmentReport()
	reporter := segmentReporterFunc(func(_ context.Context, cwd string) (domain.SegmentReport, error) {
		if cwd != "/work/app/pkg" {
			t.Fatalf("cwd = %q", cwd)
		}
		return report, nil
	})
	var text bytes.Buffer
	if err := RunSegments(context.Background(), reporter, "/work/app/pkg", false, &text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Session segments for /work/app (origin example.test/team/app) · worktrees: 1",
		"Nothing was written or uploaded.",
		"sessions          3 (claude 1, claude sub 1, codex 1, codex sub 0)",
		"linked commits    1 (1 by printed-number, 0 by same-folder-and-time) · 1 with one session, 0 with more",
		"HEAD-log commits  1 made in a worktree, 1 of them without a session",
		"Unread session records (1)",
		"[codex] /home/dev/.codex/state_9.sqlite: thread database could not be read: synthetic",
		"claude parent-session  3 records, 1 commands",
		"in this repository by working-folder",
		"segment 1  m1 .. m2  2 records, 1 commands  to the end of its turn",
		"commit " + strings.Repeat("a", 40) + "  linked by printed-number",
		"uncommitted  m3 .. m3  1 records, 0 commands",
		"place /work/app @ (no branch)  read from recorded-by-tool",
		"claude sub sub-session  spawned by parent-session",
		"1 identical copies of this session's file were read once",
		"place unknown: no record names a folder of this repository",
		"codex lead-thread  0 records, 0 commands  in this repository by origin-url",
		strings.Repeat("a", 40) + "  2027-01-15T08:00:20.000Z  local: on-a-branch",
		"session claude parent-session  linked by printed-number",
		strings.Repeat("b", 40) + "  2027-01-15T08:08:20.000Z  local: object-not-in-git",
		"made in /work/app",
		"no session",
		"claude parent-session -> sub-session  at m1 in the parent's segment 1, found by tool-call",
		"codex sendInput  lead-thread -> worker-thread  completed  2027-01-15T08:00:40.000Z",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text output lacks %q", want)
		}
	}
	if t.Failed() {
		t.Logf("output:\n%s", text.String())
	}

	var encoded bytes.Buffer
	if err := RunSegments(context.Background(), reporter, "/work/app/pkg", true, &encoded); err != nil {
		t.Fatal(err)
	}
	if strings.Count(encoded.String(), "\n") != 1 {
		t.Fatalf("JSON output is one line: %q", encoded.String())
	}
	var decoded domain.SegmentReport
	if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, report) {
		t.Fatalf("JSON does not round-trip:\n got %+v\nwant %+v", decoded, report)
	}
	for _, key := range []string{`"version":1`, `"unread":[`, `"sessions":[`, `"commits":[`, `"spawns":[`, `"agent_calls":[`, `"rule":"printed-number"`, `"scope":"origin-url"`} {
		if !strings.Contains(encoded.String(), key) {
			t.Errorf("JSON output lacks %s", key)
		}
	}
}

func TestRunSegmentsReturnsReaderErrors(t *testing.T) {
	failure := errors.New("synthetic failure")
	reporter := segmentReporterFunc(func(context.Context, string) (domain.SegmentReport, error) { return domain.SegmentReport{}, failure })
	var out bytes.Buffer
	if err := RunSegments(context.Background(), reporter, "/work/app", false, &out); !errors.Is(err, failure) || out.Len() != 0 {
		t.Fatalf("err = %v, output = %q", err, out.String())
	}
	if err := RunSegments(context.Background(), nil, "/work/app", false, &out); err == nil {
		t.Fatal("a missing reader must be an error")
	}
}

func TestSegmentsCommandContract(t *testing.T) {
	if !ReadOnlyInvocation([]string{"cxt", "segments", "--json"}) {
		t.Fatal("segments must be classified as a read-only command")
	}
	// Uploading is not reading, even though it writes nothing on this computer.
	if ReadOnlyInvocation([]string{"cxt", "segments", "push"}) || ReadOnlyInvocation([]string{"cxt", "segments", "push", "--json"}) {
		t.Fatal("segments push must not be classified as a read-only command")
	}
	for _, args := range [][]string{{"cxt", "segments", "extra"}, {"cxt", "segments", "--all"}, {"cxt", "segments", "--json", "--json"},
		{"cxt", "segments", "push", "extra"}, {"cxt", "segments", "pull"}, {"cxt", "segments", "push", "--force"}} {
		if err := Run(nil, args); err == nil {
			t.Fatalf("%v must be rejected before dispatch", args)
		}
	}
	// Without a wired reader the command fails; it never falls through to a
	// command that could write.
	if err := Run(&Container{}, []string{"cxt", "segments"}); err == nil || !strings.Contains(err.Error(), "session record reader unavailable") {
		t.Fatalf("unwired command: %v", err)
	}
	// A wired reader alone never uploads.
	reader := segmentReporterFunc(func(context.Context, string) (domain.SegmentReport, error) {
		t.Fatal("push must not fall back to the read-only report")
		return domain.SegmentReport{}, nil
	})
	if err := Run(&Container{Segments: reader}, []string{"cxt", "segments", "push"}); err == nil || !strings.Contains(err.Error(), "session report upload unavailable") {
		t.Fatalf("unwired upload: %v", err)
	}
}

type segmentUploaderFunc func(context.Context, string) (domain.SegmentPushResult, error)

func (f segmentUploaderFunc) PushSegments(ctx context.Context, cwd string) (domain.SegmentPushResult, error) {
	return f(ctx, cwd)
}

func TestRunSegmentsPushListsWhatIsNotOnTheServer(t *testing.T) {
	result := domain.SegmentPushResult{Version: 1, Stored: 1, Unchanged: 1, Refused: 1, AgentCallsWithoutSender: 2,
		Sessions: []domain.SegmentPushSession{
			{Provider: domain.ProviderClaude, ID: "stored", State: domain.SegmentPushStored, Revision: 1},
			{Provider: domain.ProviderCodex, ID: "same", State: domain.SegmentPushUnchanged, Revision: 4},
			{Provider: domain.ProviderCodex, ID: "taken", State: domain.SegmentPushRefused, Reason: "synthetic refusal"},
		},
		Unread: []domain.UnreadSessionRecord{{Provider: domain.ProviderCodex, Source: "state_5.sqlite", Reason: "synthetic layout"}},
	}
	var cwd string
	uploader := segmentUploaderFunc(func(_ context.Context, dir string) (domain.SegmentPushResult, error) {
		cwd = dir
		return result, nil
	})
	var out bytes.Buffer
	err := RunSegmentsPush(context.Background(), uploader, "/work/app", false, &out)
	// The other reports were stored, and the command still fails so the gap is noticed.
	if err == nil || !strings.Contains(err.Error(), "1 of 3 session reports were refused") || cwd != "/work/app" {
		t.Fatalf("err=%v cwd=%q", err, cwd)
	}
	for _, want := range []string{
		"Session reports: 1 stored, 1 unchanged, 1 refused (3 sessions)",
		"Only you can read these reports on the server. Nothing was written on this computer.",
		"Refused (1) — these sessions are not on the server",
		"  codex taken: synthetic refusal",
		"Unread session records (1) — these exist but were not read, so no report covers them",
		"  [codex] state_5.sqlite: synthetic layout",
		"2 agent calls were recorded by a session outside this repository and were not uploaded",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "stored:") || strings.Contains(out.String(), "claude stored") {
		t.Fatalf("a stored session is listed as a gap:\n%s", out.String())
	}

	out.Reset()
	if err := RunSegmentsPush(context.Background(), uploader, "/work/app", true, &out); err == nil {
		t.Fatal("a refused report must fail the JSON form too")
	}
	var decoded domain.SegmentPushResult
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || !reflect.DeepEqual(decoded, result) {
		t.Fatalf("json: %v\n%s", err, out.String())
	}

	result.Refused, result.Sessions = 0, result.Sessions[:2]
	out.Reset()
	if err := RunSegmentsPush(context.Background(), uploader, "/work/app", false, &out); err != nil || strings.Contains(out.String(), "Refused") {
		t.Fatalf("nothing refused: %v\n%s", err, out.String())
	}
	failure := errors.New("synthetic failure")
	failing := segmentUploaderFunc(func(context.Context, string) (domain.SegmentPushResult, error) {
		return domain.SegmentPushResult{}, failure
	})
	if err := RunSegmentsPush(context.Background(), failing, "/work/app", false, &out); !errors.Is(err, failure) {
		t.Fatalf("upload error: %v", err)
	}
	if err := RunSegmentsPush(context.Background(), nil, "/work/app", false, &out); err == nil {
		t.Fatal("a missing uploader must be an error")
	}
}
