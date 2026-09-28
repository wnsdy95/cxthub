package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"time"
)

// SAMLSettings is private, pinned configuration. Metadata is supplied by an
// Enterprise owner; protocol requests never fetch tenant-selected URLs.
type SAMLSettings struct {
	Metadata, EntityID, ACS, Certificate string
	PrivateKey                           string `json:"-"`
	AdditionalCertificates               []string
}

type SAMLStore interface {
	FederationStore
	GetSAMLConnection(context.Context, string) (domain.SAMLConnection, error)
	PutSAMLConnection(context.Context, domain.SAMLConnection) error
	DeleteSAMLConnection(context.Context, string) error
	CreateSAMLAttempt(context.Context, domain.SAMLAttempt) error
	GetSAMLAttempt(context.Context, string) (domain.SAMLAttempt, error)
	GetSAMLFinish(context.Context, string) (domain.SAMLAttempt, error)
	ReceiveSAMLAttempt(context.Context, domain.SAMLAttempt) error
	FinishSAMLAttempt(context.Context, string) error
}
type SAMLProof struct {
	FederationProof
	AssertionID string
}
type SAMLProvider interface {
	Validate(context.Context, SAMLSettings) (string, error)
	ValidateMetadata(context.Context, SAMLSettings) (string, error)
	Keys(context.Context) (certificate, privateKey string, err error)
	Metadata(context.Context, SAMLSettings) (string, error)
	Authorize(context.Context, SAMLSettings, string, string) (string, error)
	Verify(context.Context, SAMLSettings, string, []byte, time.Time) (SAMLProof, error)
}
