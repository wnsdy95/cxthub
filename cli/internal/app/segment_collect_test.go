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

type collectorStateFake struct {
	state   domain.CollectorState
	saves   int
	loadErr error
	saveErr error
}

func (f *collectorStateFake) LoadCollectorState(context.Context, string) (domain.CollectorState, error) {
	return f.state, f.loadErr
}

func (f *collectorStateFake) SaveCollectorState(_ context.Context, _ string, state domain.CollectorState) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saves++
	f.state = state
	return nil
}

// collectorFixture is a repository with two Claude Code sessions recorded on
// 2 January 2031: one that made a commit and one that made none.
type collectorFixture struct {
	service *SegmentUploadService
	remote  *segmentRemoteFake
	source  *segmentContentSourceFake
	content *segmentContentRemoteFake
	state   *collectorStateFake
	claude  *segmentSourceFake
	git     *segmentGitFake
	repo    domain.Repo
	epoch   string
	now     time.Time
	// tick is how far the clock moves each time it is read.
	tick time.Duration
}

func collectorDay(second int) int64 {
	return time.Date(2031, 1, 2, 3, 0, second, 0, time.UTC).UnixMilli()
}

func newCollectorFixture(t *testing.T) *collectorFixture {
	t.Helper()
	sha := strings.Repeat("a", 40)
	f := &collectorFixture{remote: &segmentRemoteFake{}, source: &segmentContentSourceFake{}, content: &segmentContentRemoteFake{}, state: &collectorStateFake{},
		repo: domain.Repo{ID: domain.HashContent([]byte("synthetic repository")), RemoteURL: "https://example.test/team/app"}, epoch: "epoch-1",
		now: time.Date(2031, 1, 2, 4, 0, 0, 0, time.UTC)}
	git := &segmentGitFake{records: domain.GitRecords{Root: "/work/app", Worktrees: []domain.RecordedWorktree{{Root: "/work/app", Main: true, Current: "main",
		Made: []domain.MadeCommit{{SHA: sha, AtSec: collectorDay(2) / 1000}}}},
		Commits: []domain.RecordedCommit{{SHA: sha, CreatedSec: collectorDay(2) / 1000, AuthoredSec: collectorDay(2) / 1000, AuthorEmail: "dev@example.test", Subject: "feat: synthetic", OnBranch: true}}}}
	item := func(id string, second int, commands ...domain.RecordedCommand) domain.RecordedItem {
		return domain.RecordedItem{ID: id, Turn: "t1", AtMs: collectorDay(second), Folder: "/work/app", Branch: "main", Commands: commands}
	}
	f.claude = &segmentSourceFake{batch: domain.SessionRecordBatch{Sessions: []domain.RecordedSession{
		{Provider: domain.ProviderClaude, ID: "with-commit", Scope: domain.SessionScopeFolder, Items: []domain.RecordedItem{
			item("m1", 1), item("m2", 3, domain.RecordedCommand{StartedMs: collectorDay(1), EndedMs: collectorDay(3), Folder: "/work/app", Tokens: []string{sha[:7]}}), item("m3", 4)}},
		{Provider: domain.ProviderClaude, ID: "without-commit", Scope: domain.SessionScopeFolder, Items: []domain.RecordedItem{item("w1", 10)}},
	}}}
	repo := func(context.Context, string) (domain.Repo, error) { return f.repo, nil }
	f.git = git
	f.service = NewSegmentUploadService(NewSegmentReportService(git, f.claude), f.remote, repo, "laptop").
		WithSessionContent(f.content, map[domain.ProviderKind]outbound.SessionContentSource{domain.ProviderClaude: f.source}).
		WithCollectorState(f.state, func() time.Time { at := f.now; f.now = f.now.Add(f.tick); return at }, func(context.Context, string) string { return f.epoch })
	return f
}

// began sets when automatic reading began, as an earlier run would have.
func (f *collectorFixture) began(at time.Time) {
	f.state.state = domain.CollectorState{Version: domain.CollectorStateVersion, EnabledAt: at.Format(time.RFC3339Nano)}
}

