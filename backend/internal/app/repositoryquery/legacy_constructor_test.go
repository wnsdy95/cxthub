package repositoryquery

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyConstructorProbe struct {
	reads int
}

func (probe *legacyConstructorProbe) IsPinnedRead(context.Context) bool {
	probe.reads++
	return false
}

func (probe *legacyConstructorProbe) AcquireBuild(ctx context.Context) (context.Context, func(), error) {
	probe.reads++
	return ctx, func() {}, domain.ErrIntegrity
}

func (probe *legacyConstructorProbe) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	probe.reads++
	return domain.Repo{}, domain.ErrIntegrity
}

func (probe *legacyConstructorProbe) VerifiedPR(context.Context, domain.ContentHash, []domain.HistoryEvent) ([]evidencequery.VerifiedPRContribution, error) {
	probe.reads++
	return nil, domain.ErrIntegrity
}

func (probe *legacyConstructorProbe) VerifiedGit(context.Context, domain.ContentHash, []domain.HistoryEvent) ([]evidencequery.VerifiedGitContribution, error) {
	probe.reads++
	return nil, domain.ErrIntegrity
}

func (probe *legacyConstructorProbe) ReadRepositoryGraphSourceGeneration(context.Context, domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, error) {
	probe.reads++
	return domain.RepositoryViewSourceCheckpoint{}, domain.ErrIntegrity
}

func (probe *legacyConstructorProbe) GetGitCommitTree(context.Context, domain.ContentHash, string, string) (domain.GitCommitTree, error) {
	probe.reads++
	return domain.GitCommitTree{}, domain.ErrIntegrity
}

func (probe *legacyConstructorProbe) GetGitTreeNode(context.Context, domain.ContentHash, string, string) (domain.GitTreeNode, error) {
	probe.reads++
	return domain.GitTreeNode{}, domain.ErrIntegrity
}

func (probe *legacyConstructorProbe) GetGitDelta(context.Context, domain.ContentHash, string, string, string) (domain.GitCommitDelta, error) {
	probe.reads++
	return domain.GitCommitDelta{}, domain.ErrIntegrity
}

func legacyConstructorConfig(probe *legacyConstructorProbe) LegacyProjectionConfig {
	return LegacyProjectionConfig{Scope: probe, Resources: probe, Metadata: probe, Contributions: probe,
		Generations: probe, Trees: probe, Deltas: probe, Graphs: &GraphCache{}, Branches: &BranchCache{}}
}

func TestLegacyProjectionConstructorBindsSharedStateWithoutIO(test *testing.T) {
	probe := &legacyConstructorProbe{}
	config := legacyConstructorConfig(probe)
	projection, err := NewLegacyProjection(config)
	if err != nil || projection == nil {
		test.Fatal(err)
	}
	if probe.reads != 0 || projection.graphs != config.Graphs || projection.branches != config.Branches || projection.scope != probe || projection.resources != probe || projection.metadata != probe || projection.contributions != probe {
		test.Fatal("constructor performed I/O or replaced shared dependencies", probe.reads)
	}
	for _, strict := range []bool{false, true} {
		config.RequireComplete = strict
		config.Generations, config.Trees, config.Deltas = nil, nil, nil
		projection, err := NewLegacyProjection(config)
		if err != nil || projection == nil || probe.reads != 0 {
			test.Fatal("optional readers became mandatory through exhaustion policy", strict, err, probe.reads)
		}
	}
}

func TestLegacyProjectionConstructorRejectsMissingAndTypedNil(test *testing.T) {
	for _, name := range []string{"Scope", "Resources", "Metadata", "Contributions", "Generations", "Trees", "Deltas", "Graphs", "Branches"} {
		for _, typedNil := range []bool{false, true} {
			test.Run(name+"/"+map[bool]string{false: "missing", true: "typed-nil"}[typedNil], func(test *testing.T) {
				probe := &legacyConstructorProbe{}
				config := legacyConstructorConfig(probe)
				field := reflect.ValueOf(&config).Elem().FieldByName(name)
				field.SetZero()
				if typedNil && field.Kind() == reflect.Interface {
					var missing *legacyConstructorProbe
					field.Set(reflect.ValueOf(missing))
				}
				projection, err := NewLegacyProjection(config)
				optional := name == "Generations" || name == "Trees" || name == "Deltas"
				if optional && !typedNil {
					if err != nil || projection == nil {
						test.Fatal("missing optional capability was rejected", err)
					}
				} else if projection != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
					test.Fatal("invalid declaration created a projection", projection, err)
				}
				if probe.reads != 0 {
					test.Fatal("invalid assembly performed I/O", probe.reads)
				}
			})
		}
	}
}
