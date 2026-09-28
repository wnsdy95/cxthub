package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

// CaptureSupersedes proves complete event-prefix containment for one native
// session. Proofs never replace hashing the currently owned bytes. Envelope
// changes (including branch/worktree moves) do not change session identity.
func CaptureSupersedes(ctx context.Context, previous VerifiedDocReference, previousBody []byte, next VerifiedDocReference, nextBody []byte, provider ProviderKind, session string) (bool, error) {
	oldEnvelope, oldEvents, err := captureStream(ctx, previous, previousBody)
	if err != nil {
		return false, err
	}
	nextEnvelope, nextEvents, err := captureStream(ctx, next, nextBody)
	if err != nil {
		return false, err
	}
	if provider == "" || session == "" || oldEnvelope.SourceProvider != provider || nextEnvelope.SourceProvider != provider ||
		oldEnvelope.SessionOriginID != session || nextEnvelope.SessionOriginID != session {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return bytes.HasPrefix(nextEvents, oldEvents) && (len(oldEvents) == 0 || len(oldEvents) == len(nextEvents) || nextEvents[len(oldEvents)] == ','), nil
}

func captureStream(ctx context.Context, proof VerifiedDocReference, body []byte) (CIREnvelope, []byte, error) {
	var env CIREnvelope
	if err := ctx.Err(); err != nil {
		return env, nil, err
	}
	if !proof.Valid() {
		return env, nil, ErrIntegrity
	}
	if HashContent(body) != proof.Hash() {
		// Legacy storage can be noncanonical. Reproduce its canonical content
		// address instead of trusting a proof issued for another representation.
		var cir CIRDocument
		if json.Unmarshal(body, &cir) != nil {
			return env, nil, ErrIntegrity
		}
		var err error
		body, err = ValidatedSessionDocBytes(SessionDoc{Hash: proof.Hash(), CIR: cir})
		if err != nil {
			return env, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return env, nil, err
	}
	// A valid schema proof and the exact canonical hash establish framing and
	// event validity. Decode only the small envelope; do not allocate every event
	// or scan a large transcript again merely to locate its array boundaries.
	const prefix = `{"envelope":`
	const middle = `,"events":[`
	if !bytes.HasPrefix(body, []byte(prefix)) || !bytes.HasSuffix(body, []byte(`]}`)) {
		return env, nil, ErrIntegrity
	}
	dec := json.NewDecoder(bytes.NewReader(body[len(prefix):]))
	if dec.Decode(&env) != nil {
		return env, nil, ErrIntegrity
	}
	offset := len(prefix) + int(dec.InputOffset())
	if offset+len(middle) > len(body)-2 || !bytes.HasPrefix(body[offset:], []byte(middle)) {
		return env, nil, ErrIntegrity
	}
	return env, body[offset+len(middle) : len(body)-2], nil
}
