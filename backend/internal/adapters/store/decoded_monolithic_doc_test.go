package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDecodedMonolithicVerificationDoesNotDecodeCompressionAgain(test *testing.T) {
	ctx := context.Background()
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex}, Events: []domain.CIREvent{
		{Kind: domain.EventMessage, Seq: 4, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "verified monolithic payload"}}},
	}}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		test.Fatal(err)
	}
	hash := domain.HashContent(raw)
	proof, err := verifyLegacyDecodedMonolithicDoc(ctx, hash, raw)
	if err != nil || !proof.Valid() || !bytes.Equal(proof.Bytes(), raw) {
		test.Fatal("decoded monolith did not retain verified archival content", err)
	}
	plan, ok := domain.PlanDocChunksV1(raw)
	if !ok {
		test.Fatal("missing v1 fixture")
	}
	manifest, err := json.Marshal(plan.Manifest)
	if err != nil {
		test.Fatal(err)
	}
	for _, body := range [][]byte{docCompress(raw), docCompress(manifest), docCompress(bytes.Repeat([]byte{'x'}, maxLegacyReadBytes+1))} {
		decoded, err := boundedLegacyObject(ctx, docCompress(body), maxLegacyReadBytes)
		if err != nil || !bytes.Equal(decoded, body) {
			test.Fatal("outer bounded decoder did not stop after one compression layer", err)
		}
		if verified, err := verifyLegacyDecodedMonolithicDoc(ctx, hash, decoded); !errors.Is(err, domain.ErrIntegrity) || verified.Valid() {
			test.Fatal("monolithic verifier followed a second compression layer", err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if verified, err := verifyLegacyDecodedMonolithicDoc(canceled, hash, raw); !errors.Is(err, context.Canceled) || verified.Valid() {
		test.Fatal("monolithic verification ignored cancellation", err)
	}
}
