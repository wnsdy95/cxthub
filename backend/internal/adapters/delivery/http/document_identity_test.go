package http

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestP6DocumentIdentityHeaderRejectedBeforeHandler(t *testing.T) {
	for _, v := range [][]string{{""}, {"future"}, {"cxt-manifest-sha256-v1,cxt-manifest-sha256-v1"}, {"cxt-manifest-sha256-v1", "cxt-manifest-sha256-v1"}, {strings.Repeat("x", 257)}} {
		r := httptest.NewRequest("GET", "/api/v1/health", nil)
		r.Header["X-Cxt-Doc-Identities"] = v
		w := httptest.NewRecorder()
		NewServer(nil, nil).Handler().ServeHTTP(w, r)
		if w.Code != 400 {
			t.Errorf("invalid header %q status=%d", v, w.Code)
		}
	}
}
func TestP6LegacyHeaderOmission(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/health", nil)
	w := httptest.NewRecorder()
	NewServer(nil, nil).Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestP6StreamQueryDeclaration(t *testing.T) {
	for _, tc := range []struct {
		query, header string
		want          int
		root          bool
	}{
		{"", "", 204, false}, {"?doc_identities=cxt-manifest-sha256-v1", "", 204, true},
		{"?doc_identities=", "", 400, false}, {"?doc_identities=future", "", 400, false},
		{"?doc_identities=cxt-manifest-sha256-v1&doc_identities=cxt-manifest-sha256-v1", "", 400, false},
		{"?doc_identities=%ff%XX", "", 400, false}, {"?doc_identities=cxt-manifest-sha256-v1", "cxt-manifest-sha256-v1", 400, false},
	} {
		r := httptest.NewRequest("GET", "/api/v1/repos/example/changes"+tc.query, nil)
		if tc.header != "" {
			r.Header.Set(documentIdentitiesHeader, tc.header)
		}
		w := httptest.NewRecorder()
		s := NewServer(nil, nil)
		s.withDocumentIdentityStream(func(w http.ResponseWriter, r *http.Request) {
			ids := inbound.DocumentIdentities(r.Context())
			if (len(ids) == 1) != tc.root {
				t.Fatal("wrong class", ids)
			}
			if user, system := inbound.RepositoryActor(r.Context()); user != "" || system {
				t.Fatal("query authenticated peer")
			}
			w.WriteHeader(204)
		})(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s status=%d", tc.query, w.Code)
		}
	}
	// Header middleware alone does not accept query declarations on other routes.
	s := NewServer(nil, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/other?doc_identities=cxt-manifest-sha256-v1", nil)
	s.withDocumentIdentityPeer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(inbound.DocumentIdentities(r.Context())) != 0 {
			t.Fatal("query escaped stream route")
		}
		w.WriteHeader(204)
	})).ServeHTTP(w, r)
}
func TestP6SSEClassesDoNotShareOptInState(t *testing.T) {
	var opted atomic.Bool
	h := repositoryChangeHub{}
	repo := domain.HashContent([]byte(t.Name()))
	read := func(ctx context.Context, _ domain.ContentHash) (domain.RepositoryRevision, error) {
		if user, system := inbound.RepositoryActor(ctx); user != "" || system {
			t.Error("watcher borrowed subscriber authority")
		}
		ids := inbound.DocumentIdentities(ctx)
		root := len(ids) == 1 && ids[0] == domain.DocumentIdentityRootV1
		if opted.Load() {
			if !root {
				return domain.RepositoryRevision{}, domain.ErrDocumentIdentityUpgradeRequired
			}
			return domain.RepositoryRevision{Graph: 7}, nil
		}
		return domain.RepositoryRevision{Graph: 1}, nil
	}
	old, releaseOld := h.subscribe(repo, read)
	defer releaseOld()
	capable, releaseCapable := h.subscribe(repo, read, domain.DocumentIdentityRootV1)
	defer releaseCapable()
	for _, ch := range []<-chan revisionNotice{old, capable} {
		select {
		case n := <-ch:
			if n.err != nil || n.revision.Graph != 1 {
				t.Fatal(n)
			}
		case <-time.After(time.Second):
			t.Fatal("missing first poll")
		}
	}
	opted.Store(true)
	select {
	case n := <-old:
		if !errors.Is(n.err, domain.ErrDocumentIdentityUpgradeRequired) || n.revision.Graph != 0 {
			t.Fatal("old stream leaked advanced revision", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old class was not rechecked")
	}
	select {
	case n := <-capable:
		if n.err != nil || n.revision.Graph != 7 {
			t.Fatal("capable class lost", n)
		}
	case <-time.After(time.Second):
		t.Fatal("capable class starved")
	}
}

type p6StreamBackend struct{ Backend }

func (*p6StreamBackend) RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error) {
	return domain.RepositoryRevision{}, domain.ErrDocumentIdentityUpgradeRequired
}
func TestP6SSEAdmissionBeforeSubscription(t *testing.T) {
	s := NewServer(&p6StreamBackend{}, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/repos/example/changes", nil)
	s.repositoryChanges(w, r)
	if w.Code != 409 || len(s.changes.repos) != 0 || strings.Contains(w.Body.String(), "event: revision") {
		t.Fatal("subscribed before compatibility admission", w.Code, w.Body.String())
	}
}

type p6DiscoveryBackend struct {
	Backend
	calls int
}

func (b *p6DiscoveryBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	b.calls++
	return domain.Repo{RequiredDocIdentity: domain.DocumentIdentityRootV1}, nil
}
func TestP6DiscoveryOnlyCapabilitiesForOldPeer(t *testing.T) {
	b := &p6DiscoveryBackend{}
	s := NewServer(b, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/repos/example?initial_branch=main", nil)
	s.getRepo(w, r)
	if w.Code != 200 || b.calls != 1 {
		t.Fatal(w.Code, b.calls, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["required_doc_identity"] != string(domain.DocumentIdentityRootV1) || got["root_publication_enabled"] != false {
		t.Fatal(got)
	}
	for _, field := range []string{"initial_anchor_available", "refs", "snapshots"} {
		if _, present := got[field]; present {
			t.Fatal("discovery state exposed", field)
		}
	}
}
