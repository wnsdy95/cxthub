//go:build postgres

package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestRepositoryGraphSourceProfileCursorContract(t *testing.T) {
	legacy := repositoryViewSourceCursor{
		Checkpoint: domain.RepositoryViewSourceCheckpoint{Version: 1, Scope: domain.RepositoryGraphSourceScope,
			RepoID: domain.HashContent([]byte(t.Name())), Epoch: "12345678-1234-1234-1234-123456789abc", Sequence: 9},
		Floor: 2, Baseline: 3, Mode: "baseline", Sequence: 3, Kind: "git_commit", Key: "commit",
	}
	for _, scenario := range []struct {
		name    string
		profile string
		kind    string
		scope   string
		want    error
	}{
		{"legacy", "", "git_commit", domain.RepositoryGraphSourceScope, nil},
		{"legacy tree", "", "git_tree", domain.RepositoryGraphSourceScope, nil},
		{"legacy delta", "", "git_delta", domain.RepositoryGraphSourceScope, nil},
		{"projection", domain.RepositoryGraphProjectionSourceProfile, "git_commit", domain.RepositoryGraphSourceScope, nil},
		{"unknown", "unknown", "git_commit", domain.RepositoryGraphSourceScope, domain.ErrValidation},
		{"wrong scope", domain.RepositoryGraphProjectionSourceProfile, "snapshot", domain.RepositoryViewSourceScope, domain.ErrValidation},
		{"excluded tree", domain.RepositoryGraphProjectionSourceProfile, "git_tree", domain.RepositoryGraphSourceScope, domain.ErrValidation},
		{"excluded delta", domain.RepositoryGraphProjectionSourceProfile, "git_delta", domain.RepositoryGraphSourceScope, domain.ErrValidation},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			cursor := legacy
			cursor.Profile, cursor.Kind, cursor.Checkpoint.Scope = scenario.profile, scenario.kind, scenario.scope
			raw, err := json.Marshal(cursor)
			if err != nil {
				t.Fatal(err)
			}
			if cursor.Profile == "" && strings.Contains(string(raw), `"profile"`) {
				t.Fatal("legacy cursor wire format changed")
			}
			decoded, err := decodeRepositoryViewSourceCursor(base64.RawURLEncoding.EncodeToString(raw))
			if !errors.Is(err, scenario.want) || err == nil && decoded != cursor {
				t.Fatalf("cursor profile contract changed: %+v %v", decoded, err)
			}
		})
	}
}

func TestRepositoryGraphSourceProfileRejectsInvalidRequestsBeforeRead(t *testing.T) {
	storage := &PostgresStore{}
	repo := domain.HashContent([]byte(t.Name()))
	for _, profile := range []string{domain.RepositoryGraphProjectionSourceProfile, "unknown"} {
		page, err := storage.RepositoryViewSourceChanges(context.Background(), repo, domain.RepositoryViewSourceRequest{Version: 1, Profile: profile})
		if !errors.Is(err, domain.ErrValidation) || !reflect.DeepEqual(page, domain.RepositoryViewSourcePage{}) {
			t.Fatalf("metadata source accepted graph-only profile: %+v %v", page, err)
		}
	}
	page, err := storage.RepositoryGraphSourceChanges(context.Background(), repo, domain.RepositoryViewSourceRequest{Version: 1, Profile: "unknown"})
	if !errors.Is(err, domain.ErrValidation) || !reflect.DeepEqual(page, domain.RepositoryViewSourcePage{}) {
		t.Fatalf("graph source accepted unknown profile: %+v %v", page, err)
	}
}

func TestPGRepositoryGraphSourceProfileFiltersOnlyUnusedKinds(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	before, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	seedGraphSourcePG(t, ctx, storage.pool, repo)
	for _, mode := range []string{"baseline", "delta"} {
		t.Run(mode, func(t *testing.T) {
			request := domain.RepositoryViewSourceRequest{Version: 1, Limit: 1}
			if mode == "delta" {
				request.After = &before
			}
			full, checkpoint := graphSourceDrainPG(t, ctx, storage, repo, request)
			var want []domain.RepositoryViewSourceEntry
			for _, entry := range full {
				if entry.Kind != "git_tree" && entry.Kind != "git_delta" {
					want = append(want, entry)
				}
			}
			request.Profile = domain.RepositoryGraphProjectionSourceProfile
			got, filteredCheckpoint := graphSourceDrainPG(t, ctx, storage, repo, request)
			if len(full)-len(want) != 2 || !reflect.DeepEqual(got, want) || filteredCheckpoint != checkpoint {
				t.Fatal("profile changed retained proofs, ordering, or source generation")
			}
		})
	}
}

