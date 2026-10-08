package domain

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAgentPageIdentityDomainWire(t *testing.T) {
	h := HashContent([]byte("source"))
	req := AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: MaxAgentHistoryPageBytes}
	raw, err := json.Marshal(req)
	if err != nil || string(raw) != `{"before":-1,"limit":16,"max_bytes":4194304}` {
		t.Fatalf("legacy request bytes: %s %v", raw, err)
	}
	page := AgentHistoryPage{Version: 1, Hash: h, Provider: ProviderCodex, SessionID: "native", NextBefore: -1, Turns: []AgentHistoryTurn{}}
	raw, err = json.Marshal(page)
	want := `{"version":1,"hash":"` + string(h) + `","provider":"codex","session_id":"native","total":0,"before":0,"next_before":-1,"covered":false,"turns":[]}`
	if err != nil || string(raw) != want {
		t.Fatalf("legacy response bytes: %s %v", raw, err)
	}
	for _, field := range []string{"doc_identity", "covered_by_identity"} {
		for _, payload := range []string{`null`, `"future"`, `"","` + field + `":""`, `"","` + field + `":"cxt-manifest-sha256-v1"`} {
			before := req
			if err := json.Unmarshal([]byte(`{"`+field+`":`+payload+`}`), &req); err == nil || !reflect.DeepEqual(req, before) {
				t.Fatalf("request %s %s: %v", field, payload, err)
			}
		}
	}
	for _, raw := range []string{`null`, `{"doc_identity":null}`, `{"doc_identity":"future"}`, `{"doc_identity":"","Doc_Identity":"cxt-manifest-sha256-v1"}`, `{"doc_identity":"cxt-manifest-sha256-v1","doc_identity":""}`} {
		before := page
		if err := json.Unmarshal([]byte(raw), &page); err == nil || !reflect.DeepEqual(page, before) {
			t.Fatalf("response identity ambiguity %s %v", raw, err)
		}
	}
	req.DocIdentity = DocumentIdentityRootV1
	req.CoveredBy = h
	req.CoveredByIdentity = DocumentIdentityRootV1
	raw, err = json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var round AgentHistoryPageRequest
	if err = json.Unmarshal(raw, &round); err != nil || round != req {
		t.Fatal("request round trip", err)
	}
	if err := ValidateAgentHistoryPageRequest(req); err != nil {
		t.Fatal(err)
	}
	req.CoveredBy = ""
	if ValidateAgentHistoryPageRequest(req) == nil {
		t.Fatal("orphan coverage identity")
	}
	page.DocIdentity = DocumentIdentityRootV1
	rootWire, err := json.Marshal(page)
	rootWant := `{"doc_identity":"cxt-manifest-sha256-v1",` + want[1:]
	if err != nil || string(rootWire) != rootWant {
		t.Fatalf("mirrored root wire: %s %v", rootWire, err)
	}
	var decoded AgentHistoryPage
	if err := json.Unmarshal(rootWire, &decoded); err != nil || !reflect.DeepEqual(decoded, page) {
		t.Fatal("root page round trip", err)
	}
	if page.DocumentRef() != (DocumentRef{Hash: h, Identity: DocumentIdentityRootV1}) {
		t.Fatal("page ref")
	}
}
