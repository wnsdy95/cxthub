package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
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
