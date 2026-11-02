package contextprojection

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const MaxProjectionSnapshots = 4096
const MaxCodeEvidenceReads = 8192

type CodeEvidence interface {
	Origin() string
	GetGitCommitTree(context.Context, domain.ContentHash, string, string) (domain.GitCommitTree, error)
	Relation(context.Context, string, string) (string, error)
	ExhaustTraversal() error
}

type Contribution struct {
	ID                  string
	DestinationBranchID string
	Capture             domain.ContentHash
	IntegrationSHA      string
	Valid               bool
	SourceBranch        string
	DestinationBranch   string
	PRNumber            int
	MergedAt            time.Time
}

type Contributions interface {
	Len() int
	At(int) Contribution
}
