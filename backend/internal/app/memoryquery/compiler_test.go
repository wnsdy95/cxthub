package memoryquery

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestBuildReadRejectsMissingQueries(test *testing.T) {
	bundle, err := BuildRead(context.Background(), nil, "", "", domain.RepositoryMemorySourceGeneration{}, domain.EffectiveMemorySelection{}, "", nil)
	if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(bundle, domain.MemoryReadBundle{}) {
		test.Fatal("missing compiler queries returned a bundle or lost integrity failure", bundle, err)
	}
}

func TestBuildReadRejectsMismatchedFrozenSource(test *testing.T) {
	repo := domain.HashContent([]byte("compiler-frozen-repository"))
	foreign := domain.HashContent([]byte("compiler-foreign-repository"))
	frozen := domain.RepositoryMemorySourceGeneration{Version: domain.RepositoryMemorySourceVersion,
		Epoch: "23456789-abcd-4321-9876-123456789abc", Sequence: 5,
		Graph: domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope,
			RepoID: repo, Epoch: "12345678-abcd-4321-9876-123456789abc", Sequence: 7}}
	if err := frozen.Validate(); err != nil {
		test.Fatal("invalid frozen-source fixture", err)
	}
	memoryChanged, graphChanged, epochChanged := frozen, frozen, frozen
	memoryChanged.Sequence++
	graphChanged.Graph.Sequence++
	epochChanged.Epoch = "34567890-abcd-4321-9876-123456789abc"
	for _, sample := range []struct {
		name       string
		repo       domain.ContentHash
		generation domain.RepositoryMemorySourceGeneration
		queries    *LiveQueries
	}{
		{name: "foreign-repository", repo: foreign, generation: frozen, queries: &LiveQueries{mode: LiveFrozen, frozenGeneration: &frozen}},
		{name: "memory-sequence", repo: repo, generation: memoryChanged, queries: &LiveQueries{mode: LiveFrozen, frozenGeneration: &frozen}},
		{name: "graph-sequence", repo: repo, generation: graphChanged, queries: &LiveQueries{mode: LiveFrozen, frozenGeneration: &frozen}},
		{name: "memory-epoch", repo: repo, generation: epochChanged, queries: &LiveQueries{mode: LiveFrozen, frozenGeneration: &frozen}},
		{name: "missing-source", repo: repo, generation: frozen, queries: &LiveQueries{mode: LiveFrozen}},
		{name: "live-current", repo: repo, generation: frozen, queries: &LiveQueries{mode: LiveCurrent, frozenGeneration: &frozen}},
	} {
		test.Run(sample.name, func(test *testing.T) {
			bundle, err := BuildRead(context.Background(), sample.queries, sample.repo, domain.HashContent([]byte("compiler-capture")), sample.generation, domain.EffectiveMemorySelection{}, "", nil)
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(bundle, domain.MemoryReadBundle{}) {
				test.Fatal("mismatched frozen source reached readers or returned a mislabeled bundle", bundle, err)
			}
		})
	}
}

func TestBuildReadCancellationPrecedesQueries(test *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, expire := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer expire()
	for _, sample := range []struct {
		name    string
		context context.Context
		want    error
	}{
		{name: "canceled", context: canceled, want: context.Canceled},
		{name: "deadline", context: expired, want: context.DeadlineExceeded},
	} {
		test.Run(sample.name, func(test *testing.T) {
			for _, queries := range []*LiveQueries{nil, {}} {
				bundle, err := BuildRead(sample.context, queries, "", "", domain.RepositoryMemorySourceGeneration{}, domain.EffectiveMemorySelection{}, "", nil)
				if !errors.Is(err, sample.want) || !reflect.DeepEqual(bundle, domain.MemoryReadBundle{}) {
					test.Fatal("compiler touched queries or returned partial output after cancellation", bundle, err)
				}
			}
		})
	}
}
