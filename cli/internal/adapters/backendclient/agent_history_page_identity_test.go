package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestAgentPageIdentityDuplicateWire(t *testing.T) {
	h := domain.HashContent([]byte("source"))
	raw, _ := json.Marshal(domain.AgentHistoryPage{Version: 1, Hash: h, Provider: domain.ProviderCodex, SessionID: "native", Before: 0, NextBefore: -1, Turns: []domain.AgentHistoryTurn{}})
	for _, key := range []string{"hash", "covered", "version"} {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		mutated := bytes.Replace(raw, []byte(`"`+key+`":`), []byte(`"`+key+`":`+string(fields[key])+`,"`+key+`":`), 1)
		if historyPageWireShape(mutated) == nil {
			t.Errorf("duplicate %s accepted", key)
		}
	}
}

func TestAgentPageIdentityClientBinding(t *testing.T) {
	for _, mode := range []string{"root", "root-v2", "metadata", "covered", "stripped", "unknown", "null", "duplicate", "duplicate-alias", "wrong-hash", "legacy-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			repo, h, cover := domain.HashContent([]byte("repo")), domain.HashContent([]byte("root")), domain.HashContent([]byte("cover"))
			req := domain.AgentHistoryPageRequest{Before: 0, Limit: 1, MaxBytes: 1, DocIdentity: domain.DocumentIdentityRootV1}
			if mode == "covered" {
				req.CoveredBy = cover
				req.CoveredByIdentity = domain.DocumentIdentityRootV1
			}
			if mode == "root-v2" {
				req.IncompleteTail = "omit"
			}
			calls := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				q := r.URL.Query()
				if q.Get("doc_identity") != string(req.DocIdentity) || q.Get("covered_by_identity") != string(req.CoveredByIdentity) {
					t.Errorf("missing exact identity: %s", r.URL.RawQuery)
				}
				p := domain.AgentHistoryPage{Version: 1, Hash: h, DocIdentity: domain.DocumentIdentityRootV1, Provider: domain.ProviderCodex, SessionID: "native", Before: 0, NextBefore: -1, Turns: []domain.AgentHistoryTurn{}, Covered: mode == "covered"}
				if req.IncompleteTail == "omit" {
					p.Version = 2
				}
				if mode == "stripped" || mode == "legacy-mismatch" {
					p.DocIdentity = ""
				}
				if mode == "wrong-hash" {
					p.Hash = cover
				}
				raw, _ := json.Marshal(p)
				switch mode {
				case "unknown":
					raw = bytes.Replace(raw, []byte(`"doc_identity":"cxt-manifest-sha256-v1"`), []byte(`"doc_identity":"future"`), 1)
				case "null":
					raw = bytes.Replace(raw, []byte(`"doc_identity":"cxt-manifest-sha256-v1"`), []byte(`"doc_identity":null`), 1)
				case "duplicate":
					raw = bytes.Replace(raw, []byte(`"doc_identity":`), []byte(`"doc_identity":"","doc_identity":`), 1)
				case "duplicate-alias":
					raw = bytes.Replace(raw, []byte(`"doc_identity":`), []byte(`"Doc_Identity":"","doc_identity":`), 1)
				}
				_, _ = w.Write(raw)
			}))
			defer ts.Close()
			c := NewBackendClient(func() string { return ts.URL }, func() string { return "test" }, domain.TeamIdentity{})
			_, err := c.ReadAgentHistoryPage(context.Background(), string(repo), h, req)
			good := mode == "root" || mode == "root-v2" || mode == "metadata" || mode == "covered"
			if (err == nil) != good {
				t.Fatalf("valid=%v err=%v", good, err)
			}
			if calls != 1 {
				t.Fatal("fallback", calls)
			}
		})
	}
}

func TestAgentPageIdentityLegacyWireAndInvalidRequest(t *testing.T) {
	h, repo := domain.HashContent([]byte("legacy")), domain.HashContent([]byte("repo"))
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if _, ok := q["doc_identity"]; ok {
			t.Error("legacy query changed")
		}
		if _, ok := q["covered_by_identity"]; ok {
			t.Error("legacy coverage query changed")
		}
		_, _ = io.WriteString(w, `{"version":1,"hash":"`+string(h)+`","provider":"codex","session_id":"native","total":0,"before":0,"next_before":-1,"covered":false,"turns":[]}`)
	}))
	defer ts.Close()
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "test" }, domain.TeamIdentity{})
	req := domain.AgentHistoryPageRequest{Before: 0, Limit: 1, MaxBytes: 1}
	if _, err := c.ReadAgentHistoryPage(context.Background(), string(repo), h, req); err != nil {
		t.Fatal(err)
	}
	req.DocIdentity = "future"
	if _, err := c.ReadAgentHistoryPage(context.Background(), string(repo), h, req); err == nil {
		t.Fatal("unknown requested scheme")
	}
	req.DocIdentity = ""
	req.CoveredByIdentity = domain.DocumentIdentityRootV1
	if _, err := c.ReadAgentHistoryPage(context.Background(), string(repo), h, req); err == nil {
		t.Fatal("coverage scheme without ref")
	}
	req.CoveredBy = h
	req.CoveredByIdentity = "future"
	if _, err := c.ReadAgentHistoryPage(context.Background(), string(repo), h, req); err == nil {
		t.Fatal("unknown coverage scheme")
	}
	if calls != 1 {
		t.Fatal("invalid request made HTTP call", calls)
	}
}
