package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type associationStatusReadKey struct{}

type associationStatusReadProbe struct {
	metadata    domain.Repo
	calls       []string
	scopeError  error
	finishError error
}

func (probe *associationStatusReadProbe) ReadRepository(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	probe.calls = append(probe.calls, "scope")
	if probe.scopeError != nil {
		return probe.scopeError
	}
	if repo != probe.metadata.ID {
		return domain.ErrIntegrity
	}
	if err := read(context.WithValue(ctx, associationStatusReadKey{}, probe)); err != nil {
		return err
	}
	return probe.finishError
}

func (probe *associationStatusReadProbe) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	probe.calls = append(probe.calls, "metadata")
	if ctx.Value(associationStatusReadKey{}) != probe || repo != probe.metadata.ID {
		return domain.Repo{}, domain.ErrIntegrity
	}
	return probe.metadata, nil
}

func (probe *associationStatusReadProbe) ReadGitAssociationStatus(ctx context.Context, repo domain.ContentHash, origin string, request domain.GitAssociationStatusRequest) ([]outbound.GitAssociationStatusEvidence, error) {
	probe.calls = append(probe.calls, "status")
	if ctx.Value(associationStatusReadKey{}) != probe || repo != probe.metadata.ID || origin != probe.metadata.GitRemoteURL || request.Validate() != nil {
		return nil, domain.ErrIntegrity
	}
	return []outbound.GitAssociationStatusEvidence{}, nil
}

func TestGitAssociationStatusQueriesUseBoundReadOnlyScope(test *testing.T) {
	probe := &associationStatusReadProbe{metadata: domain.Repo{ID: domain.HashContent([]byte(test.Name()))}}
	queries, err := newGitAssociationStatusQueries(probe, probe, true)
	if err != nil || len(probe.calls) != 0 {
		test.Fatal("query composition accessed storage", err, probe.calls)
	}
	request := domain.GitAssociationStatusRequest{Kind: "integration", State: "pending", Limit: 10}
	page, err := queries.GetGitAssociationStatus(test.Context(), probe.metadata.ID, request)
	if err != nil || page.RepoID != probe.metadata.ID || page.Discovery.State != "no_origin" || page.Items == nil ||
		!reflect.DeepEqual(probe.calls, []string{"scope", "metadata"}) {
		test.Fatal("origin-free status did not use its bound read", page, err, probe.calls)
	}
	probe.metadata.GitRemoteURL = "https://example.test/owner/repo"
	probe.calls = nil
	page, err = queries.GetGitAssociationStatus(test.Context(), probe.metadata.ID, request)
	if err != nil || page.Items == nil || !reflect.DeepEqual(probe.calls, []string{"scope", "metadata", "status"}) {
		test.Fatal("status query bypassed its read scope", page, err, probe.calls)
	}
	probe.calls, probe.scopeError = nil, domain.ErrForbidden
	page, err = queries.GetGitAssociationStatus(test.Context(), probe.metadata.ID, request)
	if !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(page, domain.GitAssociationStatusPage{}) ||
		!reflect.DeepEqual(probe.calls, []string{"scope"}) {
		test.Fatal("denied scope exposed status data", page, err, probe.calls)
	}
	probe.scopeError, probe.finishError = nil, domain.ErrUnauthorized
	page, err = queries.GetGitAssociationStatus(test.Context(), probe.metadata.ID, request)
	if !errors.Is(err, domain.ErrUnauthorized) || !reflect.DeepEqual(page, domain.GitAssociationStatusPage{}) {
		test.Fatal("failed snapshot exposed completed status data", page, err)
	}
}

func TestGitAssociationStatusQueriesRejectMissingReadersAndInvalidSelection(test *testing.T) {
	probe := &associationStatusReadProbe{metadata: domain.Repo{ID: domain.HashContent([]byte(test.Name()))}}
	for _, metadata := range []evidencequery.MetadataReader{nil, (*associationStatusReadProbe)(nil)} {
		if queries, err := newGitAssociationStatusQueries(metadata, probe, true); err == nil || queries != nil {
			test.Fatal("accepted missing metadata reader")
		}
	}
	for _, scope := range []evidencequery.Scope{nil, (*associationStatusReadProbe)(nil)} {
		if queries, err := newGitAssociationStatusQueries(probe, scope, true); err == nil || queries != nil {
			test.Fatal("accepted missing repository scope")
		}
	}
	metadataOnly := struct{ evidencequery.MetadataReader }{probe}
	queries, err := newGitAssociationStatusQueries(metadataOnly, probe, true)
	if err != nil {
		test.Fatal(err)
	}
	request := domain.GitAssociationStatusRequest{Kind: "integration", State: "pending", Limit: 10}
	page, err := queries.GetGitAssociationStatus(test.Context(), probe.metadata.ID, request)
	if !errors.Is(err, domain.ErrGitAssociationStatusUnavailable) || !reflect.DeepEqual(page, domain.GitAssociationStatusPage{}) {
		test.Fatal("missing status reader reported empty success", page, err)
	}
	probe.calls = nil
	request.Limit = 0
	if _, err := queries.GetGitAssociationStatus(test.Context(), probe.metadata.ID, request); !errors.Is(err, domain.ErrValidation) || len(probe.calls) != 0 {
		test.Fatal("invalid selection reached storage", err, probe.calls)
	}
}

func TestGitAssociationStatusPreservesLegacyConfigurationFence(test *testing.T) {
	_, storage, repo, _ := readServicesFixture(test)
	service := &Service{meta: storage, blobs: storage, repositories: (*store.FSStore)(nil)}
	page, err := service.GetGitAssociationStatus(systemTestContext(), repo, domain.GitAssociationStatusRequest{
		Kind: "integration", State: "pending", Limit: 10,
	})
	if !errors.Is(err, domain.ErrValidation) || !reflect.DeepEqual(page, domain.GitAssociationStatusPage{}) {
		test.Fatal("invalid legacy authority configuration bypassed its read fence", page, err)
	}
}
