package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type stagingGit struct {
	repo        domain.Repo
	branch, sha string
}

func (g *stagingGit) CurrentRepo(context.Context, string) (domain.Repo, error) { return g.repo, nil }
func (g *stagingGit) CurrentBranch(context.Context, string) (string, error)    { return g.branch, nil }
func (g *stagingGit) CurrentCommit(context.Context, string) (string, error)    { return g.sha, nil }

type stagingFixture struct {
	svc   *StagingService
	store *storage.FileStore
	git   *stagingGit
	root  string
}

func newStagingFixture(t *testing.T) stagingFixture {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	g := &stagingGit{repo: domain.Repo{ID: string(domain.HashContent([]byte(root))), LocalPath: root, DefaultBranch: "main"}, branch: "main", sha: strings.Repeat("a", 40)}
	st := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), g.branch, g.sha)
	return stagingFixture{stagingService(g, st, st), st, g, root}
}
func stagingService(g *stagingGit, st *storage.FileStore, index outbound.StagingStore) *StagingService {
	return NewStagingService(g, g, st, index, map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: capture.NewClaudeCapture(), domain.ProviderCodex: capture.NewCodexCapture()}, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec(), domain.ProviderCodex: codec.NewCodexCodec()}, capture.NewSessionCapture(st), storage.NewSyncOutbox())
}
func (f stagingFixture) source(t *testing.T, session string, texts ...string) inbound.StageSession {
	t.Helper()
	path := filepath.Join(f.root, session+".jsonl")
	var raw strings.Builder
	for n, text := range texts {
		line, _ := json.Marshal(map[string]any{"type": "user", "sessionId": session, "cwd": f.root, "gitBranch": "main", "timestamp": time.Unix(int64(n), 0).UTC().Format(time.RFC3339), "message": map[string]string{"role": "user", "content": text}})
		raw.Write(line)
		raw.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(raw.String()), 0600); err != nil {
		t.Fatal(err)
	}
	return inbound.StageSession{Provider: domain.ProviderClaude, Path: path, SessionID: session}
}
func (f stagingFixture) stage(t *testing.T, source inbound.StageSession) domain.StagingIndex {
	t.Helper()
	i, err := f.svc.Stage(context.Background(), inbound.StageInput{Cwd: f.root, Sessions: []inbound.StageSession{source}})
	if err != nil {
		t.Fatal(err)
	}
	return i
}
func stagedBySession(t *testing.T, i domain.StagingIndex, id string) domain.StagedSession {
	t.Helper()
	for _, e := range i.Entries {
		if e.SessionID == id {
			return e
		}
	}
	t.Fatalf("no staged session %s", id)
	return domain.StagedSession{}
}

func TestStagingFreezesAddIsAdditiveAndCommitNeverReadsProvider(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	a := f.source(t, "alpha", "first")
	initial := f.stage(t, a)
	first := stagedBySession(t, initial, "alpha")
	f.source(t, "alpha", "first", "later unstaged conversation")
	both := f.stage(t, f.source(t, "beta", "second source"))
	if len(both.Entries) != 2 || stagedBySession(t, both, "alpha").DocHash != first.DocHash {
		t.Fatal("add replaced another source", both)
	}
	if err := os.Remove(a.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.root, "beta.jsonl")); err != nil {
		t.Fatal(err)
	}
	op, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root, Message: "frozen", ExpectedRevision: both.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if !op.LocalFinalized || len(op.Index.Entries) != 2 {
		t.Fatalf("missing local receipt: %+v", op)
	}
	doc, err := f.store.GetDoc(ctx, first.DocHash)
	if err != nil || len(doc.CIR.Events) != first.Events {
		t.Fatal("frozen source changed", err)
	}
	i, err := f.svc.Inspect(ctx, f.root)
	if err != nil || len(i.Entries) != 0 {
		t.Fatal("committed index not consumed", i, err)
	}
	reachable, err := f.svc.stagedReachable(ctx, f.git.repo.ID, op.Position.Snapshot, first.DocHash)
	if err != nil || !reachable {
		t.Fatal("first contribution not preserved", err)
	}
	before, err := f.store.ListHistoryEvents(ctx, f.git.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.svc.ResumeCommit(ctx, f.root, op.ID)
	if err != nil || !replay.LocalFinalized {
		t.Fatal(replay, err)
	}
	after, err := f.store.ListHistoryEvents(ctx, f.git.repo.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("replay changed publications", err)
	}
	if _, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root}); !errors.Is(err, domain.ErrEmptyIndex) {
		t.Fatal("empty commit captured live", err)
	}
}

