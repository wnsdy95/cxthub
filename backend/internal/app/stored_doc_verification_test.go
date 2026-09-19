package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type referenceOnlyBlobs struct {
	outbound.BlobStore
	proof             domain.VerifiedDocReference
	verified, decoded int
	fail              error
}

func (s *referenceOnlyBlobs) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	s.decoded++
	return domain.SessionDoc{}, errors.New("unexpected full archive decode")
}
func (s *referenceOnlyBlobs) VerifyStoredDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.VerifiedDocReference, error) {
	s.verified++
	return s.proof, s.fail
}
func TestHistoryAndMetadataUseStoredDocumentVerification(t *testing.T) {
	ctx := context.Background()
	svc, st := newFsckSvc(t)
	repo := hh(t.Name())
	bindCommitTestRepo(t, st, repo)
	id := prSnapshot(t, st, repo, "original")
	doc, err := st.GetDoc(ctx, repo, id)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := domain.VerifySessionDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	blobs := &referenceOnlyBlobs{BlobStore: st, proof: verified.Reference()}
	svc.blobs = blobs
	e := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "main", Branch: "main", Kind: "position", Source: id, Target: id, SharedTarget: id, CreatedAt: time.Now().UTC()}
	if err := svc.RecordHistory(ctx, e); err != nil {
		t.Fatal(err)
	}
	snap, err := st.GetSnapshot(ctx, repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Commit(ctx, inbound.CommitInput{RepoID: repo, Snapshots: []domain.Snapshot{snap}}); err != nil {
		t.Fatal(err)
	}
	if blobs.verified != 2 || blobs.decoded != 0 {
		t.Fatalf("metadata reparsed archive: verify=%d decode=%d", blobs.verified, blobs.decoded)
	}
	// A returned zero/wrong proof or current read failure is terminal, not a
	// reason to use stale successful evidence or fall back to a weaker read.
	for i, bad := range []struct {
		proof domain.VerifiedDocReference
		err   error
	}{{domain.VerifiedDocReference{}, nil}, {verified.Reference(), domain.ErrNotFound}} {
		blobs.proof, blobs.fail = bad.proof, bad.err
		e.ID = strings.Repeat(string(rune('2'+i)), 32)
		if err := svc.RecordHistory(ctx, e); err == nil {
			t.Fatal("accepted invalid/currently missing body")
		}
	}
	if blobs.decoded != 0 {
		t.Fatal("verification failure fell back to archive decoder")
	}
}
