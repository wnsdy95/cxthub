package app

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"testing"
)

func TestCaptureIdentityOrdinaryPathsUseBoundLocalPolicy(t *testing.T) {
	for _, operation := range []string{"save", "stage", "stash"} {
		for _, rootEnabled := range []bool{false, true} {
			name := operation + "/legacy"
			if rootEnabled {
				name = operation + "/root"
			}
			t.Run(name, func(t *testing.T) {
				f := newStagingFixture(t)
				ctx := context.Background()
				origin := "https://offline.example.test/team/repo"
				observed, err := remotecfg.Observe(ctx, f.root)
				if err != nil {
					t.Fatal(err)
				}
				if err := remotecfg.Replace(ctx, observed, remotecfg.Remotes{"origin": origin}); err != nil {
					t.Fatal(err)
				}
				if rootEnabled {
					observed, err = remotecfg.Observe(ctx, f.root)
					if err != nil {
						t.Fatal(err)
					}
					if err := remotecfg.SetCaptureDocumentIdentity(ctx, observed, domain.DocumentIdentityRootV1); err != nil {
						t.Fatal(err)
					}
				}
				f.git.repo.ID = remotecfg.RepoIDFor(origin)
				wrapped := remotecfg.Wrap(f.root, f.git)
				f.svc.git = wrapped
				f.svc.save.gitCtx = wrapped
				source := f.source(t, "capture-policy", "synthetic ordinary command capture")
				var ref domain.DocumentRef
				switch operation {
				case "save":
					out, err := f.svc.save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: source.Provider, SessionPath: source.Path})
					if err != nil {
						t.Fatal(err)
					}
					snap, err := f.store.GetSnapshot(ctx, out.SnapshotID)
					if err != nil {
						t.Fatal(err)
					}
					ref = snap.DocumentRef()
				case "stage":
					index, err := f.svc.Stage(ctx, inbound.StageInput{Cwd: f.root, Sessions: []inbound.StageSession{source}})
					if err != nil {
						t.Fatal(err)
					}
					ref = index.Entries[0].DocumentRef()
				case "stash":
					save := f.svc.save
					s := NewStashService(wrapped, save.captures, save.codecs, f.store, rootStashLoad{}, save.capture)
					out, err := s.Stash(ctx, inbound.StashInput{Cwd: f.root, Provider: source.Provider, SessionPath: source.Path})
					if err != nil {
						t.Fatal(err)
					}
					snap, err := f.store.GetSnapshot(ctx, out.StashID)
					if err != nil {
						t.Fatal(err)
					}
					ref = snap.DocumentRef()
				}
				want := domain.DocumentIdentityLegacy
				if rootEnabled {
					want = domain.DocumentIdentityRootV1
				}
				if ref.Identity != want {
					t.Fatal("ordinary capture missed policy", ref)
				}
				if _, err := f.store.GetDocReference(ctx, ref); err != nil {
					t.Fatal(err)
				}
				// Clearing a preference cannot relabel or remove an already captured root.
				observed, err = remotecfg.Observe(ctx, f.root)
				if err != nil {
					t.Fatal(err)
				}
				if err := remotecfg.SetCaptureDocumentIdentity(ctx, observed, domain.DocumentIdentityLegacy); err != nil {
					t.Fatal(err)
				}
				if got, err := f.store.GetDocReference(ctx, ref); err != nil || got.DocumentRef() != ref {
					t.Fatal("changed existing identity", err)
				}
			})
		}
	}
}