func TestPGRepositoryGraphSourceProfileFiltersBeforeSizingAndLimit(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	before, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	transaction := catalogTxPG(t, ctx, storage)
	catalogExecPG(t, ctx, transaction, `INSERT INTO git_tree_nodes(repo_id,origin,oid,payload)
		VALUES($1,'https://github.com/example/source','tree',jsonb_build_object('large',repeat('x',$2)))`, string(repo), domain.MaxRepositoryViewSourceImageBytes+1)
	catalogExecPG(t, ctx, transaction, `INSERT INTO git_commit_deltas(repo_id,id,origin,commit_oid,parent_oid,payload,keys,inverse_keys)
		VALUES($1,'delta','https://github.com/example/source','commit','parent',jsonb_build_object('large',repeat('x',$2)),'{}','{}')`, string(repo), domain.MaxRepositoryViewSourceImageBytes+1)
	catalogExecPG(t, ctx, transaction, `INSERT INTO git_ref_observations(repo_id,id,payload,observed_at)
		VALUES($1,'kept','{}',now())`, string(repo))
	if err := transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"baseline", "delta"} {
		t.Run(mode, func(t *testing.T) {
			request := domain.RepositoryViewSourceRequest{Version: 1, Profile: domain.RepositoryGraphProjectionSourceProfile, Limit: 1}
			if mode == "delta" {
				request.After = &before
			}
			read, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackPG(read)
			observed := &repositoryViewSourceBudgetTx{Tx: read}
			bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: observed, owner: storage, readOnly: true})
			page, err := storage.RepositoryGraphSourceChanges(bound, repo, request)
			if err != nil || page.Profile != request.Profile || len(page.Entries) != 1 || page.Entries[0].Kind != "git_observation" || page.Entries[0].Key != "kept" {
				t.Fatalf("unused oversized images reached sizing or pagination: %+v %v", page, err)
			}
			if len(observed.queries) != 2 || observed.queries[0].images != 0 || observed.queries[0].rows > 2 || observed.queries[1].images != 1 || observed.queries[1].imageBytes != len(page.Entries[0].Value) {
				t.Fatal("profile transferred excluded payloads or exceeded descriptor limit", observed.queries)
			}
			if mode == "delta" && (page.NextCursor != "" || page.Checkpoint == nil || page.Checkpoint.Sequence <= before.Sequence) {
				t.Fatal("excluded rows consumed the page limit or lost checkpoint progress")
			}
			request.Profile = ""
			page, err = storage.RepositoryGraphSourceChanges(bound, repo, request)
			if !errors.Is(err, domain.ErrRepositoryViewSourceLimit) || !reflect.DeepEqual(page, domain.RepositoryViewSourcePage{}) {
				t.Fatalf("full source no longer enforces its image budget: %+v %v", page, err)
			}
		})
	}
	after, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE git_ref_observations SET payload=jsonb_build_object('large',repeat('x',$2)) WHERE repo_id=$1`, string(repo), domain.MaxRepositoryViewSourceImageBytes+1)
	page, err := storage.RepositoryGraphSourceChanges(ctx, repo, domain.RepositoryViewSourceRequest{Version: 1, Profile: domain.RepositoryGraphProjectionSourceProfile, After: &after})
	if !errors.Is(err, domain.ErrRepositoryViewSourceLimit) || !reflect.DeepEqual(page, domain.RepositoryViewSourcePage{}) {
		t.Fatalf("projection profile bypassed the retained evidence budget: %+v %v", page, err)
	}
}

func TestPGRepositoryGraphSourceProfileCursorIsolationAndPinnedImages(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	before, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	seedGraphSourcePG(t, ctx, storage.pool, repo)
	for _, mode := range []string{"baseline", "delta"} {
		for _, profile := range []string{"", domain.RepositoryGraphProjectionSourceProfile} {
			t.Run(mode+"/"+profile, func(t *testing.T) {
				request := domain.RepositoryViewSourceRequest{Version: 1, Profile: profile, Limit: 1}
				if mode == "delta" {
					request.After = &before
				}
				want, checkpoint := graphSourceDrainPG(t, ctx, storage, repo, request)
				first, err := storage.RepositoryGraphSourceChanges(ctx, repo, request)
				if err != nil || first.Profile != profile || first.NextCursor == "" {
					t.Fatalf("missing profile-bound continuation: %+v %v", first, err)
				}
				cursor, err := decodeRepositoryViewSourceCursor(first.NextCursor)
				if err != nil || cursor.Profile != profile || cursor.Checkpoint != checkpoint {
					t.Fatalf("cursor lost profile or checkpoint: %+v %v", cursor, err)
				}
				request.After, request.Cursor = nil, first.NextCursor
				other := request
				other.Profile = domain.RepositoryGraphProjectionSourceProfile
				if profile != "" {
					other.Profile = ""
				}
				page, err := storage.RepositoryGraphSourceChanges(ctx, repo, other)
				if !errors.Is(err, domain.ErrCatalogResetRequired) || !reflect.DeepEqual(page, domain.RepositoryViewSourcePage{}) {
					t.Fatalf("cross-profile continuation accepted: %+v %v", page, err)
				}
				catalogExecPG(t, ctx, storage.pool, `UPDATE git_commit_trees SET payload=jsonb_build_object('commit',$2::text) WHERE repo_id=$1`, string(repo), t.Name())
				rest, fixed := graphSourceDrainPG(t, ctx, storage, repo, request)
				if fixed != checkpoint || !reflect.DeepEqual(append(first.Entries, rest...), want) {
					t.Fatal("profile continuation borrowed live proof mutations")
				}
			})
		}
	}
}

func TestPGRepositoryGraphSourceProfileExcludedOnlyDeltaStillAdvances(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	before, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, storage.pool, `INSERT INTO git_tree_nodes(repo_id,origin,oid,payload)
		VALUES($1,'https://github.com/example/source','tree','{}')`, string(repo))
	page, err := storage.RepositoryGraphSourceChanges(ctx, repo, domain.RepositoryViewSourceRequest{Version: 1, Profile: domain.RepositoryGraphProjectionSourceProfile, After: &before, Limit: 1})
	current, generationErr := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil || generationErr != nil || page.Profile != domain.RepositoryGraphProjectionSourceProfile || len(page.Entries) != 0 || page.NextCursor != "" || page.Checkpoint == nil || *page.Checkpoint != current || current.Sequence <= before.Sequence {
		t.Fatalf("empty filtered delta failed to advance honestly: %+v %v %v", page, err, generationErr)
	}
	full, checkpoint := graphSourceDrainPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &before})
	if len(full) != 1 || full[0].Kind != "git_tree" || checkpoint != current {
		t.Fatal("filtered read changed the full source or generation")
	}
}
