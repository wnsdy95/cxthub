//go:build postgres

package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func pendingProjectionValidationFixture() (pendingProjectionWork, pendingProjectionSource, domain.RepositoryViewSourcePage) {
	repo := domain.HashContent([]byte("pending-projection-validation"))
	epoch := "11111111-1111-1111-1111-111111111111"
	work := pendingProjectionWork{PendingProjectionLease: domain.PendingProjectionLease{
		RepoID: repo, Epoch: epoch, Dataset: "22222222-2222-2222-2222-222222222222", Fence: 1,
		Phase: "capture", AppliedSequence: -1, InputSequence: -1}, enabled: true, retained: true, leased: true, pin: 2}
	source := pendingProjectionSource{epoch: epoch, head: 12, floor: 2, baseline: 1}
	checkpoint := pendingProjectionCheckpoint(repo, epoch, 10)
	page := domain.RepositoryViewSourcePage{Version: 1, Scope: domain.RepositoryViewSourceScope, RepoID: repo,
		Epoch: epoch, Through: 10, Mode: "baseline", Checkpoint: &checkpoint,
		Entries: []domain.RepositoryViewSourceEntry{{Sequence: 5, Kind: "pending", Key: "session-a", Value: json.RawMessage(`{"branch":"main"}`)}}}
	return work, source, page
}

func pendingProjectionValidationCursor(t *testing.T, work pendingProjectionWork, source pendingProjectionSource, page domain.RepositoryViewSourcePage) string {
	t.Helper()
	last := page.Entries[len(page.Entries)-1]
	cursor := repositoryViewSourceCursor{Checkpoint: pendingProjectionCheckpoint(work.RepoID, work.Epoch, page.Through),
		Floor: work.pin, Baseline: source.baseline, Mode: page.Mode, Sequence: last.Sequence, Kind: last.Kind, Key: last.Key}
	if page.Mode == "delta" {
		cursor.After = work.AppliedSequence
	}
	raw, err := json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestPendingProjectionStoragePageValidation(t *testing.T) {
	work, source, page := pendingProjectionValidationFixture()
	if err := validatePendingProjectionPage(work, source, page); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*domain.RepositoryViewSourcePage)
	}{
		{"version", func(page *domain.RepositoryViewSourcePage) { page.Version++ }},
		{"scope", func(page *domain.RepositoryViewSourcePage) { page.Scope = "sync-metadata-v1" }},
		{"repository", func(page *domain.RepositoryViewSourcePage) { page.RepoID = domain.HashContent([]byte("other")) }},
		{"epoch", func(page *domain.RepositoryViewSourcePage) { page.Epoch = "33333333-3333-3333-3333-333333333333" }},
		{"future", func(page *domain.RepositoryViewSourcePage) { page.Through = source.head + 1 }},
		{"before-floor", func(page *domain.RepositoryViewSourcePage) { page.Through = source.floor - 1 }},
		{"checkpoint", func(page *domain.RepositoryViewSourcePage) { page.Checkpoint = nil }},
		{"checkpoint-sequence", func(page *domain.RepositoryViewSourcePage) { page.Checkpoint.Sequence-- }},
		{"wrong-mode", func(page *domain.RepositoryViewSourcePage) { page.Mode = "delta" }},
		{"duplicate", func(page *domain.RepositoryViewSourcePage) { page.Entries = append(page.Entries, page.Entries[0]) }},
		{"missing-key", func(page *domain.RepositoryViewSourcePage) { page.Entries[0].Key = "" }},
		{"unknown-kind", func(page *domain.RepositoryViewSourcePage) { page.Entries[0].Kind = "secret" }},
		{"missing-image", func(page *domain.RepositoryViewSourcePage) { page.Entries[0].Value = nil }},
		{"null-image", func(page *domain.RepositoryViewSourcePage) { page.Entries[0].Value = json.RawMessage(`null`) }},
		{"array-image", func(page *domain.RepositoryViewSourcePage) { page.Entries[0].Value = json.RawMessage(`[]`) }},
		{"future-entry", func(page *domain.RepositoryViewSourcePage) { page.Entries[0].Sequence = page.Through + 1 }},
		{"baseline-delete", func(page *domain.RepositoryViewSourcePage) {
			page.Entries[0].Deleted = true
			page.Entries[0].Value = nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			work, source, page := pendingProjectionValidationFixture()
			test.change(&page)
			if err := validatePendingProjectionPage(work, source, page); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("malformed page accepted: %v", err)
			}
		})
	}
}

