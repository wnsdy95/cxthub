package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// CredentialAssurances shares the database identity transaction with browser
// proofs, membership, credential lifecycle and audit. There is no FS emulation.
type CredentialAssurances interface {
	FederationStore
	GetAssurancePolicy(context.Context, string) (domain.AssurancePolicy, error)
	PutAssurancePolicy(context.Context, domain.AssurancePolicy) error
	ListCredentialAssurances(context.Context, string, string) ([]domain.CredentialAssurance, error)
	PutCredentialAssurance(context.Context, domain.CredentialAssurance) error
}
