package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

func conversationManifestEnvelope(raw []byte) (string, error) {
	var envelope CIREnvelope
	if json.Unmarshal(raw, &envelope) != nil || (envelope.CIRVersion != CIRVersionV1 && envelope.CIRVersion != CIRVersionV2) {
		return "", ErrConversationManifest
	}
	canonical, err := canonicalJSON(envelope)
	if err != nil || !bytes.Equal(raw, canonical) {
		return "", ErrConversationManifest
	}
	return envelope.CIRVersion, nil
}

func conversationManifestEvent(ctx context.Context, version string, raw []byte) (int, error) {
	// A fresh verifier delegates to the existing strict typed event check. Its
	// pending event proof is discarded, never admitted or reused across calls.
	var verifier CanonicalDocVerifier
	var pending []canonicalEventProof
	return verifier.event(ctx, version, raw, &pending)
}