func TestPendingProjectionStorageCursorKeepsFixedThrough(t *testing.T) {
	for _, mode := range []string{"baseline", "delta"} {
		t.Run(mode, func(t *testing.T) {
			work, source, page := pendingProjectionValidationFixture()
			page.Mode = mode
			if mode == "delta" {
				work.AppliedSequence = 4
			}
			page.Checkpoint = nil
			page.NextCursor = pendingProjectionValidationCursor(t, work, source, page)
			if err := validatePendingProjectionPage(work, source, page); err != nil {
				t.Fatal(err)
			}
			work.Cursor = page.NextCursor
			if err := validatePendingProjectionPage(work, source, page); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("replayed page accepted: %v", err)
			}
			page.Entries[0].Key = "session-b"
			page.NextCursor = pendingProjectionValidationCursor(t, work, source, page)
			if err := validatePendingProjectionPage(work, source, page); err != nil {
				t.Fatalf("same-transaction continuation rejected: %v", err)
			}
			page.Through++
			page.NextCursor = pendingProjectionValidationCursor(t, work, source, page)
			if err := validatePendingProjectionPage(work, source, page); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("moving through accepted: %v", err)
			}
			page.Through--
			page.NextCursor = ""
			checkpoint := pendingProjectionCheckpoint(work.RepoID, work.Epoch, page.Through)
			page.Checkpoint = &checkpoint
			if err := validatePendingProjectionPage(work, source, page); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPendingProjectionStorageRejectsPartialWithoutProgressAndNoncanonicalCursor(t *testing.T) {
	work, source, page := pendingProjectionValidationFixture()
	page.Checkpoint = nil
	page.NextCursor = pendingProjectionValidationCursor(t, work, source, page)
	raw, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	page.NextCursor = base64.RawURLEncoding.EncodeToString(append(raw, ' '))
	if err := validatePendingProjectionPage(work, source, page); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("noncanonical cursor accepted: %v", err)
	}
	page.NextCursor = pendingProjectionValidationCursor(t, work, source, page)
	page.Entries = nil
	if err := validatePendingProjectionPage(work, source, page); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty partial page accepted: %v", err)
	}
}

func TestPendingProjectionStorageDeltaTombstone(t *testing.T) {
	work, source, page := pendingProjectionValidationFixture()
	work.AppliedSequence, page.Mode = 4, "delta"
	page.Entries[0].Deleted, page.Entries[0].Value = true, nil
	if err := validatePendingProjectionPage(work, source, page); err != nil {
		t.Fatal(err)
	}
	page.Entries[0].Sequence = work.AppliedSequence
	if err := validatePendingProjectionPage(work, source, page); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("old delta accepted: %v", err)
	}
}

func TestPendingProjectionStoragePageBudget(t *testing.T) {
	work, source, page := pendingProjectionValidationFixture()
	page.Entries[0].Value = json.RawMessage(`{"data":"` + strings.Repeat("x", domain.MaxRepositoryViewSourceImageBytes) + `"}`)
	if err := validatePendingProjectionPage(work, source, page); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
		t.Fatalf("oversized image accepted: %v", err)
	}
}

func TestPendingProjectionStorageLeaseFencing(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*pendingProjectionWork)
	}{
		{"disabled", func(work *pendingProjectionWork) { work.enabled = false }},
		{"expired", func(work *pendingProjectionWork) { work.leased = false }},
		{"retention-expired", func(work *pendingProjectionWork) { work.retained = false }},
		{"fence", func(work *pendingProjectionWork) { work.Fence++ }},
		{"dataset", func(work *pendingProjectionWork) { work.Dataset = "33333333-3333-3333-3333-333333333333" }},
		{"epoch", func(work *pendingProjectionWork) { work.Epoch = "33333333-3333-3333-3333-333333333333" }},
		{"cursor", func(work *pendingProjectionWork) { work.Cursor = "changed" }},
		{"applied", func(work *pendingProjectionWork) { work.AppliedSequence++ }},
		{"input", func(work *pendingProjectionWork) { work.InputSequence++ }},
		{"phase", func(work *pendingProjectionWork) { work.Phase = "project" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			work, _, _ := pendingProjectionValidationFixture()
			lease := work.PendingProjectionLease
			if !pendingProjectionLeaseValid(lease) || !pendingProjectionLeaseMatches(work, lease) {
				t.Fatal("valid lease rejected")
			}
			test.change(&work)
			if pendingProjectionLeaseMatches(work, lease) {
				t.Fatal("stale lease accepted")
			}
		})
	}
}

