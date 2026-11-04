package domain

import (
	"bytes"
	"encoding/json"
)

func decodeCanonicalCIREnvelope(raw []byte) (CIREnvelope, error) {
	var envelope CIREnvelope
	if json.Unmarshal(raw, &envelope) != nil {
		return CIREnvelope{}, ErrIntegrity
	}
	if err := ValidateCIRVersion(CIRDocument{Envelope: envelope}); err != nil {
		return CIREnvelope{}, err
	}
	canonical, err := canonicalJSON(envelope)
	if err != nil || !bytes.Equal(canonical, raw) {
		return CIREnvelope{}, ErrIntegrity
	}
	return envelope, nil
}
