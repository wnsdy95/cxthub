package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type IdentityKeyCipher interface {
	IdentityCipher
	ActiveKeyID() string
	Inspect(purpose, sealed string) (string, error)
}

// IdentityKeyStore is only wired into the operator command. A page and all its
// replacements/audit receipt commit together under the identity transaction.
type IdentityKeyStore interface {
	IdentityTransactions
	ListIdentitySecrets(context.Context, string, string, int) ([]domain.IdentitySecret, error)
	ReplaceIdentitySecret(context.Context, domain.IdentitySecret, string) error
	GetIdentityKeyBatch(context.Context, string, string) (domain.IdentityKeyBatch, error)
	PutIdentityKeyBatch(context.Context, domain.IdentityKeyBatch) error
}
