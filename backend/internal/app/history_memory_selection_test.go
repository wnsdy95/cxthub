package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type historyMemorySelectionStore interface {
	outbound.MetadataStore
	outbound.BlobStore
	outbound.HistoryStore
	outbound.RepositoryStore
}
type historyMemorySelectionFixture struct {
	svc                *Service
	st                 historyMemorySelectionStore
	ctx                context.Context
	repo               domain.ContentHash
	repository, member string
	before, after      domain.HistoryEvent
	other              domain.ContentHash
}

func newHistoryMemorySelectionFixture(t *testing.T, svc *Service, st historyMemorySelectionStore, repo domain.ContentHash) historyMemorySelectionFixture {
	t.Helper()
	ctx := context.Background()
	name := "ms" + strings.TrimPrefix(string(hh(t.Name()+time.Now().String())), "sha256:")[:20]
	owner := domain.User{ID: "dev:" + name, Username: name, Name: name, Email: name + "@example.test"}
	member := domain.User{ID: "dev:" + name + "m", Username: name + "m", Name: "member", Email: name + "m@example.test"}
	for _, u := range []domain.User{owner, member} {
		if err := st.UpsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	record := domain.Repository{ID: domain.NewID("ws_"), OwnerID: owner.ID, OwnerUsername: name, Name: "memory", Slug: "memory", CreatedAt: time.Now().UTC()}
	if err := st.CreateRepository(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(ctx, domain.Membership{RepositoryID: record.ID, UserID: member.ID, Role: domain.RoleMember}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	snapshot := func(label string) domain.ContentHash {
		doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: label}}}}}}
		raw, err := domain.CanonicalBytes(doc.CIR)
		if err != nil {
			t.Fatal(err)
		}
		doc.Hash = domain.HashContent(raw)
		if _, err = st.PutDoc(ctx, repo, doc); err != nil {
			t.Fatal(err)
		}
		if err = st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main", Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull}); err != nil {
			t.Fatal(err)
		}
		return doc.Hash
	}
	id, other := snapshot("selected"), snapshot("other")
	before := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: domain.LegacyContextBranchID(string(repo), "main"), Branch: "main", Kind: "position", Source: id, Target: id, SharedTarget: id, MemoryPinned: true, GitAfter: strings.Repeat("a", 40), WorktreeID: strings.Repeat("b", 32), CreatedAt: time.Now().UTC()}
	memory, err := st.PutMemory(ctx, repo, domain.MemoryDigest{SnapshotID: id, Summary: "first root"})
	if err != nil {
		t.Fatal(err)
	}
	after := before
	after.ID = strings.Repeat("2", 32)
	after.MemoryHash = memory
	after.MemorySelectionParent = before.ID
	after.CreatedAt = before.CreatedAt.Add(-time.Minute)
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: before.BranchID, Target: id}
	if err = st.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	return historyMemorySelectionFixture{svc, st, inbound.WithRepositoryActor(ctx, member.ID), repo, record.ID, member.ID, before, after, other}
}

func TestHistoryMemorySelectionAcceptanceFS(t *testing.T) {
	runHistoryMemorySelectionAcceptance(t, func(t *testing.T) historyMemorySelectionFixture {
		svc, st := newFsckSvc(t)
		return newHistoryMemorySelectionFixture(t, svc, st, hh(t.Name()))
	})
}

