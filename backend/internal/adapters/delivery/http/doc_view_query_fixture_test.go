package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type documentViewFixtureEvents interface {
	ReadDocViewEvents(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int, int, string) (domain.DocViewPage, error)
}

type documentViewFixtureFragments interface {
	ReadDocViewFragment(context.Context, domain.ContentHash, domain.ContentHash, int, int, int) (domain.DocViewFragment, error)
}

type documentViewFixtureVisible interface {
	ReadDocVisiblePage(context.Context, domain.ContentHash, domain.ContentHash, domain.DocVisiblePageRequest) (domain.DocVisiblePage, error)
}

type documentViewFixture struct {
	events    documentViewFixtureEvents
	fragments documentViewFixtureFragments
	visible   documentViewFixtureVisible
}

func (fixture documentViewFixture) ReadDocViewEvents(ctx context.Context, repo, hash, base domain.ContentHash, offset, limit int, view string) (domain.DocViewPage, error) {
	if fixture.events == nil {
		return domain.DocViewPage{}, domain.ErrDocRangeIndexUnavailable
	}
	return fixture.events.ReadDocViewEvents(ctx, repo, hash, base, offset, limit, view)
}

func (fixture documentViewFixture) ReadDocViewFragment(ctx context.Context, repo, hash domain.ContentHash, index, offset, budget int) (domain.DocViewFragment, error) {
	if fixture.fragments == nil {
		return domain.DocViewFragment{}, domain.ErrDocRangeIndexUnavailable
	}
	return fixture.fragments.ReadDocViewFragment(ctx, repo, hash, index, offset, budget)
}

func (fixture documentViewFixture) ReadDocVisiblePage(ctx context.Context, repo, hash domain.ContentHash, request domain.DocVisiblePageRequest) (domain.DocVisiblePage, error) {
	if fixture.visible == nil {
		return domain.DocVisiblePage{}, domain.ErrDocVisibleProjectionUnsupported
	}
	return fixture.visible.ReadDocVisiblePage(ctx, repo, hash, request)
}

func configureDocumentViewTestServer(server *Server) *Server {
	events, _ := server.b.(documentViewFixtureEvents)
	fragments, _ := server.b.(documentViewFixtureFragments)
	visible, _ := server.b.(documentViewFixtureVisible)
	if events != nil || fragments != nil || visible != nil {
		server.documentViews = documentViewFixture{events: events, fragments: fragments, visible: visible}
	}
	return server
}

func newDocumentViewTestServer(backend Backend, identity IdentityBackend) *Server {
	return configureDocumentViewTestServer(NewServer(backend, identity))
}