func TestStagingReaddGenerationsPartialRecordAndUnstage(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	a := f.source(t, "alpha", "first")
	initial := f.stage(t, a)
	old := stagedBySession(t, initial, "alpha")
	b := f.source(t, "beta", "independent")
	f.stage(t, b)
	f.source(t, "alpha", "first", "second")
	file, err := os.OpenFile(a.Path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`{"type":"user","message":`)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	grown := f.stage(t, a)
	next := stagedBySession(t, grown, "alpha")
	if len(grown.Entries) != 2 || next.Generation != old.Generation || next.DocHash == old.DocHash || next.Events <= old.Events {
		t.Fatal("bad incremental generation", grown)
	}
	f.source(t, "alpha", "rewritten source")
	replaced := f.stage(t, a)
	if len(replaced.Entries) != 3 {
		t.Fatal("replacement discarded old source generation", replaced)
	}
	index, err := f.svc.Unstage(ctx, inbound.UnstageInput{Cwd: f.root, Keys: []domain.ContentHash{next.Key}, ExpectedRevision: replaced.Revision})
	if err != nil || len(index.Entries) != 2 {
		t.Fatal(index, err)
	}
	if _, err := f.store.GetDoc(ctx, next.DocHash); err != nil {
		t.Fatal("unstage deleted immutable source", err)
	}
	if _, err := f.store.GetDoc(ctx, old.DocHash); err != nil {
		t.Fatal("re-add deleted prior immutable source", err)
	}
	if _, err := f.svc.Unstage(ctx, inbound.UnstageInput{Cwd: f.root, All: true, ExpectedRevision: replaced.Revision}); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatal("stale unstage accepted", err)
	}
}

func TestStagingCodeChangeFailsBeforeConsumption(t *testing.T) {
	f := newStagingFixture(t)
	index := f.stage(t, f.source(t, "alpha", "first"))
	f.git.sha = strings.Repeat("b", 40)
	if _, err := f.svc.Commit(context.Background(), inbound.StagingCommitInput{Cwd: f.root}); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatal(err)
	}
	after, err := f.svc.Inspect(context.Background(), f.root)
	if err != nil || after.Revision != index.Revision {
		t.Fatal(after, err)
	}
}

func TestStagingLegacyProviderSelectorNeverCapturesImplicitly(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	f.source(t, "alpha", "live content must not become a frozen legacy selection")
	path := filepath.Join(f.root, ".cxt", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte("{\"staged\":[\"claude\",\"codex\"],\"default_provider\":\"claude\"}\n")
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	index, err := f.svc.Inspect(ctx, f.root)
	if err != nil || len(index.Entries) != 0 || index.Sequence != 0 {
		t.Fatal("legacy selector became frozen capture", index, err)
	}
	if _, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root}); !errors.Is(err, domain.ErrEmptyIndex) {
		t.Fatal("commit fell back to live legacy selector", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != string(legacy) {
		t.Fatal("legacy selector was consumed or changed", string(raw), err)
	}
}

func TestStagingHookCaptureDoesNotConsumeManualIndexAndGCRespectsPin(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	a := f.source(t, "alpha", "first")
	save := f.svc.save
	pending, err := save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: a.Provider, SessionPath: a.Path, Pending: true, Message: domain.HookMessagePrefix + " old"})
	if err != nil {
		t.Fatal(err)
	}
	index := f.stage(t, a)
	if index.Entries[0].DocHash != pending.SnapshotID {
		t.Fatal("fixture must stage pending identity")
	}
	f.source(t, "alpha", "first", "newer capture")
	newer, err := save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: a.Provider, SessionPath: a.Path, Pending: true, Message: domain.HookMessagePrefix + " new"})
	if err != nil {
		t.Fatal(err)
	}
	if newer.SnapshotID == pending.SnapshotID {
		t.Fatal("fixture did not grow")
	}
	if _, err := f.store.GetDoc(ctx, pending.SnapshotID); err != nil {
		t.Fatal("GC collected staged capture", err)
	}
	if _, err := save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: a.Provider, SessionPath: a.Path, Message: "automatic Git commit"}); err != nil {
		t.Fatal(err)
	}
	after, err := f.svc.Inspect(ctx, f.root)
	if err != nil || after.Revision != index.Revision {
		t.Fatal("hook consumed manual index", after, err)
	}
	if _, _, err := f.store.RepackObjects(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.GetDoc(ctx, pending.SnapshotID); err != nil {
		t.Fatal("repack lost staged source", err)
	}
}

