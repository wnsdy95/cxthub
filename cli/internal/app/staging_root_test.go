package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func TestRootStagingStashRestartCommitReplay(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	source := f.source(t, "alpha", "frozen root")
	index, err := f.svc.Stage(ctx, inbound.StageInput{Cwd: f.root, Sessions: []inbound.StageSession{source}, DocIdentity: domain.DocumentIdentityRootV1})
	if err != nil {
		t.Fatal(err)
	}
	entry := index.Entries[0]
	if index.Version != 2 || entry.DocIdentity != domain.DocumentIdentityRootV1 {
		t.Fatalf("root index identity lost: %+v", index)
	}
	stash, err := f.svc.StashIndex(ctx, f.root, index.Revision)
	if err != nil || stash.Version != 2 {
		t.Fatal(stash, err)
	}
	if err := os.Remove(source.Path); err != nil {
		t.Fatal(err)
	}
	st := storage.NewWorktreeFileStore(f.root, filepath.Join(f.root, ".git"), f.git.branch, f.git.sha)
	svc := stagingService(f.git, st, st)
	current, err := svc.Inspect(ctx, f.root)
	if err != nil {
		t.Fatal(err)
	}
	popped, err := svc.PopIndex(ctx, f.root, stash.ID, current.Revision)
	if err != nil || popped.Entries[0].DocumentRef() != entry.DocumentRef() {
		t.Fatal(popped, err)
	}
	op, err := svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root, ExpectedRevision: popped.Revision})
	if err != nil || op.Version != 3 || !op.LocalFinalized {
		t.Fatal(op, err)
	}
	snap, err := st.GetSnapshot(ctx, entry.DocHash)
	if err != nil || !entry.DocumentRef().MatchesSnapshot(snap) {
		t.Fatal(snap, err)
	}
	again, err := svc.ResumeCommit(ctx, f.root, op.ID)
	if err != nil || again.Index.Entries[0].DocumentRef() != entry.DocumentRef() {
		t.Fatal(again, err)
	}
	if _, err := st.GetDoc(ctx, entry.DocHash); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		t.Fatal("hash-only read accepted root", err)
	}
}

func TestRootSaveRetainsLegacyParentAndCaptureGC(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	source := f.source(t, "alpha", "old")
	legacy, err := f.svc.save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: source.Provider, SessionPath: source.Path, Message: domain.HookMessagePrefix + " old"})
	if err != nil {
		t.Fatal(err)
	}
	f.source(t, "alpha", "old", "root growth")
	root, err := f.svc.save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: source.Provider, SessionPath: source.Path, DocIdentity: domain.DocumentIdentityRootV1})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := f.store.GetSnapshot(ctx, root.SnapshotID)
	if err != nil || snap.DocIdentity != domain.DocumentIdentityRootV1 || len(snap.Parents) != 1 || snap.Parents[0] != legacy.SnapshotID {
		t.Fatal(snap, err)
	}
	f.svc.save.collectHookLeaf(ctx, f.git.repo.ID, legacy.SnapshotID, root.SnapshotID)
	if _, err := f.store.GetSnapshot(ctx, legacy.SnapshotID); err != nil {
		t.Fatal("root GC lost legacy ancestor", err)
	}
}

func TestRootCaptureCancellationDoesNotPublish(t *testing.T) {
	f := newStagingFixture(t)
	source := f.source(t, "cancel", "synthetic")
	before, err := f.store.ListRefs(context.Background(), f.git.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = f.svc.save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: source.Provider, SessionPath: source.Path, DocIdentity: domain.DocumentIdentityRootV1})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	refs, err := f.store.ListRefs(context.Background(), f.git.repo.ID)
	if err != nil || !reflect.DeepEqual(before, refs) {
		t.Fatal("cancel published refs", refs, err)
	}
}

