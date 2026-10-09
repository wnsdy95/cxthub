package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

func conversationManifestEnvelope(raw []byte) (string, error) {
	var envelope Envelope
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
	// Mirror CanonicalDocVerifier's typed event check in the independent module.
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var event Event
	if err := json.Unmarshal(raw, &event); err != nil {
		return 0, ErrConversationManifest
	}
	doc := CIRDocument{Envelope: Envelope{CIRVersion: version}, Events: []Event{event}}
	if err := ValidateCIRVersion(doc); err != nil {
		return 0, err
	}
	event = canonicalEvents(doc.Events)[0]
	canonical, err := canonicalDecodedEventJSON(event)
	if err != nil || !bytes.Equal(canonical, raw) {
		return 0, ErrConversationManifest
	}
	return event.Seq, ctx.Err()
}
