package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// GitEvidenceReader reads immutable commit/tree objects from a configured Git
// host. Repository origin comes from trusted repository metadata, never a caller
// supplied URL. A missing/ambiguous comparison parent must fail explicitly.
type GitEvidenceReader interface {
	ReadCommitDelta(context.Context, string, string, string) (domain.GitCommitDelta, error)
	IsGitAncestor(context.Context, string, string, string) (bool, error)
}
