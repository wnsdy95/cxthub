package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func rootFrozenFixture(t *testing.T, wrongSnapshot bool) (frozenFixture, domain.ContentHash) {
	t.Helper()
	f := newFrozenFixture(t)
	ctx := context.Background()
	e := f.index.Entries[0]
	d, err := f.store.GetDoc(ctx, e.DocHash)
	if err != nil {
		t.Fatal(err)
	}
	d.CIR.Envelope.CIRVersion = domain.CIRVersionV2
	d.CIR.Events = []domain.Event{{Seq: 0, Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "frozen root"}}}}
	m, bodies, err := domain.ConversationManifestForCIR(d.CIR)
	if err != nil {
		t.Fatal(err)
	}
	h, err := domain.ConversationManifestHash(m)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	for id, b := range bodies {
		if err := f.store.PutChunk(ctx, id, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.PutConversationManifest(ctx, domain.DocumentRepresentation{Hash: h, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}); err != nil {
		t.Fatal(err)
	}
	snap, err := f.store.GetSnapshot(ctx, e.DocHash)
	if err != nil {
		t.Fatal(err)
	}
	snap.ID = h
	snap.DocHash = h
	snap.DocIdentity = domain.DocumentIdentityRootV1
	if wrongSnapshot {
		snap.DocIdentity = ""
	}
	if err := f.store.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	next := f.index
	next.Entries = append([]domain.StagedSession{}, f.index.Entries...)
	next.Entries[0].DocHash = h
	next.Entries[0].DocIdentity = domain.DocumentIdentityRootV1
	next.Entries[0].Events = 1
	next.Sequence++
	next = next.WithRevision()
	if err := f.store.CompareAndSwapStaging(ctx, f.index.Revision, next, f.position); err != nil {
		t.Fatal(err)
	}
	f.index = next
	f.op.Index = next
	f.op.Version = domain.RootStagingCommitVersion
	f.op.Position.Snapshot = h
	f.op.Position.SharedTarget = h
	f.op.Position.Selection.Source = h
	f.op.Position.Selection.Target = h
	f.op.Ref.Target = h
	return f, m.Chunks[0].Hash
}
func TestRootStagingInterruptedJournalRecoveryChecksCurrentBytes(t *testing.T) {
	for _, mode := range []string{"valid", "corrupt", "wrong-snapshot", "legacy-journal"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f, chunk := rootFrozenFixture(t, mode == "wrong-snapshot")
			if mode == "legacy-journal" {
				f.op.Version = 2
				if _, err := f.store.FinalizeStagingCommit(ctx, f.op); !errors.Is(err, domain.ErrStagingVersion) {
					t.Fatal(err)
				}
				return
			}
			if err := f.store.writeStagingOperation(f.op); err != nil {
				t.Fatal(err)
			}
			if mode == "corrupt" {
				if err := os.WriteFile(f.store.objectPath("chunks", chunk), []byte("bad"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			fresh := NewWorktreeFileStore(f.store.repoRoot, filepath.Join(f.store.repoRoot, ".git"), "main", f.store.gitCommit)
			op, err := fresh.ResumeStagingCommit(ctx, f.repo, f.op.ID)
			if mode != "valid" {
				if err == nil {
					t.Fatal("invalid frozen operation published")
				}
				ref, e := fresh.GetRef(ctx, f.repo, domain.RefBranch, "main")
				if e != nil || ref.Target != f.op.ExpectedRef.Target {
					t.Fatal(ref, e)
				}
				return
			}
			if err != nil || !op.LocalFinalized || op.Version != 3 {
				t.Fatal(op, err)
			}
			snap, err := fresh.GetSnapshot(ctx, op.Ref.Target)
			if err != nil || !op.Index.Entries[0].DocumentRef().MatchesSnapshot(snap) {
				t.Fatal(snap, err)
			}
			index, _, err := fresh.ReadStaging(ctx, f.repo)
			if err != nil || len(index.Entries) != 0 || index.Version != 1 {
				t.Fatal(index, err)
			}
		})
	}
}
