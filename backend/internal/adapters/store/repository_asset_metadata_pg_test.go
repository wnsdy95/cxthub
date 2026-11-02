//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func repositoryAssetMetadataReadPG(test *testing.T, ctx context.Context, storage *PostgresStore, expected domain.Repo) {
	test.Helper()
	actual, err := storage.GetRepo(ctx, expected.ID)
	if err != nil || !reflect.DeepEqual(actual, expected) {
		test.Fatalf("GetRepo metadata=%+v, want %+v: %v", actual, expected, err)
	}
	repositories, err := storage.ListRepos(ctx, "default")
	if err != nil {
		test.Fatal("ListRepos metadata read failed", err)
	}
	matches := 0
	for _, repository := range repositories {
		if repository.ID == expected.ID {
			matches++
			if !reflect.DeepEqual(repository, expected) {
				test.Fatalf("ListRepos metadata=%+v, want %+v", repository, expected)
			}
		}
	}
	if matches != 1 {
		test.Fatal("ListRepos did not return the owned repository exactly once", matches)
	}
}

func repositoryAssetMetadataRowPG(test *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) string {
	test.Helper()
	var row string
	if err := storage.db(ctx).QueryRow(ctx, `SELECT to_jsonb(replica)::text FROM public.repos replica WHERE id=$1`, repo).Scan(&row); err != nil {
		test.Fatal("could not read the actual repository row", err)
	}
	return row
}

