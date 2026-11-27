package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// RepoAddressMover gives a stored repository another identifier in one
// transaction, in every place that names it. Only an operator runs it.
type RepoAddressMover interface {
	MoveRepoAddress(context.Context, domain.RepoAddressMoveRequest) (domain.RepoAddressMove, error)
}
