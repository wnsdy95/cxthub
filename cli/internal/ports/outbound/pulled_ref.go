package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// PulledRefCASStore prevents a network-delayed pull from overwriting a local
// commit or another pull. Nil expected means the named ref was absent.
type PulledRefCASStore interface {
	CompareAndSwapPulledRef(context.Context, *domain.Ref, domain.Ref) error
}
