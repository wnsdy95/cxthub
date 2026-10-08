package store

import (
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestSnapshotRefsRejectUnknownDocumentIdentity(t *testing.T) {
	snap := domain.Snapshot{ID: domain.HashContent([]byte("snapshot")), DocHash: domain.HashContent([]byte("snapshot")), RepoID: domain.HashContent([]byte("repo"))}
	for _, identity := range []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1} {
		snap.DocIdentity = identity
		if err := validateSnapshotRefs(snap); err != nil {
			t.Fatal("known metadata identity", err)
		}
	}
	snap.DocIdentity = "unsupported-future-identity"
	if err := validateSnapshotRefs(snap); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		t.Fatal("unknown metadata identity accepted", err)
	}
}
