package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// These projections test port presence, not transaction semantics. No embedded
// transaction method is called; actual restart/publication uses PostgreSQL.
type a11Metadata struct {
	outbound.MetadataStore
	outbound.RepositoryDocumentIdentityStore
	outbound.RepositoryTransactions
}

type a11Blobs struct {
	outbound.BlobStore
	outbound.DocJobStore
	outbound.StoredDocVerifier
	outbound.VerifiedDocReader
	outbound.ConversationChunkReader
}

func TestRootStartupCapabilityMatrix(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	meta := a11Metadata{MetadataStore: st, RepositoryDocumentIdentityStore: st}
	blobs := a11Blobs{st, st, st, st, st}
	for _, tc := range []struct {
		name       string
		meta       outbound.MetadataStore
		blobs      outbound.BlobStore
		readable   bool
		admissible bool
	}{
		{"complete", meta, blobs, true, true},
		{"missing-identity-store", struct {
			outbound.MetadataStore
			outbound.RepositoryTransactions
		}{st, nil}, blobs, false, false},
		{"development-no-transactions", st, blobs, true, false},
		{"missing-job-publication", meta, struct {
			outbound.BlobStore
			outbound.StoredDocVerifier
			outbound.VerifiedDocReader
			outbound.ConversationChunkReader
		}{st, st, st, st}, false, false},
		{"missing-stored-verifier", meta, struct {
			outbound.BlobStore
			outbound.DocJobStore
			outbound.VerifiedDocReader
			outbound.ConversationChunkReader
		}{st, st, st, st}, false, false},
		{"missing-verified-root-reader", meta, struct {
			outbound.BlobStore
			outbound.DocJobStore
			outbound.StoredDocVerifier
			outbound.ConversationChunkReader
		}{st, st, st, st}, false, false},
		{"missing-bounded-chunk-reader", meta, struct {
			outbound.BlobStore
			outbound.DocJobStore
			outbound.StoredDocVerifier
			outbound.VerifiedDocReader
		}{st, st, st, st}, false, false},
		{"nil-metadata", nil, blobs, false, false},
		{"nil-blobs", meta, nil, false, false},
		{"typed-nil-metadata", (*a11Metadata)(nil), blobs, false, false},
		{"typed-nil-blobs", meta, (*a11Blobs)(nil), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewService(tc.meta, tc.blobs, nil, nil, nil)
			want := []domain.DocumentIdentity{domain.DocumentIdentityLegacy}
			if conversationRootReleaseReady && tc.readable {
				want = append(want, domain.DocumentIdentityRootV1)
			}
			check := func() {
				t.Helper()
				if got := svc.DocumentIdentitiesSupported(); !reflect.DeepEqual(got, want) {
					t.Errorf("advertisement=%v want=%v", got, want)
				}
			}
			check()
			if err := svc.ConfigureConversationRootPublication(false); err != nil || svc.RootPublicationEnabled() {
				t.Fatal("default-off startup changed", err)
			}
			err := svc.ConfigureConversationRootPublication(true)
			if conversationRootReleaseReady && tc.admissible {
				if err != nil || !svc.RootPublicationEnabled() {
					t.Fatal("complete ready adapter did not admit", err)
				}
			} else if !errors.Is(err, domain.ErrRootPublicationDisabled) || svc.RootPublicationEnabled() {
				t.Errorf("incomplete/unready adapter admitted: enabled=%v err=%v", svc.RootPublicationEnabled(), err)
			}
			check()
			if err := svc.ConfigureConversationRootPublication(false); err != nil || svc.RootPublicationEnabled() {
				t.Fatal("could not disable new admission", err)
			}
			check()
			ids := svc.DocumentIdentitiesSupported()
			ids[0] = "unknown"
			check()
			worker := svc.DocumentIdentityWorkerContext(inbound.WithRepositoryActor(context.Background(), "same-actor"))
			if id, system := inbound.RepositoryActor(worker); id != "same-actor" || system || !reflect.DeepEqual(inbound.DocumentIdentities(worker), want) {
				t.Fatal("worker declaration changed actor or claimed missing capabilities")
			}
		})
	}
}

func TestRootStartupLegacyReadWithAdmissionOff(t *testing.T) {
	svc, st := newFsckSvc(t)
	repo := hh(t.Name())
	bindCommitTestRepo(t, st, repo)
	doc := putHistoryDoc(t, st, repo, historyEnvelope(), historyMessage(domain.RoleUser, "legacy still readable"))
	if err := svc.ConfigureConversationRootPublication(false); err != nil {
		t.Fatal(err)
	}
	page, err := svc.ReadDocEvents(systemTestContext(), repo, doc.Hash, "", 0, 1)
	if err != nil || len(page.Events) != 1 || !reflect.DeepEqual(page.Events[0], doc.CIR.Events[0]) {
		t.Fatal("legacy read changed", page, err)
	}
	if svc.RootPublicationEnabled() {
		t.Fatal("development adapter enabled root admission")
	}
	if hasDocumentIdentity(svc.DocumentIdentitiesSupported(), domain.DocumentIdentityRootV1) != conversationRootReleaseReady {
		t.Fatal("complete development readers lost binary support with admission off")
	}
}
