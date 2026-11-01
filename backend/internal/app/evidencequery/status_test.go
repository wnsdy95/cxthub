package evidencequery

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type statusScopeKey struct{}

type statusProbe struct {
	calls       []string
	metadata    domain.Repo
	revision    domain.RepositoryRevision
	jobs        []domain.GitScanJob
	head        domain.GitHeadScan
	trees       *gitEvidenceTestTrees
	deltas      *gitEvidenceTestDeltas
	scopeError  error
	finishError error
	repoError   error
	revError    error
	listError   error
	headError   error
	cursor      string
	limit       int
	origin      string
	cancel      context.CancelFunc
}

func newStatusProbe() *statusProbe {
	return &statusProbe{
		metadata: domain.Repo{ID: gitEvidenceTestRepo, GitRemoteURL: gitEvidenceTestOrigin},
		revision: domain.RepositoryRevision{Evidence: 9007199254740993, Graph: 7, Pending: 11},
	}
}

func (probe *statusProbe) ReadRepository(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	probe.calls = append(probe.calls, "scope")
	if probe.scopeError != nil {
		return probe.scopeError
	}
	if err := read(context.WithValue(ctx, statusScopeKey{}, probe)); err != nil {
		return err
	}
	return probe.finishError
}

func (probe *statusProbe) record(ctx context.Context, repo domain.ContentHash, operation string) error {
	probe.calls = append(probe.calls, operation)
	if ctx.Value(statusScopeKey{}) != probe || repo != gitEvidenceTestRepo {
		return domain.ErrIntegrity
	}
	return nil
}

func (probe *statusProbe) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	if err := probe.record(ctx, repo, "metadata"); err != nil {
		return domain.Repo{}, err
	}
	return probe.metadata, probe.repoError
}

func (probe *statusProbe) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	if err := probe.record(ctx, repo, "revision"); err != nil {
		return domain.RepositoryRevision{}, err
	}
	if probe.cancel != nil {
		probe.cancel()
	}
	return probe.revision, probe.revError
}

func (probe *statusProbe) ListGitScans(ctx context.Context, repo domain.ContentHash, cursor string, limit int) ([]domain.GitScanJob, error) {
	if err := probe.record(ctx, repo, "scans"); err != nil {
		return nil, err
	}
	probe.cursor, probe.limit = cursor, limit
	return probe.jobs, probe.listError
}

func (probe *statusProbe) GetGitHeadScan(ctx context.Context, repo domain.ContentHash, origin string) (domain.GitHeadScan, error) {
	if err := probe.record(ctx, repo, "head"); err != nil {
		return domain.GitHeadScan{}, err
	}
	probe.origin = origin
	return probe.head, probe.headError
}

func (probe *statusProbe) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, commit string) (domain.GitCommitTree, error) {
	if err := probe.record(ctx, repo, "commit"); err != nil {
		return domain.GitCommitTree{}, err
	}
	if origin != probe.metadata.GitRemoteURL {
		return domain.GitCommitTree{}, domain.ErrIntegrity
	}
	return probe.trees.GetGitCommitTree(ctx, repo, origin, commit)
}

func (probe *statusProbe) GetGitTreeNode(ctx context.Context, repo domain.ContentHash, origin, node string) (domain.GitTreeNode, error) {
	if err := probe.record(ctx, repo, "node"); err != nil {
		return domain.GitTreeNode{}, err
	}
	if origin != probe.metadata.GitRemoteURL {
		return domain.GitTreeNode{}, domain.ErrIntegrity
	}
	return probe.trees.GetGitTreeNode(ctx, repo, origin, node)
}

func (probe *statusProbe) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, commit, parent string) (domain.GitCommitDelta, error) {
	if err := probe.record(ctx, repo, "delta"); err != nil {
		return domain.GitCommitDelta{}, err
	}
	if origin != probe.metadata.GitRemoteURL {
		return domain.GitCommitDelta{}, domain.ErrIntegrity
	}
	return probe.deltas.GetGitDelta(ctx, repo, origin, commit, parent)
}

func TestStatusConstructorsBindOnlyDeclaredReadPorts(test *testing.T) {
	probe := newStatusProbe()
	scanConfig := ScanConfig{Scope: probe, Metadata: probe, Scans: probe}
	codeConfig := CodeConfig{Scope: probe, Metadata: probe}
	scans, scanError := NewScanQueries(scanConfig)
	code, codeError := NewCodeQueries(codeConfig)
	if scanError != nil || codeError != nil || scans == nil || code == nil || len(probe.calls) != 0 {
		test.Fatal("constructors accessed storage", scans, scanError, code, codeError, probe.calls)
	}
	if scans.scope != probe || scans.metadata != probe || scans.scans != probe || scans.heads != nil || code.scope != probe || code.metadata != probe || code.revisions != nil || code.trees != nil || code.deltas != nil {
		test.Fatal("constructors rediscovered optional capabilities or substituted readers")
	}
	replacement := newStatusProbe()
	scanConfig.Metadata, scanConfig.Scans = replacement, replacement
	codeConfig.Metadata = replacement
	if scans.metadata != probe || scans.scans != probe || code.metadata != probe || len(replacement.calls) != 0 {
		test.Fatal("query retained mutable configuration rather than bound readers")
	}
}

func TestStatusConstructorsRejectMissingAndTypedNilDeclarations(test *testing.T) {
	for _, kind := range []string{"scan", "code"} {
		for _, typedNil := range []bool{false, true} {
			probe := newStatusProbe()
			var missing *statusProbe
			var declaration any
			if typedNil {
				declaration = missing
			}
			var config any
			var fields []string
			if kind == "scan" {
				config = &ScanConfig{Scope: probe, Metadata: probe, Scans: probe, Heads: probe}
				fields = []string{"Scope", "Metadata", "Scans", "Heads"}
			} else {
				config = &CodeConfig{Scope: probe, Metadata: probe, Revisions: probe, Trees: probe, Deltas: probe}
				fields = []string{"Scope", "Metadata", "Revisions", "Trees", "Deltas"}
			}
			for _, field := range fields {
				selected := reflect.ValueOf(config).Elem().FieldByName(field)
				original := reflect.New(selected.Type()).Elem()
				original.Set(selected)
				if typedNil {
					selected.Set(reflect.ValueOf(declaration))
				} else {
					selected.SetZero()
				}
				optional := field == "Heads" || field == "Revisions" || field == "Trees" || field == "Deltas"
				var err error
				var constructed bool
				if kind == "scan" {
					queries, failure := NewScanQueries(*config.(*ScanConfig))
					err, constructed = failure, queries != nil
				} else {
					queries, failure := NewCodeQueries(*config.(*CodeConfig))
					err, constructed = failure, queries != nil
				}
				selected.Set(original)
				if optional && !typedNil {
					if err != nil || !constructed {
						test.Fatal("absent optional capability rejected", kind, field, err)
					}
				} else {
					want := domain.ErrValidation
					if optional {
						want = domain.ErrCatalogUnsupported
					}
					if constructed || !errors.Is(err, want) {
						test.Fatal("invalid declaration accepted or wrong error", kind, field, typedNil, err)
					}
				}
				if len(probe.calls) != 0 {
					test.Fatal("construction performed I/O", probe.calls)
				}
			}
		}
	}
}