func (f *collectorFixture) run(t *testing.T) domain.SegmentPushResult {
	t.Helper()
	f.remote.uploads, f.source.read, f.content.docs = nil, nil, nil
	result, err := f.service.CollectSegments(context.Background(), "/work/app")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCollectorBeginsNowAndLeavesEarlierSessionsForARequest(t *testing.T) {
	f := newCollectorFixture(t)
	result := f.run(t)
	// Both sessions were recorded before this first run: nothing is sent.
	if !result.Automatic || result.BeforeAutomatic != 2 || len(result.Sessions) != 0 || len(f.remote.uploads) != 0 || len(f.content.docs) != 0 {
		t.Fatalf("first run: %+v uploads=%d", result, len(f.remote.uploads))
	}
	state := f.state.state
	if state.Version != domain.CollectorStateVersion || state.EnabledAt != "2031-01-02T04:00:00Z" || state.LastRun == nil || state.LastRun.Error != "" || f.state.saves != 1 {
		t.Fatalf("state after the first run: %+v", state)
	}
	// A session that goes on after that moment is read from then on, whole.
	f.claude.batch.Sessions[1].Items = append(f.claude.batch.Sessions[1].Items, domain.RecordedItem{ID: "w2", Turn: "t2", AtMs: f.now.Add(time.Minute).UnixMilli(), Folder: "/work/app", Branch: "main"})
	f.now = f.now.Add(2 * time.Minute)
	result = f.run(t)
	if result.BeforeAutomatic != 1 || result.Stored != 1 || len(f.remote.uploads) != 1 || f.remote.uploads[0].SessionID != "without-commit" || f.remote.uploads[0].Body.Records != 2 {
		t.Fatalf("after the session went on: %+v", result)
	}
	// The moment automatic reading began does not move.
	if f.state.state.EnabledAt != "2031-01-02T04:00:00Z" {
		t.Fatalf("began at %s", f.state.state.EnabledAt)
	}
	// The run a person asks for still sends everything.
	f.remote.uploads = nil
	asked, err := f.service.PushSegments(context.Background(), "/work/app")
	if err != nil || asked.Automatic || asked.BeforeAutomatic != 0 || len(f.remote.uploads) != 2 {
		t.Fatalf("on request: %+v uploads=%d err=%v", asked, len(f.remote.uploads), err)
	}
}

// The pause before the next run is measured from how long this one took.
func TestCollectorRecordsHowLongARunTook(t *testing.T) {
	f := newCollectorFixture(t)
	f.run(t)
	if f.state.state.LastRun == nil || f.state.state.LastRun.Millis != 0 {
		t.Fatalf("a run on a clock that stands still took time: %+v", f.state.state.LastRun)
	}
	f.tick = 3 * time.Second
	started := f.now
	f.run(t)
	run := f.state.state.LastRun
	if run == nil || run.At != started.Format(time.RFC3339Nano) || run.Millis < 3000 || run.Millis%3000 != 0 {
		t.Fatalf("the run that began at %s: %+v", started.Format(time.RFC3339Nano), run)
	}
}

func TestCollectorSendsOnlyWhatChangedSinceItsLastRun(t *testing.T) {
	f := newCollectorFixture(t)
	f.began(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
	first := f.run(t)
	if first.Stored != 2 || first.BeforeAutomatic != 0 || len(f.remote.uploads) != 2 || len(f.source.read) != 2 || len(f.content.docs) != 2 {
		t.Fatalf("first run: %+v", first)
	}
	sent := f.remote.uploads[0].Body
	if sent.Content == nil || sent.Segments[0].Events != 6 {
		t.Fatalf("first report: %+v", sent)
	}

	// Nothing changed: nothing is read back, built or sent.
	second := f.run(t)
	if second.Stored != 0 || second.Unchanged != 2 || len(f.remote.uploads) != 0 || len(f.source.read) != 0 || len(f.content.docs) != 0 {
		t.Fatalf("second run: %+v uploads=%d reads=%d docs=%d", second, len(f.remote.uploads), len(f.source.read), len(f.content.docs))
	}
	if second.Sessions[0].Content != domain.SegmentContentStored || second.Sessions[0].State != domain.SegmentPushUnchanged || second.ContentNotUploaded != 0 {
		t.Fatalf("second run sessions: %+v", second.Sessions)
	}

	// One session grew: only that one is read back and sent.
	f.claude.batch.Sessions[1].Items = append(f.claude.batch.Sessions[1].Items, domain.RecordedItem{ID: "w2", Turn: "t2", AtMs: collectorDay(20), Folder: "/work/app", Branch: "main"})
	third := f.run(t)
	if third.Stored != 1 || third.Unchanged != 1 || len(f.remote.uploads) != 1 || f.remote.uploads[0].SessionID != "without-commit" || len(f.source.read) != 1 || len(f.content.docs) != 1 {
		t.Fatalf("after one session grew: %+v uploads=%d reads=%d", third, len(f.remote.uploads), len(f.source.read))
	}
	if grown := f.remote.uploads[0].Body; grown.Content.Events != 4 || grown.Uncommitted.Events != 4 {
		t.Fatalf("the grown report: %+v %+v", grown.Content, grown.Uncommitted)
	}

	// Another masking policy or document identity: the conversation is built
	// again; the report is sent only if that changed it.
	f.epoch = "epoch-2"
	fourth := f.run(t)
	if len(f.source.read) != 2 || len(f.content.docs) != 2 || len(f.remote.uploads) != 0 || fourth.Unchanged != 2 {
		t.Fatalf("another epoch: %+v uploads=%d reads=%d", fourth, len(f.remote.uploads), len(f.source.read))
	}

	// Another repository or server: what was sent there was not sent here.
	f.repo.RemoteURL = "https://example.test/team/moved"
	fifth := f.run(t)
	if fifth.Stored != 2 || len(f.remote.uploads) != 2 {
		t.Fatalf("another target: %+v", fifth)
	}
	// A lost state only makes the next run send more.
	began := f.state.state.EnabledAt
	f.state.state = domain.CollectorState{Version: domain.CollectorStateVersion, EnabledAt: began}
	if again := f.run(t); again.Stored != 2 || len(f.remote.uploads) != 2 {
		t.Fatalf("after losing the memory: %+v", again)
	}
}

func TestCollectorRemembersOnlyWhatTheServerTook(t *testing.T) {
	f := newCollectorFixture(t)
	f.began(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
	// One report is refused: it is tried again on the next run, the other is not.
	f.remote.answer = func(upload domain.AnchoredSessionUpload) (domain.AnchoredSessionReceipt, error) {
		if upload.SessionID == "with-commit" {
			return domain.AnchoredSessionReceipt{}, segmentUploadStatus(409)
		}
		return domain.AnchoredSessionReceipt{Revision: 1, Changed: true}, nil
	}
	if result := f.run(t); result.Refused != 1 || result.Stored != 1 {
		t.Fatalf("a refused report: %+v", result)
	}
	result := f.run(t)
	if len(f.remote.uploads) != 1 || f.remote.uploads[0].SessionID != "with-commit" || result.Refused != 1 || result.Unchanged != 1 {
		t.Fatalf("the run after a refusal: %+v uploads=%d", result, len(f.remote.uploads))
	}
	// Its conversation went up the first time and is not built again.
	if len(f.source.read) != 0 {
		t.Fatalf("the conversation was read back again: %d", len(f.source.read))
	}

	// A conversation that could not go up is tried again; one that is not there is not.
	g := newCollectorFixture(t)
	g.began(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
	g.source.missing = map[string][]string{"with-commit": {"m2"}}
	if result := g.run(t); result.ContentNotUploaded != 1 || result.Stored != 2 {
		t.Fatalf("a conversation that could not be read back: %+v", result)
	}
	g.source.missing = nil
	result = g.run(t)
	if len(g.source.read) != 1 || g.source.read[0].ID != "with-commit" || result.Stored != 1 || result.ContentNotUploaded != 0 || g.remote.uploads[0].Body.Content == nil {
		t.Fatalf("the run after: %+v reads=%d", result, len(g.source.read))
	}

	// A caller the server does not accept: the run stops, and the state says why.
	h := newCollectorFixture(t)
	h.began(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
	refused := segmentUploadStatus(401)
	calls := 0
	h.remote.answer = func(domain.AnchoredSessionUpload) (domain.AnchoredSessionReceipt, error) {
		calls++
		if calls == 2 {
			return domain.AnchoredSessionReceipt{}, refused
		}
		return domain.AnchoredSessionReceipt{Revision: 1, Changed: true}, nil
	}
	if _, err := h.service.CollectSegments(context.Background(), "/work/app"); !errors.Is(err, refused) {
		t.Fatalf("a refused caller: %v", err)
	}
	state := h.state.state
	if state.LastRun == nil || !strings.Contains(state.LastRun.Error, "401") || state.LastRun.Stored != 1 || len(state.Sessions) != 2 {
		t.Fatalf("state after a stopped run: %+v", state)
	}
	// What was sent before the failure is remembered; what failed is not.
	sentKeys := 0
	for _, session := range state.Sessions {
		if session.Sent != "" {
			sentKeys++
		}
	}
	if sentKeys != 1 {
		t.Fatalf("remembered reports: %+v", state.Sessions)
	}
}

func TestCollectorRegistersBranchesOnlyWhenTheyChanged(t *testing.T) {
	f := newCollectorFixture(t)
	f.began(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
	a := strings.Repeat("a", 32)
	store := &branchIDStoreFake{branches: []domain.LocalBranch{{Name: "main", ID: a, IssuedFor: "main", Remote: "main"}}}
	ledger := &branchLedgerRemoteFake{}
	f.service.WithBranchIDs(store, ledger, func() (string, error) { return strings.Repeat("b", 32), nil })
	want := &domain.SegmentPushBranches{WithID: 1, OnOrigin: 1, Registered: 1}
	for run := 1; run <= 2; run++ {
		if result := f.run(t); result.Branches == nil || *result.Branches != *want {
			t.Fatalf("run %d: %+v", run, result.Branches)
		}
	}
	if len(ledger.reports) != 1 {
		t.Fatalf("registered %d times for the same branches", len(ledger.reports))
	}
	// A branch that appeared on origin: one more report, with both.
	store.branches = append(store.branches, domain.LocalBranch{Name: "topic", Remote: "topic"})
	if result := f.run(t); result.Branches.Assigned != 1 || result.Branches.Registered != 2 || len(ledger.reports) != 2 || len(ledger.reports[1]) != 2 {
		t.Fatalf("after a new branch: %+v reports=%+v", result.Branches, ledger.reports)
	}
	// A refused report is not remembered as sent.
	ledger.err = segmentUploadStatus(404)
	store.branches = append(store.branches, domain.LocalBranch{Name: "later", Remote: "later"})
	f.run(t)
	ledger.err = nil
	if result := f.run(t); len(ledger.reports) != 4 || result.Branches.Registered != 3 {
		t.Fatalf("after a refused report: %+v reports=%d", result.Branches, len(ledger.reports))
	}
}

type worktreeListRemoteFake struct {
	repo, machine string
	reports       [][]domain.WorktreeRegistration
	err           error
}

func (f *worktreeListRemoteFake) ReportWorktrees(_ context.Context, repo, machine string, worktrees []domain.WorktreeRegistration) error {
	f.repo, f.machine = repo, machine
	f.reports = append(f.reports, worktrees)
	return f.err
}

func TestCollectorReportsTheWorktreeListOnlyWhenItChanged(t *testing.T) {
	f := newCollectorFixture(t)
	f.began(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
	list := &worktreeListRemoteFake{}
	f.service.WithWorktreeList(list)
	for run := 1; run <= 2; run++ {
		if result := f.run(t); result.Worktrees == nil || *result.Worktrees != (domain.SegmentPushWorktrees{Listed: 1, Reported: true}) {
			t.Fatalf("run %d: %+v", run, result.Worktrees)
		}
	}
	if len(list.reports) != 1 || list.repo != string(f.repo.ID) || list.machine != "laptop" ||
		len(list.reports[0]) != 1 || list.reports[0][0] != (domain.WorktreeRegistration{Name: "app", Branch: "main"}) {
		t.Fatalf("reported %d times for the same list: %+v", len(list.reports), list)
	}
	// A new worktree, and another branch checked out in the first one: one more report, with both.
	f.git.records.Worktrees[0].Current = "feature"
	f.git.records.Worktrees = append(f.git.records.Worktrees, domain.RecordedWorktree{Root: "/work/app-fix"})
	if result := f.run(t); result.Worktrees.Listed != 2 || len(list.reports) != 2 ||
		len(list.reports[1]) != 2 || list.reports[1][0] != (domain.WorktreeRegistration{Name: "app", Branch: "feature"}) || list.reports[1][1] != (domain.WorktreeRegistration{Name: "app-fix"}) {
		t.Fatalf("after the list changed: %+v reports=%+v", result.Worktrees, list.reports)
	}
	// A refused list is not remembered as sent, and does not stop the sessions.
	list.err = segmentUploadStatus(404)
	f.git.records.Worktrees = f.git.records.Worktrees[:1]
	refused := f.run(t)
	if refused.Worktrees.Reported || refused.Worktrees.NotReported == "" || refused.Worktrees.Listed != 1 || len(refused.Sessions) == 0 {
		t.Fatalf("a refused list: %+v sessions=%d", refused.Worktrees, len(refused.Sessions))
	}
	list.err = nil
	if result := f.run(t); len(list.reports) != 4 || !result.Worktrees.Reported {
		t.Fatalf("after a refused list: %+v reports=%d", result.Worktrees, len(list.reports))
	}
	// Sending to another server repository starts over.
	f.repo.ID = domain.HashContent([]byte("another repository"))
	if f.run(t); len(list.reports) != 5 {
		t.Fatalf("another target: reports=%d", len(list.reports))
	}
}

func TestCollectorNeedsItsMemory(t *testing.T) {
	f := newCollectorFixture(t)
	failure := errors.New("synthetic failure")
	f.state.loadErr = failure
	if _, err := f.service.CollectSegments(context.Background(), "/work/app"); !errors.Is(err, failure) || len(f.remote.uploads) != 0 {
		t.Fatalf("a state that cannot be read: %v", err)
	}
	f.state.loadErr, f.state.saveErr = nil, failure
	f.began(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
	if _, err := f.service.CollectSegments(context.Background(), "/work/app"); !errors.Is(err, failure) {
		t.Fatalf("a state that cannot be saved: %v", err)
	}
	plain := segmentUploadFixture(&segmentRemoteFake{})
	if _, err := plain.CollectSegments(context.Background(), "/work/app"); err == nil {
		t.Fatal("a service without a state store must not collect")
	}
	// A state from a time that cannot be read begins again now.
	g := newCollectorFixture(t)
	g.state.state = domain.CollectorState{Version: domain.CollectorStateVersion, EnabledAt: "yesterday"}
	if result := g.run(t); result.BeforeAutomatic != 2 || g.state.state.EnabledAt != "2031-01-02T04:00:00Z" {
		t.Fatalf("an unreadable beginning: %+v %s", result, g.state.state.EnabledAt)
	}
	if !reflect.DeepEqual(g.state.state.Sessions, map[string]domain.CollectorSessionState{}) {
		t.Fatalf("sessions: %+v", g.state.state.Sessions)
	}
}
