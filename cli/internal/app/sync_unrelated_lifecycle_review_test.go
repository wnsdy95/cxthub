package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestReviewServiceSelectedMainSurvivesForeignLifecycleDisagreement(t *testing.T) {
	for _, collision := range []bool{false, true} {
		t.Run(map[bool]string{false: "protected_empty_modern_first", true: "unrelated_conflict"}[collision], func(t *testing.T) {
			f := &strictPublicationFixture{t: t, ctx: context.Background(), root: t.TempDir(), repo: string(domain.HashContent([]byte(t.Name())))}
			f.st = storage.NewFileStore(f.root)
			f.a = publicationSnapshot(t, f.st, f.repo, "main", nil, nil)
			f.x = publicationSnapshot(t, f.st, f.repo, "foreign", nil, nil)
			main := f.event(1, "birth", "main-R", "main", f.a)
			local := f.event(3, "birth", "local-X", "web-fork-x", f.x)
			rename := f.event(4, "rename", local.BranchID, "web-fork-renamed", f.x)
			rename.PreviousBranch, rename.BindingParent = local.Branch, local.ID
			archive := f.event(5, "archive", local.BranchID, rename.Branch, f.x)
			archive.BindingParent = rename.ID
			for _, e := range []domain.HistoryEvent{main, local, rename, archive} {
				f.put(e)
			}
			if err := f.st.PutRef(f.ctx, domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "main", BranchID: main.BranchID, Target: f.a}); err != nil {
				t.Fatal(err)
			}
			server := f.event(2, "birth", "server-X", "web-fork-x", f.x)
			f.r = &strictPublicationRemote{protocol: 1, accepted: []domain.HistoryEvent{server}, memoryObjects: map[domain.ContentHash]domain.MemoryDigest{}, attachments: map[domain.ContentHash]domain.ContentHash{}}
			if !collision {
				f.r.accepted = nil
			} else {
				f.r.setRef(domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: server.Branch, BranchID: server.BranchID, Target: f.x})
			}
			f.svc = newTestSyncService(f.st, f.r, pushOrderGit{repo: domain.Repo{ID: f.repo, LocalPath: f.root}})
			_, err := f.svc.Push(f.ctx, inbound.SyncInput{Cwd: f.root, Publication: &domain.PublicationScope{Branches: []domain.PublicationBranch{{Branch: "main", BranchID: main.BranchID}}}})
			if err != nil {
				t.Fatalf("real service/FileStore selected main blocked: %v; transport effects=%v", err, f.r.calls)
			}
			if len(f.r.writes) != 1 || f.r.writes[0].Name != "main" || f.r.writes[0].BranchID != main.BranchID {
				t.Fatalf("wrong final ref: %+v", f.r.writes)
			}
			for _, e := range f.r.sent {
				if e.BranchID != main.BranchID {
					t.Fatalf("foreign history applied: %+v", e)
				}
			}
			for _, id := range f.r.snaps {
				if id == f.x {
					t.Fatal("unrelated foreign object uploaded")
				}
			}
		})
	}
}