type readdBeforeFinalize struct {
	outbound.StagingStore
	mutate func()
}

func (r *readdBeforeFinalize) FinalizeStagingCommit(ctx context.Context, op domain.StagingCommit) (domain.StagingCommit, error) {
	r.mutate()
	return r.StagingStore.FinalizeStagingCommit(ctx, op)
}
func TestStagingConcurrentReaddConflictsWithoutConsumingGrowth(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	a := f.source(t, "alpha", "first")
	old := f.stage(t, a)
	var winner domain.StagingIndex
	proxy := &readdBeforeFinalize{StagingStore: f.store, mutate: func() { f.source(t, "alpha", "first", "concurrent growth"); winner = f.stage(t, a) }}
	commit := stagingService(f.git, f.store, proxy)
	if _, err := commit.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root, ExpectedRevision: old.Revision}); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatal("concurrent add was overwritten", err)
	}
	actual, err := f.svc.Inspect(ctx, f.root)
	if err != nil || actual.Revision != winner.Revision || len(actual.Entries) != 1 {
		t.Fatal(actual, err)
	}
}

func TestStagingCommitPreservesGrowingPendingPointer(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	a := f.source(t, "alpha", "first")
	index := f.stage(t, a)
	f.source(t, "alpha", "first", "new pending")
	pending, err := f.svc.save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: a.Provider, SessionPath: a.Path, Pending: true, Message: domain.HookMessagePrefix + " new"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root, ExpectedRevision: index.Revision}); err != nil {
		t.Fatal(err)
	}
	ps, err := f.store.ListPendings(ctx, f.git.repo.ID)
	if err != nil || len(ps) != 1 || ps[0].Target != pending.SnapshotID {
		t.Fatal("commit lost future pending", ps, err)
	}
}

