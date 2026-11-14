package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type publicationSourceProbe struct {
	proof          domain.VerifiedDocReference
	found          bool
	attestFailure  error
	verifyFailure  error
	attested, read int
}

func (probe *publicationSourceProbe) FinalizedDocReference(context.Context, domain.ContentHash, domain.ContentHash) (domain.VerifiedDocReference, bool, error) {
	probe.attested++
	return probe.proof, probe.found, probe.attestFailure
}

func (probe *publicationSourceProbe) VerifyStoredDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.VerifiedDocReference, error) {
	probe.read++
	return probe.proof, probe.verifyFailure
}

func TestPublicationSourcesReadDocumentBytesOnlyWithoutAFinalizationRecord(t *testing.T) {
	raw := []byte(`{"envelope":{"cir_version":"1"},"events":[]}`)
	canonical, err := domain.CanonicalBytes(domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}, Events: []domain.CIREvent{}})
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(canonical)
	proof, err := domain.VerifyStoredDocBytes(hash, raw)
	if err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte("repository"))
	snapshot := domain.Snapshot{ID: hash, DocHash: hash, RepoID: repo}
	other := domain.HashContent([]byte("another document"))
	failure := errors.New("store unavailable")
	for _, test := range []struct {
		name           string
		probe          publicationSourceProbe
		snapshot       domain.Snapshot
		want           error
		attested, read int
	}{
		{name: "finalization record", probe: publicationSourceProbe{proof: proof, found: true}, snapshot: snapshot, attested: 1},
		{name: "no record", probe: publicationSourceProbe{proof: proof}, snapshot: snapshot, attested: 1, read: 1},
		{name: "no record and damaged bytes", probe: publicationSourceProbe{verifyFailure: domain.ErrIntegrity}, snapshot: snapshot, want: domain.ErrIntegrity, attested: 1, read: 1},
		{name: "record lookup fails", probe: publicationSourceProbe{proof: proof, found: true, attestFailure: failure}, snapshot: snapshot, want: failure, attested: 1},
		{name: "record for another document", probe: publicationSourceProbe{proof: proof, found: true}, snapshot: domain.Snapshot{ID: other, DocHash: other, RepoID: repo}, want: domain.ErrIntegrity, attested: 1},
		// A root snapshot whose stored document is a legacy document gets the
		// same refusal the byte verifier's legacy proof would give.
		{name: "root snapshot of a legacy record", probe: publicationSourceProbe{proof: proof, found: true},
			snapshot: domain.Snapshot{ID: hash, DocHash: hash, RepoID: repo, DocIdentity: domain.DocumentIdentityRootV1}, want: domain.ErrIntegrity, attested: 1},
		{name: "root snapshot without a record", probe: publicationSourceProbe{verifyFailure: failure},
			snapshot: domain.Snapshot{ID: hash, DocHash: hash, RepoID: repo, DocIdentity: domain.DocumentIdentityRootV1}, want: failure, attested: 1, read: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe := test.probe
			sources := &documentPublicationSources{stored: &probe, finalized: &probe}
			if err := sources.verifyReference(context.Background(), repo, test.snapshot); !errors.Is(err, test.want) || (test.want == nil && err != nil) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
			if probe.attested != test.attested || probe.read != test.read {
				t.Fatalf("attested=%d read=%d want %d and %d", probe.attested, probe.read, test.attested, test.read)
			}
		})
	}
	t.Run("store without attestation", func(t *testing.T) {
		probe := publicationSourceProbe{proof: proof, found: true}
		var absent *publicationSourceProbe
		sources := &documentPublicationSources{stored: &probe, finalized: absent}
		if err := sources.verifyReference(context.Background(), repo, snapshot); err != nil || probe.read != 1 || probe.attested != 0 {
			t.Fatalf("err=%v attested=%d read=%d", err, probe.attested, probe.read)
		}
	})
}
