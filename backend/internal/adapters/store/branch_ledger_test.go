package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func branchLedgerStoreID(letter string) string { return strings.Repeat(letter, 32) }

func branchLedgerStoreTime(minute int) time.Time {
	return time.Date(2031, 1, 2, 3, minute, 0, 0, time.UTC)
}

func branchLedgerStoreLifetime(repo domain.ContentHash, id, name string, opened int, linked ...string) domain.BranchLifetime {
	lifetime := domain.BranchLifetime{RepoID: repo, LedgerID: id, Name: name, OpenedAt: branchLedgerStoreTime(opened),
		OpenedBy: domain.BranchEvidence{Kind: domain.BranchEvidenceRegistered, Account: "member-a"}, Linked: []domain.BranchLinkedID{}}
	for _, other := range linked {
		lifetime.Linked = append(lifetime.Linked, domain.BranchLinkedID{ID: other, LinkedAt: branchLedgerStoreTime(opened + 1),
			By: domain.BranchEvidence{Kind: domain.BranchEvidenceRegistered, Account: "member-b"}})
	}
	return lifetime
}

// checkBranchLedgerPersistence is the contract both stores keep.
func checkBranchLedgerPersistence(t *testing.T, ctx context.Context, storage outbound.BranchLedgerStore, repo domain.ContentHash) {
	t.Helper()
	a, b, c, d := branchLedgerStoreID("a"), branchLedgerStoreID("b"), branchLedgerStoreID("c"), branchLedgerStoreID("d")
	if listed, err := storage.ListBranchLifetimes(ctx, repo, ""); err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("an empty ledger: %#v %v", listed, err)
	}
	if _, err := storage.AliveBranchLifetime(ctx, repo, "topic"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("no lifetime under the name: %v", err)
	}
	first := branchLedgerStoreLifetime(repo, a, "topic", 1, b)
	if err := storage.PutBranchLifetime(ctx, first); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.AliveBranchLifetime(ctx, repo, "topic"); err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("alive under the name:\n got %+v\nwant %+v\n%v", got, first, err)
	}
	// Its own ID and one joined to it both lead to the lifetime.
	for _, id := range []string{a, b} {
		if got, err := storage.BranchLifetimeHolding(ctx, repo, id); err != nil || got.LedgerID != a {
			t.Fatalf("holder of %s: %+v %v", id[:1], got, err)
		}
	}
	if _, err := storage.BranchLifetimeHolding(ctx, repo, c); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an ID nobody holds: %v", err)
	}

	// A name alive on the remote has one lifetime.
	if err := storage.PutBranchLifetime(ctx, branchLedgerStoreLifetime(repo, c, "topic", 2)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a second lifetime alive under the name: %v", err)
	}
	// An ID belongs to one lifetime, as its own or joined.
	if err := storage.PutBranchLifetime(ctx, branchLedgerStoreLifetime(repo, c, "another", 2, b)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a joined ID held by two lifetimes: %v", err)
	}
	if err := storage.PutBranchLifetime(ctx, branchLedgerStoreLifetime(repo, b, "another", 2)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a joined ID as another lifetime's own: %v", err)
	}
	if listed, err := storage.ListBranchLifetimes(ctx, repo, ""); err != nil || len(listed) != 1 {
		t.Fatalf("a refused write left something: %+v %v", listed, err)
	}

	// Closing keeps the lifetime; the name is then free for another.
	closedAt := branchLedgerStoreTime(3)
	closed := first
	closed.ClosedAt, closed.ClosedBy = &closedAt, &domain.BranchEvidence{Kind: domain.BranchEvidenceHostingPush, Source: "observation-1"}
	if err := storage.PutBranchLifetime(ctx, closed); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.AliveBranchLifetime(ctx, repo, "topic"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a closed lifetime is still alive: %v", err)
	}
	if got, err := storage.BranchLifetimeHolding(ctx, repo, b); err != nil || got.Alive() || !got.ClosedAt.Equal(closedAt) {
		t.Fatalf("the closed lifetime: %+v %v", got, err)
	}
	second := branchLedgerStoreLifetime(repo, c, "topic", 4)
	if err := storage.PutBranchLifetime(ctx, second); err != nil {
		t.Fatal(err)
	}
	other := branchLedgerStoreLifetime(repo, d, "another", 5)
	if err := storage.PutBranchLifetime(ctx, other); err != nil {
		t.Fatal(err)
	}
	listed, err := storage.ListBranchLifetimes(ctx, repo, "topic")
	if err != nil || len(listed) != 2 || listed[0].LedgerID != a || listed[1].LedgerID != c {
		t.Fatalf("lifetimes of one name, oldest first: %+v %v", listed, err)
	}
	if all, err := storage.ListBranchLifetimes(ctx, repo, ""); err != nil || len(all) != 3 || all[2].LedgerID != d {
		t.Fatalf("every lifetime: %+v %v", all, err)
	}
	// Reopening the closed one while another is alive under the name is refused.
	if err := storage.PutBranchLifetime(ctx, first); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("reopening beside a living lifetime: %v", err)
	}
	// What is not a lifetime is never stored.
	invalid := branchLedgerStoreLifetime(repo, "not-an-id", "topic", 6)
	if err := storage.PutBranchLifetime(ctx, invalid); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an invalid lifetime: %v", err)
	}
	// Another repository has its own ledger.
	elsewhere := domain.HashContent([]byte(string(repo) + "/elsewhere"))
	if _, err := storage.BranchLifetimeHolding(ctx, elsewhere, a); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an ID of another repository: %v", err)
	}
}

func TestFSBranchLedgerPersistence(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	checkBranchLedgerPersistence(t, context.Background(), storage, repo)

	// A ledger changed on disk into one that breaks its promises is an
	// integrity failure, not another ledger.
	path := storage.branchLedgerPath(repo)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), `"name":"another"`, `"name":"topic"`, 1)
	if tampered == string(raw) {
		t.Fatal("the stored ledger does not hold the name to change")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ListBranchLifetimes(context.Background(), repo, ""); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("two lifetimes alive under one name on disk: %v", err)
	}
}
