package repositoryquery

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestGraphReadinessBeforeReadActivationUsesOnlyQualifiedMetadata(test *testing.T) {
	fixture := newStateTestFixture(test, false)
	ctx := inbound.WithSystemActor(context.Background())
	result, err := fixture.queries.GetGraphReadiness(ctx, fixture.repo)
	if err != nil || result.Selected || !result.Projection.Complete || !result.Projection.Current || result.Projection.Generation == "" {
		test.Fatal("qualified candidate cannot be checked before activating reads", result, err)
	}
	if !reflect.DeepEqual(fixture.ports.calls, []string{"graph-status"}) || len(fixture.metadata.calls) != 0 || fixture.scope.pins != 1 || fixture.resources.acquired != 1 || fixture.resources.released != 1 {
		test.Fatal("readiness loaded graph payloads or escaped admission", fixture.ports.calls, fixture.metadata.calls)
	}
	state, err := fixture.queries.RepositoryChangeState(context.Background(), fixture.repo)
	if err != nil || state.GraphProjection != nil {
		test.Fatal("candidate check changed live read routing", state, err)
	}
	fixture.queries.graphRepos[fixture.repo] = true
	fixture.ports.graph.Source.Sequence++
	fixture.ports.graph.Ready = false
	retained, err := fixture.queries.GetGraphReadiness(ctx, fixture.repo)
	if err != nil || !retained.Selected || !retained.Projection.Complete || retained.Projection.Current || retained.Projection.Generation != result.Projection.Generation {
		test.Fatal("retained result was called current or lost its qualification", retained, err)
	}
}

func TestGraphReadinessQualificationFailures(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*domain.GraphProjectionRead)
		state  string
		err    error
	}{
		{name: "missing proof", change: func(read *domain.GraphProjectionRead) { read.Qualification = nil }, state: "catching_up"},
		{name: "mismatch", change: func(read *domain.GraphProjectionRead) {
			read.Qualification.Actual = domain.HashContent([]byte("different"))
		}, state: "needs_attention"},
		{name: "quarantined", change: func(read *domain.GraphProjectionRead) {
			read.NeedsAttention = true
			read.Ready = false
			read.Complete = false
		}, state: "needs_attention"},
		{name: "wrong source", change: func(read *domain.GraphProjectionRead) { read.Source.RepoID = domain.HashContent([]byte("other")) }, err: domain.ErrIntegrity},
		{name: "wrong applied", change: func(read *domain.GraphProjectionRead) { read.Applied.RepoID = domain.HashContent([]byte("other")) }, err: domain.ErrIntegrity},
		{name: "payload in status", change: func(read *domain.GraphProjectionRead) { read.Parts = []domain.GraphProjectionPart{{}} }, err: domain.ErrIntegrity},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newStateTestFixture(test, false)
			scenario.change(&fixture.ports.graph)
			result, err := fixture.queries.GetGraphReadiness(inbound.WithSystemActor(context.Background()), fixture.repo)
			if !errors.Is(err, scenario.err) || scenario.err == nil && (result.Projection.State != scenario.state || result.Projection.Complete || result.Projection.Generation != "") || scenario.err != nil && !reflect.DeepEqual(result, domain.GraphReadiness{}) {
				test.Fatal("unsafe readiness result", result, err)
			}
		})
	}
}

func TestGraphReadinessRequiresAuthorizationAndAtomicCompletion(test *testing.T) {
	fixture := newStateTestFixture(test, false)
	ctx := inbound.WithSystemActor(context.Background())
	denied := inbound.WithDocumentReadAuthorizer(ctx, func(context.Context, domain.ContentHash) (context.Context, error) {
		return nil, domain.ErrForbidden
	})
	if result, err := fixture.queries.GetGraphReadiness(denied, fixture.repo); !errors.Is(err, domain.ErrForbidden) || result.Selected || len(fixture.ports.calls) != 0 {
		test.Fatal("denied caller read readiness", result, err)
	}
	fixture.scope.finishErr = domain.ErrForbidden
	if result, err := fixture.queries.GetGraphReadiness(ctx, fixture.repo); !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(result, domain.GraphReadiness{}) {
		test.Fatal("failed pinned read leaked candidate", result, err)
	}
	fixture.queries.atomicRead = false
	if _, err := fixture.queries.GetGraphReadiness(ctx, fixture.repo); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("nontransactional status claimed readiness", err)
	}
}
