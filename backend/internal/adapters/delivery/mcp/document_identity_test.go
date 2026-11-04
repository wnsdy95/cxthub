package mcp

import (
	"context"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type identityContextBackend struct {
	fakeContextBackend
	supported []domain.DocumentIdentity
	observed  []domain.DocumentIdentity
	actor     string
	system    bool
}

func (b *identityContextBackend) DocumentIdentitiesSupported() []domain.DocumentIdentity {
	return b.supported
}
func (*identityContextBackend) RootPublicationEnabled() bool { return false }
func (b *identityContextBackend) ListRepos(ctx context.Context, _ string) ([]domain.Repo, error) {
	b.observed = inbound.DocumentIdentities(ctx)
	b.actor, b.system = inbound.RepositoryActor(ctx)
	return nil, nil
}
func TestMCPUsesBinaryDocumentCompatibilityWithoutElevatingActor(t *testing.T) {
	for _, ids := range [][]domain.DocumentIdentity{{domain.DocumentIdentityLegacy}, {domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}} {
		backend := &identityContextBackend{supported: ids}
		server := &Server{context: backend}
		ctx := inbound.WithRepositoryActor(context.Background(), "reader")
		// A caller-supplied declaration is replaced, not merged with support.
		ctx = inbound.WithDocumentIdentities(ctx, []domain.DocumentIdentity{"unknown"})
		if _, err := server.runTool(ctx, domain.User{ID: "reader"}, "repository_list", nil); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(backend.observed, ids) || backend.actor != "reader" || backend.system {
			t.Fatalf("compatibility or authority changed: %+v", backend)
		}
	}
}

func TestMCPExplicitDocumentIdentitiesDoNotRediscoverArchiveCapabilities(test *testing.T) {
	backend := &identityContextBackend{supported: []domain.DocumentIdentity{"unsupported-backend-declaration"}}
	server := &Server{context: backend}
	identities := []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}
	if err := server.SetDocumentIdentities(identities); err != nil {
		test.Fatal(err)
	}
	identities[1] = "mutated-after-construction"
	ctx := inbound.WithRepositoryActor(context.Background(), "reader")
	if _, err := server.runTool(ctx, domain.User{ID: "reader"}, "repository_list", nil); err != nil {
		test.Fatal(err)
	}
	expected := []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}
	if !reflect.DeepEqual(backend.observed, expected) || backend.actor != "reader" || backend.system {
		test.Fatal("explicit document compatibility or actor changed", backend)
	}
	for _, invalid := range [][]domain.DocumentIdentity{nil, {domain.DocumentIdentityRootV1}, {domain.DocumentIdentityLegacy, domain.DocumentIdentityLegacy}, {domain.DocumentIdentityLegacy, "unknown"}} {
		if err := server.SetDocumentIdentities(invalid); err == nil || !reflect.DeepEqual(server.documentIdentities, expected) {
			test.Fatal("invalid replacement altered the active compatibility declaration", invalid, err)
		}
	}
}
