package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestAgentDocumentExplicitRootFreshValidation(t *testing.T) {
	cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "synthetic-root", Fidelity: domain.FidelityFull}, Events: []domain.Event{{Seq: 0, Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "synthetic input"}}}}}
	manifest, _, err := domain.ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	original := domain.SessionDoc{Hash: hash, Identity: domain.DocumentIdentityRootV1, CIR: cir}
	ref := original.DocumentRef()
	repo := domain.HashContent([]byte("synthetic-repo"))
	mode := "valid"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("missing current auth")
		}
		if mode == "revoked" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		doc := original
		switch mode {
		case "stripped":
			doc.Identity = domain.DocumentIdentityLegacy
		case "wrong-hash":
			doc.Hash = repo
		case "changed":
			doc.CIR.Events = []domain.Event{{Seq: 0, Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "changed input"}}}}
		}
		if err := json.NewEncoder(w).Encode(doc); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client := NewBackendClient(func() string { return server.URL }, func() string { return "fixture-token" }, domain.TeamIdentity{})
	for _, modeValue := range []string{"valid", "stripped", "wrong-hash", "changed", "revoked"} {
		mode = modeValue
		doc, err := client.FetchAgentDocumentReference(context.Background(), string(repo), ref)
		if mode == "valid" {
			if err != nil || doc.DocumentRef() != ref {
				t.Fatalf("valid root: %v", err)
			}
		} else if err == nil {
			t.Fatalf("accepted %s after previous valid read", mode)
		}
		personal, used, err := client.FetchPersonalWorkDocumentReference(context.Background(), string(repo), ref, 1<<20)
		if mode == "valid" {
			if err != nil || used <= 0 || personal.DocumentRef() != ref {
				t.Fatalf("valid bounded personal root: used=%d err=%v", used, err)
			}
		} else if err == nil {
			t.Fatalf("personal handoff accepted %s", mode)
		}
	}
	if calls != 10 {
		t.Fatalf("cached authorization/body: %d", calls)
	}
	mode = "valid"
	if _, err := client.FetchAgentDocument(context.Background(), string(repo), hash); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("legacy read accepted root: %v", err)
	}
	before := calls
	invalid := ref
	invalid.Identity = "unsupported"
	if _, err := client.FetchAgentDocumentReference(context.Background(), string(repo), invalid); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) || calls != before {
		t.Fatalf("invalid declaration reached network: %v", err)
	}
}
