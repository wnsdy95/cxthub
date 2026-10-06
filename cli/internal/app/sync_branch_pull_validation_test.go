package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type reviewTransferRemote struct {
	outbound.RemoteSync
	plan                                                                           domain.BranchPullPlan
	snapshots                                                                      []domain.Snapshot
	memories                                                                       map[domain.ContentHash]domain.MemoryDigest
	memoryErrors                                                                   map[domain.ContentHash]error
	capability                                                                     outbound.PullCapabilities
	planErr                                                                        error
	onPlan                                                                         func()
	planCalls, broadCalls, mutableCalls, memoryCalls, settingsCalls, metadataCount int
}

func (r *reviewTransferRemote) SyncRemoteIdentity() string { return "review-scoped-endpoint" }
func (r *reviewTransferRemote) PullCapabilities(context.Context, string) (outbound.PullCapabilities, error) {
	return r.capability, nil
}
func (r *reviewTransferRemote) PullSelectedBranchTo(_ context.Context, _ string, _ domain.BranchPullRequest, states map[domain.ContentHash]domain.ContentHash, _ []domain.ContentHash, _ outbound.PullDocumentReceiver) (domain.BranchPullPlan, []domain.Snapshot, error) {
	r.planCalls++
	if r.onPlan != nil {
		r.onPlan()
	}
	if r.planErr != nil {
		return domain.BranchPullPlan{}, nil, r.planErr
	}
	var incoming []domain.Snapshot
	for _, s := range r.snapshots {
		token, _ := domain.SnapshotStateHash(s)
		if states[s.ID] != token {
			incoming = append(incoming, s)
		}
	}
	r.metadataCount += len(incoming)
	return r.plan, incoming, nil
}
func (r *reviewTransferRemote) Pull(context.Context, string, map[domain.ContentHash]domain.ContentHash, []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	r.broadCalls++
	return nil, nil, nil, errors.New("review sentinel: broad pull")
}
func (r *reviewTransferRemote) PullMemory(context.Context, string, domain.ContentHash) (domain.MemoryDigest, error) {
	r.mutableCalls++
	return domain.MemoryDigest{}, errors.New("review sentinel: mutable memory pointer")
}
func (r *reviewTransferRemote) PullMemoryObject(_ context.Context, _ string, h domain.ContentHash) (domain.MemoryDigest, error) {
	r.memoryCalls++
	if e := r.memoryErrors[h]; e != nil {
		return domain.MemoryDigest{}, e
	}
	d, ok := r.memories[h]
	if !ok {
		return d, domain.ErrNotFound
	}
	return d, nil
}
func (r *reviewTransferRemote) PullSettingsObject(context.Context, string, domain.ContentHash) (domain.SettingsBundle, error) {
	r.settingsCalls++
	return domain.SettingsBundle{}, domain.ErrNotFound
}

type reviewTransferFixture struct {
	st             *storage.FileStore
	remote         *reviewTransferRemote
	repo           string
	target, source domain.ContentHash
	before, full   outbound.RemoteObservation
}

func reviewTransferSetup(t *testing.T, warm bool) reviewTransferFixture {
	t.Helper()
	return reviewTransferSetupAt(t, warm, t.TempDir())
}

