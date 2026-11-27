package http

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"net/http/httptest"
	"strings"
	"testing"
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