func TestPendingProjectionStorageLeaseDuration(t *testing.T) {
	storage := &PostgresStore{}
	work, _, _ := pendingProjectionValidationFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, duration := range []time.Duration{0, time.Second - 1, 5*time.Minute + 1, time.Second, time.Minute, 5 * time.Minute} {
		want := domain.ErrValidation
		if duration >= time.Second && duration <= 5*time.Minute {
			want = context.Canceled
		}
		if _, err := storage.ClaimPendingProjection(ctx, duration); !errors.Is(err, want) {
			t.Fatalf("claim duration %s: %v, want %v", duration, err, want)
		}
		if err := storage.RenewPendingProjection(ctx, work.PendingProjectionLease, duration); !errors.Is(err, want) {
			t.Fatalf("renew duration %s: %v, want %v", duration, err, want)
		}
	}
}

func TestPendingProjectionStorageRejectsCallerWritesAndForeignTransactions(t *testing.T) {
	storage := &PostgresStore{}
	work, _, _ := pendingProjectionValidationFixture()
	for _, owner := range []*PostgresStore{storage, {}} {
		ctx := context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: owner, repo: work.RepoID})
		if err := storage.ConfigurePendingProjection(ctx, work.RepoID, true); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("configuration escaped caller transaction: %v", err)
		}
		if _, err := storage.ClaimPendingProjection(ctx, time.Minute); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("claim escaped caller transaction: %v", err)
		}
		if _, err := storage.ReadPendingProjection(ctx, work.RepoID); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("read escaped caller transaction: %v", err)
		}
		lease := work.PendingProjectionLease
		lease.Phase, lease.InputSequence = "project", 10
		if _, err := storage.LoadPendingProjectionInputs(ctx, lease); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("inputs escaped caller transaction: %v", err)
		}
	}
}

func TestPendingProjectionStorageValidatesOutputCodecAndBudget(t *testing.T) {
	work, _, _ := pendingProjectionValidationFixture()
	ctx := context.Background()
	parts, err := domain.EncodePendingProjection(ctx, work.RepoID, domain.PendingStateView{Projection: "on-hold-v1", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePendingProjectionParts(ctx, work.RepoID, parts); err != nil {
		t.Fatal(err)
	}
	if err := validatePendingProjectionParts(ctx, domain.HashContent([]byte("other")), parts); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("cross-repository output accepted: %v", err)
	}
	if err := validatePendingProjectionParts(ctx, work.RepoID, append(parts, parts[0])); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("duplicate output accepted: %v", err)
	}
	if err := validatePendingProjectionParts(ctx, work.RepoID, nil); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("missing header accepted: %v", err)
	}
	parts[0].Value = json.RawMessage(`{"data":"` + strings.Repeat("x", domain.MaxPendingProjectionOutputBytes) + `"}`)
	if err := validatePendingProjectionParts(ctx, work.RepoID, parts); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
		t.Fatalf("oversized output accepted: %v", err)
	}
}

func TestPendingProjectionStorageReadyKeyIndex(t *testing.T) {
	header := domain.PendingProjectionPart{Kind: "header", Key: "view", Value: json.RawMessage(`{"order":{"archive":[],"hold":[],"pending":["current-session"],"ref":["current-ref"],"snapshot":[]}}`)}
	kinds, keys, err := pendingProjectionReadyPartKeys(header)
	if err != nil || len(keys) != 2 || len(kinds) != 2 || kinds[0] != "pending" || keys[0] != "current-session" || kinds[1] != "ref" || keys[1] != "current-ref" {
		t.Fatalf("ready key pointers: kinds=%v keys=%v err=%v", kinds, keys, err)
	}
	for _, raw := range []string{
		`{"order":{"archive":[],"hold":[],"pending":["duplicate","duplicate"],"ref":[],"snapshot":[]}}`,
		`{"order":{"archive":[],"hold":[],"pending":[""],"ref":[],"snapshot":[]}}`,
		`{"order":{"archive":[],"hold":[],"pending":[],"ref":[],"unknown":[]}}`,
		`{"order":{"archive":[],"hold":[],"pending":[],"ref":[]}}`,
	} {
		header.Value = json.RawMessage(raw)
		if _, _, err := pendingProjectionReadyPartKeys(header); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatalf("invalid ready key index accepted: %v", err)
		}
	}
	order := map[string][]string{"archive": {}, "hold": {}, "pending": make([]string, domain.MaxPendingProjectionOutputRows), "ref": {}, "snapshot": {}}
	raw, err := json.Marshal(map[string]any{"order": order})
	if err != nil {
		t.Fatal(err)
	}
	header.Value = raw
	if _, _, err := pendingProjectionReadyPartKeys(header); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
		t.Fatalf("oversized ready key index accepted: %v", err)
	}
}
