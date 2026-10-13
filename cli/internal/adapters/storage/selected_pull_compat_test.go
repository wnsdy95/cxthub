package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

func pullReceiptFixture(test *testing.T, format string) (*FileStore, outbound.SelectedPullPlan, []byte) {
	test.Helper()
	root := test.TempDir()
	code := strings.Repeat("a", 40)
	store := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", code)
	repo := domain.HashContent([]byte("synthetic receipt repository"))
	snapshot := domain.HashContent([]byte("synthetic snapshot"))
	plan := outbound.SelectedPullPlan{
		Version: 1, RepoID: repo, Remote: "https://receipt.example.test/api/v1",
		Expected: outbound.CheckoutState{Position: &domain.WorkingPosition{
			RepoID: repo, WorktreeID: store.worktreeID, Branch: "main", BranchID: "synthetic-branch", GitCommit: code, Snapshot: snapshot,
		}},
		Selection: domain.ContextSelection{CodeCommit: code, Branch: "main", Position: snapshot, Scope: "current"},
		Context: domain.ContextQueryView{
			Version: 1, Position: snapshot, StateHash: domain.HashContent([]byte("synthetic context")),
			Inclusion: &domain.BranchContext{
				BranchID: "synthetic-branch", SnapshotID: snapshot,
				Merges: []domain.BranchContextMerge{
					{EventID: strings.Repeat("b", 32), Source: snapshot, MergeSHA: code, PRNumber: 1, State: "included", Reason: "synthetic"},
					{EventID: strings.Repeat("c", 32), Source: snapshot, MergeSHA: code, PRNumber: 2, State: "included", Reason: "synthetic", Order: 1},
				},
			},
		},
		Memory: []domain.EffectiveMemoryPage{{Items: []domain.EffectiveMemoryItem{{Text: "synthetic memory"}}}},
	}
	if format == "acknowledged" {
		plan.Context.Inclusion.Merges[0].DeliveryAcknowledged = true
	}
	if format == "no-merges" {
		plan.Context.Inclusion.Merges = nil
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		test.Fatal(err)
	}
	if format == "legacy" {
		raw = bytes.ReplaceAll(raw, []byte(`"delivery_acknowledged":false,`), nil)
	}
	plan.ID = domain.HashContent(raw)
	raw = bytes.Replace(raw, []byte(`"id":""`), []byte(`"id":"`+plan.ID+`"`), 1)
	return store, plan, append(append([]byte(`{"plan":`), raw...), []byte(`,"applied_at":"2026-01-02T03:04:05Z"}`)...)
}

func writePullReceiptFixture(test *testing.T, store *FileStore, plan outbound.SelectedPullPlan, raw []byte) string {
	test.Helper()
	path, err := store.appliedPullPath(plan.RepoID, plan.Remote)
	if err != nil {
		test.Fatal(err)
	}
	if err := writeAtomic(path, raw); err != nil {
		test.Fatal(err)
	}
	return path
}

func TestAppliedPullReceiptCompatibilityPreservesIdentityAndFiles(test *testing.T) {
	for _, format := range []string{"legacy", "current", "acknowledged", "no-merges"} {
		test.Run(format, func(test *testing.T) {
			store, plan, raw := pullReceiptFixture(test, format)
			if format == "legacy" {
				var unadapted outbound.SelectedPullReceipt
				if err := json.Unmarshal(raw, &unadapted); err != nil {
					test.Fatal(err)
				}
				if outbound.SelectedPullPlanID(unadapted.Plan) == plan.ID {
					test.Fatal("fixture does not reproduce the legacy receipt hash mismatch")
				}
			}
			path := writePullReceiptFixture(test, store, plan, raw)
			immutable := filepath.Join(store.storeDir(), "pull-receipts", hexOf(plan.ID)+".json")
			if err := writeAtomic(immutable, raw); err != nil {
				test.Fatal(err)
			}
			before := catalogTree(test, store.storeDir())
			info, err := os.Stat(path)
			if err != nil {
				test.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				receipt, err := store.ReadAppliedPull(context.Background(), plan.RepoID, plan.Remote)
				if err != nil {
					test.Fatal(err)
				}
				if receipt.Plan.ID != plan.ID || outbound.SelectedPullPlanID(receipt.Plan) != plan.ID {
					test.Fatal("receipt identity changed during validation")
				}
				encoded, err := json.Marshal(receipt)
				if err != nil || !bytes.Equal(encoded, raw) {
					test.Fatalf("receipt encoding changed: %v", err)
				}
				if format != "no-merges" && receipt.Plan.Context.Inclusion.Merges[0].DeliveryAcknowledged != (format == "acknowledged") {
					test.Fatal("acknowledgement changed")
				}
				if format == "legacy" {
					receipt.Plan.Context.Inclusion.Merges[0].DeliveryAcknowledged = true
					if outbound.SelectedPullPlanID(receipt.Plan) == plan.ID {
						test.Fatal("legacy encoding hid an in-memory acknowledgement change")
					}
				}
			}
			after, err := os.Stat(path)
			if err != nil || !after.ModTime().Equal(info.ModTime()) || !reflect.DeepEqual(before, catalogTree(test, store.storeDir())) {
				test.Fatalf("receipt read mutated the store: %v", err)
			}
		})
	}
}

