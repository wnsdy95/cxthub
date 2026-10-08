package store

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestFSSnapshotIdentityCannotBeRelabeled(t *testing.T) {
	for _, identity := range []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1} {
		s := NewFSStore(t.TempDir())
		ctx := context.Background()
		repo := domain.HashContent([]byte("synthetic repo"))
		hash := domain.HashContent([]byte("synthetic metadata " + identity))
		original := domain.Snapshot{ID: hash, RepoID: repo, DocHash: hash, DocIdentity: identity, Branch: domain.StashBranchLabel}
		if err := s.PutSnapshot(ctx, original); err != nil {
			t.Fatal(err)
		}
		if err := s.PutSnapshot(ctx, original); err != nil {
			t.Fatal(err)
		}
		changed := original
		changed.Branch = "main"
		if identity == domain.DocumentIdentityLegacy {
			changed.DocIdentity = domain.DocumentIdentityRootV1
		} else {
			changed.DocIdentity = domain.DocumentIdentityLegacy
		}
		if err := s.PutSnapshot(ctx, changed); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal("relabel accepted", err)
		}
		got, err := s.GetSnapshot(ctx, repo, hash)
		if err != nil || got.DocumentRef() != original.DocumentRef() || got.Branch != original.Branch {
			t.Fatal("relabel changed snapshot", got, err)
		}
	}
}
