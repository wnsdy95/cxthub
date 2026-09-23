package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func pendingGCCIR(provider domain.ProviderKind, messages ...string) domain.CIRDocument {
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{
		CIRVersion: "1", SourceProvider: provider, SessionOriginID: "native-session",
		Cwd: "/work/main", GitBranch: "main", Fidelity: domain.FidelityFull,
	}}
	for i, message := range messages {
		cir.Events = append(cir.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: i, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: message}},
		})
	}
	return cir
}

func putPendingGCCapture(t *testing.T, st *store.FSStore, repo domain.ContentHash, cir domain.CIRDocument) domain.Snapshot {
	t.Helper()
	ctx := systemTestContext()
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(raw)
	if _, err := st.PutDoc(ctx, repo, domain.SessionDoc{Hash: hash, CIR: cir}); err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: hash, DocHash: hash, RepoID: repo,
		Provider: cir.Envelope.SourceProvider, SessionID: cir.Envelope.SessionOriginID,
		Branch: cir.Envelope.GitBranch, Message: domain.HookMessagePrefix + " capture",
	}
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

func assertPendingGCCapture(t *testing.T, st *store.FSStore, snap domain.Snapshot) {
	t.Helper()
	ctx := systemTestContext()
	if _, err := st.GetSnapshot(ctx, snap.RepoID, snap.ID); err != nil {
		t.Errorf("capture metadata lost: %s: %v", snap.ID, err)
	}
	if _, err := st.GetDoc(ctx, snap.RepoID, snap.DocHash); err != nil {
		t.Errorf("capture document lost: %s: %v", snap.DocHash, err)
	}
}

func TestPutPendingLateCapturePreservesNewerData(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		for _, tc := range []struct {
			name     string
			messages []string
		}{
			{name: "shorter", messages: []string{"first"}},
			{name: "divergent", messages: []string{"first", "offline fork"}},
			{name: "longer divergent", messages: []string{"first", "offline fork", "more offline work"}},
		} {
			t.Run(string(provider)+"/"+tc.name, func(t *testing.T) {
				ctx := systemTestContext()
				svc, st := newFsckSvc(t)
				repo := hh("late pending gc")
				newer := putPendingGCCapture(t, st, repo, pendingGCCIR(provider, "first", "newer work"))
				late := putPendingGCCapture(t, st, repo, pendingGCCIR(provider, tc.messages...))
				p := domain.Pending{SessionID: newer.SessionID, Provider: provider, Branch: "main", Target: newer.ID}
				if err := svc.PutPending(ctx, repo, p.SessionID, p); err != nil {
					t.Fatal(err)
				}
				p.Target = late.ID
				if err := svc.PutPending(ctx, repo, p.SessionID, p); err != nil {
					t.Fatal(err)
				}
				got, err := svc.ListPendings(ctx, repo)
				if err != nil || len(got) != 1 || got[0].Target != late.ID || got[0].SessionID != p.SessionID {
					t.Fatalf("per-session pointer replacement changed: %+v err=%v", got, err)
				}
				assertPendingGCCapture(t, st, newer)
				assertPendingGCCapture(t, st, late)
			})
		}
	}
}

