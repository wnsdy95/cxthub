package inbound

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"testing"
)

func TestP6ContextCopiesAndDoesNotAuthenticate(t *testing.T) {
	ids := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
	ctx := WithDocumentIdentities(context.Background(), ids)
	ids[0] = "future"
	got := DocumentIdentities(ctx)
	if len(got) != 1 || got[0] != domain.DocumentIdentityRootV1 {
		t.Fatal("input aliased")
	}
	got[0] = "future"
	if DocumentIdentities(ctx)[0] != domain.DocumentIdentityRootV1 {
		t.Fatal("output aliased")
	}
	if user, system := RepositoryActor(ctx); user != "" || system {
		t.Fatal("declaration authenticated caller")
	}
}
func TestP6ProfileIdentityStrictAndAbsentPreserved(t *testing.T) {
	for _, body := range []string{`{"required_doc_identity":null}`, `{"required_doc_identity":"future"}`, `{"required_doc_identity":"","required_doc_identity":"cxt-manifest-sha256-v1"}`, `{"REQUIRED_DOC_IDENTITY":"cxt-manifest-sha256-v1"}`} {
		var p RepoProfilePatch
		if json.Unmarshal([]byte(body), &p) == nil {
			t.Errorf("accepted %s", body)
		}
	}
	for _, body := range []string{`{}`, `{"description":"old","description":"new","legacy_extension":1}`} {
		var p RepoProfilePatch
		if err := json.Unmarshal([]byte(body), &p); err != nil || p.RequiredDocIdentity != nil {
			t.Fatalf("legacy patch changed: %+v %v", p, err)
		}
	}
	var p RepoProfilePatch
	if err := json.Unmarshal([]byte(`{"required_doc_identity":"cxt-manifest-sha256-v1"}`), &p); err != nil || p.RequiredDocIdentity == nil || *p.RequiredDocIdentity != domain.DocumentIdentityRootV1 {
		t.Fatal(p, err)
	}
}
