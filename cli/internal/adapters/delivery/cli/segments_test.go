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
	// Uploading one session, removing one and allowing one again name it by
	// its tool and its identifier, and none of them is read-only.
	for _, args := range [][]string{{"cxt", "segments", "push", "claude", "session-1"}, {"cxt", "segments", "remove", "claude", "session-1", "--yes"},
		{"cxt", "segments", "remove", "codex", "thread-1"}, {"cxt", "segments", "allow", "codex", "thread-1", "--json"}} {
		if ReadOnlyInvocation(args) {
			t.Fatalf("%v must not be classified as a read-only command", args)
		}
		if _, _, err := parseCommand(args[1], args[2:]); err != nil {
			t.Fatalf("%v was rejected: %v", args, err)
		}
	}
	for _, args := range [][]string{{"cxt", "segments", "remove"}, {"cxt", "segments", "remove", "claude"}, {"cxt", "segments", "remove", "other", "session-1", "--yes"},
		{"cxt", "segments", "allow", "claude", "../escape"}, {"cxt", "segments", "allow", "claude", "session-1", "--yes"}, {"cxt", "segments", "push", "claude"},
		{"cxt", "segments", "push", "--yes"}, {"cxt", "segments", "--yes"}, {"cxt", "segments", "log", "--yes"}} {
		if err := Run(nil, args); err == nil {
			t.Fatalf("%v must be rejected before dispatch", args)
		}
	}
	// Reading what .cxt keeps is reading, for every session or for one.
	if !ReadOnlyInvocation([]string{"cxt", "segments", "log"}) || !ReadOnlyInvocation([]string{"cxt", "segments", "log", "codex", "thread-1", "--json"}) {
		t.Fatal("segments log must be classified as a read-only command")
	}
	for _, args := range [][]string{{"cxt", "segments", "extra"}, {"cxt", "segments", "--all"}, {"cxt", "segments", "--json", "--json"},
		{"cxt", "segments", "push", "extra"}, {"cxt", "segments", "pull"}, {"cxt", "segments", "push", "--force"},
		{"cxt", "segments", "log", "claude"}, {"cxt", "segments", "log", "other", "session-1"}, {"cxt", "segments", "log", "claude", "../escape"},
		{"cxt", "segments", "log", "claude", "session-1", "extra"}} {
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

// PushSegmentsOf answers what the function answers, with the sessions asked
// for named as not found, so that a test sees which were asked for.
func (f segmentUploaderFunc) PushSegmentsOf(ctx context.Context, cwd string, keys []domain.SessionUploadKey) (domain.SegmentPushResult, error) {
	result, err := f(ctx, cwd)
	result.NotFound = keys
	return result, err
}

func TestRunSegmentsPushListsWhatIsNotOnTheServer(t *testing.T) {
	result := domain.SegmentPushResult{Version: 1, Stored: 1, Unchanged: 1, Refused: 1, ContentNotUploaded: 1, AgentCallsWithoutSender: 2,
		Sessions: []domain.SegmentPushSession{
			{Provider: domain.ProviderClaude, ID: "stored", State: domain.SegmentPushStored, Revision: 1, Content: domain.SegmentContentStored},
			{Provider: domain.ProviderCodex, ID: "same", State: domain.SegmentPushUnchanged, Revision: 4, Content: domain.SegmentContentNotUploaded, ContentReason: "synthetic content gap"},
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
	err := RunSegmentsPush(context.Background(), uploader, "/work/app", nil, false, &out)
	// The other reports were stored, and the command still fails so the gap is noticed.
	if err == nil || !strings.Contains(err.Error(), "1 of 3 session reports were refused") || cwd != "/work/app" {
		t.Fatalf("err=%v cwd=%q", err, cwd)
	}
	for _, want := range []string{
		"Session reports: 1 stored, 1 unchanged, 1 refused (3 sessions)",
		"Conversation: uploaded for 1 sessions, not uploaded for 1",
		"Without conversation (1) — these reports are on the server, their conversation is not",
		"  codex same: synthetic content gap",
		"On the server a report is read by you alone until what it explains is pushed; a pushed part is read by whoever may view the repository.",
		"Nothing was written on this computer.",
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
	if err := RunSegmentsPush(context.Background(), uploader, "/work/app", nil, true, &out); err == nil {
		t.Fatal("a refused report must fail the JSON form too")
	}
	var decoded domain.SegmentPushResult
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || !reflect.DeepEqual(decoded, result) {
		t.Fatalf("json: %v\n%s", err, out.String())
	}

	// A report without its conversation is listed, and is not a failure.
	result.Refused, result.Sessions = 0, result.Sessions[:2]
	out.Reset()
	if err := RunSegmentsPush(context.Background(), uploader, "/work/app", nil, false, &out); err != nil || strings.Contains(out.String(), "Refused") || !strings.Contains(out.String(), "Without conversation (1)") {
		t.Fatalf("nothing refused: %v\n%s", err, out.String())
	}
	// A run that handles branch IDs says what it wrote here and what it could not do.
	withBranches := result
	withBranches.Branches = &domain.SegmentPushBranches{WithID: 4, Assigned: 1, OnOrigin: 3, Registered: 0, NotRegistered: "synthetic refusal"}
	out.Reset()
	branchRun := segmentUploaderFunc(func(context.Context, string) (domain.SegmentPushResult, error) { return withBranches, nil })
	if err := RunSegmentsPush(context.Background(), branchRun, "/work/app", nil, false, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Branch IDs: 4 branches have one (1 new) · 0 of 3 on origin registered",
		"  IDs were not registered in this run: synthetic refusal",
		"On this computer only branch IDs in Git's branch configuration were written.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "Nothing was written on this computer") || strings.Contains(out.String(), "not assigned") {
		t.Fatalf("branch run output:\n%s", out.String())
	}
	// A run that uploads no conversation at all says nothing about it.
	plain := domain.SegmentPushResult{Version: 1, Stored: 1, Sessions: []domain.SegmentPushSession{{Provider: domain.ProviderClaude, ID: "stored", State: domain.SegmentPushStored, Revision: 1}}}
	out.Reset()
	quiet := segmentUploaderFunc(func(context.Context, string) (domain.SegmentPushResult, error) { return plain, nil })
	if err := RunSegmentsPush(context.Background(), quiet, "/work/app", nil, false, &out); err != nil || strings.Contains(out.String(), "Conversation") {
		t.Fatalf("no conversation: %v\n%s", err, out.String())
	}
	// A run that keeps copies says where, and says when one was not written.
	plain.LocalRecords = &domain.SegmentPushLocalRecords{Kept: 1, NotKept: "synthetic disk failure"}
	out.Reset()
	if err := RunSegmentsPush(context.Background(), quiet, "/work/app", nil, false, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Copies: what was sent for 1 sessions is kept under .cxt/sessions, each with a log of its uploads",
		"  a copy was not written in this run: synthetic disk failure",
		"On this computer only the copies in .cxt/sessions were written.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	failure := errors.New("synthetic failure")
	failing := segmentUploaderFunc(func(context.Context, string) (domain.SegmentPushResult, error) {
		return domain.SegmentPushResult{}, failure
	})
	if err := RunSegmentsPush(context.Background(), failing, "/work/app", nil, false, &out); !errors.Is(err, failure) {
		t.Fatalf("upload error: %v", err)
	}
	if err := RunSegmentsPush(context.Background(), nil, "/work/app", nil, false, &out); err == nil {
		t.Fatal("a missing uploader must be an error")
	}
}

type segmentCareFake struct {
	asked    *domain.SessionUploadKey
	listings []domain.SessionUploadListing
	removed  domain.SegmentRemoveResult
	allowed  domain.SegmentAllowResult
	err      error
}

func (f *segmentCareFake) RemoveSession(_ context.Context, _ string, key domain.SessionUploadKey) (domain.SegmentRemoveResult, error) {
	f.asked = &key
	return f.removed, f.err
}

func (f *segmentCareFake) AllowSession(_ context.Context, _ string, key domain.SessionUploadKey) (domain.SegmentAllowResult, error) {
	f.asked = &key
	return f.allowed, f.err
}

func (f *segmentCareFake) UploadLog(_ context.Context, _ string, key *domain.SessionUploadKey) ([]domain.SessionUploadListing, error) {
	f.asked = key
	return f.listings, f.err
}

func TestSegmentsLogSaysWhatIsKeptAndWhatHappened(t *testing.T) {
	doc := domain.ContentHash("sha256:" + strings.Repeat("a", 64))
	care := &segmentCareFake{listings: []domain.SessionUploadListing{
		{Provider: domain.ProviderClaude, SessionID: "session-1", ReportKept: true, ConversationKept: true, Segments: 2, Records: 9, Doc: doc, Events: 14, Lines: 3,
			Log: []domain.SessionUploadLogEntry{{At: "2031-01-02T03:04:05Z", Automatic: true, Report: domain.SegmentPushStored, Revision: 4, Content: domain.SegmentContentStored, Events: 14, Summaries: 1}}},
		{Provider: domain.ProviderCodex, SessionID: "thread-1", Lines: 2,
			Log: []domain.SessionUploadLogEntry{{At: "2031-01-02T03:05:05Z", Report: domain.SegmentPushRefused, Reason: "synthetic refusal"}}},
	}}
	var out strings.Builder
	if err := RunSegmentsLog(context.Background(), care, "/work/app", nil, false, &out); err != nil || care.asked != nil {
		t.Fatalf("%v asked=%+v", err, care.asked)
	}
	for _, want := range []string{
		"Upload records kept under .cxt/sessions (2 sessions)",
		"claude session-1",
		"  report copy        2 segments, 9 records",
		"  conversation copy  kept; the report names 14 events in " + string(doc),
		"  last of 3 log lines:",
		"  2031-01-02T03:04:05Z  automatic  report stored (revision 4) · conversation of 14 events · 1 summaries stored",
		"codex thread-1",
		"  report copy        none",
		"  conversation copy  none",
		"  2031-01-02T03:05:05Z  requested  report refused: synthetic refusal",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	// One session is asked for by its tool and identifier; other things done
	// to it have their own lines.
	care.listings = []domain.SessionUploadListing{{Provider: domain.ProviderClaude, SessionID: "session-1", Lines: 2, Log: []domain.SessionUploadLogEntry{
		{At: "2031-01-02T03:06:00Z", Content: domain.SegmentContentNotUploaded, ContentReason: "synthetic gap", Report: domain.SegmentPushUnchanged, SummaryReason: "synthetic summary gap"},
		{At: "2031-01-02T03:07:00Z", Action: "removed from the server"}}}}
	out.Reset()
	if err := RunSegmentsLog(context.Background(), care, "/work/app", []string{"claude", "session-1"}, false, &out); err != nil || care.asked == nil || *care.asked != (domain.SessionUploadKey{Provider: domain.ProviderClaude, SessionID: "session-1"}) {
		t.Fatalf("%v asked=%+v", err, care.asked)
	}
	for _, want := range []string{
		"  2031-01-02T03:06:00Z  requested  report unchanged · conversation not uploaded: synthetic gap · a summary was not stored: synthetic summary gap",
		"  2031-01-02T03:07:00Z  removed from the server",
	} {
		if !strings.Contains(out.String(), want) || strings.Contains(out.String(), "Upload records kept") {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	// JSON is one object, also when nothing is kept.
	care.listings = nil
	out.Reset()
	if err := RunSegmentsLog(context.Background(), care, "/work/app", nil, true, &out); err != nil || strings.TrimSpace(out.String()) != `{"version":1,"sessions":[]}` {
		t.Fatalf("%v %s", err, out.String())
	}
	out.Reset()
	if err := RunSegmentsLog(context.Background(), care, "/work/app", nil, false, &out); err != nil || !strings.Contains(out.String(), "none yet: cxt segments push, or the collector, writes them when it uploads") {
		t.Fatalf("%v %s", err, out.String())
	}
	if err := RunSegmentsLog(context.Background(), nil, "/work/app", nil, false, &out); err == nil {
		t.Fatal("a missing service must be an error")
	}
}

func TestSegmentsPushOfOneSessionAndWhatWasRemoved(t *testing.T) {
	result := domain.SegmentPushResult{Version: 1, Stored: 1, Removed: 1, Sessions: []domain.SegmentPushSession{
		{Provider: domain.ProviderClaude, ID: "kept", State: domain.SegmentPushStored, Revision: 2},
		{Provider: domain.ProviderClaude, ID: "gone", State: domain.SegmentPushRemoved, Reason: "you removed it from the server on 2031-01-02T03:04:05Z; cxt segments allow claude gone lets it be uploaded again"},
	}}
	var asked []domain.SessionUploadKey
	uploader := segmentUploaderFunc(func(context.Context, string) (domain.SegmentPushResult, error) { return result, nil })
	var out strings.Builder
	// A removed session is said, with what brings it back, and is no failure.
	if err := RunSegmentsPush(context.Background(), uploader, "/work/app", nil, false, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Session reports: 1 stored, 0 unchanged, 0 refused, 1 removed from the server (2 sessions)",
		"Removed (1) — these sessions were removed from the server and nothing of them was sent",
		"  claude gone: you removed it from the server on 2031-01-02T03:04:05Z; cxt segments allow claude gone lets it be uploaded again",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	// One session is asked for by name; one the tools did not record fails.
	named := segmentNamedUploader{result: domain.SegmentPushResult{Version: 1, Stored: 1, Sessions: result.Sessions[:1]}, asked: &asked}
	out.Reset()
	if err := RunSegmentsPush(context.Background(), named, "/work/app", []string{"claude", "kept"}, false, &out); err != nil || len(asked) != 1 || asked[0] != (domain.SessionUploadKey{Provider: domain.ProviderClaude, SessionID: "kept"}) {
		t.Fatalf("%v asked=%+v", err, asked)
	}
	named.result = domain.SegmentPushResult{Version: 1, NotFound: []domain.SessionUploadKey{{Provider: domain.ProviderCodex, SessionID: "absent"}}}
	if err := RunSegmentsPush(context.Background(), named, "/work/app", []string{"codex", "absent"}, false, &out); err == nil || !strings.Contains(err.Error(), "codex absent is not among the sessions the tools recorded for this repository on this computer") {
		t.Fatalf("a session the tools did not record: %v", err)
	}
}

type segmentNamedUploader struct {
	result domain.SegmentPushResult
	asked  *[]domain.SessionUploadKey
}

func (u segmentNamedUploader) PushSegments(context.Context, string) (domain.SegmentPushResult, error) {
	return domain.SegmentPushResult{}, errors.New("every session was asked for")
}

func (u segmentNamedUploader) PushSegmentsOf(_ context.Context, _ string, keys []domain.SessionUploadKey) (domain.SegmentPushResult, error) {
	*u.asked = keys
	return u.result, nil
}

func TestSegmentsRemoveNeedsToBeConfirmedAndSaysWhatBringsItBack(t *testing.T) {
	care := &segmentCareFake{removed: domain.SegmentRemoveResult{Version: 1, Removed: []domain.AnchoredSessionRemoval{
		{Provider: domain.ProviderClaude, SessionID: "session-1", RemovedAt: "2031-01-02T03:04:05Z", ByYou: true},
		{Provider: domain.ProviderClaude, SessionID: "sub-1", RemovedAt: "2031-01-02T03:04:05Z", ByYou: true}}}}
	var out strings.Builder
	// Tried without confirmation: nothing reaches the service, and the
	// command fails saying what it would do.
	err := RunSegmentsRemove(context.Background(), care, "/work/app", []string{"claude", "session-1"}, false, false, &out)
	if err == nil || care.asked != nil || !strings.Contains(err.Error(), "nothing was removed") || !strings.Contains(err.Error(), "cxt segments remove claude session-1 --yes") ||
		!strings.Contains(err.Error(), "for every reader") || out.Len() != 0 {
		t.Fatalf("unconfirmed: %v asked=%+v out=%q", err, care.asked, out.String())
	}
	if err := RunSegmentsRemove(context.Background(), care, "/work/app", []string{"claude", "session-1"}, true, false, &out); err != nil || care.asked == nil || care.asked.SessionID != "session-1" {
		t.Fatalf("confirmed: %v asked=%+v", err, care.asked)
	}
	for _, want := range []string{
		"Removed from the server (2 sessions): the report, the summaries and the conversation of each",
		"  claude session-1", "  claude sub-1",
		"Nothing on this computer was removed: the tool's records and the copies under .cxt/sessions stay.",
		"  cxt segments allow claude session-1", "  cxt segments allow claude sub-1",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := RunSegmentsRemove(context.Background(), care, "/work/app", []string{"claude", "session-1"}, true, true, &out); err != nil || !strings.HasPrefix(out.String(), `{"version":1,"removed":[{"provider":"claude","session_id":"session-1"`) {
		t.Fatalf("json: %v %s", err, out.String())
	}
	care.err = errors.New("synthetic refusal")
	if err := RunSegmentsRemove(context.Background(), care, "/work/app", []string{"claude", "session-1"}, true, false, &out); err == nil {
		t.Fatal("a refused removal succeeded")
	}
	if err := RunSegmentsRemove(context.Background(), nil, "/work/app", []string{"claude", "session-1"}, true, false, &out); err == nil {
		t.Fatal("a missing service must be an error")
	}
}

func TestSegmentsAllowSaysWhetherTheSessionWentUpAgain(t *testing.T) {
	key := domain.SessionUploadKey{Provider: domain.ProviderClaude, SessionID: "session-1"}
	care := &segmentCareFake{allowed: domain.SegmentAllowResult{Version: 1, Allowed: key, Upload: domain.SegmentPushResult{Version: 1, Stored: 1,
		Sessions: []domain.SegmentPushSession{{Provider: key.Provider, ID: key.SessionID, State: domain.SegmentPushStored, Revision: 1, Content: domain.SegmentContentStored, Summaries: 2}}}}}
	var out strings.Builder
	if err := RunSegmentsAllow(context.Background(), care, "/work/app", []string{"claude", "session-1"}, false, &out); err != nil || *care.asked != key {
		t.Fatalf("%v asked=%+v", err, care.asked)
	}
	for _, want := range []string{"claude session-1 is allowed on the server again.", "Uploaded again: report stored (revision 1), with its conversation, 2 summaries"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	// The tool's records are gone: allowed, not uploaded, and the command fails so that it is noticed.
	care.allowed.Upload = domain.SegmentPushResult{Version: 1, NotFound: []domain.SessionUploadKey{key}}
	out.Reset()
	if err := RunSegmentsAllow(context.Background(), care, "/work/app", []string{"claude", "session-1"}, false, &out); err == nil ||
		!strings.Contains(out.String(), "It was not uploaded: the tool's records of this session are no longer on this computer.") {
		t.Fatalf("records gone: %v\n%s", err, out.String())
	}
	care.allowed.Upload = domain.SegmentPushResult{Version: 1, Refused: 1, Sessions: []domain.SegmentPushSession{{Provider: key.Provider, ID: key.SessionID, State: domain.SegmentPushRefused, Reason: "synthetic refusal"}}}
	out.Reset()
	if err := RunSegmentsAllow(context.Background(), care, "/work/app", []string{"claude", "session-1"}, false, &out); err == nil || !strings.Contains(out.String(), "It was not uploaded: synthetic refusal") {
		t.Fatalf("refused again: %v\n%s", err, out.String())
	}
	care.err = errors.New("synthetic refusal")
	if err := RunSegmentsAllow(context.Background(), care, "/work/app", []string{"claude", "session-1"}, false, &out); err == nil {
		t.Fatal("a refused allow succeeded")
	}
}