func reviewTransferSetupAt(t *testing.T, warm bool, root string) reviewTransferFixture {
	t.Helper()
	ctx := context.Background()
	st := storage.NewFileStore(root)
	repo := string(domain.HashContent([]byte("review scoped repo")))
	docs := []domain.SessionDoc{pullDoc(t, "review target"), pullDoc(t, "review source")}
	var snaps []domain.Snapshot
	for _, d := range docs {
		if _, e := st.PutDoc(ctx, d); e != nil {
			t.Fatal(e)
		}
		s := domain.Snapshot{RepoID: repo, ID: d.Hash, DocHash: d.Hash}
		snaps = append(snaps, s)
		if e := st.PutSnapshot(ctx, s); e != nil {
			t.Fatal(e)
		}
	}
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "feature", BranchID: "review-R", Target: docs[0].Hash}
	birth := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: repo, BranchID: ref.BranchID, Branch: ref.Name, Kind: "birth", Source: docs[1].Hash, Target: ref.Target, CreatedAt: time.Unix(1, 0)}
	plan := domain.BranchPullPlan{Version: 1, RepoID: repo, Branch: ref.Name, ContextProtocol: 1, SelectedRef: ref, Refs: []domain.Ref{ref}, History: []domain.HistoryEvent{birth}, SnapshotStates: map[domain.ContentHash]domain.ContentHash{}}
	for _, s := range snaps {
		plan.SnapshotIndex = append(plan.SnapshotIndex, s.ID)
		plan.SnapshotStates[s.ID], _ = domain.SnapshotStateHash(s)
	}
	remote := &reviewTransferRemote{plan: plan, snapshots: snaps, memories: map[domain.ContentHash]domain.MemoryDigest{}, memoryErrors: map[domain.ContentHash]error{}, capability: outbound.PullCapabilities{ContextProtocol: 1, BranchPlanVersion: 1}}
	full, e := st.ReadRemoteObservation(ctx, repo, remote.SyncRemoteIdentity())
	if e != nil {
		t.Fatal(e)
	}
	full.Snapshots = snaps
	if e = st.CompareAndSwapRemoteObservation(ctx, "", full); e != nil {
		t.Fatal(e)
	}
	full, e = st.ReadRemoteObservation(ctx, repo, remote.SyncRemoteIdentity())
	if e != nil {
		t.Fatal(e)
	}
	f := reviewTransferFixture{st: st, remote: remote, repo: repo, target: snaps[0].ID, source: snaps[1].ID, full: full}
	if warm {
		f.seed(t)
	}
	f.before, e = st.ReadScopedRemoteObservation(ctx, repo, remote.SyncRemoteIdentity(), "feature")
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func (f *reviewTransferFixture) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	old, e := f.st.ReadScopedRemoteObservation(ctx, f.repo, f.remote.SyncRemoteIdentity(), "feature")
	if e != nil {
		t.Fatal(e)
	}
	next := old
	next.Snapshots = f.remote.snapshots
	next.Refs = f.remote.plan.Refs
	next.History = f.remote.plan.History
	if e = f.st.CompareAndSwapRemoteObservation(ctx, old.Revision, next); e != nil {
		t.Fatal(e)
	}
	f.before, e = f.st.ReadScopedRemoteObservation(ctx, f.repo, f.remote.SyncRemoteIdentity(), "feature")
	if e != nil {
		t.Fatal(e)
	}
}
func (f reviewTransferFixture) fetch() (inbound.RemoteBranchObservation, error) {
	return newTestSyncService(f.st, f.remote, nil).ResolveRemoteBranchObservation(context.Background(), inbound.SyncInput{RepoID: f.repo}, "feature")
}
func (f reviewTransferFixture) assertUnadopted(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	full, e := f.st.ReadRemoteObservation(ctx, f.repo, f.remote.SyncRemoteIdentity())
	if e != nil || !reflect.DeepEqual(full, f.full) {
		t.Fatalf("full repair observation changed: %v", e)
	}
	if _, e = f.st.GetRef(ctx, f.repo, domain.RefBranch, "feature"); !errors.Is(e, domain.ErrNotFound) {
		t.Fatalf("ref adopted: %v", e)
	}
	history, e := f.st.ListHistoryEvents(ctx, f.repo)
	if e != nil || len(history) != 0 {
		t.Fatalf("history adopted: %+v %v", history, e)
	}
	for _, s := range f.remote.snapshots {
		local, e := f.st.GetSnapshot(ctx, s.ID)
		if e != nil || local.MemoryHash != "" || len(local.GraftParents) != 0 {
			t.Fatalf("local attachment/graft changed: %+v %v", local, e)
		}
	}
}

