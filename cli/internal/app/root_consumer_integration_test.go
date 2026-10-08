package app

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// Exercise actual local commit/materialization boundaries, not just the shared
// explicit-reference helper. All provider files and stores are synthetic.
func TestStoredConversationConsumers(t *testing.T) {
	for _, identity := range []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1} {
		name := "legacy"
		if identity != "" {
			name = "root"
		}
		t.Run(name, func(t *testing.T) {
			f := newStagingFixture(t)
			ctx := context.Background()
			const text = "Synthetic decision: retain immutable conversation chunks."
			source := f.source(t, "consumer-source", text)
			saved, err := f.svc.save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: source.Provider, SessionPath: source.Path, DocIdentity: identity})
			if err != nil {
				t.Fatal(err)
			}
			snap, err := f.store.GetSnapshot(ctx, saved.SnapshotID)
			if err != nil || snap.DocIdentity != identity {
				t.Fatal(snap, err)
			}
			doc, err := f.store.GetDocReference(ctx, snap.DocumentRef())
			if err != nil {
				t.Fatal(err)
			}
			mem := NewMemorizeService(f.git, nil, nil, nil, memory.NewRuleDistiller(), f.store)
			memorized, err := mem.Memorize(ctx, inbound.MemorizeInput{Cwd: f.root, Ref: string(snap.ID), Provider: source.Provider})
			if err != nil {
				t.Fatalf("memorize through real store: %v", err)
			}
			attached, err := f.store.GetSnapshot(ctx, snap.ID)
			if err != nil || attached.MemoryHash == "" || attached.MemoryHash != memorized.MemoryHash {
				t.Fatal(attached, err)
			}
			seed := NewBranchSeedService(f.git, f.store, memory.NewRuleDistiller(), nil, nil, nil)
			load := newLoadSvc(f.store)
			for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
				out, err := load.Load(ctx, inbound.LoadInput{Ref: string(snap.ID), TargetProvider: provider, Mode: domain.FidelityFull, Cwd: f.root})
				if err != nil {
					t.Fatalf("%s load: %v", provider, err)
				}
				raw, err := os.ReadFile(out.WrittenPath)
				if err != nil || !strings.Contains(string(raw), text) {
					t.Fatalf("%s materialization lost source text: %v", provider, err)
				}
				outSeed, err := seed.Seed(ctx, inbound.SeedInput{Cwd: f.root, FromBranch: "main", NewBranch: "feature/" + string(provider), Provider: provider})
				if err != nil {
					t.Fatalf("%s seed: %v", provider, err)
				}
				seedSnap, err := f.store.GetSnapshot(ctx, outSeed.SnapshotID)
				if err != nil || seedSnap.MemoryHash == "" {
					t.Fatalf("%s seed did not inherit memory: %v", provider, err)
				}
				seedDoc, err := f.store.GetDocReference(ctx, seedSnap.DocumentRef())
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, event := range seedDoc.CIR.Events {
					for _, block := range event.Blocks {
						found = found || strings.Contains(block.Text, text)
					}
				}
				if !found {
					t.Fatalf("%s seed lost conversation", provider)
				}
			}
			if identity == domain.DocumentIdentityLegacy {
				return
			}
			manifest, _, err := domain.ConversationManifestForCIR(doc.CIR)
			if err != nil || len(manifest.Chunks) == 0 {
				t.Fatal(manifest, err)
			}
			if err := os.Remove(filepath.Join(f.root, ".cxt", "objects", "chunks", strings.TrimPrefix(string(manifest.Chunks[0].Hash), "sha256:"))); err != nil {
				t.Fatal(err)
			}
			beforeRefs, err := f.store.ListRefs(ctx, f.git.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			before, err := f.store.GetSnapshot(ctx, snap.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := mem.Memorize(ctx, inbound.MemorizeInput{Cwd: f.root, Ref: string(snap.ID)}); err == nil {
				t.Fatal("warm memorize accepted missing current chunk")
			}
			for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
				if _, err := load.Load(ctx, inbound.LoadInput{Ref: string(snap.ID), TargetProvider: provider, Mode: domain.FidelityFull, Cwd: f.root}); err == nil {
					t.Fatalf("%s warm load accepted missing chunk", provider)
				}
				if _, err := seed.Seed(ctx, inbound.SeedInput{Cwd: f.root, FromBranch: "main", NewBranch: "feature/invalid-" + string(provider), Provider: provider}); err == nil {
					t.Fatalf("%s warm seed accepted missing chunk", provider)
				}
			}
			after, err := f.store.GetSnapshot(ctx, snap.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("failure changed snapshot: %v", err)
			}
			afterRefs, err := f.store.ListRefs(ctx, f.git.repo.ID)
			if err != nil || !reflect.DeepEqual(beforeRefs, afterRefs) {
				t.Fatalf("failure changed refs: %v", err)
			}
		})
	}
}