func TestPGRepositoryAssetMetadataRoundTripAndRollback(test *testing.T) {
	fixture, storage := settingsCodexCurrentPG(test)
	repo := settingsCodexRepositoryPG(test, fixture, storage)
	baseline, err := storage.GetRepo(fixture.ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	var description, website, topics *string
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT description,website,topics FROM public.repos WHERE id=$1`, repo).Scan(&description, &website, &topics); err != nil {
		test.Fatal(err)
	}
	if description != nil || website != nil || topics != nil || baseline.Description != "" || baseline.Website != "" || baseline.Topics != nil {
		test.Fatal("legacy SQL NULL about columns must remain empty strings and nil topics", baseline)
	}
	repositoryAssetMetadataReadPG(test, fixture.ctx, storage, baseline)
	for _, scenario := range []struct {
		name   string
		topics []string
	}{
		{name: "json-null", topics: nil},
		{name: "empty-array", topics: []string{}},
		{name: "duplicates-and-unicode", topics: []string{"\ud55c\uae00", "\u91cd\u590d", "\ud55c\uae00", "🧭", "e\u0301", "é"}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			expected := baseline
			expected.Description = "  \uc124\uba85 — " + scenario.name + "\nretained whitespace  "
			expected.Website = "https://example.test/\uacbd\ub85c?kind=" + scenario.name
			expected.Topics = scenario.topics
			if err := storage.UpdateRepoAbout(fixture.ctx, repo, expected.Description, expected.Website, expected.Topics); err != nil {
				test.Fatal(err)
			}
			beforeRead := repositoryAssetMetadataRowPG(test, fixture.ctx, storage, repo)
			repositoryAssetMetadataReadPG(test, fixture.ctx, storage, expected)
			if afterRead := repositoryAssetMetadataRowPG(test, fixture.ctx, storage, repo); afterRead != beforeRead {
				test.Fatal("metadata round-trip read repaired or normalized the actual row")
			}
		})
	}
	committed, err := storage.GetRepo(fixture.ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	before := repositoryAssetMetadataRowPG(test, fixture.ctx, storage, repo)
	rollback := errors.New("rollback actual uncommitted repository about writes")
	err = storage.WithinRepository(fixture.ctx, repo, func(bound context.Context) error {
		uncommitted := committed
		uncommitted.Description = "uncommitted \uc124\uba85"
		uncommitted.Website = "https://example.test/uncommitted"
		uncommitted.Topics = []string{"transaction", "\ud55c\uae00", "transaction"}
		if err := storage.UpdateRepoAbout(bound, repo, uncommitted.Description, uncommitted.Website, uncommitted.Topics); err != nil {
			return err
		}
		dirty := repositoryAssetMetadataRowPG(test, bound, storage, repo)
		if dirty == before {
			test.Fatal("transaction never changed the actual about columns")
		}
		repositoryAssetMetadataReadPG(test, bound, storage, uncommitted)
		if afterRead := repositoryAssetMetadataRowPG(test, bound, storage, repo); afterRead != dirty {
			test.Fatal("bound metadata read changed its uncommitted row")
		}
		var visible string
		if err := fixture.observer.QueryRow(fixture.ctx, `SELECT to_jsonb(replica)::text FROM public.repos replica WHERE id=$1`, repo).Scan(&visible); err != nil {
			return err
		}
		if visible != before {
			test.Fatal("uncommitted metadata escaped its bound transaction")
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		test.Fatal("repository metadata rollback failed", err)
	}
	if after := repositoryAssetMetadataRowPG(test, fixture.ctx, storage, repo); after != before {
		test.Fatal("rolled-back about write changed the actual repository row")
	}
	repositoryAssetMetadataReadPG(test, fixture.ctx, storage, committed)
}

func TestPGRepositoryAssetMetadataTopicsDecodeAndCorruptionWithoutRepair(test *testing.T) {
	fixture, storage := settingsCodexCurrentPG(test)
	repo := settingsCodexRepositoryPG(test, fixture, storage)
	baseline, err := storage.GetRepo(fixture.ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	before := repositoryAssetMetadataRowPG(test, fixture.ctx, storage, repo)
	for _, corruption := range []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "whitespace", raw: " \t\r\n"},
		{name: "malformed", raw: `["unterminated"`},
		{name: "object", raw: `{"topic":"unexpected"}`},
		{name: "string", raw: `"unexpected"`},
		{name: "number-element", raw: `["valid",42]`},
		{name: "boolean-element", raw: `[false]`},
		{name: "legacy-null-element", raw: `["valid",null]`},
		{name: "nested-array", raw: `[["nested"]]`},
	} {
		test.Run(corruption.name, func(test *testing.T) {
			rollback := errors.New("rollback owned corrupt topics fixture")
			err := storage.WithinRepository(fixture.ctx, repo, func(bound context.Context) error {
				if _, err := storage.db(bound).Exec(bound, `SET LOCAL session_replication_role=replica`); err != nil {
					return err
				}
				changed, err := storage.db(bound).Exec(bound, `UPDATE public.repos SET topics=$2 WHERE id=$1`, repo, corruption.raw)
				if err != nil {
					return err
				}
				if changed.RowsAffected() != 1 {
					test.Fatal("corruption fixture did not change exactly its owned repository")
				}
				dirty := repositoryAssetMetadataRowPG(test, bound, storage, repo)
				if corruption.name == "legacy-null-element" {
					expected := baseline
					if err := json.Unmarshal([]byte(corruption.raw), &expected.Topics); err != nil || !reflect.DeepEqual(expected.Topics, []string{"valid", ""}) {
						test.Fatal("legacy topics must retain the existing native Repo JSON decode semantics", err)
					}
					repositoryAssetMetadataReadPG(test, bound, storage, expected)
					if afterRead := repositoryAssetMetadataRowPG(test, bound, storage, repo); afterRead != dirty {
						test.Fatal("legacy null topic element was rewritten by a read")
					}
					return rollback
				}
				actual, err := storage.GetRepo(bound, repo)
				if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(actual, domain.Repo{}) {
					test.Fatal("GetRepo trusted corrupt topics or returned partial metadata", actual, err)
				}
				repositories, err := storage.ListRepos(bound, "default")
				if !errors.Is(err, domain.ErrIntegrity) || repositories != nil {
					test.Fatal("ListRepos trusted corrupt topics or returned partial metadata", repositories, err)
				}
				if afterRead := repositoryAssetMetadataRowPG(test, bound, storage, repo); afterRead != dirty {
					test.Fatal("corrupt topics were repaired or normalized by a read")
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				test.Fatal("corrupt-row transaction did not reach its rollback boundary", err)
			}
			if after := repositoryAssetMetadataRowPG(test, fixture.ctx, storage, repo); after != before {
				test.Fatal("corrupt topics or attempted read repair escaped rollback")
			}
			repositoryAssetMetadataReadPG(test, fixture.ctx, storage, baseline)
		})
	}
}