func runHistoryMemorySelectionAcceptance(t *testing.T, makeFixture func(*testing.T) historyMemorySelectionFixture) {
	for _, mode := range []string{"exact", "missing predecessor", "different branch", "different identity", "different alias", "different worktree", "different code", "different target", "unpinned predecessor", "nonempty predecessor", "predecessor owner", "attach predecessor", "non-position successor", "non-root digest", "foreign digest owner", "contradictory owner", "corrupt digest hash", "legacy BindingParent"} {
		t.Run(mode, func(t *testing.T) {
			f := makeFixture(t)
			before, after := f.before, f.after
			switch mode {
			case "missing predecessor":
				after.MemorySelectionParent = strings.Repeat("9", 32)
			case "different branch":
				after.Branch = "another"
			case "different identity":
				after.BranchID = "another-identity"
			case "different alias":
				after.LocalBranch = "another-alias"
			case "different worktree":
				after.WorktreeID = strings.Repeat("c", 32)
			case "different code":
				after.GitAfter = strings.Repeat("c", 40)
			case "different target":
				after.Source = f.other
				after.Target = f.other
				var err error
				after.MemoryHash, err = f.st.PutMemory(f.ctx, f.repo, domain.MemoryDigest{SnapshotID: f.other, Summary: "other root"})
				if err != nil {
					t.Fatal(err)
				}
			case "unpinned predecessor":
				before.MemoryPinned = false
			case "nonempty predecessor":
				before.MemoryHash = after.MemoryHash
			case "predecessor owner":
				before.MemorySource = before.Target
			case "attach predecessor":
				before.Kind = "attach"
			case "non-position successor":
				after.Kind = "advance"
			case "non-root digest":
				var err error
				after.MemoryHash, err = f.st.PutMemory(f.ctx, f.repo, domain.MemoryDigest{SnapshotID: after.Target, PreviousMemoryHash: after.MemoryHash, Summary: "later"})
				if err != nil {
					t.Fatal(err)
				}
			case "foreign digest owner":
				var err error
				after.MemoryHash, err = f.st.PutMemory(f.ctx, f.repo, domain.MemoryDigest{SnapshotID: f.other, Summary: "foreign root"})
				if err != nil {
					t.Fatal(err)
				}
				after.MemorySource = f.other
			case "contradictory owner":
				after.MemorySource = f.other
			case "legacy BindingParent":
				before.MemoryPinned = false
				after.MemorySelectionParent = ""
				after.BindingParent = before.ID
				after.WorktreeID = strings.Repeat("c", 32)
			}
			if err := f.svc.RecordHistory(f.ctx, before); err != nil {
				t.Fatal("predecessor setup", err)
			}
			if mode == "corrupt digest hash" {
				f.svc.blobs = &historyMemorySelectionCorruptBlob{BlobStore: f.svc.blobs}
			}
			refsBefore, err := f.st.ListRefs(f.ctx, f.repo)
			if err != nil {
				t.Fatal(err)
			}
			err = f.svc.RecordHistory(f.ctx, after)
			wantSuccess := mode == "exact" || mode == "legacy BindingParent"
			if wantSuccess && err != nil {
				t.Fatal("valid event rejected", err)
			}
			if !wantSuccess && err == nil {
				t.Fatal("invalid memory-selection dependency accepted")
			}
			events, listErr := f.st.ListHistoryEvents(f.ctx, f.repo)
			if listErr != nil {
				t.Fatal(listErr)
			}
			var gotBefore, gotAfter *domain.HistoryEvent
			for i := range events {
				if events[i].ID == before.ID {
					gotBefore = &events[i]
				}
				if events[i].ID == after.ID {
					gotAfter = &events[i]
				}
			}
			if gotBefore == nil || !reflect.DeepEqual(*gotBefore, before) {
				t.Fatal("explicit predecessor changed")
			}
			if wantSuccess {
				if gotAfter == nil || !reflect.DeepEqual(*gotAfter, after) {
					t.Fatal("full new field/payload was not persisted")
				}
			} else if gotAfter != nil {
				t.Fatal("rejected dependency persisted")
			}
			refsAfter, listErr := f.st.ListRefs(f.ctx, f.repo)
			if listErr != nil {
				t.Fatal(listErr)
			}
			// Accepted ordinary history adds retention tags, but cannot move branches.
			// Rejection must leave the complete ref set unchanged.
			if wantSuccess {
				refsBefore = historyMemorySelectionBranches(refsBefore)
				refsAfter = historyMemorySelectionBranches(refsAfter)
			}
			if !reflect.DeepEqual(refsBefore, refsAfter) {
				t.Fatal("unexpected ref mutation")
			}
		})
	}
	t.Run("exact replay after later branch state", func(t *testing.T) {
		f := makeFixture(t)
		for _, e := range []domain.HistoryEvent{f.before, f.after} {
			if err := f.svc.RecordHistory(f.ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		ref, err := f.st.GetRef(f.ctx, f.repo, domain.RefBranch, "main")
		if err != nil {
			t.Fatal(err)
		}
		old := ref.Target
		ref.Target = f.other
		if err = f.st.CompareAndSwapRef(f.ctx, f.repo, ref, old); err != nil {
			t.Fatal(err)
		}
		prior, err := f.st.ListHistoryEvents(f.ctx, f.repo)
		if err != nil {
			t.Fatal(err)
		}
		// Exact replay remains an immutable acknowledgement, not a new proof read.
		f.svc.blobs = &historyMemorySelectionCorruptBlob{BlobStore: f.svc.blobs}
		if err = f.svc.RecordHistory(f.ctx, f.after); err != nil {
			t.Fatal("exact replay rejected", err)
		}
		changed := f.after
		changed.MemorySelectionParent = strings.Repeat("9", 32)
		if err = f.svc.RecordHistory(f.ctx, changed); !errors.Is(err, domain.ErrRefConflict) {
			t.Fatalf("immutable duplicate replaced/incorrect error: %v", err)
		}
		current, err := f.st.GetRef(f.ctx, f.repo, domain.RefBranch, "main")
		if err != nil || current != ref {
			t.Fatal("replay retargeted branch", err)
		}
		events, err := f.st.ListHistoryEvents(f.ctx, f.repo)
		if err != nil || !reflect.DeepEqual(events, prior) {
			t.Fatal("replay changed history", err)
		}
	})
}

type historyMemorySelectionCorruptBlob struct{ outbound.BlobStore }

func (s *historyMemorySelectionCorruptBlob) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	m, err := s.BlobStore.GetMemory(ctx, repo, hash)
	m.Summary += " altered after verified read"
	return m, err
}

func historyMemorySelectionBranches(refs []domain.Ref) []domain.Ref {
	out := []domain.Ref{}
	for _, ref := range refs {
		if ref.Kind == domain.RefBranch {
			out = append(out, ref)
		}
	}
	return out
}
