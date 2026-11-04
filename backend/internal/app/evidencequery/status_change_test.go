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

type statusChangeProbe struct {
	*statusProbe
	job       domain.GitChangeJob
	summaries []domain.GitChangeSummary
	getError  error
	getID     string
}

func (probe *statusChangeProbe) GetGitChange(ctx context.Context, repo domain.ContentHash, id string) (domain.GitChangeJob, error) {
	if err := probe.record(ctx, repo, "change"); err != nil {
		return domain.GitChangeJob{}, err
	}
	probe.getID = id
	return probe.job, probe.getError
}

func (probe *statusChangeProbe) ListGitChanges(ctx context.Context, repo domain.ContentHash, cursor string, limit int) ([]domain.GitChangeSummary, error) {
	if err := probe.record(ctx, repo, "changes"); err != nil {
		return nil, err
	}
	probe.cursor, probe.limit = cursor, limit
	return probe.summaries, probe.listError
}

func statusChangeQueries(test testing.TB, config ChangeConfig) *ChangeQueries {
	test.Helper()
	queries, err := NewChangeQueries(config)
	if err != nil {
		test.Fatal(err)
	}
	return queries
}

func TestChangeConstructorBindsOnlyExplicitReadersWithoutIO(test *testing.T) {
	probe := &statusChangeProbe{statusProbe: newStatusProbe()}
	queries := statusChangeQueries(test, ChangeConfig{Scope: probe, Changes: probe, AtomicRead: true})
	if queries.scope != probe || queries.changes != probe || !queries.atomicRead || len(probe.calls) != 0 {
		test.Fatal("constructor substituted declared dependencies or accessed storage", probe.calls)
	}
	var missing *statusChangeProbe
	for _, config := range []ChangeConfig{
		{}, {Changes: probe}, {Scope: probe}, {Scope: missing, Changes: probe}, {Scope: probe, Changes: missing},
	} {
		if queries, err := NewChangeQueries(config); queries != nil || !errors.Is(err, domain.ErrValidation) || len(probe.calls) != 0 {
			test.Fatal("absent or typed-nil dependency accepted or read", queries, err, probe.calls)
		}
	}
}

func TestChangeQueriesGetPreservesRawEvidenceAndErrorResults(test *testing.T) {
	for _, atomic := range []bool{false, true} {
		for _, stage := range []string{"success", "scope", "missing", "reader", "finish"} {
			probe := &statusChangeProbe{statusProbe: newStatusProbe()}
			probe.job = domain.GitChangeJob{ID: strings.Repeat("a", 64), State: "completed", Version: 9007199254740993,
				Result: &domain.GitReversalEvidence{}}
			failure := errors.New("change query failure")
			wantError := error(nil)
			wantCalls := []string{"scope", "change"}
			switch stage {
			case "scope":
				probe.scopeError, wantError, wantCalls = failure, failure, []string{"scope"}
			case "missing":
				probe.getError = fmt.Errorf("change: %w", domain.ErrNotFound)
				wantError = probe.getError
			case "reader":
				probe.getError, wantError = failure, failure
			case "finish":
				probe.finishError, wantError = failure, failure
			}
			view, err := statusChangeQueries(test, ChangeConfig{Scope: probe, Changes: probe, AtomicRead: atomic}).Get(context.Background(), gitEvidenceTestRepo, probe.job.ID)
			want := probe.job
			if stage == "scope" || atomic && wantError != nil {
				want = domain.GitChangeJob{}
			}
			if !errors.Is(err, wantError) || !reflect.DeepEqual(view, want) || !reflect.DeepEqual(probe.calls, wantCalls) || stage != "scope" && probe.getID != probe.job.ID {
				test.Fatal("get changed evidence, failure precedence, or pinned result policy", atomic, stage, view, err, probe.calls)
			}
		}
	}
}