func TestScopedPRRequiresExactWitness(t *testing.T) {
	for _, mode := range []string{"valid", "conflicting-pins", "missing", "wrong-identity", "wrong-target", "unpinned", "archive-clone"} {
		t.Run(mode, func(t *testing.T) {
			f := reviewTransferSetup(t, false)
			head := strings.Repeat("1", 40)
			pin := domain.HistoryEvent{ID: strings.Repeat("b", 32), RepoID: f.repo, BranchID: "review-X", Branch: "source", Kind: "position", Target: f.source, GitAfter: head, MemoryPinned: true, CreatedAt: time.Unix(2, 0)}
			receipt := domain.HistoryEvent{ID: strings.Repeat("c", 32), RepoID: f.repo, BranchID: "review-R", Branch: "feature", Kind: "pr-merge", SourceBranchID: "review-X", Source: f.source, Target: f.target, PRCompleted: true, PR: &domain.PullRequestMerge{Number: 1, BaseBranch: "feature", HeadBranch: "source", HeadSHA: head, MergeSHA: strings.Repeat("2", 40)}, CreatedAt: time.Unix(3, 0)}
			switch mode {
			case "wrong-identity":
				pin.BranchID = "reused-X"
			case "wrong-target":
				pin.Target = f.target
			case "unpinned":
				pin.MemoryPinned = false
			case "archive-clone":
				born := pin
				born.ID = strings.Repeat("d", 32)
				born.Kind = "birth"
				born.MemoryPinned = false
				pin.Kind = "archive"
				pin.BindingParent = born.ID
				f.remote.plan.History = append(f.remote.plan.History, born)
			}
			f.remote.plan.History = append(f.remote.plan.History, receipt)
			if mode != "missing" {
				f.remote.plan.History = append(f.remote.plan.History, pin)
			}
			if mode == "conflicting-pins" {
				digest := domain.MemoryDigest{SnapshotID: f.source, Summary: "competing nonempty source pin"}
				hash, e := f.st.PutMemory(context.Background(), digest)
				if e != nil {
					t.Fatal(e)
				}
				other := pin
				other.ID = strings.Repeat("e", 32)
				other.MemoryHash = hash
				f.remote.plan.History = append(f.remote.plan.History, other)
			}
			got, err := f.fetch()
			after, e := f.st.ReadScopedRemoteObservation(context.Background(), f.repo, f.remote.SyncRemoteIdentity(), "feature")
			if e != nil {
				t.Fatal(e)
			}
			f.assertUnadopted(t)
			if mode == "valid" || mode == "conflicting-pins" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Errorf("accepted completed PR without exact ordinary witness: selected=%s history=%d", got.Ref.Target, len(got.History))
			}
			if after.Revision != f.before.Revision {
				t.Error("incomplete PR evidence committed as successful scoped observation")
			}
		})
	}
}

