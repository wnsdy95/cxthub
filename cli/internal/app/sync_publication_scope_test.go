package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// All data is synthetic; unexpected remote methods panic through the nil port.
// Hooks run synchronously at real transport boundaries against real FileStore
// and outbox locks, so queue interleavings are deterministic without sleeps.
type strictPublicationRemote struct {
	outbound.RemoteSync
	protocol                                  int
	accepted, sent                            []domain.HistoryEvent
	refs, writes                              []domain.Ref
	snaps, docs, settings, promoted, memories []domain.ContentHash
	grafts                                    []domain.GraftQueueEvent
	unsync                                    []string
	pr, pending                               int
	calls                                     []string
	onObject, onNegotiate, onGraft            func()
	failGraft                                 error
	failSnapshotOnce                          bool
	lostID                                    string
	memoryObjects                             map[domain.ContentHash]domain.MemoryDigest
	attachments                               map[domain.ContentHash]domain.ContentHash
}

func (r *strictPublicationRemote) RegisterRepo(_ context.Context, repo domain.Repo) (domain.Repo, error) {
	return repo, nil
}
func (r *strictPublicationRemote) ContextProtocol(context.Context, string) (int, error) {
	return r.protocol, nil
}
func (r *strictPublicationRemote) PullHistoryEvents(context.Context, string) ([]domain.HistoryEvent, error) {
	return slices.Clone(r.accepted), nil
}
func (r *strictPublicationRemote) RemoteManifest(_ context.Context, repo string) (domain.Manifest, error) {
	return domain.Manifest{RepoID: repo, Refs: slices.Clone(r.refs), MemoryAttachments: r.attachments}, nil
}
func (r *strictPublicationRemote) NegotiatePushObjects(_ context.Context, _ string, snaps, docs []domain.ContentHash) (outbound.PushObjectWants, error) {
	if r.onNegotiate != nil {
		f := r.onNegotiate
		r.onNegotiate = nil
		f()
	}
	return outbound.PushObjectWants{Snapshots: snaps, Docs: docs}, nil
}
func (r *strictPublicationRemote) Push(_ context.Context, _ string, snaps []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, force, appendMode bool) error {
	if len(snaps)+len(docs) > 0 {
		if r.onObject != nil {
			f := r.onObject
			r.onObject = nil
			f()
		}
	}
	if len(snaps) > 0 && r.failSnapshotOnce {
		r.failSnapshotOnce = false
		return domain.ErrNotFound
	}
	for _, s := range snaps {
		r.snaps = append(r.snaps, s.ID)
		r.calls = append(r.calls, "snapshot")
	}
	for _, d := range docs {
		r.docs = append(r.docs, d.Hash)
		r.calls = append(r.calls, "doc")
	}
	for _, ref := range refs {
		r.writes = append(r.writes, ref)
		r.calls = append(r.calls, "ref")
		r.setRef(ref)
	}
	return nil
}
func (r *strictPublicationRemote) setRef(ref domain.Ref) {
	for i, old := range r.refs {
		if old.Name == ref.Name {
			r.refs[i] = ref
			return
		}
	}
	r.refs = append(r.refs, ref)
}
func (r *strictPublicationRemote) PushHistoryEvent(_ context.Context, e domain.HistoryEvent) error {
	if e.Creation != nil && e.Creation.OriginBranchID != "" {
		found := false
		for _, w := range r.accepted {
			found = found || w.BranchID == e.Creation.OriginBranchID
		}
		if !found {
			return errors.New("origin witness arrived after consumer")
		}
	}
	for _, h := range []domain.ContentHash{e.Source, e.Target, e.SharedTarget, e.MemorySource} {
		if h != "" && !slices.Contains(r.snaps, h) {
			return fmt.Errorf("history dependency absent: %s", h)
		}
	}
	if e.MemoryHash != "" {
		if _, ok := r.memoryObjects[e.MemoryHash]; !ok {
			return errors.New("pinned memory object absent")
		}
	}
	r.calls = append(r.calls, "history")
	r.sent = append(r.sent, e)
	r.accepted = append(r.accepted, e)
	if e.Kind == "birth" || e.Kind == "advance" {
		r.setRef(domain.Ref{RepoID: e.RepoID, Kind: domain.RefBranch, Name: e.Branch, BranchID: e.BranchID, Target: e.Target})
	}
	if e.ID == r.lostID {
		r.lostID = ""
		return errors.New("lost acknowledgement")
	}
	return nil
}
func (r *strictPublicationRemote) GraftSnapshotParents(_ context.Context, _ string, id domain.ContentHash, parents []domain.ContentHash, seq uint64) error {
	r.calls = append(r.calls, "graft")
	e := domain.GraftQueueEvent{Snapshot: string(id), ExpectedSeq: seq}
	for _, p := range parents {
		e.Parents = append(e.Parents, string(p))
	}
	r.grafts = append(r.grafts, e)
	if r.onGraft != nil {
		f := r.onGraft
		r.onGraft = nil
		f()
	}
	return r.failGraft
}
func (r *strictPublicationRemote) PromoteSnapshotMessage(_ context.Context, _ string, id domain.ContentHash, _ string) error {
	r.promoted = append(r.promoted, id)
	return nil
}
func (r *strictPublicationRemote) PushSettingsObject(_ context.Context, _ string, id domain.ContentHash, _ domain.SettingsBundle) error {
	r.settings = append(r.settings, id)
	return nil
}
func (r *strictPublicationRemote) PushMemory(_ context.Context, _ string, m domain.MemoryDigest) error {
	if !slices.Contains(r.snaps, m.SnapshotID) {
		return domain.ErrNotFound
	}
	if r.attachments[m.SnapshotID] != m.PreviousMemoryHash {
		return domain.ErrSyncConflict
	}
	hash, _ := domain.MemoryDigestHash(m)
	r.memoryObjects[hash] = m
	r.attachments[m.SnapshotID] = hash
	r.memories = append(r.memories, m.SnapshotID)
	return nil
}
func (r *strictPublicationRemote) PullMemoryObject(_ context.Context, _ string, h domain.ContentHash) (domain.MemoryDigest, error) {
	m, ok := r.memoryObjects[h]
	if !ok {
		return m, domain.ErrNotFound
	}
	return m, nil
}
func (r *strictPublicationRemote) DeleteUnsyncRemote(_ context.Context, _ string, name string) error {
	r.unsync = append(r.unsync, name)
	return nil
}
func (r *strictPublicationRemote) SubmitPRPromotion(context.Context, string, domain.PullRequestMerge) error {
	r.pr++
	return nil
}
func (r *strictPublicationRemote) CompareAndDeletePendingRemote(context.Context, string, string, domain.ContentHash) (bool, error) {
	r.pending++
	return true, nil
}
func (r *strictPublicationRemote) PushPending(context.Context, string, domain.Pending) error {
	r.pending++
	return nil
}

