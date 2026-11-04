package evidencequery

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type Profile uint8

const (
	Compatibility Profile = iota + 1
	CompleteEvidence
)

type MetadataReader interface {
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
}

type PRLedgerReader interface {
	ListPRContributions(context.Context, domain.ContentHash) ([]domain.PRContribution, error)
}

type PRHistoryReader interface {
	GetPRHistoryCheckpoint(context.Context, domain.ContentHash, string, int64, int) (domain.PRHistoryCheckpoint, error)
	GetPRRefObservation(context.Context, domain.ContentHash, string, string) (domain.GitRefObservation, error)
	ListPRParticipations(context.Context, domain.ContentHash, string, int) ([]domain.PRParticipation, error)
}

type GitLedgerReader interface {
	ListGitContributions(context.Context, domain.ContentHash) ([]domain.GitContribution, error)
}

type GitHeadReader interface {
	GetGitContextHead(context.Context, domain.ContentHash, string) (domain.GitContextHead, error)
	ListGitContributionProofs(context.Context, domain.ContentHash) ([]domain.GitContributionProof, error)
}

type GitAdmissionReader interface {
	GetGitContributionAdmission(context.Context, domain.ContentHash, string) (domain.GitContributionAdmission, error)
}

type GitReceiptReader interface {
	GetGitHeadReceipt(context.Context, domain.ContentHash, string, int64) (domain.GitHeadReceipt, error)
}

type Config struct {
	Profile      Profile
	Metadata     MetadataReader
	PRLedger     PRLedgerReader
	PRHistory    PRHistoryReader
	GitLedger    GitLedgerReader
	GitHeads     GitHeadReader
	GitAdmission GitAdmissionReader
	GitReceipts  GitReceiptReader
}

type Queries struct {
	metadata     MetadataReader
	prLedger     PRLedgerReader
	prHistory    PRHistoryReader
	gitLedger    GitLedgerReader
	gitHeads     GitHeadReader
	gitAdmission GitAdmissionReader
	gitReceipts  GitReceiptReader
}

type VerifiedPRContribution struct {
	Record       domain.PRContribution          `json:"-"`
	Valid        bool                           `json:"-"`
	Publications map[string]domain.HistoryEvent `json:"-"`
	MergedAt     time.Time                      `json:"-"`
}

type VerifiedGitContribution struct {
	Record            domain.GitContribution         `json:"-"`
	Valid             bool                           `json:"-"`
	Publications      map[string]domain.HistoryEvent `json:"-"`
	SourceBranch      string                         `json:"-"`
	DestinationBranch string                         `json:"-"`
}

func New(config Config) (*Queries, error) {
	if config.Profile != Compatibility && config.Profile != CompleteEvidence {
		return nil, fmt.Errorf("%w: evidence query profile is required", domain.ErrValidation)
	}
	if absent(config.Metadata) {
		return nil, fmt.Errorf("%w: evidence metadata reader is required", domain.ErrCatalogUnsupported)
	}
	for _, dependency := range []any{config.PRLedger, config.PRHistory, config.GitLedger, config.GitHeads, config.GitAdmission, config.GitReceipts} {
		if dependency != nil && absent(dependency) || config.Profile == CompleteEvidence && absent(dependency) {
			return nil, fmt.Errorf("%w: evidence reader is required", domain.ErrCatalogUnsupported)
		}
	}
	return &Queries{metadata: config.Metadata, prLedger: config.PRLedger, prHistory: config.PRHistory,
		gitLedger: config.GitLedger, gitHeads: config.GitHeads, gitAdmission: config.GitAdmission, gitReceipts: config.GitReceipts}, nil
}

func absent(dependency any) bool {
	if dependency == nil {
		return true
	}
	value := reflect.ValueOf(dependency)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