func TestStagingCodexFreezesCompleteRecordsAndKeepsCompaction(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	path := filepath.Join(f.root, "rollout.jsonl")
	lines := []string{
		`{"timestamp":"2026-09-30T00:00:00Z","type":"session_meta","payload":{"id":"codex-stage","cwd":"` + f.root + `","model":"gpt-5-codex"}}`,
		`{"timestamp":"2026-09-30T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"original"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	input := inbound.StageSession{Provider: domain.ProviderCodex, Path: path, SessionID: "codex-stage"}
	first := f.stage(t, input)
	before := first.Entries[0]
	lines = append(lines, `{"timestamp":"2026-09-30T00:00:02Z","type":"compacted","payload":{"message":"condensed context","replacement_history":[]}}`)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"+`{"type":"response_item",`), 0600); err != nil {
		t.Fatal(err)
	}
	after := f.stage(t, input)
	if len(after.Entries) != 1 || after.Entries[0].Generation != before.Generation || after.Entries[0].Events <= before.Events {
		t.Fatal(after)
	}
	op, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := f.store.GetDoc(ctx, op.Index.Entries[0].DocHash)
	if err != nil || doc.CIR.Envelope.CompactionCount != 1 {
		t.Fatal(doc.CIR.Envelope, err)
	}
}

func TestStagingSameGitCommitAllowsTwoWorktreeContributions(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	peerStore := storage.NewWorktreeFileStore(f.root, filepath.Join(f.root, ".git", "worktrees", "peer"), "main", f.git.sha)
	peer := stagingService(f.git, peerStore, peerStore)
	a := f.source(t, "alpha", "first worker")
	b := f.source(t, "beta", "second worker")
	first := f.stage(t, a)
	second, err := peer.Stage(ctx, inbound.StageInput{Cwd: f.root, Sessions: []inbound.StageSession{b}})
	if err != nil {
		t.Fatal(err)
	}
	one, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root, ExpectedRevision: first.Revision})
	if err != nil {
		t.Fatal(err)
	}
	two, err := peer.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root, ExpectedRevision: second.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if one.ID == two.ID || one.Index.WorktreeID == two.Index.WorktreeID || one.Position.GitCommit != two.Position.GitCommit {
		t.Fatal("contribution identity collapsed")
	}
	for _, op := range []domain.StagingCommit{one, two} {
		included, err := f.svc.stagedReachable(ctx, f.git.repo.ID, two.Position.Snapshot, op.Index.Entries[0].DocHash)
		if err != nil || !included {
			t.Fatal("contributor lost", op.ID, err)
		}
	}
	retained, err := f.store.GetWorkingPosition(ctx)
	if err != nil || retained.Snapshot != one.Position.Snapshot {
		t.Fatal("second worker moved first cursor", retained, err)
	}
}

func TestStagingRestageAfterCommitTracksCoveredPrefix(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	a := f.source(t, "alpha", "first")
	first := f.stage(t, a)
	if _, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root}); err != nil {
		t.Fatal(err)
	}
	f.source(t, "alpha", "first", "new work")
	next := f.stage(t, a)
	if len(next.Entries) != 1 || next.Entries[0].Generation != first.Entries[0].Generation || next.Entries[0].StartEvent != first.Entries[0].Events || next.Entries[0].Events <= next.Entries[0].StartEvent {
		t.Fatal("coverage lost", first, next)
	}
}

func TestStagingRewindDoesNotTreatFutureCommitAsCovered(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	a := f.source(t, "alpha", "first")
	initial := f.stage(t, a)
	first, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root})
	if err != nil {
		t.Fatal(err)
	}
	f.source(t, "alpha", "first", "future committed event")
	f.stage(t, a)
	future, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root})
	if err != nil {
		t.Fatal(err)
	}
	p := first.Position
	p.Rewound = true
	p.SharedTarget = future.Position.Snapshot
	p.Selection = nil
	if err := f.store.PutWorkingPosition(ctx, p); err != nil {
		t.Fatal(err)
	}
	f.source(t, "alpha", "first", "future committed event", "new event after selecting old point")
	staged := f.stage(t, a)
	if len(staged.Entries) != 1 || staged.Entries[0].StartEvent != initial.Entries[0].Events {
		t.Fatalf("future coverage consumed selected history: %+v", staged.Entries)
	}
}

func TestStagingExistingHookDocumentsPreserveParentsAndQueuePromotion(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	a := f.source(t, "alpha", "first pending")
	b := f.source(t, "beta", "second pending")
	before := map[domain.ContentHash][]domain.ContentHash{}
	for _, source := range []inbound.StageSession{a, b} {
		out, err := f.svc.save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: source.Provider, SessionPath: source.Path, Pending: true, Message: domain.HookMessagePrefix + " pending"})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := f.store.GetSnapshot(ctx, out.SnapshotID)
		if err != nil {
			t.Fatal(err)
		}
		before[out.SnapshotID] = snapshot.Parents
		f.stage(t, source)
	}
	op, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root, Message: "publish exact staged sessions"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := storage.NewSyncOutbox().ListPromotions(ctx, f.root)
	if err != nil {
		t.Fatal(err)
	}
	for hash, parents := range before {
		snap, err := f.store.GetSnapshot(ctx, hash)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(snap.Parents, parents) || snap.Message != "publish exact staged sessions" || queued[hash] != snap.Message {
			t.Fatal("existing hook publication changed ancestry or lacks outbox", snap, queued)
		}
		included, err := f.svc.stagedReachable(ctx, f.git.repo.ID, op.Position.Snapshot, hash)
		if err != nil || !included {
			t.Fatal("hook source omitted", hash, err)
		}
	}
}

func TestFrozenCommitPersistsExactObservationBeforePublication(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	f.stage(t, f.source(t, "one", "frozen one"))
	f.stage(t, f.source(t, "two", "frozen two"))
	op, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root})
	if err != nil {
		t.Fatal(err)
	}
	history, err := f.store.ListHistoryEvents(ctx, f.git.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	observations := domain.StagingObservations(op)
	for _, p := range append(append([]domain.HistoryEvent{}, op.Publications...), *op.Position.Selection) {
		found := false
		for _, o := range history {
			if o.Kind == "position" && o.ID != p.ID && o.RepoID == p.RepoID && o.BranchID == p.BranchID && o.Branch == p.Branch && o.LocalBranch == p.LocalBranch && o.WorktreeID == p.WorktreeID && o.GitAfter == p.GitAfter && o.Target == p.Target {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("publication %s has no exact accepted-source candidate", p.ID)
		}
	}
	again, err := f.svc.ResumeCommit(ctx, f.root, op.ID)
	if err != nil || !reflect.DeepEqual(observations, domain.StagingObservations(again)) {
		t.Fatalf("retry changed observation IDs: %v", err)
	}
}