type strictPublicationFixture struct {
	t          *testing.T
	ctx        context.Context
	st         *storage.FileStore
	svc        *SyncRepoService
	r          *strictPublicationRemote
	root, repo string
	a, b, x    domain.ContentHash
	birth      domain.HistoryEvent
}

func newStrictPublication(t *testing.T) *strictPublicationFixture {
	t.Helper()
	f := &strictPublicationFixture{t: t, ctx: context.Background(), root: t.TempDir(), repo: string(domain.HashContent([]byte("strict-publication-repo")))}
	f.st = storage.NewFileStore(f.root)
	f.a = publicationSnapshot(t, f.st, f.repo, "A", nil, nil)
	f.b = publicationSnapshot(t, f.st, f.repo, "B", []domain.ContentHash{f.a}, nil)
	f.x = publicationSnapshot(t, f.st, f.repo, "X", nil, nil)
	f.birth = f.event(1, "birth", "R", "feature", f.b)
	f.put(f.birth)
	f.put(f.event(2, "birth", "X", "main", f.x))
	for _, ref := range []domain.Ref{{RepoID: f.repo, Kind: domain.RefBranch, Name: "feature", BranchID: "R", Target: f.b}, {RepoID: f.repo, Kind: domain.RefBranch, Name: "main", BranchID: "X", Target: f.x}} {
		if err := f.st.PutRef(f.ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	f.r = &strictPublicationRemote{protocol: 1, memoryObjects: map[domain.ContentHash]domain.MemoryDigest{}, attachments: map[domain.ContentHash]domain.ContentHash{}}
	f.svc = newTestSyncService(f.st, f.r, pushOrderGit{repo: domain.Repo{ID: f.repo, LocalPath: f.root}})
	return f
}
func (f *strictPublicationFixture) event(n int, kind, id, name string, target domain.ContentHash) domain.HistoryEvent {
	e := domain.HistoryEvent{ID: fmt.Sprintf("%032x", n), RepoID: f.repo, Kind: kind, BranchID: id, Branch: name, Target: target, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(int64(n), 0).UTC()}
	if kind == "publish" {
		e.Source = target
	}
	return e
}
func (f *strictPublicationFixture) put(e domain.HistoryEvent) {
	f.t.Helper()
	if err := f.st.PutHistoryEvent(f.ctx, e); err != nil {
		f.t.Fatal(err)
	}
}
func (f *strictPublicationFixture) input() inbound.SyncInput {
	return inbound.SyncInput{Cwd: f.root, Ref: "feature"}
}
func (f *strictPublicationFixture) graft(id, parent domain.ContentHash) {
	f.t.Helper()
	snap, err := f.st.GetSnapshot(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = queueGraft(f.root, id, parent, snap.GraftSeq); err != nil {
		f.t.Fatal(err)
	}
	snap.GraftSeq++
	snap.Grafted = true
	snap.GraftParents = append(snap.GraftParents, parent)
	if err = f.st.PutSnapshot(f.ctx, snap); err != nil {
		f.t.Fatal(err)
	}
}
func (f *strictPublicationFixture) queue(events []domain.GraftQueueEvent) {
	f.t.Helper()
	if err := writeGraftQueue(f.root, "", graftQueueState{Version: 1, Events: events}); err != nil {
		f.t.Fatal(err)
	}
}
func (f *strictPublicationFixture) noPublication() {
	f.t.Helper()
	if len(f.r.sent) > 0 || len(f.r.writes) > 0 {
		f.t.Fatalf("published after deferral: history=%v refs=%v", f.r.sent, f.r.writes)
	}
}

func TestStrictPublicationRefAndIdentitySameEffects(t *testing.T) {
	var expected *strictPublicationRemote
	for _, explicit := range []bool{true, false} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			f := newStrictPublication(t)
			foreign := f.event(3, "advance", "X", "main", f.x)
			foreign.Source = f.x
			f.put(foreign)
			f.put(f.event(4, "publish", "X", "main", f.x))
			if err := f.svc.outbox.EnqueuePromotion(f.ctx, f.root, f.b, "selected"); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.outbox.EnqueuePromotion(f.ctx, f.root, f.x, "foreign"); err != nil {
				t.Fatal(err)
			}
			in := f.input()
			if !explicit {
				in.Ref = ""
				in.Publication = &domain.PublicationScope{Branches: []domain.PublicationBranch{{Branch: "feature", BranchID: "R"}}}
			}
			out, err := f.svc.Push(f.ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			if len(out.NewRefs) != 1 || out.NewRefs[0].BranchID != "R" {
				t.Fatal(out)
			}
			if slices.Contains(f.r.snaps, f.x) || slices.Contains(f.r.docs, f.x) || slices.Contains(f.r.promoted, f.x) {
				t.Fatal("unselected object effects")
			}
			if !reflect.DeepEqual(f.r.sent, []domain.HistoryEvent{f.birth}) || len(f.r.writes) != 1 {
				t.Fatalf("foreign history/prerequisite escaped: %+v %+v", f.r.sent, f.r.writes)
			}
			if len(f.r.unsync) != 0 {
				t.Fatal(f.r.unsync)
			}
			q, _ := f.svc.outbox.ListPromotions(f.ctx, f.root)
			if q[f.x] != "foreign" || len(q) != 1 {
				t.Fatal(q)
			}
			local, _ := storage.NewFileStore(f.root).ListHistoryEvents(f.ctx, f.repo)
			if len(local) != 4 {
				t.Fatal("unsent durable history removed")
			}
			if expected != nil && (!reflect.DeepEqual(expected.sent, f.r.sent) || !reflect.DeepEqual(expected.writes, f.r.writes) || !reflect.DeepEqual(expected.snaps, f.r.snaps)) {
				t.Fatal("selector policies differ")
			}
			expected = f.r
		})
	}
}

func TestStrictPublicationForeignWitnessAndGuard(t *testing.T) {
	f := newStrictPublication(t)
	// Birth X is accepted read evidence; foreign position is explicitly needed by attach R.
	attach := f.event(10, "attach", "R", "feature", f.b)
	attach.Creation = &domain.GitCreation{Evidence: "process-argv", Command: []string{"git", "branch", "feature", "source"}, StartRef: "source", StartCommit: attach.GitAfter, OriginBranch: "source", OriginBranchID: "Y"}
	witness := f.event(11, "position", "Y", "source", f.x)
	witness.CreatedAt = time.Unix(99999, 0).UTC()
	f.put(attach)
	f.put(witness)
	_, err := f.svc.Push(f.ctx, f.input())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range f.r.sent {
		ids = append(ids, e.ID)
		if e.BranchID == "X" {
			t.Fatal("foreign lifecycle published")
		}
	}
	if slices.Index(ids, witness.ID) > slices.Index(ids, attach.ID) {
		t.Fatal("clock order overrode causality")
	}
	plan := domain.PublicationPlan{Authority: []domain.PublicationBranch{{Branch: "feature", BranchID: "R"}}, HistoryToSend: []domain.HistoryEvent{witness}}
	if !publicationAllowsEvent(plan, witness) {
		t.Fatal("exact ordinary witness rejected")
	}
	changed := witness
	changed.Target = f.a
	if publicationAllowsEvent(plan, changed) {
		t.Fatal("same-ID altered payload authorized")
	}
	changed = witness
	changed.Kind = "publish"
	changed.Source = changed.Target
	plan.HistoryToSend = []domain.HistoryEvent{changed}
	if publicationAllowsEvent(plan, changed) {
		t.Fatal("foreign completion authorized")
	}
}

func TestStrictPublicationHistoryOnlyPreservesQueues(t *testing.T) {
	f := newStrictPublication(t)
	pr := domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	if err := f.st.QueuePRDelivery(f.ctx, f.repo, pr); err != nil {
		t.Fatal(err)
	}
	if err := f.st.PutPending(f.ctx, domain.Pending{RepoID: f.repo, SessionID: "pending", Target: f.b}); err != nil {
		t.Fatal(err)
	}
	in := f.input()
	in.Ref = ""
	in.Publication = &domain.PublicationScope{Branches: []domain.PublicationBranch{{Branch: "feature", BranchID: "R"}}, HistoryOnly: true}
	f.r.lostID = f.birth.ID
	if _, err := f.svc.Push(f.ctx, in); err == nil {
		t.Fatal("expected lost acknowledgement")
	}
	f.r.sent = nil
	if _, err := f.svc.Push(f.ctx, in); err != nil {
		t.Fatal(err)
	}
	if len(f.r.sent) > 0 || len(f.r.writes) > 0 || len(f.r.unsync) > 0 || f.r.pr != 0 || f.r.pending != 0 {
		t.Fatal("history-only performed other effects")
	}
	reopen := storage.NewFileStore(f.root)
	jobs, _ := reopen.PendingPRDeliveries(f.ctx, f.repo)
	pending, _ := reopen.ListPendings(f.ctx, f.repo)
	if len(jobs) != 1 || len(pending) != 1 {
		t.Fatal("queue obligations removed")
	}
}

func TestStrictPublicationGraftFrozenPrefix(t *testing.T) {
	for _, scenario := range []string{"excluded-head", "append-excluded-before-send", "append-excluded-during-send", "consumed-before-send", "consumed-after-send", "changed-head-after-send", "conflict", "graph-changed", "new-selected-tail"} {
		t.Run(scenario, func(t *testing.T) {
			f := newStrictPublication(t)
			f.graft(f.b, f.a)
			initial, _ := readGraftQueue(f.root, "")
			prefix := initial.Events[0]
			excluded := domain.GraftQueueEvent{Snapshot: string(f.x), Parents: []string{string(f.a)}, ExpectedSeq: 0}
			appendExcluded := func() { state, _ := readGraftQueue(f.root, ""); f.queue(append(state.Events, excluded)) }
			switch scenario {
			case "excluded-head":
				f.queue([]domain.GraftQueueEvent{excluded, prefix})
			case "append-excluded-before-send":
				f.r.onObject = appendExcluded
			case "append-excluded-during-send":
				f.r.onGraft = appendExcluded
			case "consumed-before-send":
				f.r.onObject = func() { f.queue(nil) }
			case "consumed-after-send":
				f.r.onGraft = func() { f.queue(nil) }
			case "changed-head-after-send":
				f.r.onGraft = func() { e := prefix; e.ExpectedSeq++; f.queue([]domain.GraftQueueEvent{e}) }
			case "conflict":
				f.r.failGraft = fakeStatusError{status: 409}
			case "graph-changed":
				f.r.onGraft = func() {
					snap, _ := f.st.GetSnapshot(f.ctx, f.b)
					snap.GraftSeq += 5
					if err := f.st.ReconcileGraftState(f.ctx, snap); err != nil {
						t.Fatal(err)
					}
				}
			case "new-selected-tail":
				f.r.onGraft = func() { e := prefix; e.ExpectedSeq++; f.queue([]domain.GraftQueueEvent{prefix, e}) }
			}
			ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
			defer cancel()
			_, err := f.svc.Push(ctx, f.input())
			success := strings.HasPrefix(scenario, "append-excluded")
			if success {
				if err != nil {
					t.Fatal(err)
				}
				if len(f.r.grafts) != 1 || !sameGraftQueueEvent(f.r.grafts[0], prefix) {
					t.Fatal("sent a nonplanned graft")
				}
				q, _ := readGraftQueue(f.root, "")
				if len(q.Events) != 1 || !sameGraftQueueEvent(q.Events[0], excluded) {
					t.Fatal("excluded tail consumed")
				}
			} else {
				if !errors.Is(err, domain.ErrSyncConflict) {
					t.Fatalf("want conflict: %v", err)
				}
				f.noPublication()
				if scenario == "excluded-head" && len(f.r.docs)+len(f.r.snaps)+len(f.r.grafts) > 0 {
					t.Fatal("blocked prefix sent prerequisites")
				}
				if scenario == "consumed-before-send" && len(f.r.grafts) > 0 {
					t.Fatal("stale head sent")
				}
				if scenario == "conflict" {
					q, _ := readGraftQueue(f.root, "")
					if len(q.Events) != 1 {
						t.Fatal("conflict removed unacknowledged event")
					}
				}
			}
		})
	}
}

func TestStrictPublicationDependencyObjectsAndHistoricalMemory(t *testing.T) {
	f := newStrictPublication(t)
	bundle := domain.SettingsBundle{Kind: "claude"}
	setting, err := f.st.PutSettingsObject(f.ctx, bundle)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := f.st.PutDoc(f.ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: "owner"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.st.PutSnapshot(f.ctx, domain.Snapshot{ID: owner, DocHash: owner, RepoID: f.repo, ClaudeSettings: setting}); err != nil {
		t.Fatal(err)
	}
	old := domain.MemoryDigest{SnapshotID: owner, Summary: "old", Fragments: []domain.MemoryFragment{{SourceSnapshot: f.a}}}
	h1, err := f.st.PutMemory(f.ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	next := old
	next.Summary = "new"
	next.PreviousMemoryHash = h1
	h2, err := f.st.PutMemory(f.ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.st.CompareAndSwapSnapshotMemory(f.ctx, owner, "", h2); err != nil {
		t.Fatal(err)
	}
	pin := f.event(12, "position", "R", "feature", f.b)
	pin.MemorySource = owner
	pin.MemoryHash = h1
	pin.MemoryPinned = true
	f.put(pin)
	if _, err = f.svc.Push(f.ctx, f.input()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.r.snaps, owner) || slices.Contains(f.r.snaps, f.x) || !reflect.DeepEqual(f.r.memories, []domain.ContentHash{owner, owner}) || !reflect.DeepEqual(f.r.settings, []domain.ContentHash{setting}) {
		t.Fatalf("wrong closure: snapshots=%v memory=%v settings=%v", f.r.snaps, f.r.memories, f.r.settings)
	}
	if _, ok := f.r.memoryObjects[h1]; !ok {
		t.Fatal("historical pin missing")
	}
}

func TestStrictPublicationDefersBeforeEffects(t *testing.T) {
	for _, scenario := range []string{"empty-scope", "legacy-server", "missing-root", "missing-parent", "legacy-graft", "foreign-dependency", "missing-pin"} {
		t.Run(scenario, func(t *testing.T) {
			f := newStrictPublication(t)
			in := f.input()
			switch scenario {
			case "empty-scope":
				in.Ref = ""
				in.Publication = &domain.PublicationScope{}
			case "legacy-server":
				f.r.protocol = 0
			case "missing-root":
				in.Cwd = ""
				in.RepoID = f.repo
			case "missing-parent":
				snap, _ := f.st.GetSnapshot(f.ctx, f.b)
				snap.GraftParents = []domain.ContentHash{domain.HashContent([]byte("missing"))}
				if err := f.st.PutSnapshot(f.ctx, snap); err != nil {
					t.Fatal(err)
				}
			case "legacy-graft":
				f.graft(f.b, f.a)
				q, _ := readGraftQueue(f.root, "")
				q.Events[0].Legacy = true
				f.queue(q.Events)
			case "foreign-dependency":
				e := f.event(12, "attach", "R", "feature", f.b)
				e.BindingParent = fmt.Sprintf("%032x", 2)
				f.put(e)
			case "missing-pin":
				m := domain.MemoryDigest{SnapshotID: f.b, Summary: "unattached pin"}
				h, err := f.st.PutMemory(f.ctx, m)
				if err != nil {
					t.Fatal(err)
				}
				e := f.event(12, "position", "R", "feature", f.b)
				e.MemoryHash = h
				e.MemoryPinned = true
				f.put(e)
			}
			_, err := f.svc.Push(f.ctx, in)
			if err == nil {
				t.Fatal("unsafe push succeeded")
			}
			if scenario == "legacy-server" && (!errors.Is(err, domain.ErrContextProtocolRequired) || errors.Is(err, domain.ErrSyncConflict)) {
				t.Fatalf("onboarding incorrectly permits append retry: %v", err)
			}
			f.noPublication()
			if len(f.r.snaps)+len(f.r.docs)+len(f.r.grafts)+len(f.r.memories) > 0 {
				t.Fatal("sent before complete preflight")
			}
		})
	}
}

func TestStrictPublicationFrozenAcrossObjectRetry(t *testing.T) {
	f := newStrictPublication(t)
	f.r.failSnapshotOnce = true
	var late domain.ContentHash
	f.r.onObject = func() {
		late = publicationSnapshot(t, f.st, f.repo, "late", nil, nil)
		e := f.event(20, "archive", "R", "feature", "")
		e.Source = f.b
		e.BindingParent = f.birth.ID
		f.put(e)
		e = f.event(21, "birth", "new-R", "feature", late)
		e.BindingParent = fmt.Sprintf("%032x", 20)
		f.put(e)
		if err := f.st.PutRef(f.ctx, domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "feature", BranchID: "new-R", Target: late}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.Push(f.ctx, f.input()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.r.snaps, late) || len(f.r.writes) != 1 || f.r.writes[0].BranchID != "R" || f.r.writes[0].Target != f.b {
		t.Fatal("retry reselected newer identity")
	}
	for _, e := range f.r.sent {
		if e.BranchID != "R" || e.Kind == "archive" {
			t.Fatal("retry widened history")
		}
	}
}

func TestStrictPublicationBarePushStillAll(t *testing.T) {
	f := newStrictPublication(t)
	if _, err := f.svc.Push(f.ctx, inbound.SyncInput{Cwd: f.root}); err != nil {
		t.Fatal(err)
	}
	branches := map[string]bool{}
	for _, ref := range f.r.writes {
		if ref.Kind == domain.RefBranch {
			branches[ref.Name] = true
		}
	}
	if !slices.Contains(f.r.snaps, f.x) || !branches["main"] || !branches["feature"] || len(f.r.sent) != 2 {
		t.Fatalf("bare push became selected: refs=%v history=%v", f.r.writes, f.r.sent)
	}
}

func TestStrictPublicationTwoGraftPrefixAndContinuationCAS(t *testing.T) {
	f := newStrictPublication(t)
	c := publicationSnapshot(t, f.st, f.repo, "C", []domain.ContentHash{f.b}, nil)
	f.graft(f.b, f.a)
	f.graft(c, f.a)
	if err := f.st.PutRef(f.ctx, domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "feature", BranchID: "R", Target: c}); err != nil {
		t.Fatal(err)
	}
	advance := f.event(13, "advance", "R", "feature", c)
	advance.Source = f.b
	f.put(advance)
	f.r.accepted = []domain.HistoryEvent{f.birth}
	f.r.refs = []domain.Ref{{RepoID: f.repo, Kind: domain.RefBranch, Name: "feature", BranchID: "R", Target: f.a}}
	if _, err := f.svc.Push(f.ctx, f.input()); err != nil {
		t.Fatal(err)
	}
	if len(f.r.grafts) != 2 || len(f.r.writes) != 2 || f.r.writes[0].Target != f.b || f.r.writes[1].Target != c {
		t.Fatalf("lost prefix/prerequisite order: grafts=%v refs=%v", f.r.grafts, f.r.writes)
	}
	queue, _ := readGraftQueue(f.root, "")
	if len(queue.Events) != 0 {
		t.Fatal(queue.Events)
	}
}

func TestStrictPublicationContinuationCannotPushReusedRemoteName(t *testing.T) {
	f := newStrictPublication(t)
	advance := f.event(13, "advance", "R", "feature", f.b)
	advance.Source = f.a
	f.put(advance)
	f.r.accepted = []domain.HistoryEvent{f.birth}
	f.r.refs = []domain.Ref{{RepoID: f.repo, Kind: domain.RefBranch, Name: "feature", BranchID: "other", Target: f.a}}
	if _, err := f.svc.Push(f.ctx, f.input()); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("reused remote name accepted: %v", err)
	}
	f.noPublication()
}
