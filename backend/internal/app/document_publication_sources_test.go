package app

import (
	"context"

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
