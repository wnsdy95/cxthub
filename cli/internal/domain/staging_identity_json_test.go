package domain

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRootStagedIdentityJSONPreservesLegacyAndRejectsAmbiguity(t *testing.T) {
	for _, raw := range []string{`{"doc_identity":"cxt-manifest-sha256-v1","doc_identity":""}`, `{"doc_identity":"cxt-manifest-sha256-v1","DOC_IDENTITY":""}`, `{"doc_identity":"cxt-manifest-sha256-v1","doc_identity":"cxt-manifest-sha256-v1"}`, `{"doc_identity":null}`, `{"doc_identity":"unknown"}`} {
		before := StagedSession{SessionID: "unchanged", DocIdentity: DocumentIdentityRootV1}
		got := before
		if err := json.Unmarshal([]byte(raw), &got); err == nil {
			t.Fatalf("accepted %s", raw)
		}
		if !reflect.DeepEqual(got, before) {
			t.Fatal("error mutated decoder destination")
		}
	}
	for _, identity := range []DocumentIdentity{DocumentIdentityLegacy, DocumentIdentityRootV1} {
		input := StagedSession{SessionID: "synthetic", DocIdentity: identity}
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		var got StagedSession
		if err := json.Unmarshal(raw, &got); err != nil || got != input {
			t.Fatal(got, err)
		}
		again, err := json.Marshal(got)
		if err != nil || string(raw) != string(again) {
			t.Fatal("encoding changed", err)
		}
	}
	var legacy StagedSession
	if err := json.Unmarshal([]byte(`{"session_id":"legacy","future_field":true}`), &legacy); err != nil || legacy.DocIdentity != "" || legacy.SessionID != "legacy" {
		t.Fatal("legacy compatibility narrowed", err)
	}
}
