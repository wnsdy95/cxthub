package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type personalRootBackend struct {
	*personalBackendFixture
	used      int64
	remaining int64
}

func (f *personalRootBackend) FetchPersonalWorkDocumentReference(ctx context.Context, repo string, ref domain.DocumentRef, remaining int64) (domain.SessionDoc, int64, error) {
	f.remaining = remaining
	doc, used, err := f.FetchPersonalWorkDocument(ctx, repo, ref.Hash, remaining)
	if f.used != 0 {
		used = f.used
	}
	return doc, used, err
}

func rootPersonalFixture(t *testing.T) (string, personalWorkArtifact, *personalRootBackend) {
	t.Helper()
	cwd, a, f := personalFixture(t)
	manifest, _, err := domain.ConversationManifestForCIR(f.doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	f.doc.Hash, f.doc.Identity = hash, domain.DocumentIdentityRootV1
	f.snap.ID, f.snap.DocHash, f.snap.DocIdentity = hash, hash, f.doc.Identity
	for i := range a.State.Sources {
		a.State.Sources[i].SnapshotID, a.State.Sources[i].DocHash, a.State.Sources[i].DocIdentity = hash, hash, f.doc.Identity
	}
	for i := range a.State.Constraints {
		a.State.Constraints[i].Source.SnapshotID, a.State.Constraints[i].Source.DocHash, a.State.Constraints[i].Source.DocIdentity = hash, hash, f.doc.Identity
	}
	return cwd, a, &personalRootBackend{personalBackendFixture: f}
}

func TestPersonalWorkExplicitRootPreservesScopeAndExactConstraints(t *testing.T) {
	cwd, a, f := rootPersonalFixture(t)
	path := writePersonalFixture(t, cwd, a)
	r, scope, err := importPersonalWork(context.Background(), f, a.RepositoryID, cwd, path, domain.PersonalWorkScope{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.ReadPersonalWork(context.Background(), a.RepositoryID, scope)
	if err != nil || !reflect.DeepEqual(got, a.State) {
		t.Fatalf("changed state: %+v / %v", got, err)
	}
	if f.reads != 3 || f.remaining != 8<<20 {
		t.Fatalf("validation/budget: reads=%d remaining=%d", f.reads, f.remaining)
	}
	// Every new import must revalidate current bytes, not trust a prior root read.
	f.doc.CIR.Events[0].Blocks[0].Text = "Deploy now."
	if r, _, err := importPersonalWork(context.Background(), f, a.RepositoryID, cwd, path, domain.PersonalWorkScope{}); err == nil || r != nil {
		t.Fatal("accepted current corruption after valid read")
	}
}

func TestPersonalWorkRootRejectsRelabelingAndPreservesLimits(t *testing.T) {
	for name, mutate := range map[string]func(*personalWorkArtifact, *personalRootBackend){
		"source identity stripped":     func(a *personalWorkArtifact, f *personalRootBackend) { a.State.Sources[0].DocIdentity = "" },
		"snapshot identity stripped":   func(a *personalWorkArtifact, f *personalRootBackend) { f.snap.DocIdentity = "" },
		"body identity stripped":       func(a *personalWorkArtifact, f *personalRootBackend) { f.doc.Identity = "" },
		"constraint identity stripped": func(a *personalWorkArtifact, f *personalRootBackend) { a.State.Constraints[0].Source.DocIdentity = "" },
		"wrong author":                 func(a *personalWorkArtifact, f *personalRootBackend) { f.snap.Author.Email = "bob@example.test" },
		"wrong session":                func(a *personalWorkArtifact, f *personalRootBackend) { f.snap.SessionID = "other" },
		"unknown identity":             func(a *personalWorkArtifact, f *personalRootBackend) { a.State.Sources[0].DocIdentity = "unknown" },
		"over byte budget":             func(a *personalWorkArtifact, f *personalRootBackend) { f.used = (8 << 20) + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			cwd, a, f := rootPersonalFixture(t)
			mutate(&a, f)
			if name == "unknown identity" {
				if err := validatePersonalWorkSources(context.Background(), f, a.RepositoryID, f.email, a.State); err == nil || f.reads != 0 {
					t.Fatal("unknown identity reached backend")
				}
				return
			}
			path := writePersonalFixture(t, cwd, a)
			if r, _, err := importPersonalWork(context.Background(), f, a.RepositoryID, cwd, path, domain.PersonalWorkScope{}); err == nil || r != nil {
				t.Fatal("accepted unbound or over-budget root")
			}
		})
	}
	t.Run("hash-only reader", func(t *testing.T) {
		cwd, a, f := rootPersonalFixture(t)
		path := writePersonalFixture(t, cwd, a)
		if r, _, err := importPersonalWork(context.Background(), f.personalBackendFixture, a.RepositoryID, cwd, path, domain.PersonalWorkScope{}); err == nil || r != nil || f.reads != 2 {
			t.Fatalf("root reached legacy reader: reads=%d err=%v", f.reads, err)
		}
	})
}
