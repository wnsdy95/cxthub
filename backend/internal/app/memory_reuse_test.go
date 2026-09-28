package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestMemoryReuse(t *testing.T) {
	svc, _ := newFsckSvc(t)
	repo := hh(t.Name())
	if _, err := svc.meta.PutRepo(systemTestContext(), domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	testMemoryReuse(t, svc, repo)
}

func reuseSnapshot(t *testing.T, svc *Service, repo domain.ContentHash, text string) domain.ContentHash {
	t.Helper()
	ctx := systemTestContext()
	cir := pendingGCCIR(domain.ProviderCodex, string(repo)+text)
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.HashContent(raw)
	if _, err := svc.blobs.PutDoc(ctx, repo, domain.SessionDoc{Hash: id, CIR: cir}); err != nil {
		t.Fatal(err)
	}
	if err := svc.meta.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	return id
}

func testMemoryReuse(t *testing.T, svc *Service, repo domain.ContentHash) {
	t.Helper()
	ctx := systemTestContext()
	for _, typed := range []bool{false, true} {
		name := "legacy"
		if typed {
			name = "typed"
		}
		t.Run(name, func(t *testing.T) {
			source := reuseSnapshot(t, svc, repo, name+"/source")
			target := reuseSnapshot(t, svc, repo, name+"/target")
			base := domain.MemoryDigest{SnapshotID: source, Provider: domain.ProviderClaude, Summary: strings.Repeat("preserve immutable selected memory ", 5000), KeyFacts: []string{"same facts"}, OpenTasks: []string{}, Fragments: []domain.MemoryFragment{{SourceSnapshot: source, Summary: "provenance"}}, GraftCoverage: &domain.MemoryGraftCoverage{ProjectionVersion: 1}}
			if typed {
				base.ClaimsVersion = 1
				base.Fragments[0].Claims = []domain.MemoryClaim{{Kind: "rationale", Text: "original decision"}}
			}
			baseHash, err := svc.PutMemoryDigestCAS(ctx, repo, base)
			if err != nil {
				t.Fatal(err)
			}
			want := base
			want.SnapshotID, want.Provider = target, domain.ProviderCodex
			hash, err := domain.MemoryDigestHash(want)
			if err != nil {
				t.Fatal(err)
			}
			in := inbound.MemoryReuse{Version: 1, BaseHash: baseHash, MemoryHash: hash, Provider: want.Provider}
			bad := in
			bad.MemoryHash = hh("incorrect expected digest")
			if _, err := svc.ReuseMemoryDigest(ctx, repo, target, bad); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatalf("wrong hash: %v", err)
			}
			bad = in
			bad.Version = 2
			if _, err := svc.ReuseMemoryDigest(ctx, repo, target, bad); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("unknown version: %v", err)
			}
			bad = in
			bad.BaseHash = hh("missing base")
			if _, err := svc.ReuseMemoryDigest(ctx, repo, target, bad); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("missing base: %v", err)
			}
			if got, _ := svc.meta.GetSnapshot(ctx, repo, target); got.MemoryHash != "" {
				t.Fatal("failed validation changed pointer")
			}
			for i := 0; i < 2; i++ {
				if got, err := svc.ReuseMemoryDigest(ctx, repo, target, in); err != nil || got != hash {
					t.Fatalf("reuse/retry: %s %v", got, err)
				}
			}
			got, err := svc.GetMemoryDigest(ctx, repo, target)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("digest identity/provenance changed: %v", err)
			}
			original, err := svc.GetMemoryDigest(ctx, repo, source)
			if err != nil || !reflect.DeepEqual(original, base) {
				t.Fatal("source mutated")
			}
			// The source remains immutable after a new attachment on its snapshot.
			changed := base
			changed.PreviousMemoryHash = baseHash
			changed.Summary = "later memory"
			if _, err := svc.PutMemoryDigestCAS(ctx, repo, changed); err != nil {
				t.Fatal(err)
			}
			next := want
			next.PreviousMemoryHash = hash
			next.Provider = domain.ProviderClaude
			nextHash, _ := domain.MemoryDigestHash(next)
			in.MemoryHash, in.PreviousMemoryHash, in.Provider = nextHash, hash, next.Provider
			if _, err := svc.ReuseMemoryDigest(ctx, repo, target, in); err != nil {
				t.Fatal(err)
			}
			in.MemoryHash, in.PreviousMemoryHash, in.Provider = hash, "", want.Provider
			if _, err := svc.ReuseMemoryDigest(ctx, repo, target, in); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("stale pointer: %v", err)
			}
			if got, _ := svc.meta.GetSnapshot(ctx, repo, target); got.MemoryHash != nextHash {
				t.Fatal("stale retry rewound target")
			}
			foreign := hh(string(repo) + name + "foreign")
			if _, err := svc.meta.PutRepo(ctx, domain.Repo{ID: foreign}); err != nil {
				t.Fatal(err)
			}
			foreignTarget := reuseSnapshot(t, svc, foreign, "target")
			if _, err := svc.ReuseMemoryDigest(ctx, foreign, foreignTarget, in); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("cross-repo reuse: %v", err)
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := svc.ReuseMemoryDigest(cancelled, repo, target, in); err == nil {
				t.Fatal("cancelled request succeeded")
			}
		})
	}
}

type corruptReuseBase struct{ outbound.BlobStore }

func (s corruptReuseBase) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	d, err := s.BlobStore.GetMemory(ctx, repo, hash)
	d.Summary += " corrupted"
	return d, err
}

func TestMemoryReuseRejectsCorruptBase(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx, repo := systemTestContext(), hh(t.Name())
	source := reuseSnapshot(t, svc, repo, "source")
	target := reuseSnapshot(t, svc, repo, "target")
	d := domain.MemoryDigest{SnapshotID: source, Summary: "original"}
	base, err := svc.PutMemoryDigestCAS(ctx, repo, d)
	if err != nil {
		t.Fatal(err)
	}
	d.SnapshotID = target
	hash, _ := domain.MemoryDigestHash(d)
	svc.blobs = corruptReuseBase{st}
	if _, err := svc.ReuseMemoryDigest(ctx, repo, target, inbound.MemoryReuse{Version: 1, BaseHash: base, MemoryHash: hash}); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("corrupted base accepted: %v", err)
	}
	if got, _ := st.GetSnapshot(ctx, repo, target); got.MemoryHash != "" {
		t.Fatal("corruption changed pointer")
	}
}
