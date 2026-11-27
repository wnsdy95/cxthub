package domain

import (
	"errors"
	"testing"
)

func TestDocRangeExternalEnvelopeRejectsInvalidReferenceShapes(test *testing.T) {
	identity := DocumentIdentityLegacy
	generation := DocRangeGeneration{Version: 6, SourceIdentity: &identity, Algorithm: 1,
		Hash: HashContent([]byte("original document")), EnvelopeRef: &DocRangeEnvelopeReference{
			Hash: HashContent([]byte("header")), Bytes: MaxConversationManifestBytes + 1}}
	if err := generation.Validate(generation.Hash); err != nil {
		test.Fatal(err)
	}
	for _, change := range []string{"inline", "small", "huge", "hash", "old-version", "identity", "missing"} {
		test.Run(change, func(test *testing.T) {
			candidate := generation
			reference := *generation.EnvelopeRef
			candidate.EnvelopeRef = &reference
			switch change {
			case "inline":
				candidate.Envelope = []byte(`{"cir_version":"2"}`)
			case "small":
				candidate.EnvelopeRef.Bytes = MaxConversationManifestBytes
			case "huge":
				candidate.EnvelopeRef.Bytes = MaxConversationDocumentBytes
			case "hash":
				candidate.EnvelopeRef.Hash = "invalid"
			case "old-version":
				candidate.Version = 5
			case "identity":
				candidate.SourceIdentity = nil
			case "missing":
				candidate.EnvelopeRef = nil
			}
			if err := candidate.Validate(candidate.Hash); !errors.Is(err, ErrIntegrity) {
				test.Fatal("invalid envelope reference was accepted", change, err)
			}
		})
	}
	verified := VerifiedDocRangeGeneration{generation: generation}
	copy := verified.Descriptor()
	copy.EnvelopeRef.Hash = HashContent([]byte("different header"))
	if verified.Descriptor().EnvelopeRef.Hash != generation.EnvelopeRef.Hash {
		test.Fatal("descriptor exposed a mutable envelope reference")
	}
}