func TestAppliedPullReceiptCompatibilityRejectsMutations(test *testing.T) {
	for _, fixture := range []struct {
		name, format, before, after string
		all                         bool
	}{
		{"legacy-add-false", "legacy", `"destination_branch_id":`, `"delivery_acknowledged":false,"destination_branch_id":`, true},
		{"legacy-add-true", "legacy", `"destination_branch_id":`, `"delivery_acknowledged":true,"destination_branch_id":`, true},
		{"legacy-mixed-false", "legacy", `"destination_branch_id":`, `"delivery_acknowledged":false,"destination_branch_id":`, false},
		{"legacy-null", "legacy", `"destination_branch_id":`, `"delivery_acknowledged":null,"destination_branch_id":`, true},
		{"current-strip-false", "current", `"delivery_acknowledged":false,`, ``, true},
		{"current-mixed-absence", "current", `"delivery_acknowledged":false,`, ``, false},
		{"current-promote-false", "current", `"delivery_acknowledged":false`, `"delivery_acknowledged":true`, false},
		{"acknowledged-downgrade", "acknowledged", `"delivery_acknowledged":true`, `"delivery_acknowledged":false`, false},
		{"acknowledged-strip", "acknowledged", `"delivery_acknowledged":true,`, ``, false},
		{"acknowledged-null", "acknowledged", `"delivery_acknowledged":true`, `"delivery_acknowledged":null`, false},
		{"duplicate-true-false", "current", `"delivery_acknowledged":false`, `"delivery_acknowledged":true,"delivery_acknowledged":false`, false},
		{"duplicate-false-true", "acknowledged", `"delivery_acknowledged":true`, `"delivery_acknowledged":false,"delivery_acknowledged":true`, false},
		{"duplicate-case-alias", "current", `"delivery_acknowledged":false`, `"DELIVERY_ACKNOWLEDGED":true,"delivery_acknowledged":false`, false},
		{"legacy-merge-content", "legacy", `"reason":"synthetic"`, `"reason":"altered"`, false},
		{"current-merge-content", "current", `"reason":"synthetic"`, `"reason":"altered"`, false},
		{"legacy-memory-content", "legacy", `"text":"synthetic memory"`, `"text":"altered memory"`, false},
		{"legacy-remote", "legacy", `https://receipt.example.test/api/v1`, `https://other.example.test/api/v1`, false},
		{"legacy-version", "legacy", `"version":1`, `"version":2`, false},
		{"legacy-plan-id", "legacy", `"id":"sha256:`, `"id":"sha512:`, false},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			store, plan, raw := pullReceiptFixture(test, fixture.format)
			count := 1
			if fixture.all {
				count = -1
			}
			mutated := bytes.Replace(raw, []byte(fixture.before), []byte(fixture.after), count)
			if bytes.Equal(mutated, raw) {
				test.Fatal("mutation did not change fixture")
			}
			writePullReceiptFixture(test, store, plan, mutated)
			before := catalogTree(test, store.storeDir())
			if _, err := store.ReadAppliedPull(context.Background(), plan.RepoID, plan.Remote); !errors.Is(err, domain.ErrHashMismatch) {
				test.Fatalf("mutated receipt was not rejected: %v", err)
			}
			if !reflect.DeepEqual(before, catalogTree(test, store.storeDir())) {
				test.Fatal("invalid receipt was repaired during read")
			}
		})
	}
}

func TestAppliedPullCanFollowLegacyReceiptWithoutRewritingIt(test *testing.T) {
	store, original, raw := pullReceiptFixture(test, "legacy")
	writePullReceiptFixture(test, store, original, raw)
	immutable := filepath.Join(store.storeDir(), "pull-receipts", hexOf(original.ID)+".json")
	if err := writeAtomic(immutable, raw); err != nil {
		test.Fatal(err)
	}
	ctx := context.Background()
	if err := store.PutWorkingPosition(ctx, *original.Expected.Position); err != nil {
		test.Fatal(err)
	}
	state, err := store.ReadCheckoutState(ctx, original.RepoID)
	if err != nil {
		test.Fatal(err)
	}
	next := original
	next.Previous, next.Expected = original.ID, state
	next.Context.Inclusion.Merges[0].DeliveryAcknowledged = true
	next.ID = outbound.SelectedPullPlanID(next)
	receipt, err := store.ApplySelectedPull(ctx, next)
	if err != nil || receipt.Plan.ID != next.ID {
		test.Fatalf("could not follow legacy receipt: %v", err)
	}
	stored, err := store.ReadAppliedPull(ctx, next.RepoID, next.Remote)
	if err != nil || stored.Plan.Previous != original.ID || !stored.Plan.Context.Inclusion.Merges[0].DeliveryAcknowledged {
		test.Fatalf("current receipt lost predecessor or acknowledgement: %v", err)
	}
	after, err := os.ReadFile(immutable)
	if err != nil || !bytes.Equal(after, raw) {
		test.Fatalf("legacy immutable receipt was rewritten: %v", err)
	}
}
