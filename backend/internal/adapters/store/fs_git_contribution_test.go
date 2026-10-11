package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type gitContributionTestStore interface {
	outbound.GitContributionStore
	outbound.MetadataStore
	outbound.RepositoryDocumentIdentityStore
	outbound.RepositoryRevisions
}

func gitContributionStoreFixture(t *testing.T, storage gitContributionTestStore) (context.Context, domain.GitContribution, domain.GitContributionMemory) {
	t.Helper()
	ctx := context.Background()
	contribution := domain.GitContribution{
		RepoID: domain.HashContent([]byte(t.Name())), GitOrigin: "https://github.com/example/ledger",
		ProviderRepositoryID: 22, IntegrationSHA: strings.Repeat("b", 40),
		DestinationBranchID: "main-identity", SourceBranchID: "feature-identity",
		Capture: domain.HashContent([]byte("capture")), OriginalHead: strings.Repeat("c", 40),
	}.WithID()
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: contribution.RepoID, GitRemoteURL: contribution.GitOrigin}); err != nil {
		t.Fatal(err)
	}
	memory := domain.GitContributionMemory{RepoID: contribution.RepoID, ContributionID: contribution.ID, PublicationID: strings.Repeat("d", 32), MemoryHash: domain.HashContent([]byte("memory")), MemorySource: contribution.Capture, MemoryPinned: true}.WithID()
	return ctx, contribution, memory
}

func checkGitContributionLedger(t *testing.T, storage gitContributionTestStore, replica outbound.GitContributionStore) {
	t.Helper()
	ctx, contribution, memory := gitContributionStoreFixture(t, storage)
	before, err := storage.RepositoryRevision(ctx, contribution.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := storage.ListGitContributions(ctx, contribution.RepoID); err != nil || rows == nil || len(rows) != 0 {
		t.Fatal("empty ledger", rows, err)
	}
	if rows, err := storage.ListGitContributionMemories(ctx, contribution.RepoID); err != nil || rows == nil || len(rows) != 0 {
		t.Fatal("empty memory ledger", rows, err)
	}
	if err := storage.PutGitContributionMemory(ctx, memory); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("missing contribution accepted", err)
	}
	for range 2 {
		if err := storage.PutGitContribution(ctx, contribution); err != nil {
			t.Fatal(err)
		}
	}
	if err := replica.PutGitContribution(ctx, contribution); err != nil {
		t.Fatal("replica retry", err)
	}
	if rows, err := replica.ListGitContributions(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.GitContribution{contribution}) {
		t.Fatal("duplicate delivery created semantic rows", rows, err)
	}
	if err := storage.PutGitContributionMemory(ctx, memory); err != nil {
		t.Fatal("memory blob existence belongs to application", err)
	}
	if err := replica.PutGitContributionMemory(ctx, memory); err != nil {
		t.Fatal("memory retry", err)
	}
	altered := contribution
	altered.Capture = domain.HashContent([]byte("different capture"))
	if err := storage.PutGitContribution(ctx, altered); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("alteration accepted", err)
	}
	alteredMemory := memory
	alteredMemory.MemoryHash = domain.HashContent([]byte("memory-v2"))
	if err := storage.PutGitContributionMemory(ctx, alteredMemory); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("memory alteration accepted", err)
	}
	versions := []domain.GitContributionMemory{memory, alteredMemory.WithID()}
	otherPublication := memory
	otherPublication.PublicationID = strings.Repeat("a", 32)
	versions = append(versions, otherPublication.WithID())
	empty := memory
	empty.MemoryHash, empty.MemorySource = "", ""
	versions = append(versions, empty.WithID())
	unpinned := memory
	unpinned.MemoryPinned = false
	if err := storage.PutGitContributionMemory(ctx, unpinned.WithID()); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("unpinned memory persisted", err)
	}
	for _, version := range versions {
		if err := storage.PutGitContributionMemory(ctx, version); err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(versions, func(left, right int) bool { return versions[left].ID < versions[right].ID })
	if rows, err := replica.ListGitContributionMemories(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, versions) {
		t.Fatal("memory versions lost or unsorted", rows, err)
	}
	other := contribution
	other.Capture = altered.Capture
	other = other.WithID()
	if err := storage.PutGitContribution(ctx, other); err != nil {
		t.Fatal(err)
	}
	want := []domain.GitContribution{contribution, other}
	sort.Slice(want, func(left, right int) bool { return want[left].ID < want[right].ID })
	if rows, err := replica.ListGitContributions(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatal("semantic ledger lost or unsorted", rows, err)
	}
	foreign := contribution
	foreign.RepoID = domain.HashContent([]byte(t.Name() + "/foreign"))
	foreign = foreign.WithID()
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: foreign.RepoID}); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutGitContribution(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	wrongRepo := memory
	wrongRepo.RepoID = foreign.RepoID
	if err := storage.PutGitContributionMemory(ctx, wrongRepo.WithID()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("cross-repo memory accepted", err)
	}
	if rows, err := replica.ListGitContributionMemories(ctx, foreign.RepoID); err != nil || len(rows) != 0 {
		t.Fatal("foreign memory leaked", rows, err)
	}
	if rows, err := replica.ListGitContributions(ctx, foreign.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.GitContribution{foreign}) {
		t.Fatal("foreign contribution leaked", rows, err)
	}
	if after, err := storage.RepositoryRevision(ctx, contribution.RepoID); err != nil || after != before {
		t.Fatal("ledger storage must leave revision ownership to the application transaction", before, after, err)
	}
}