func TestScopedWarmHistoricalMemory(t *testing.T) {
	for _, mode := range []string{"repair", "missing-ancestor", "wrong-ancestor-owner", "corrupt-ancestor", "explicit-owner-mismatch", "pinned-empty"} {
		t.Run(mode, func(t *testing.T) {
			f := reviewTransferSetup(t, false)
			parent := domain.MemoryDigest{SnapshotID: f.source, Summary: "historical ancestor"}
			if mode == "wrong-ancestor-owner" {
				parent.SnapshotID = f.target
			}
			ph, _ := domain.MemoryDigestHash(parent)
			pin := domain.MemoryDigest{SnapshotID: f.source, Summary: "historical pin", PreviousMemoryHash: ph}
			h, _ := domain.MemoryDigestHash(pin)
			if _, err := f.st.PutMemory(context.Background(), pin); err != nil {
				t.Fatal(err)
			}
			event := domain.HistoryEvent{ID: strings.Repeat("b", 32), RepoID: f.repo, BranchID: "review-R", Branch: "feature", Kind: "position", Target: f.target, Source: f.source, MemorySource: f.source, MemoryHash: h, MemoryPinned: true, CreatedAt: time.Unix(2, 0)}
			if mode == "explicit-owner-mismatch" {
				event.MemorySource = f.target
			}
			if mode == "pinned-empty" {
				event.MemoryHash = ""
				event.MemorySource = ""
			}
			if mode == "corrupt-ancestor" {
				parent.Summary = "changed bytes"
			}
			f.remote.plan.History = append(f.remote.plan.History, event)
			f.remote.memories[ph] = parent
			if mode == "missing-ancestor" {
				delete(f.remote.memories, ph)
			}
			f.seed(t)
			_, err := f.fetch()
			success := mode == "repair" || mode == "pinned-empty"
			if (err == nil) != success {
				t.Fatalf("success=%v err=%v", success, err)
			}
			if f.remote.metadataCount != 0 || f.remote.mutableCalls != 0 {
				t.Fatalf("warm metadata=%d mutable-pointer=%d", f.remote.metadataCount, f.remote.mutableCalls)
			}
			if mode == "repair" {
				if _, e := f.st.GetMemory(context.Background(), ph); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "pinned-empty" && f.remote.memoryCalls != 0 {
				t.Fatal("pinned empty loaded memory")
			}
			if !success {
				after, e := f.st.ReadScopedRemoteObservation(context.Background(), f.repo, f.remote.SyncRemoteIdentity(), "feature")
				if e != nil || !reflect.DeepEqual(after, f.before) {
					t.Fatalf("failed memory verification changed observation: %v", e)
				}
			}
			f.assertUnadopted(t)
		})
	}
}

func TestScopedCASLossPreservesWinner(t *testing.T) {
	f := reviewTransferSetup(t, true)
	var winner outbound.RemoteObservation
	f.remote.onPlan = func() {
		winner = f.before
		event := f.remote.plan.History[0]
		event.ID = strings.Repeat("e", 32)
		event.Kind = "position"
		winner.History = append(append([]domain.HistoryEvent{}, winner.History...), event)
		if e := f.st.CompareAndSwapRemoteObservation(context.Background(), f.before.Revision, winner); e != nil {
			t.Fatal(e)
		}
		var e error
		winner, e = f.st.ReadScopedRemoteObservation(context.Background(), f.repo, f.remote.SyncRemoteIdentity(), "feature")
		if e != nil {
			t.Fatal(e)
		}
	}
	got, err := f.fetch()
	if !errors.Is(err, domain.ErrSyncConflict) || got.Ref.Target != "" {
		t.Fatalf("CAS loser returned proof: %+v %v", got, err)
	}
	after, e := f.st.ReadScopedRemoteObservation(context.Background(), f.repo, f.remote.SyncRemoteIdentity(), "feature")
	if e != nil || !reflect.DeepEqual(after, winner) {
		t.Fatalf("winner overwritten: %v", e)
	}
	f.assertUnadopted(t)
}

func TestScopedSupportedFailureNeverFallsBack(t *testing.T) {
	for _, status := range []int{401, 403, 404, 409, 422, 500, 501} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := reviewTransferSetup(t, true)
			sentinel := fmt.Errorf("plan status %d", status)
			f.remote.planErr = sentinel
			_, err := f.fetch()
			if !errors.Is(err, sentinel) || f.remote.broadCalls != 0 {
				t.Fatalf("lost supported-plan cause or fell back: %v broad=%d", err, f.remote.broadCalls)
			}
			after, e := f.st.ReadScopedRemoteObservation(context.Background(), f.repo, f.remote.SyncRemoteIdentity(), "feature")
			if e != nil || !reflect.DeepEqual(after, f.before) {
				t.Fatal("failed plan recorded observation")
			}
			f.assertUnadopted(t)
		})
	}
}

