package evidencequery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func statusScanQueries(test testing.TB, config ScanConfig) *ScanQueries {
	test.Helper()
	queries, err := NewScanQueries(config)
	if err != nil {
		test.Fatal(err)
	}
	return queries
}

func TestScanQueriesPreservePagingAndExplicitHeadReconciliation(test *testing.T) {
	for _, count := range []int{0, 1, 2, 3} {
		probe := newStatusProbe()
		probe.jobs = []domain.GitScanJob{}
		for index := 0; index < count; index++ {
			probe.jobs = append(probe.jobs, domain.GitScanJob{ID: fmt.Sprintf("job-%d", index), State: "waiting"})
		}
		probe.head = domain.GitHeadScan{State: "retrying", Reason: "offline", Page: 3, Version: 9007199254740993}
		queries := statusScanQueries(test, ScanConfig{Scope: probe, Metadata: probe, Scans: probe, Heads: probe})
		cursor := strings.Repeat("a", 64)
		page, err := queries.ListScans(context.Background(), gitEvidenceTestRepo, cursor, 2)
		want := domain.GitScanPage{Items: probe.jobs, Reconciliation: &probe.head}
		if count > 2 {
			want.Items, want.NextCursor = probe.jobs[:2], probe.jobs[1].ID
		}
		if err != nil || !reflect.DeepEqual(page, want) || probe.cursor != cursor || probe.limit != 3 || probe.origin != probe.metadata.GitRemoteURL || !reflect.DeepEqual(probe.calls, []string{"scope", "scans", "metadata", "head"}) {
			test.Fatal("paging, reconciliation, or pinned read order changed", count, page, err, probe)
		}
	}
}

func TestScanQueriesDoNotRediscoverAbsentHeadReadersOrNormalizeItems(test *testing.T) {
	for _, jobs := range [][]domain.GitScanJob{nil, {}} {
		for _, limit := range []int{1, 100} {
			probe := newStatusProbe()
			probe.jobs, probe.repoError, probe.headError = jobs, domain.ErrIntegrity, domain.ErrIntegrity
			queries := statusScanQueries(test, ScanConfig{Scope: probe, Metadata: probe, Scans: probe})
			page, err := queries.ListScans(context.Background(), gitEvidenceTestRepo, "", limit)
			if err != nil || !reflect.DeepEqual(page.Items, jobs) || page.Reconciliation != nil || page.NextCursor != "" || probe.limit != limit+1 || !reflect.DeepEqual(probe.calls, []string{"scope", "scans"}) {
				test.Fatal("optional head reader was rediscovered, list normalized, or limit changed", page, err, probe)
			}
		}
	}
}

func TestScanQueriesValidateInsideScopeBeforeStorage(test *testing.T) {
	for _, request := range []struct {
		repo   domain.ContentHash
		cursor string
		limit  int
	}{
		{domain.ContentHash("bad"), "", 1},
		{gitEvidenceTestRepo, "", 0},
		{gitEvidenceTestRepo, "", 101},
		{gitEvidenceTestRepo, "bad", 1},
		{gitEvidenceTestRepo, strings.Repeat("g", 64), 1},
	} {
		for _, failure := range []error{nil, domain.ErrForbidden} {
			probe := newStatusProbe()
			probe.scopeError = failure
			queries := statusScanQueries(test, ScanConfig{Scope: probe, Metadata: probe, Scans: probe, Heads: probe})
			page, err := queries.ListScans(context.Background(), request.repo, request.cursor, request.limit)
			wantError := failure
			wantPage := domain.GitScanPage{}
			if failure == nil {
				wantError, wantPage.Items = domain.ErrValidation, []domain.GitScanJob{}
			}
			if !errors.Is(err, wantError) || !reflect.DeepEqual(page, wantPage) || !reflect.DeepEqual(probe.calls, []string{"scope"}) {
				test.Fatal("validation bypassed scope precedence or accessed storage", request, page, err, probe.calls)
			}
		}
	}
}

func TestScanQueriesPreserveErrorsPartialPagesAndAtomicFailures(test *testing.T) {
	for _, atomic := range []bool{false, true} {
		for _, stage := range []string{"scope", "scans", "metadata", "head", "finish", "missing", "wrapped missing"} {
			probe := newStatusProbe()
			probe.jobs = []domain.GitScanJob{{ID: "first"}, {ID: "extra"}}
			failure := errors.New("status reader failure")
			wantCalls := []string{"scope", "scans", "metadata", "head"}
			wantPage := domain.GitScanPage{Items: probe.jobs[:1], NextCursor: "first"}
			wantError := failure
			switch stage {
			case "scope":
				probe.scopeError, wantCalls, wantPage = failure, []string{"scope"}, domain.GitScanPage{}
			case "scans":
				probe.listError, wantCalls, wantPage = failure, []string{"scope", "scans"}, domain.GitScanPage{Items: []domain.GitScanJob{}}
			case "metadata":
				probe.repoError, wantCalls = failure, []string{"scope", "scans", "metadata"}
			case "head":
				probe.headError = failure
			case "finish":
				probe.finishError = failure
				wantPage.Reconciliation = &probe.head
			case "missing":
				probe.headError, wantError = domain.ErrNotFound, nil
			case "wrapped missing":
				probe.headError, wantError = fmt.Errorf("head: %w", domain.ErrNotFound), nil
			}
			queries := statusScanQueries(test, ScanConfig{Scope: probe, Metadata: probe, Scans: probe, Heads: probe, AtomicRead: atomic})
			page, err := queries.ListScans(context.Background(), gitEvidenceTestRepo, "", 1)
			if atomic && wantError != nil {
				wantPage = domain.GitScanPage{}
			}
			if !errors.Is(err, wantError) || !reflect.DeepEqual(page, wantPage) || !reflect.DeepEqual(probe.calls, wantCalls) {
				test.Fatal("error precedence, missing head, or atomic/legacy result changed", atomic, stage, page, err, probe.calls)
			}
		}
	}
}

func TestScanQueriesUseMetadataOriginWithoutNewIdentityRules(test *testing.T) {
	probe := newStatusProbe()
	probe.metadata.ID = domain.HashContent([]byte("historical metadata reply"))
	probe.metadata.GitRemoteURL = ""
	queries := statusScanQueries(test, ScanConfig{Scope: probe, Metadata: probe, Scans: probe, Heads: probe})
	page, err := queries.ListScans(context.Background(), gitEvidenceTestRepo, "", 1)
	if err != nil || page.Reconciliation == nil || probe.origin != "" || !reflect.DeepEqual(probe.calls, []string{"scope", "scans", "metadata", "head"}) {
		test.Fatal("scan orchestration introduced code-evidence identity/origin validation", page, err, probe.calls)
	}
}
