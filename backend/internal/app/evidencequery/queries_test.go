package evidencequery

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type constructorProbe struct {
	reads int
}

func (probe *constructorProbe) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	probe.reads++
	return domain.Repo{}, domain.ErrIntegrity
}

func (probe *constructorProbe) ListPRContributions(context.Context, domain.ContentHash) ([]domain.PRContribution, error) {
	probe.reads++
	return nil, domain.ErrIntegrity
}

func (probe *constructorProbe) GetPRHistoryCheckpoint(context.Context, domain.ContentHash, string, int64, int) (domain.PRHistoryCheckpoint, error) {
	probe.reads++
	return domain.PRHistoryCheckpoint{}, domain.ErrIntegrity
}

func (probe *constructorProbe) GetPRRefObservation(context.Context, domain.ContentHash, string, string) (domain.GitRefObservation, error) {
	probe.reads++
	return domain.GitRefObservation{}, domain.ErrIntegrity
}

func (probe *constructorProbe) ListPRParticipations(context.Context, domain.ContentHash, string, int) ([]domain.PRParticipation, error) {
	probe.reads++
	return nil, domain.ErrIntegrity
}

func (probe *constructorProbe) ListGitContributions(context.Context, domain.ContentHash) ([]domain.GitContribution, error) {
	probe.reads++
	return nil, domain.ErrIntegrity
}

func (probe *constructorProbe) GetGitContextHead(context.Context, domain.ContentHash, string) (domain.GitContextHead, error) {
	probe.reads++
	return domain.GitContextHead{}, domain.ErrIntegrity
}

func (probe *constructorProbe) ListGitContributionProofs(context.Context, domain.ContentHash) ([]domain.GitContributionProof, error) {
	probe.reads++
	return nil, domain.ErrIntegrity
}

func (probe *constructorProbe) GetGitContributionAdmission(context.Context, domain.ContentHash, string) (domain.GitContributionAdmission, error) {
	probe.reads++
	return domain.GitContributionAdmission{}, domain.ErrIntegrity
}

func (probe *constructorProbe) GetGitHeadReceipt(context.Context, domain.ContentHash, string, int64) (domain.GitHeadReceipt, error) {
	probe.reads++
	return domain.GitHeadReceipt{}, domain.ErrIntegrity
}

func constructorConfig(probe *constructorProbe, profile Profile) Config {
	return Config{Profile: profile, Metadata: probe, PRLedger: probe, PRHistory: probe, GitLedger: probe,
		GitHeads: probe, GitAdmission: probe, GitReceipts: probe}
}

func TestEvidenceConstructorBindsOnlyDeclaredReadersWithoutIO(test *testing.T) {
	for _, profile := range []Profile{Compatibility, CompleteEvidence} {
		probe := &constructorProbe{}
		queries, err := New(constructorConfig(probe, profile))
		if err != nil || queries == nil || queries.metadata != probe || queries.prLedger != probe || queries.prHistory != probe || queries.gitLedger != probe || queries.gitHeads != probe || queries.gitAdmission != probe || queries.gitReceipts != probe || probe.reads != 0 {
			test.Fatal("constructor replaced explicit readers or performed I/O", queries, err, probe.reads)
		}
	}
	probe := &constructorProbe{}
	if queries, err := New(Config{Profile: Compatibility, Metadata: probe}); err != nil || queries == nil || queries.prLedger != nil || queries.gitLedger != nil || probe.reads != 0 {
		test.Fatal("explicit compatibility lost absent optional readers", queries, err)
	}
}

func TestEvidenceConstructorRejectsInvalidProfilesAndAbsentReaders(test *testing.T) {
	for _, profile := range []Profile{0, 99} {
		if queries, err := New(constructorConfig(&constructorProbe{}, profile)); queries != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("missing or unknown profile silently selected compatibility", queries, err)
		}
	}
	for _, profile := range []Profile{Compatibility, CompleteEvidence} {
		for _, field := range []string{"Metadata", "PRLedger", "PRHistory", "GitLedger", "GitHeads", "GitAdmission", "GitReceipts"} {
			for _, typedNil := range []bool{false, true} {
				probe := &constructorProbe{}
				config := constructorConfig(probe, profile)
				selected := reflect.ValueOf(&config).Elem().FieldByName(field)
				if typedNil {
					var missing *constructorProbe
					selected.Set(reflect.ValueOf(missing))
				} else {
					selected.SetZero()
				}
				queries, err := New(config)
				allowed := profile == Compatibility && field != "Metadata" && !typedNil
				if allowed {
					if queries == nil || err != nil {
						test.Fatal("explicit optional reader rejected", profile, field, err)
					}
				} else if queries != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
					test.Fatal("missing or typed-nil reader accepted", profile, field, typedNil, queries, err)
				}
				if probe.reads != 0 {
					test.Fatal("invalid construction accessed evidence", probe.reads)
				}
			}
		}
	}
}

func TestEvidenceResultsPreservePrivateWireShape(test *testing.T) {
	for _, result := range []any{
		VerifiedPRContribution{Record: domain.PRContribution{ID: "internal"}, Valid: true, Publications: map[string]domain.HistoryEvent{"publication": {ID: "internal"}}},
		VerifiedGitContribution{Record: domain.GitContribution{ID: "internal"}, Valid: true, SourceBranch: "internal"},
	} {
		encoded, err := json.Marshal(result)
		if err != nil || string(encoded) != "{}" {
			test.Fatal("shared result exposed private evidence as a new API payload", string(encoded), err)
		}
	}
}
