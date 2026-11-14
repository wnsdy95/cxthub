package domain

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestVerifiedDocReferenceBindsIdentityAndOwnsNoMutableInput(t *testing.T) {
	raw := []byte(`{"envelope":{"cir_version":"1"},"events":[]}`)
	// Non-canonical legacy JSON needs the same canonical identity, not raw SHA.
	doc := SessionDoc{CIR: CIRDocument{Envelope: CIREnvelope{CIRVersion: "1"}, Events: []CIREvent{}}}
	canonical, err := CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = HashContent(canonical)
	p, err := VerifyStoredDocBytes(doc.Hash, raw)
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		raw[i] = 'x'
	}
	if !p.Matches(Snapshot{ID: doc.Hash, DocHash: doc.Hash}) {
		t.Fatal("caller changed proof")
	}
	other := HashContent([]byte("other"))
	if p.Matches(Snapshot{ID: other, DocHash: doc.Hash}) || p.Matches(Snapshot{ID: doc.Hash, DocHash: other}) || (VerifiedDocReference{}).Valid() {
		t.Fatal("unbound or zero proof accepted")
	}
	// Unknown data that would be dropped by typed decoding must not be accepted
	// under a hash calculated directly over its bytes.
	forged := bytes.Replace(canonical, []byte(`"events":[]`), []byte(`"events":[],"unknown":"field"`), 1)
	if _, err := VerifyStoredDocBytes(HashContent(forged), forged); err == nil {
		t.Fatal("raw hash bypassed canonical schema")
	}
}

func TestFinalizedDocReferenceNeedsAValidCompletedLegacyJob(t *testing.T) {
	canonical, err := CanonicalBytes(CIRDocument{Envelope: CIREnvelope{CIRVersion: "1"}, Events: []CIREvent{
		{Kind: EventMessage, Seq: 0, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "finalized"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := PlanDocChunks(canonical)
	if !ok {
		t.Fatal("no chunk plan")
	}
	repo, hash := HashContent([]byte("repository")), HashContent(canonical)
	job, err := NewDocFinalizationJob(repo, hash, plan.Manifest, time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"waiting", "running", "retrying", "rejected"} {
		pending := job
		pending.State = state
		if _, err := FinalizedDocReference(pending); !errors.Is(err, ErrValidation) {
			t.Fatalf("%s job attested a document: %v", state, err)
		}
	}
	job.State = "completed"
	proof, err := FinalizedDocReference(job)
	if err != nil || proof.DocumentRef() != (DocumentRef{Hash: hash, Identity: DocumentIdentityLegacy}) || !proof.Matches(Snapshot{ID: hash, DocHash: hash}) {
		t.Fatalf("completed job not attested: %+v %v", proof.DocumentRef(), err)
	}
	// The identity binds repository, hash and manifest: a record edited to name
	// another document is not a valid job.
	forged := job
	forged.DocHash = HashContent([]byte("another document"))
	if _, err := FinalizedDocReference(forged); err == nil {
		t.Fatal("job with a mismatching identity attested a document")
	}
	root := job
	root.DocIdentity = DocumentIdentityRootV1
	if _, err := FinalizedDocReference(root); err == nil {
		t.Fatal("root job attested a legacy reference")
	}
}