func TestChangeQueriesListUsesLimitPlusOneAndPreservesNilItems(test *testing.T) {
	for _, summaries := range [][]domain.GitChangeSummary{nil, {}, {{ID: "first"}}, {{ID: "first"}, {ID: "extra"}}} {
		probe := &statusChangeProbe{statusProbe: newStatusProbe(), summaries: summaries}
		cursor := strings.Repeat("b", 64)
		page, err := statusChangeQueries(test, ChangeConfig{Scope: probe, Changes: probe}).List(context.Background(), gitEvidenceTestRepo, cursor, 1)
		want := domain.GitChangePage{Items: summaries}
		if len(summaries) > 1 {
			want.Items, want.NextCursor = summaries[:1], summaries[0].ID
		}
		if err != nil || !reflect.DeepEqual(page, want) || probe.cursor != cursor || probe.limit != 2 || !reflect.DeepEqual(probe.calls, []string{"scope", "changes"}) {
			test.Fatal("summary pagination, null/empty shape, or scope changed", page, err, probe)
		}
	}
	probe := &statusChangeProbe{statusProbe: newStatusProbe()}
	if _, err := statusChangeQueries(test, ChangeConfig{Scope: probe, Changes: probe}).List(context.Background(), gitEvidenceTestRepo, "", 100); err != nil || probe.limit != 101 {
		test.Fatal("maximum valid page did not request the extra cursor row", err, probe.limit)
	}
}

func TestChangeQueriesValidateAfterScopeBeforeStorage(test *testing.T) {
	for _, atomic := range []bool{false, true} {
		for _, request := range []struct {
			repo   domain.ContentHash
			cursor string
			limit  int
		}{
			{domain.ContentHash("bad"), "", 1}, {gitEvidenceTestRepo, "bad", 1},
			{gitEvidenceTestRepo, "", 0}, {gitEvidenceTestRepo, "", 101},
		} {
			probe := &statusChangeProbe{statusProbe: newStatusProbe()}
			queries := statusChangeQueries(test, ChangeConfig{Scope: probe, Changes: probe, AtomicRead: atomic})
			page, err := queries.List(context.Background(), request.repo, request.cursor, request.limit)
			want := domain.GitChangePage{Items: []domain.GitChangeSummary{}}
			if atomic {
				want = domain.GitChangePage{}
			}
			failure := error(domain.ErrValidation)
			if domain.ValidateContentHash(request.repo) != nil {
				failure = domain.ErrIntegrity
			}
			if !errors.Is(err, failure) || !reflect.DeepEqual(page, want) || !reflect.DeepEqual(probe.calls, []string{"scope"}) {
				test.Fatal("list validation moved or touched storage", atomic, request, page, err, probe.calls)
			}
			probe.calls, probe.scopeError = nil, domain.ErrForbidden
			page, err = queries.List(context.Background(), request.repo, request.cursor, request.limit)
			if !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(page, domain.GitChangePage{}) || !reflect.DeepEqual(probe.calls, []string{"scope"}) {
				test.Fatal("scope failure lost precedence over invalid list request", page, err, probe.calls)
			}
		}
	}
	for _, request := range []struct {
		repo domain.ContentHash
		id   string
	}{{domain.ContentHash("bad"), strings.Repeat("a", 64)}, {gitEvidenceTestRepo, "bad"}} {
		probe := &statusChangeProbe{statusProbe: newStatusProbe()}
		view, err := statusChangeQueries(test, ChangeConfig{Scope: probe, Changes: probe}).Get(context.Background(), request.repo, request.id)
		failure := error(domain.ErrValidation)
		if domain.ValidateContentHash(request.repo) != nil {
			failure = domain.ErrIntegrity
		}
		if !errors.Is(err, failure) || !reflect.DeepEqual(view, domain.GitChangeJob{}) || !reflect.DeepEqual(probe.calls, []string{"scope"}) {
			test.Fatal("get validation moved behind evidence reader", view, err, probe.calls)
		}
	}
}

func TestChangeQueriesListRetainsLegacyAndAtomicErrorShapes(test *testing.T) {
	for _, atomic := range []bool{false, true} {
		for _, finish := range []bool{false, true} {
			probe := &statusChangeProbe{statusProbe: newStatusProbe(), summaries: []domain.GitChangeSummary{{ID: "first"}, {ID: "extra"}}}
			failure := errors.New("change list failure")
			want := domain.GitChangePage{Items: []domain.GitChangeSummary{}}
			if finish {
				probe.finishError = failure
				want.Items, want.NextCursor = probe.summaries[:1], "first"
			} else {
				probe.listError = failure
			}
			if atomic {
				want = domain.GitChangePage{}
			}
			page, err := statusChangeQueries(test, ChangeConfig{Scope: probe, Changes: probe, AtomicRead: atomic}).List(context.Background(), gitEvidenceTestRepo, "", 1)
			if !errors.Is(err, failure) || !reflect.DeepEqual(page, want) || !reflect.DeepEqual(probe.calls, []string{"scope", "changes"}) {
				test.Fatal("list failure leaked reader rows, lost partial cursor, or leaked atomic result", atomic, finish, page, err, probe.calls)
			}
		}
	}
}
