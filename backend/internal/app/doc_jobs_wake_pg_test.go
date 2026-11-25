//go:build postgres

package app

import (
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// The document worker is woken when the job is committed, not while the
// transaction that stores it is open: a look before the commit finds nothing,
// and the upload would wait for the worker's next look after all.
func TestPGDocumentWorkerIsWokenAfterTheJobIsCommitted(t *testing.T) {
	svc, st, _ := collaborationPG(t)
	ctx := systemTestContext()
	now := time.Now().UTC()
	user := domain.User{ID: domain.NewID("user_"), Username: fmt.Sprintf("woken%d", now.UnixNano()), Name: "Woken", Email: "woken@example.test"}
	if err := st.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	namespace := domain.Namespace{ID: domain.NewID("ns_"), Slug: user.Username, Kind: domain.NamespaceUser, UserID: user.ID, CreatedAt: now}
	if err := st.CreateNamespace(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	record := domain.Repository{ID: domain.NewID("ws_"), Name: "Woken", OwnerID: user.ID, OwnerUsername: user.Username, OwnerNamespaceID: namespace.ID, Slug: "woken", CreatedAt: now}
	if err := st.CreateRepository(ctx, record); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(record.ID))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "woken after the commit"}}}}}
	raw, _ := domain.CanonicalBytes(cir)
	plan, _ := domain.PlanDocChunks(raw)
	if _, _, err := st.PutChunks(ctx, repo, plan.Bodies); err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(raw)
	expected, err := domain.NewDocFinalizationJob(repo, hash, plan.Manifest, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// What the woken worker would find: the job, read on another connection
	// than the one that stores it.
	woken, found := 0, error(nil)
	svc.documentJobStored = func() {
		woken++
		_, found = st.GetDocJob(ctx, repo, expected.ID)
	}
	accepted, err := svc.SubmitDocFinalization(ctx, repo, inbound.ChunkedDoc{Hash: hash, Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks})
	if err != nil || accepted.ID != expected.ID {
		t.Fatalf("submitted job %q, expected %q: %v", accepted.ID, expected.ID, err)
	}
	if woken != 1 || found != nil {
		t.Fatalf("the worker was woken %d times, and the job it would look for: %v", woken, found)
	}
	// A document that cannot be accepted stores no job and wakes nobody.
	if _, err := svc.SubmitDocFinalization(ctx, repo, inbound.ChunkedDoc{Hash: domain.HashContent([]byte("no such document")), Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: []domain.ContentHash{domain.HashContent([]byte("no such chunk"))}}); err == nil || woken != 1 {
		t.Fatalf("a document whose chunks are not there: %v, woken %d times", err, woken)
	}
}
