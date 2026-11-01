package repositoryquery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type graphRouterCall struct {
	ctx      context.Context
	repo     domain.ContentHash
	endpoint string
	position string
}

type graphRouterProbe struct {
	view    domain.RepositoryView
	pending domain.PendingView
	graph   domain.GraphState
	err     error
	calls   []graphRouterCall
}

var _ inbound.RepositoryGraphQueries = (*graphRouterProbe)(nil)

func (probe *graphRouterProbe) GetRepositoryView(ctx context.Context, repo domain.ContentHash) (domain.RepositoryView, error) {
	probe.calls = append(probe.calls, graphRouterCall{ctx: ctx, repo: repo, endpoint: "view"})
	if probe.err != nil {
		return domain.RepositoryView{}, probe.err
	}
	return probe.view, nil
}

func (probe *graphRouterProbe) GetPendingView(ctx context.Context, repo domain.ContentHash) (domain.PendingView, error) {
	probe.calls = append(probe.calls, graphRouterCall{ctx: ctx, repo: repo, endpoint: "pending"})
	if probe.err != nil {
		return domain.PendingView{}, probe.err
	}
	return probe.pending, nil
}

func (probe *graphRouterProbe) QueryGraphState(ctx context.Context, repo domain.ContentHash, position string) (domain.GraphState, error) {
	probe.calls = append(probe.calls, graphRouterCall{ctx: ctx, repo: repo, endpoint: "graph", position: position})
	if probe.err != nil {
		return domain.GraphState{}, probe.err
	}
	return probe.graph, nil
}

func graphRouterEndpoint(queries *GraphQueries, ctx context.Context, repo domain.ContentHash, endpoint string) (any, error) {
	switch endpoint {
	case "view":
		return queries.GetRepositoryView(ctx, repo)
	case "pending":
		return queries.GetPendingView(ctx, repo)
	default:
		return queries.QueryGraphState(ctx, repo, "selected-position")
	}
}

func TestGraphRouterConstructorRequiresExplicitDeclarationsWithoutIO(test *testing.T) {
	repo := domain.HashContent([]byte("published repository"))
	for _, scenario := range []struct {
		name      string
		legacy    bool
		published bool
		selected  bool
		typedNil  string
		wantError bool
	}{
		{name: "legacy only", legacy: true},
		{name: "unselected published port", legacy: true, published: true},
		{name: "selected published port", legacy: true, published: true, selected: true},
		{name: "missing legacy", published: true, wantError: true},
		{name: "selected missing published", legacy: true, selected: true, wantError: true},
		{name: "typed nil legacy", published: true, typedNil: "legacy", wantError: true},
		{name: "selected typed nil published", legacy: true, selected: true, typedNil: "published", wantError: true},
		{name: "unused typed nil published", legacy: true, typedNil: "published", wantError: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			legacy, published := &graphRouterProbe{}, &graphRouterProbe{}
			config := GraphConfig{}
			if scenario.legacy {
				config.Legacy = legacy
			}
			if scenario.published {
				config.Published = published
			}
			if scenario.selected {
				config.PublishedRepos = []domain.ContentHash{repo}
			}
			var missing *graphRouterProbe
			if scenario.typedNil == "legacy" {
				config.Legacy = missing
			}
			if scenario.typedNil == "published" {
				config.Published = missing
			}
			queries, err := NewGraph(config)
			if scenario.wantError {
				if queries != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
					test.Fatal("invalid declaration produced a graph router", queries, err)
				}
			} else if err != nil || queries == nil || queries.legacy != config.Legacy || queries.published != config.Published {
				test.Fatal("constructor replaced an explicit narrow port", queries, err)
			}
			if len(legacy.calls) != 0 || len(published.calls) != 0 {
				test.Fatal("router constructor performed I/O")
			}
		})
	}
	for _, invalid := range []domain.ContentHash{"", "invalid", domain.ContentHash("sha256:" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ0123")} {
		queries, err := NewGraph(GraphConfig{Legacy: &graphRouterProbe{}, Published: &graphRouterProbe{}, PublishedRepos: []domain.ContentHash{repo, invalid}})
		if queries != nil || !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("malformed rollout repository accepted", queries, err)
		}
	}
}