func TestRootStagingReaddPreservesGenerationAndCoveredPrefix(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	source := f.source(t, "alpha", "first")
	legacy := f.stage(t, source)
	op, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root})
	if err != nil {
		t.Fatal(err)
	}
	f.source(t, "alpha", "first", "root suffix")
	index, err := f.svc.Stage(ctx, inbound.StageInput{Cwd: f.root, Sessions: []inbound.StageSession{source}, DocIdentity: domain.DocumentIdentityRootV1})
	if err != nil {
		t.Fatal(err)
	}
	e := index.Entries[0]
	if e.Generation != legacy.Entries[0].Generation || e.StartEvent != legacy.Entries[0].Events || e.DocIdentity != domain.DocumentIdentityRootV1 {
		t.Fatal(e)
	}
	next, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := f.store.GetSnapshot(ctx, next.Position.Snapshot)
	if err != nil || len(snap.Parents) != 1 || snap.Parents[0] != op.Position.Snapshot {
		t.Fatal(snap, err)
	}
}

func TestRootStagingRejectsStrippedIdentityBeforeRefChange(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	source := f.source(t, "alpha", "root")
	index, err := f.svc.Stage(ctx, inbound.StageInput{Cwd: f.root, Sessions: []inbound.StageSession{source}, DocIdentity: domain.DocumentIdentityRootV1})
	if err != nil {
		t.Fatal(err)
	}
	_, position, err := f.store.ReadStaging(ctx, f.git.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	stripped := index
	stripped.Entries = append([]domain.StagedSession{}, index.Entries...)
	stripped.Entries[0].DocIdentity = ""
	stripped = stripped.WithRevision()
	before, err := f.store.ListRefs(ctx, f.git.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CompareAndSwapStaging(ctx, index.Revision, stripped, position); err == nil {
		t.Fatal("identity downgrade accepted")
	}
	after, err := f.store.ListRefs(ctx, f.git.repo.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("rejected index changed refs", err)
	}
	current, err := f.svc.Inspect(ctx, f.root)
	if err != nil || current.Revision != index.Revision {
		t.Fatal("rejected index changed frozen entry", err)
	}
}

type rootCaptureCancelCodec struct {
	outbound.ProviderCodec
	cancel context.CancelFunc
}

func (c rootCaptureCancelCodec) Decode(ctx context.Context, raw []byte) (domain.CIRDocument, error) {
	doc, err := c.ProviderCodec.Decode(ctx, raw)
	c.cancel()
	return doc, err
}
func TestRootCaptureCancellationAfterDecodeDoesNotPublish(t *testing.T) {
	f := newStagingFixture(t)
	source := f.source(t, "cancel", "synthetic")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.svc.save.codecs[source.Provider] = rootCaptureCancelCodec{ProviderCodec: f.svc.save.codecs[source.Provider], cancel: cancel}
	before, err := f.store.ListRefs(context.Background(), f.git.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: source.Provider, SessionPath: source.Path, DocIdentity: domain.DocumentIdentityRootV1})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	after, err := f.store.ListRefs(context.Background(), f.git.repo.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("cancel changed refs", err)
	}
	snapshots, err := f.store.ListSnapshots(context.Background(), f.git.repo.ID, "")
	if err != nil || len(snapshots) != 0 {
		t.Fatal("cancel published snapshot", err)
	}
}

type rootStashLoad struct{ inbound.LoadSession }

func (rootStashLoad) Load(context.Context, inbound.LoadInput) (inbound.LoadOutput, error) {
	return inbound.LoadOutput{}, nil
}
func TestRootSessionStashSnapshotRetainsIdentityAfterRestart(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	source := f.source(t, "stash", "synthetic root")
	save := f.svc.save
	svc := NewStashService(f.git, save.captures, save.codecs, f.store, rootStashLoad{}, save.capture)
	out, err := svc.Stash(ctx, inbound.StashInput{Cwd: f.root, Provider: source.Provider, SessionPath: source.Path, DocIdentity: domain.DocumentIdentityRootV1})
	if err != nil {
		t.Fatal(err)
	}
	fresh := storage.NewWorktreeFileStore(f.root, filepath.Join(f.root, ".git"), f.git.branch, f.git.sha)
	stack, err := fresh.StashList(ctx, f.git.repo.ID)
	if err != nil || len(stack) != 1 || stack[0].Snapshot != out.StashID {
		t.Fatal(stack, err)
	}
	snap, err := fresh.GetSnapshot(ctx, out.StashID)
	if err != nil || snap.DocIdentity != domain.DocumentIdentityRootV1 {
		t.Fatal(snap, err)
	}
	if _, err := fresh.GetDocReference(ctx, snap.DocumentRef()); err != nil {
		t.Fatal(err)
	}
}
