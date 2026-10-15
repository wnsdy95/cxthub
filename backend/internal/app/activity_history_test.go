package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type activityTestStore interface {
	outbound.MetadataStore
	outbound.BlobStore
	outbound.HistoryStore
	outbound.RepositoryStore
}

type activityCountingStore struct {
	activityTestStore
	historyReads   int
	inventoryReads int
}

func (storage *activityCountingStore) ListRepos(ctx context.Context, team string) ([]domain.Repo, error) {
	storage.inventoryReads++
	return storage.activityTestStore.ListRepos(ctx, team)
}

func (storage *activityCountingStore) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	storage.historyReads++
	return storage.activityTestStore.ListHistoryEvents(ctx, repo)
}

func (storage *activityCountingStore) ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error) {
	panic("activity must not enumerate snapshots")
}

func (storage *activityCountingStore) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	panic("activity must not perform per-publication snapshot reads")
}

func (storage *activityCountingStore) WithinRepository(context.Context, domain.ContentHash, func(context.Context) error) error {
	panic("activity must not write")
}

func (storage *activityCountingStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if transactions, ok := storage.activityTestStore.(outbound.RepositoryTransactions); ok {
		return transactions.WithinReadSnapshot(ctx, read)
	}
	return read(ctx)
}

func TestActivityCommitHistoryFS(t *testing.T) {
	checkActivityCommitHistory(t, store.NewFSStore(t.TempDir()))
}

func checkActivityCommitHistory(t *testing.T, storage activityTestStore) {
	t.Helper()
	ctx := systemTestContext()
	owner := domain.User{ID: domain.NewID("user_"), Username: domain.NewID("owner-"), Name: "Owner"}
	if err := storage.UpsertUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	repository := domain.Repository{ID: domain.NewID("ws_"), OwnerID: owner.ID, OwnerUsername: owner.Username, Name: "History", Slug: "history", Visibility: domain.VisibilityPublic}
	if err := storage.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(repository.ID))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: repository.ID, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	putSnapshot := func(text, branch, message string, author domain.TeamIdentity) domain.ContentHash {
		t.Helper()
		doc := domain.SessionDoc{CIR: domain.CIRDocument{}}
		doc.CIR.Envelope.CIRVersion, doc.CIR.Envelope.SourceProvider = "1", "claude"
		doc.CIR.Events = []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}}
		raw, err := domain.CanonicalBytes(doc.CIR)
		if err != nil {
			t.Fatal(err)
		}
		doc.Hash = domain.HashContent(raw)
		if _, err := storage.PutDoc(ctx, repo, doc); err != nil {
			t.Fatal(err)
		}
		if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, Branch: branch, Message: message,
			Author: author, CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Provider: "claude", Fidelity: "full"}); err != nil {
			t.Fatal(err)
		}
		return doc.Hash
	}
	captureAuthor := domain.TeamIdentity{Name: "Collaborator, not the Git author", Email: "capture@example.com"}
	old := putSnapshot("old reused capture", "main", domain.HookMessagePrefix+"capture", captureAuthor)
	uncommitted := putSnapshot("not committed", "main", "[git abc123] not proof", domain.TeamIdentity{Name: owner.Name})
	stash := putSnapshot("stash later finalized", domain.StashBranchLabel, "stash", domain.TeamIdentity{})
	observed := &activityCountingStore{activityTestStore: storage}
	service := NewService(observed, storage, nil, nil, storage)
	assertDays := func(want map[string]int) {
		t.Helper()
		observed.inventoryReads, observed.historyReads = 0, 0
		got, err := service.Contributions(ctx, []string{repository.ID, repository.ID})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("days = %v, %v; want %v", got, err, want)
		}
		if observed.inventoryReads != 1 || observed.historyReads != 1 {
			t.Fatalf("reads = inventory %d, history %d", observed.inventoryReads, observed.historyReads)
		}
	}
	assertDays(map[string]int{})
	observation := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), Branch: "main", BranchID: "main", Kind: "position",
		Source: uncommitted, Target: uncommitted, GitAfter: strings.Repeat("f", 40), CreatedAt: time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)}
	if err := storage.ApplyHistoryEvent(ctx, observation); err != nil {
		t.Fatal(err)
	}
	assertDays(map[string]int{})
	publication := observation
	publication.Kind, publication.Source, publication.Target = "publish", old, old
	publication.ID, publication.GitAfter = strings.Repeat("2", 32), strings.Repeat("a", 40)
	publication.CreatedAt = time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	publish := func(event domain.HistoryEvent) {
		t.Helper()
		proof := event
		proof.ID, proof.Kind = strings.Repeat("e", 16)+event.ID[16:], "position"
		proof.CreatedAt = time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
		if err := storage.ApplyHistoryEvent(ctx, proof); err != nil {
			t.Fatalf("proof %s: %v", proof.ID, err)
		}
		for range 2 {
			if err := storage.ApplyHistoryEvent(ctx, event); err != nil {
				t.Fatalf("publication %s: %v", event.ID, err)
			}
		}
	}
	duplicate := publication
	duplicate.ID, duplicate.Source, duplicate.Target = strings.Repeat("3", 32), stash, stash
	duplicate.Branch, duplicate.BranchID = "feature", "feature"
	duplicate.CreatedAt = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	publish(duplicate)
	publish(publication)
	publication.ID, publication.GitAfter = strings.Repeat("4", 32), strings.Repeat("b", 40)
	publication.CreatedAt, _ = time.Parse(time.RFC3339, "2026-10-01T00:30:00+02:00")
	publish(publication)
	assertDays(map[string]int{"2026-09-01": 1, "2026-09-30": 1})
	months, err := service.Activity(ctx, []domain.Repository{repository})
	if err != nil || len(months) != 1 || months[0].Month != "2026-09" || months[0].CommitTotal != 2 || len(months[0].CommitRepos) != 1 || months[0].CommitRepos[0].Count != 2 {
		t.Fatalf("monthly parity = %+v, %v", months, err)
	}
	if got, err := service.Contributions(ctx, []string{domain.NewID("ws_")}); err != nil || len(got) != 0 {
		t.Fatalf("foreign scope counted: %v, %v", got, err)
	}
	if got, err := service.Activity(ctx, nil); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty scope = %v, %v", got, err)
	}
	changed := publication
	changed.CreatedAt = changed.CreatedAt.Add(time.Hour)
	if err := storage.ApplyHistoryEvent(ctx, changed); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatalf("mutated retry accepted: %v", err)
	}
	assertDays(map[string]int{"2026-09-01": 1, "2026-09-30": 1})
}