type reviewSettingsStore struct {
	*storage.FileStore
	bundle domain.SettingsBundle
}

func (s reviewSettingsStore) GetSettingsObject(context.Context, domain.ContentHash) (domain.SettingsBundle, error) {
	return s.bundle, nil
}

func TestScopedWarmSettingsBoundary(t *testing.T) {
	for _, mode := range []string{"cached-valid", "cached-corrupt", "inventory-missing", "inventory-wrong-kind", "inventory-extra", "missing-body"} {
		t.Run(mode, func(t *testing.T) {
			f := reviewTransferSetup(t, false)
			bundle := domain.SettingsBundle{Kind: "claude", Files: []domain.SettingsFile{{Path: "rules.md", ContentB64: "b2s="}}}
			hash, err := domain.SettingsObjectHash(bundle)
			if err != nil {
				t.Fatal(err)
			}
			f.remote.snapshots[0].ClaudeSettings = hash
			f.remote.plan.SettingsObjects = []domain.BranchPullSettings{{Kind: "claude", Hash: hash}}
			f.remote.plan.SnapshotStates[f.target], _ = domain.SnapshotStateHash(f.remote.snapshots[0])
			switch mode {
			case "inventory-missing":
				f.remote.plan.SettingsObjects = nil
			case "inventory-wrong-kind":
				f.remote.plan.SettingsObjects[0].Kind = "codex"
			case "inventory-extra":
				f.remote.plan.SettingsObjects = append(f.remote.plan.SettingsObjects, domain.BranchPullSettings{Kind: "agents", Hash: domain.HashContent([]byte("extra settings"))})
			}
			f.seed(t)
			if mode == "cached-corrupt" {
				bundle.Files[0].ContentB64 = "dGFtcGVyZWQ="
			}
			var store outbound.SessionStore = reviewSettingsStore{FileStore: f.st, bundle: bundle}
			if mode == "missing-body" {
				store = f.st
			}
			_, err = newTestSyncService(store, f.remote, nil).ResolveRemoteBranchObservation(context.Background(), inbound.SyncInput{RepoID: f.repo}, "feature")
			if mode == "cached-valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatal("invalid settings evidence succeeded")
				}
				after, e := f.st.ReadScopedRemoteObservation(context.Background(), f.repo, f.remote.SyncRemoteIdentity(), "feature")
				if e != nil || !reflect.DeepEqual(after, f.before) {
					t.Fatal("settings failure committed observation")
				}
			}
			if f.remote.metadataCount != 0 {
				t.Fatal("fixture was not warm")
			}
			f.assertUnadopted(t)
		})
	}
}

func TestScopedCapabilityFallbackGate(t *testing.T) {
	for _, version := range []int{0, -1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f := reviewTransferSetup(t, true)
			f.remote.capability.BranchPlanVersion = version
			_, err := f.fetch()
			if version == 0 {
				if err == nil || !strings.Contains(err.Error(), "review sentinel: broad pull") || f.remote.broadCalls != 1 || f.remote.planCalls != 0 {
					t.Fatalf("legacy selection failed: %v broad=%d plan=%d", err, f.remote.broadCalls, f.remote.planCalls)
				}
			} else if !errors.Is(err, domain.ErrSyncConflict) || f.remote.broadCalls != 0 || f.remote.planCalls != 0 {
				t.Fatalf("unsupported capability fell back: %v broad=%d plan=%d", err, f.remote.broadCalls, f.remote.planCalls)
			}
			after, e := f.st.ReadScopedRemoteObservation(context.Background(), f.repo, f.remote.SyncRemoteIdentity(), "feature")
			if e != nil || !reflect.DeepEqual(after, f.before) {
				t.Fatal("fallback modified branch observation")
			}
			f.assertUnadopted(t)
		})
	}
}