func TestGraphRouterCopiesDeduplicatesAndDispatchesImmutableSelections(test *testing.T) {
	selected := domain.HashContent([]byte("selected"))
	unselected := domain.HashContent([]byte("unselected"))
	legacy := &graphRouterProbe{view: domain.RepositoryView{DefaultBranch: "legacy"},
		pending: domain.PendingView{Revision: domain.RepositoryRevision{Pending: 3}}, graph: domain.GraphState{PrimaryBranch: "legacy"}}
	stale := &domain.GraphReadGeneration{Generation: domain.HashContent([]byte("retained")), Complete: true, Current: false}
	published := &graphRouterProbe{view: domain.RepositoryView{DefaultBranch: "published", ReadGeneration: stale},
		pending: domain.PendingView{Revision: domain.RepositoryRevision{Pending: 9}, ReadGeneration: stale},
		graph:   domain.GraphState{PrimaryBranch: "published", ReadGeneration: stale}}
	repos := []domain.ContentHash{selected, selected}
	queries, err := NewGraph(GraphConfig{Legacy: legacy, Published: published, PublishedRepos: repos})
	if err != nil || len(queries.enabled) != 1 {
		test.Fatal("router did not deduplicate rollout flags", err)
	}
	repos[0], repos[1] = unselected, "invalid"
	repos = append(repos, unselected)
	ctx := inbound.WithRepositoryActor(context.Background(), "caller")
	for _, repo := range []domain.ContentHash{selected, unselected} {
		for _, endpoint := range []string{"view", "pending", "graph"} {
			result, err := graphRouterEndpoint(queries, ctx, repo, endpoint)
			port := legacy
			if repo == selected {
				port = published
			}
			var expected any
			switch endpoint {
			case "view":
				expected = port.view
			case "pending":
				expected = port.pending
			default:
				expected = port.graph
			}
			if err != nil || !reflect.DeepEqual(result, expected) {
				test.Fatal("router reinterpreted a selected current/stale payload", repo, endpoint, result, err)
			}
			call := port.calls[len(port.calls)-1]
			if call.ctx != ctx || call.repo != repo || call.endpoint != endpoint || endpoint == "graph" && call.position != "selected-position" {
				test.Fatal("router changed the caller context, repository, or position", call)
			}
		}
	}
	if len(published.calls) != 3 || len(legacy.calls) != 3 || !queries.enabled[selected] || queries.enabled[unselected] || queries.enabled["invalid"] {
		test.Fatal("caller slice mutation changed root-lifetime routing")
	}
}

func TestGraphRouterSelectedErrorsNeverFallback(test *testing.T) {
	for _, selected := range []bool{false, true} {
		for _, endpoint := range []string{"view", "pending", "graph"} {
			for _, failure := range []error{domain.ErrForbidden, domain.ErrGraphProjectionNotReady, domain.ErrGraphProjectionFailed, domain.ErrIntegrity, domain.ErrResourceOverloaded, context.Canceled} {
				test.Run(fmt.Sprintf("selected=%t/%s/%v", selected, endpoint, failure), func(test *testing.T) {
					repo := domain.HashContent([]byte(test.Name()))
					legacy, published := &graphRouterProbe{}, &graphRouterProbe{}
					selectedPort, unusedPort := legacy, published
					config := GraphConfig{Legacy: legacy, Published: published}
					if selected {
						config.PublishedRepos = []domain.ContentHash{repo}
						selectedPort, unusedPort = published, legacy
					}
					selectedPort.err = fmt.Errorf("selected reader: %w", failure)
					queries, err := NewGraph(config)
					if err != nil {
						test.Fatal(err)
					}
					result, err := graphRouterEndpoint(queries, context.Background(), repo, endpoint)
					if err != selectedPort.err || !reflect.DeepEqual(result, legacyReadZero(endpoint)) || len(selectedPort.calls) != 1 || len(unusedPort.calls) != 0 {
						test.Fatal("selected error triggered fallback or was changed", result, err)
					}
				})
			}
		}
	}
}

func TestGraphRouterValidatesInputsBeforeEitherPort(test *testing.T) {
	legacy, published := &graphRouterProbe{}, &graphRouterProbe{}
	queries, err := NewGraph(GraphConfig{Legacy: legacy, Published: published})
	if err != nil {
		test.Fatal(err)
	}
	for _, endpoint := range []string{"view", "pending", "graph"} {
		result, err := graphRouterEndpoint(queries, context.Background(), "invalid", endpoint)
		if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(result, legacyReadZero(endpoint)) || len(legacy.calls) != 0 || len(published.calls) != 0 {
			test.Fatal("invalid request reached a selected reader", endpoint, result, err)
		}
	}
}