func checkGitContributionConcurrency(t *testing.T, storage gitContributionTestStore, replica outbound.GitContributionStore) {
	t.Helper()
	ctx, contribution, memory := gitContributionStoreFixture(t, storage)
	var workers sync.WaitGroup
	results := make(chan error, 16)
	start := make(chan struct{})
	for index := range 16 {
		workers.Go(func() {
			<-start
			writer := outbound.GitContributionStore(storage)
			if index%2 == 0 {
				writer = replica
			}
			if err := writer.PutGitContribution(ctx, contribution); err != nil {
				results <- err
				return
			}
			results <- writer.PutGitContributionMemory(ctx, memory)
		})
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if rows, err := replica.ListGitContributions(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.GitContribution{contribution}) {
		t.Fatal("concurrent semantic duplicates", rows, err)
	}
	if rows, err := replica.ListGitContributionMemories(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.GitContributionMemory{memory}) {
		t.Fatal("concurrent memory duplicates", rows, err)
	}
}

func checkGitContributionPolicyAndCancellation(t *testing.T, storage gitContributionTestStore) {
	t.Helper()
	ctx, contribution, memory := gitContributionStoreFixture(t, storage)
	if err := storage.PutGitContribution(ctx, contribution); err != nil {
		t.Fatal(err)
	}
	if err := storage.RequireDocumentIdentity(ctx, contribution.RepoID, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutGitContribution(ctx, contribution); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("duplicate bypassed policy", err)
	}
	if err := storage.PutGitContributionMemory(ctx, memory); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("memory bypassed policy", err)
	}
	capable := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if err := storage.PutGitContribution(capable, contribution); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutGitContributionMemory(capable, memory); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(capable)
	cancel()
	operations := []func() error{
		func() error { return storage.PutGitContribution(canceled, contribution) },
		func() error { return storage.PutGitContributionMemory(canceled, memory) },
		func() error { _, err := storage.ListGitContributions(canceled, contribution.RepoID); return err },
		func() error { _, err := storage.ListGitContributionMemories(canceled, contribution.RepoID); return err },
	}
	for index, operation := range operations {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation ignored", index, err)
		}
	}
	if _, err := storage.ListGitContributions(ctx, "../repo"); err == nil {
		t.Fatal("invalid repo listed")
	}
	if _, err := storage.ListGitContributionMemories(ctx, "../repo"); err == nil {
		t.Fatal("invalid repo memories listed")
	}
	contribution.ID = ""
	if err := storage.PutGitContribution(capable, contribution); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("missing key accepted", err)
	}
	memory.ID = ""
	if err := storage.PutGitContributionMemory(capable, memory); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("missing memory key accepted", err)
	}
}

func TestFSGitContribution(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	replica := NewFSStore(storage.dataDir)
	t.Run("ledger", func(t *testing.T) { checkGitContributionLedger(t, storage, replica) })
	t.Run("concurrency", func(t *testing.T) { checkGitContributionConcurrency(t, storage, replica) })
	t.Run("policy", func(t *testing.T) { checkGitContributionPolicyAndCancellation(t, storage) })
}

func TestFSGitContributionReopen(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	ctx, contribution, memory := gitContributionStoreFixture(t, storage)
	if err := storage.PutGitContribution(ctx, contribution); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutGitContributionMemory(ctx, memory); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFSStore(storage.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := reopened.ListGitContributions(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.GitContribution{contribution}) {
		t.Fatal(rows, err)
	}
	if rows, err := reopened.ListGitContributionMemories(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.GitContributionMemory{memory}) {
		t.Fatal(rows, err)
	}
}

func TestFSGitContributionCorruption(t *testing.T) {
	for _, kind := range []string{"contribution-key", "contribution-scope", "contribution-payload", "memory-key", "memory-scope", "memory-parent", "memory-payload"} {
		t.Run(kind, func(t *testing.T) {
			storage := NewFSStore(t.TempDir())
			ctx, contribution, memory := gitContributionStoreFixture(t, storage)
			if err := storage.PutGitContribution(ctx, contribution); err != nil {
				t.Fatal(err)
			}
			if err := storage.PutGitContributionMemory(ctx, memory); err != nil {
				t.Fatal(err)
			}
			var target string
			var value any
			if strings.HasPrefix(kind, "contribution") {
				target = storage.gitEvidencePath("git-contributions", contribution.RepoID, contribution.ID)
				other := contribution
				other.Capture = domain.HashContent([]byte("other"))
				if kind == "contribution-scope" {
					other.RepoID = domain.HashContent([]byte("foreign"))
				}
				value = other.WithID()
			} else {
				target = storage.gitEvidencePath("git-contribution-memories", memory.RepoID, memory.ID)
				other := memory
				other.MemoryHash = domain.HashContent([]byte("other"))
				if kind == "memory-scope" {
					other.RepoID = domain.HashContent([]byte("foreign"))
				}
				value = other.WithID()
				if kind == "memory-parent" {
					value = memory
					if err := os.Remove(storage.gitEvidencePath("git-contributions", contribution.RepoID, contribution.ID)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if strings.HasSuffix(kind, "payload") {
				value = struct{}{}
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			var readErr error
			if strings.HasPrefix(kind, "contribution") {
				_, readErr = storage.ListGitContributions(ctx, contribution.RepoID)
				if err := storage.PutGitContribution(ctx, contribution); err == nil {
					t.Fatal("corrupt row overwritten")
				}
			} else {
				_, readErr = storage.ListGitContributionMemories(ctx, contribution.RepoID)
				if err := storage.PutGitContributionMemory(ctx, memory); err == nil {
					t.Fatal("corrupt memory overwritten")
				}
			}
			if !errors.Is(readErr, domain.ErrIntegrity) {
				t.Fatal("corruption not detected", readErr)
			}
		})
	}
}