func TestPutPendingGCRequiresUnreferencedSupersededLeaf(t *testing.T) {
	for _, guard := range []string{"none", "branch", "tag", "pending", "natural child", "graft child", "memory", "legacy memory"} {
		t.Run(guard, func(t *testing.T) {
			ctx := systemTestContext()
			svc, st := newFsckSvc(t)
			repo := hh("pending gc references")
			old := putPendingGCCapture(t, st, repo, pendingGCCIR(domain.ProviderClaude, "first"))
			nextDoc := pendingGCCIR(domain.ProviderClaude, "first", "new work")
			nextDoc.Envelope.Cwd, nextDoc.Envelope.GitBranch = "/work/other-worktree", "feature/other"
			next := putPendingGCCapture(t, st, repo, nextDoc)
			switch guard {
			case "legacy memory":
				if err := st.PutMemoryMeta(ctx, repo, domain.MemoryDigest{SnapshotID: old.ID, Summary: "legacy memory"}); err != nil {
					t.Fatal(err)
				}
			case "memory":
				if _, err := svc.PutMemoryDigestCAS(ctx, repo, domain.MemoryDigest{SnapshotID: old.ID, Summary: "independent memory absent from successor"}); err != nil {
					t.Fatal(err)
				}
			case "branch", "tag":
				kind := domain.RefBranch
				if guard == "tag" {
					kind = domain.RefTag
				}
				if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: kind, Name: "retained", Target: old.ID}, ""); err != nil {
					t.Fatal(err)
				}
			case "pending":
				if err := st.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: "other-pointer", Provider: old.Provider, Target: old.ID}); err != nil {
					t.Fatal(err)
				}
			case "natural child", "graft child":
				child := domain.Snapshot{ID: hh("unreferenced child"), DocHash: hh("unreferenced child"), RepoID: repo}
				switch guard {
				case "natural child":
					child.Parents = []domain.ContentHash{old.ID}
				case "graft child":
					child.GraftParents, child.Grafted = []domain.ContentHash{old.ID}, true
				}
				if err := st.PutSnapshot(ctx, child); err != nil {
					t.Fatal(err)
				}
			}
			refsBefore, err := st.ListRefs(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			p := domain.Pending{SessionID: old.SessionID, Provider: old.Provider, Branch: old.Branch, Target: old.ID}
			if err := svc.PutPending(ctx, repo, p.SessionID, p); err != nil {
				t.Fatal(err)
			}
			p.Branch, p.Target = next.Branch, next.ID
			if err := svc.PutPending(ctx, repo, p.SessionID, p); err != nil {
				t.Fatal(err)
			}
			if guard == "none" {
				if _, err := st.GetSnapshot(ctx, repo, old.ID); !errors.Is(err, domain.ErrNotFound) {
					t.Errorf("superseded leaf was not collected: %v", err)
				}
				if _, err := st.GetDoc(ctx, repo, old.DocHash); !errors.Is(err, domain.ErrNotFound) {
					t.Errorf("superseded document was not collected: %v", err)
				}
			} else {
				assertPendingGCCapture(t, st, old)
			}
			assertPendingGCCapture(t, st, next)
			refsAfter, err := st.ListRefs(ctx, repo)
			if err != nil || !reflect.DeepEqual(refsBefore, refsAfter) {
				t.Errorf("replacement changed refs: before=%v after=%v err=%v", refsBefore, refsAfter, err)
			}
			got, err := st.ListPendings(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			bySession := map[string]domain.Pending{}
			for _, p := range got {
				bySession[p.SessionID] = p
			}
			if bySession[next.SessionID].Target != next.ID || bySession[next.SessionID].Branch != next.Branch {
				t.Errorf("native session could not move worktrees: %v", got)
			}
			if guard == "pending" && bySession["other-pointer"].Target != old.ID {
				t.Errorf("replacement changed another pending: %v", got)
			}
		})
	}
}

type unreadablePendingGCDoc struct {
	outbound.BlobStore
	hash domain.ContentHash
}

func (s *unreadablePendingGCDoc) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	if hash == s.hash {
		return domain.SessionDoc{}, errors.New("document unreadable")
	}
	return s.BlobStore.GetDoc(ctx, repo, hash)
}

func TestGCHookLeafPreservesUnverifiedReplacement(t *testing.T) {
	for _, reason := range []string{"missing successor", "old doc unreadable", "new doc unreadable", "different provider", "different session"} {
		t.Run(reason, func(t *testing.T) {
			ctx := systemTestContext()
			svc, st := newFsckSvc(t)
			repo := hh("unverified gc successor")
			old := putPendingGCCapture(t, st, repo, pendingGCCIR(domain.ProviderClaude, "first"))
			nextDoc := pendingGCCIR(domain.ProviderClaude, "first", "continued")
			if reason == "different provider" {
				nextDoc.Envelope.SourceProvider = domain.ProviderCodex
			}
			if reason == "different session" {
				nextDoc.Envelope.SessionOriginID = "different-session"
			}
			next := putPendingGCCapture(t, st, repo, nextDoc)
			current := next.ID
			switch reason {
			case "missing successor":
				current = hh("absent replacement")
			case "old doc unreadable":
				svc.blobs = &unreadablePendingGCDoc{BlobStore: st, hash: old.DocHash}
			case "new doc unreadable":
				svc.blobs = &unreadablePendingGCDoc{BlobStore: st, hash: next.DocHash}
			}
			svc.gcHookLeaf(ctx, repo, old.ID, current)
			assertPendingGCCapture(t, st, old)
			assertPendingGCCapture(t, st, next)
		})
	}
}

func TestPendingReleaseWithoutSuccessorPreservesCapture(t *testing.T) {
	for _, mode := range []string{"legacy", "CAS"} {
		t.Run(mode, func(t *testing.T) {
			ctx := systemTestContext()
			svc, st := newFsckSvc(t)
			repo := hh("pending release gc")
			snap := putPendingGCCapture(t, st, repo, pendingGCCIR(domain.ProviderClaude, "uncommitted data"))
			if err := svc.PutPending(ctx, repo, snap.SessionID, domain.Pending{Provider: snap.Provider, Branch: snap.Branch, Target: snap.ID}); err != nil {
				t.Fatal(err)
			}
			if mode == "legacy" {
				if err := svc.DeletePending(ctx, repo, snap.SessionID); err != nil {
					t.Fatal(err)
				}
			} else if deleted, err := svc.CompareAndDeletePending(ctx, repo, snap.SessionID, snap.ID); err != nil || !deleted {
				t.Fatalf("matching CAS failed: deleted=%v err=%v", deleted, err)
			}
			got, err := st.ListPendings(ctx, repo)
			if err != nil || len(got) != 0 {
				t.Fatalf("pointer release failed: %v err=%v", got, err)
			}
			assertPendingGCCapture(t, st, snap)
		})
	}
}
