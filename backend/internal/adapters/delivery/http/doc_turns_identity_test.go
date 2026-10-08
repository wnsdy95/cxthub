package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type historyIdentityBackend struct {
	Backend
	got   domain.AgentHistoryPageRequest
	calls int
}

func (b *historyIdentityBackend) ReadAgentHistoryPage(_ context.Context, _, hash domain.ContentHash, req domain.AgentHistoryPageRequest) (domain.AgentHistoryPage, error) {
	b.got = req
	b.calls++
	return domain.AgentHistoryPage{Version: 1, Hash: hash, DocIdentity: req.DocIdentity, Provider: domain.ProviderCodex, SessionID: "native", NextBefore: -1, Turns: []domain.AgentHistoryTurn{}}, nil
}
func TestAgentPageIdentityHTTPQuery(t *testing.T) {
	b := &historyIdentityBackend{}
	s := NewServer(b, nil)
	h := domain.HashContent([]byte("root"))
	for _, tc := range []struct {
		query string
		ok    bool
	}{
		{"", true},
		{"doc_identity=cxt-manifest-sha256-v1", true},
		{"covered_by=" + url.QueryEscape(string(h)) + "&covered_by_identity=cxt-manifest-sha256-v1", true},
		{"doc_identity=", true},
		{"doc_identity=%ZZ", false},
		{"doc_identity=null", false}, {"doc_identity=future", false},
		{"doc_identity=&doc_identity=cxt-manifest-sha256-v1", false},
		{"covered_by_identity=&covered_by_identity=", false},
		{"covered_by_identity=cxt-manifest-sha256-v1", false},
		{"covered_by=" + url.QueryEscape(string(h)) + "&covered_by_identity=future", false},
		{"Doc_Identity=cxt-manifest-sha256-v1", false}, {"Covered_By_Identity=cxt-manifest-sha256-v1", false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/turns?"+tc.query, nil)
			req.SetPathValue("hash", string(h))
			req.SetPathValue("repoID", string(domain.HashContent([]byte("repo"))))
			before := b.calls
			w := httptest.NewRecorder()
			s.getDocTurns(w, req)
			if (w.Code == 200) != tc.ok {
				t.Fatalf("status=%d", w.Code)
			}
			if !tc.ok {
				if b.calls != before {
					t.Fatal("invalid query reached backend")
				}
				return
			}
			if b.calls != before+1 || b.got.DocIdentity != domain.DocumentIdentity(req.URL.Query().Get("doc_identity")) || b.got.CoveredByIdentity != domain.DocumentIdentity(req.URL.Query().Get("covered_by_identity")) {
				t.Fatal("query identity lost")
			}
			var page domain.AgentHistoryPage
			if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.DocIdentity != b.got.DocIdentity {
				t.Fatal("response identity lost", err)
			}
		})
	}
}
