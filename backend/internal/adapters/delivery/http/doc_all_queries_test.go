package http

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type docAllHTTPCall struct {
	method    string
	repo      domain.ContentHash
	hash      domain.ContentHash
	arguments any
}

type docAllHTTPQueries struct {
	output any
	cause  error
	read   func(context.Context, string) error
	calls  []docAllHTTPCall
}

func (queries *docAllHTTPQueries) record(ctx context.Context, repo, hash domain.ContentHash, method string, arguments any) error {
	queries.calls = append(queries.calls, docAllHTTPCall{method: method, repo: repo, hash: hash, arguments: arguments})
	if queries.read != nil {
		return queries.read(ctx, method)
	}
	return queries.cause
}

func (queries *docAllHTTPQueries) ReadDocFragments(ctx context.Context, repo, hash domain.ContentHash, index, offset, limit, budget int) (domain.DocFragmentPage, error) {
	page, _ := queries.output.(domain.DocFragmentPage)
	return page, queries.record(ctx, repo, hash, "fragments", []int{index, offset, limit, budget})
}

func (queries *docAllHTTPQueries) SearchDocEvents(ctx context.Context, repo, hash domain.ContentHash, query string, after, limit int) ([]domain.DocEventIndex, error) {
	page, _ := queries.output.([]domain.DocEventIndex)
	return page, queries.record(ctx, repo, hash, "search", []any{query, after, limit})
}

func (queries *docAllHTTPQueries) ReadDocViewEvents(ctx context.Context, repo, hash domain.ContentHash, offset, limit int, view string) (domain.DocViewPage, error) {
	var base domain.ContentHash
	_ = base
	page, _ := queries.output.(domain.DocViewPage)
	return page, queries.record(ctx, repo, hash, "view", []any{base, offset, limit, view})
}

func (queries *docAllHTTPQueries) ReadDocViewFragment(ctx context.Context, repo, hash domain.ContentHash, index, offset, budget int) (domain.DocViewFragment, error) {
	page, _ := queries.output.(domain.DocViewFragment)
	return page, queries.record(ctx, repo, hash, "view-fragment", []int{index, offset, budget})
}

type docAllHTTPSnapshotKey struct{}

type docAllHTTPPermit func(context.Context, string, string) error

func (permit docAllHTTPPermit) AuthorizeSnapshot(ctx context.Context, repository, actor string) error {
	return permit(ctx, repository, actor)
}

type docAllHTTPIdentity struct {
	*docVisiblePolicyIdentity
	test             *testing.T
	admissions       int
	snapshotResolves int
	snapshotPermits  int
	snapshotErr      error
}

func (identity *docAllHTTPIdentity) ResolveUser(ctx context.Context, token string) (domain.User, error) {
	if ctx.Value(docAllHTTPSnapshotKey{}) == true {
		identity.test.Fatal("ordinary ResolveUser ran inside a pinned document snapshot")
	}
	return identity.docVisiblePolicyIdentity.ResolveUser(ctx, token)
}

func (identity *docAllHTTPIdentity) AdmitRepositoryRead(ctx context.Context, repository, actor string) (inbound.RepositoryReadPermit, error) {
	if ctx.Value(docAllHTTPSnapshotKey{}) == true || repository != "repository" || actor != "reader" {
		identity.test.Fatal("document read admission did not precede its pinned snapshot", repository, actor)
	}
	identity.admissions++
	return docAllHTTPPermit(func(bound context.Context, requestedRepository, requestedActor string) error {
		if bound.Value(docAllHTTPSnapshotKey{}) != true || requestedRepository != repository || requestedActor != actor {
			identity.test.Fatal("snapshot permit lost its pinned context, repository or actor", requestedRepository, requestedActor)
		}
		identity.snapshotPermits++
		return identity.snapshotErr
	}), nil
}

func (identity *docAllHTTPIdentity) ResolveUserForSnapshot(ctx context.Context, token string) (domain.User, error) {
	if ctx.Value(docAllHTTPSnapshotKey{}) != true || token != "session" {
		identity.test.Fatal("snapshot identity resolution lost the original credential or pinned context", token)
	}
	identity.snapshotResolves++
	return identity.user, nil
}

func (*docAllHTTPIdentity) ResolveMCPUserForSnapshot(context.Context, string, ...string) (domain.User, error) {
	panic("HTTP document queries must not resolve an MCP snapshot identity")
}

var _ inbound.DocumentQueries = (*docAllHTTPQueries)(nil)
var _ inbound.SnapshotReadIdentity = (*docAllHTTPIdentity)(nil)
