package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestMemoryReadWorkInternalAuthority(test *testing.T) {
	service := &Service{}
	repo := domain.HashContent([]byte(test.Name()))
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: repo}}
	allow := func(ctx context.Context, _ domain.ContentHash) (context.Context, error) { return ctx, nil }
	for _, ctx := range []context.Context{context.Background(), inbound.WithDocumentReadAuthorizer(systemTestContext(), allow)} {
		if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); !errors.Is(err, domain.ErrForbidden) {
			test.Fatal("untrusted context requested work", err)
		}
		if _, err := service.ReadMemoryPreparationStatus(ctx, repo, request); !errors.Is(err, domain.ErrForbidden) {
			test.Fatal("untrusted context read internal work", err)
		}
	}
	request.Cursor = "existing-page"
	if _, err := service.RequestMemoryReadPreparation(systemTestContext(), repo, request); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("page continuation enqueued preparation", err)
	}
}

func TestMemoryReadWorkDefaultOff(test *testing.T) {
	service := &Service{}
	ctx := context.Background()
	if err := service.ConfigureMemoryReadPreparations(ctx, nil); err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal("disabled worker accessed storage", err)
	}
	service.RunMemoryReadPreparationWorker(ctx)
	for _, limit := range []int{0, -1, 129} {
		if err := service.ProcessMemoryReadPreparations(ctx, limit); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("unbounded batch accepted", limit, err)
		}
	}
	if err := service.ConfigureMemoryReadPreparations(ctx, []domain.ContentHash{domain.HashContent([]byte(test.Name()))}); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("worker enabled without required ports", err)
	}
	if err := service.ConfigureMemoryReadPreparations(ctx, make([]domain.ContentHash, domain.MaxMemoryReadWorkerRepos+1)); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("unbounded repository scope accepted", err)
	}
}

func TestMemoryReadWorkTargetIgnoresPagination(test *testing.T) {
	repo := domain.HashContent([]byte(test.Name()))
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: repo, CodeCommit: strings.Repeat("a", 40)}, Content: "all", Limit: 1}
	first, err := memoryReadTarget(repo, request)
	if err != nil {
		test.Fatal(err)
	}
	request.Limit, request.Cursor, request.Content = 50, "continuation", ""
	second, err := memoryReadTarget(repo, request)
	if err != nil || first != second {
		test.Fatal("page limit or cursor changed durable target", err)
	}
	request.Content = "claims"
	third, err := memoryReadTarget(repo, request)
	if err != nil || third == first {
		test.Fatal("view mode did not distinguish target", err)
	}
}

func TestMemoryReadWorkFailuresAreBoundedAndSanitized(test *testing.T) {
	for _, sample := range []struct {
		err    error
		reason string
		retry  bool
	}{
		{errMemoryReadCleanupPending, "retention_cleanup", true},
		{domain.ErrMemoryReadComparisonMismatch, "comparison_mismatch", false},
		{errors.Join(domain.ErrMemoryReadComparisonMismatch, domain.ErrConflict), "comparison_mismatch", false},
		{domain.ErrConflict, "source_changed", true},
		{domain.ErrRefConflict, "source_changed", true},
		{domain.ErrCatalogResetRequired, "source_reset", true},
		{domain.ErrMemoryCaptureExpired, "retention_expired", true},
		{domain.ErrRepositoryViewSourceLimit, "resource_limit", false},
		{domain.ErrMemoryProjectionLimit, "resource_limit", false},
		{domain.ErrIntegrity, "invalid_source", false},
		{domain.ErrValidation, "invalid_source", false},
		{domain.ErrNotFound, "invalid_source", false},
		{domain.ErrCatalogUnsupported, "invalid_source", false},
		{domain.ErrForbidden, "access_denied", false},
		{domain.ErrDocumentIdentityUpgradeRequired, "identity_upgrade_required", true},
		{context.DeadlineExceeded, "temporary_failure", true},
		{errors.New("sensitive raw service failure"), "temporary_failure", true},
	} {
		reason, retry := memoryReadWorkFailure(sample.err)
		if reason != sample.reason || retry != sample.retry || !domain.MemoryReadWorkReasonValid(reason) {
			test.Fatal(sample.err, reason, retry)
		}
	}
}
