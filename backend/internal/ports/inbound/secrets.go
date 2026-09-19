package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type SaveSecretsInput struct {
	RepoID domain.ContentHash
	// ActorID is resolved from authenticated credentials by the inbound adapter.
	ActorID  string
	Envelope []byte
	Edit     domain.SecretsEdit
}

type SaveSecretsOutput struct {
	Status   string `json:"status"`
	Revision string `json:"revision"`
}

type SaveSecrets interface {
	SaveSecrets(context.Context, SaveSecretsInput) (SaveSecretsOutput, error)
}
