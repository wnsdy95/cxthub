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
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type prContributionTestStore interface {
	outbound.PRContributionStore
	outbound.MetadataStore
	outbound.RepositoryDocumentIdentityStore
	RecordGitRefObservation(context.Context, domain.GitRefObservation, time.Time) error
	GetPRRefObservation(context.Context, domain.ContentHash, string, string) (domain.GitRefObservation, error)
}

func prContributionStoreFixture(t *testing.T, storage prContributionTestStore) (context.Context, domain.PRContribution, domain.PRContributionMemory) {
	t.Helper()
	ctx := context.Background()
	contribution := domain.PRContribution{
		RepoID: domain.HashContent([]byte(t.Name())), GitOrigin: "https://github.com/example/ledger",
		ProviderRepositoryID: 22, PullRequestID: 33,
		PR:                  domain.PullRequestMerge{Number: 17, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)},
		DestinationBranchID: "main-identity", SourceBranchID: "feature-identity",
		Capture: domain.HashContent([]byte("capture")), OriginalHead: strings.Repeat("c", 40),
	}.WithID()
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: contribution.RepoID, GitRemoteURL: contribution.GitOrigin}); err != nil {
		t.Fatal(err)
	}
	memory := domain.PRContributionMemory{RepoID: contribution.RepoID, ContributionID: contribution.ID, PublicationID: strings.Repeat("d", 32), MemoryHash: domain.HashContent([]byte("memory")), MemorySource: contribution.Capture, MemoryPinned: true}.WithID()
	return ctx, contribution, memory
}

func prContributionObservation(contribution domain.PRContribution) domain.GitRefObservation {
	return domain.GitRefObservation{
		RepoID: contribution.RepoID, GitOrigin: contribution.GitOrigin, Delivery: "pr-observation",
		Source: "github-pull-request", Ref: "refs/heads/" + contribution.PR.HeadBranch, After: contribution.PR.HeadSHA,
		PullRequest: &domain.GitPullRequestObservation{InstallationID: 11, RepositoryID: contribution.ProviderRepositoryID, ID: contribution.PullRequestID, Number: contribution.PR.Number, Action: "opened", HeadRepositoryID: contribution.ProviderRepositoryID, BaseRepositoryID: contribution.ProviderRepositoryID, BaseRef: contribution.PR.BaseBranch},
	}.WithID()
}

func checkPRContributionLedger(t *testing.T, storage prContributionTestStore, replica outbound.PRContributionStore) {
	t.Helper()
	ctx, contribution, memory := prContributionStoreFixture(t, storage)
	if rows, err := storage.ListPRContributions(ctx, contribution.RepoID); err != nil || rows == nil || len(rows) != 0 {
		t.Fatal("empty ledger", rows, err)
	}
	if rows, err := storage.ListPRContributionMemories(ctx, contribution.RepoID); err != nil || rows == nil || len(rows) != 0 {
		t.Fatal("empty memory ledger", rows, err)
	}
	if err := storage.PutPRContributionMemory(ctx, memory); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("missing contribution accepted", err)
	}
	firstWitness := domain.PRParticipation{RepoID: contribution.RepoID, GitOrigin: contribution.GitOrigin, ProviderRepositoryID: contribution.ProviderRepositoryID, PullRequestID: contribution.PullRequestID, Number: contribution.PR.Number, MergeSHA: contribution.PR.MergeSHA, PublicationID: memory.PublicationID, Capture: contribution.Capture, BranchID: contribution.SourceBranchID, OriginalHead: contribution.OriginalHead, ObservationID: strings.Repeat("e", 64)}.WithID()
	secondWitness := firstWitness
	secondWitness.ObservationID = strings.Repeat("f", 64)
	secondWitness.PublicationID = strings.Repeat("a", 32)
	secondWitness = secondWitness.WithID()
	if firstWitness.ID == secondWitness.ID || firstWitness.Validate() != nil || secondWitness.Validate() != nil {
		t.Fatal("witnesses should differ")
	}
	for _, witness := range []domain.PRParticipation{firstWitness, secondWitness} {
		semantic := domain.PRContribution{RepoID: witness.RepoID, GitOrigin: witness.GitOrigin, ProviderRepositoryID: witness.ProviderRepositoryID, PullRequestID: witness.PullRequestID, PR: contribution.PR, DestinationBranchID: contribution.DestinationBranchID, SourceBranchID: witness.BranchID, Capture: witness.Capture, OriginalHead: witness.OriginalHead}.WithID()
		if semantic != contribution {
			t.Fatal("witness changed semantic key")
		}
		if err := storage.PutPRContribution(ctx, semantic); err != nil {
			t.Fatal(err)
		}
	}
	if err := replica.PutPRContribution(ctx, contribution); err != nil {
		t.Fatal("replica retry", err)
	}
	if rows, err := replica.ListPRContributions(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.PRContribution{contribution}) {
		t.Fatal("duplicate witness created semantic rows", rows, err)
	}
	if err := storage.PutPRContributionMemory(ctx, memory); err != nil {
		t.Fatal("memory blob existence belongs to application", err)
	}
	if err := replica.PutPRContributionMemory(ctx, memory); err != nil {
		t.Fatal("memory retry", err)
	}
	altered := contribution
	altered.Capture = domain.HashContent([]byte("different capture"))
	if err := storage.PutPRContribution(ctx, altered); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("alteration accepted", err)
	}
	alteredMemory := memory
	alteredMemory.MemoryHash = domain.HashContent([]byte("memory-v2"))
	if err := storage.PutPRContributionMemory(ctx, alteredMemory); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("memory alteration accepted", err)
	}
	versions := []domain.PRContributionMemory{memory, alteredMemory.WithID()}
	otherPublication := memory
	otherPublication.PublicationID = secondWitness.PublicationID
	versions = append(versions, otherPublication.WithID())
	empty := memory
	empty.MemoryHash, empty.MemorySource = "", ""
	versions = append(versions, empty.WithID())
	unpinned := memory
	unpinned.MemoryPinned = false
	if err := storage.PutPRContributionMemory(ctx, unpinned.WithID()); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("unpinned memory persisted", err)
	}
	for _, version := range versions {
		if err := storage.PutPRContributionMemory(ctx, version); err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(versions, func(left, right int) bool { return versions[left].ID < versions[right].ID })
	if rows, err := replica.ListPRContributionMemories(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, versions) {
		t.Fatal("memory versions lost or unsorted", rows, err)
	}
	other := contribution
	other.Capture = altered.Capture
	other = other.WithID()
	if err := storage.PutPRContribution(ctx, other); err != nil {
		t.Fatal(err)
	}
	want := []domain.PRContribution{contribution, other}
	sort.Slice(want, func(left, right int) bool { return want[left].ID < want[right].ID })
	if rows, err := replica.ListPRContributions(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatal("semantic ledger lost or unsorted", rows, err)
	}
	foreign := contribution
	foreign.RepoID = domain.HashContent([]byte(t.Name() + "/foreign"))
	foreign = foreign.WithID()
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: foreign.RepoID}); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutPRContribution(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	wrongRepo := memory
	wrongRepo.RepoID = foreign.RepoID
	if err := storage.PutPRContributionMemory(ctx, wrongRepo.WithID()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("cross-repo memory accepted", err)
	}
	if rows, err := replica.ListPRContributionMemories(ctx, foreign.RepoID); err != nil || len(rows) != 0 {
		t.Fatal("foreign memory leaked", rows, err)
	}
	if rows, err := replica.ListPRContributions(ctx, foreign.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.PRContribution{foreign}) {
		t.Fatal("foreign contribution leaked", rows, err)
	}
}

func checkPRContributionConcurrency(t *testing.T, storage prContributionTestStore, replica outbound.PRContributionStore) {
	t.Helper()
	ctx, contribution, memory := prContributionStoreFixture(t, storage)
	var workers sync.WaitGroup
	results := make(chan error, 16)
	start := make(chan struct{})
	for index := range 16 {
		workers.Go(func() {
			<-start
			writer := outbound.PRContributionStore(storage)
			if index%2 == 0 {
				writer = replica
			}
			if err := writer.PutPRContribution(ctx, contribution); err != nil {
				results <- err
				return
			}
			results <- writer.PutPRContributionMemory(ctx, memory)
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
	if rows, err := replica.ListPRContributions(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.PRContribution{contribution}) {
		t.Fatal("concurrent semantic duplicates", rows, err)
	}
	if rows, err := replica.ListPRContributionMemories(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.PRContributionMemory{memory}) {
		t.Fatal("concurrent memory duplicates", rows, err)
	}
}

func checkPRContributionPolicyAndCancellation(t *testing.T, storage prContributionTestStore) {
	t.Helper()
	ctx, contribution, memory := prContributionStoreFixture(t, storage)
	if err := storage.PutPRContribution(ctx, contribution); err != nil {
		t.Fatal(err)
	}
	if err := storage.RequireDocumentIdentity(ctx, contribution.RepoID, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutPRContribution(ctx, contribution); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("duplicate bypassed policy", err)
	}
	if err := storage.PutPRContributionMemory(ctx, memory); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("memory bypassed policy", err)
	}
	capable := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if err := storage.PutPRContribution(capable, contribution); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutPRContributionMemory(capable, memory); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(capable)
	cancel()
	operations := []func() error{
		func() error { return storage.PutPRContribution(canceled, contribution) },
		func() error { return storage.PutPRContributionMemory(canceled, memory) },
		func() error { _, err := storage.ListPRContributions(canceled, contribution.RepoID); return err },
		func() error { _, err := storage.ListPRContributionMemories(canceled, contribution.RepoID); return err },
		func() error {
			_, err := storage.GetPRRefObservation(canceled, contribution.RepoID, contribution.GitOrigin, strings.Repeat("e", 64))
			return err
		},
	}
	for index, operation := range operations {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation ignored", index, err)
		}
	}
	if _, err := storage.ListPRContributions(ctx, "../repo"); err == nil {
		t.Fatal("invalid repo listed")
	}
	if _, err := storage.ListPRContributionMemories(ctx, "../repo"); err == nil {
		t.Fatal("invalid repo memories listed")
	}
	contribution.ID = ""
	if err := storage.PutPRContribution(capable, contribution); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("missing key accepted", err)
	}
	memory.ID = ""
	if err := storage.PutPRContributionMemory(capable, memory); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("missing memory key accepted", err)
	}
}

func checkPRRefObservationLookup(t *testing.T, storage prContributionTestStore) {
	t.Helper()
	ctx, contribution, _ := prContributionStoreFixture(t, storage)
	observation := prContributionObservation(contribution)
	if _, err := storage.GetPRRefObservation(ctx, contribution.RepoID, contribution.GitOrigin, observation.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("missing observation", err)
	}
	if err := storage.RecordGitRefObservation(ctx, observation, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.GetPRRefObservation(ctx, contribution.RepoID, contribution.GitOrigin, observation.ID); err != nil || !reflect.DeepEqual(got, observation) {
		t.Fatal("lookup mismatch", got, err)
	}
	if _, err := storage.GetPRRefObservation(ctx, contribution.RepoID, "https://github.com/example/foreign", observation.ID); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("wrong origin accepted", err)
	}
	if _, err := storage.GetPRRefObservation(ctx, domain.HashContent([]byte("foreign")), contribution.GitOrigin, observation.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("wrong repo accepted", err)
	}
	for _, id := range []string{"", strings.Repeat("a", 63), strings.Repeat("A", 64), "../outside"} {
		if _, err := storage.GetPRRefObservation(ctx, contribution.RepoID, contribution.GitOrigin, id); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid lookup ID accepted", id, err)
		}
	}
	if _, err := storage.GetPRRefObservation(ctx, "bad", contribution.GitOrigin, observation.ID); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("invalid lookup repo", err)
	}
	if _, err := storage.GetPRRefObservation(ctx, contribution.RepoID, "", observation.ID); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("invalid lookup origin", err)
	}
}

func TestFSPRContribution(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	replica := NewFSStore(storage.dataDir)
	t.Run("ledger", func(t *testing.T) { checkPRContributionLedger(t, storage, replica) })
	t.Run("concurrency", func(t *testing.T) { checkPRContributionConcurrency(t, storage, replica) })
	t.Run("policy", func(t *testing.T) { checkPRContributionPolicyAndCancellation(t, storage) })
	t.Run("observation-lookup", func(t *testing.T) { checkPRRefObservationLookup(t, storage) })
}

func TestFSPRContributionReopen(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	ctx, contribution, memory := prContributionStoreFixture(t, storage)
	if err := storage.PutPRContribution(ctx, contribution); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutPRContributionMemory(ctx, memory); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFSStore(storage.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := reopened.ListPRContributions(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.PRContribution{contribution}) {
		t.Fatal(rows, err)
	}
	if rows, err := reopened.ListPRContributionMemories(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.PRContributionMemory{memory}) {
		t.Fatal(rows, err)
	}
}

func TestFSPRRefObservationDirectLookup(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	ctx, contribution, _ := prContributionStoreFixture(t, storage)
	observation := prContributionObservation(contribution)
	if err := storage.RecordGitRefObservation(ctx, observation, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	unrelated := storage.gitEvidencePath("git-observations", contribution.RepoID, strings.Repeat("f", 64))
	if err := os.WriteFile(unrelated, []byte("invalid unrelated JSON"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.GetPRRefObservation(ctx, contribution.RepoID, contribution.GitOrigin, observation.ID); err != nil || !reflect.DeepEqual(got, observation) {
		t.Fatal("direct lookup scanned unrelated observations", got, err)
	}
}

func TestFSPRContributionCorruption(t *testing.T) {
	for _, kind := range []string{"contribution-key", "contribution-scope", "contribution-payload", "memory-key", "memory-scope", "memory-parent", "memory-payload", "observation-key", "observation-scope", "observation-origin", "observation-payload"} {
		t.Run(kind, func(t *testing.T) {
			storage := NewFSStore(t.TempDir())
			ctx, contribution, memory := prContributionStoreFixture(t, storage)
			if err := storage.PutPRContribution(ctx, contribution); err != nil {
				t.Fatal(err)
			}
			if err := storage.PutPRContributionMemory(ctx, memory); err != nil {
				t.Fatal(err)
			}
			observation := prContributionObservation(contribution)
			if err := storage.RecordGitRefObservation(ctx, observation, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			var target string
			var value any
			switch {
			case strings.HasPrefix(kind, "contribution"):
				target = storage.gitEvidencePath("pr-contributions", contribution.RepoID, contribution.ID)
				other := contribution
				other.Capture = domain.HashContent([]byte("other"))
				if kind == "contribution-scope" {
					other.RepoID = domain.HashContent([]byte("foreign"))
				}
				value = other.WithID()
			case strings.HasPrefix(kind, "memory"):
				target = storage.gitEvidencePath("pr-contribution-memories", memory.RepoID, memory.ID)
				other := memory
				other.MemoryHash = domain.HashContent([]byte("other"))
				if kind == "memory-scope" {
					other.RepoID = domain.HashContent([]byte("foreign"))
				}
				value = other.WithID()
				if kind == "memory-parent" {
					value = memory
					if err := os.Remove(storage.gitEvidencePath("pr-contributions", contribution.RepoID, contribution.ID)); err != nil {
						t.Fatal(err)
					}
				}
			default:
				target = storage.gitEvidencePath("git-observations", observation.RepoID, observation.ID)
				other := observation
				switch kind {
				case "observation-key":
					other.Delivery = "other-delivery"
				case "observation-scope":
					other.RepoID = domain.HashContent([]byte("foreign"))
				case "observation-origin":
					other.GitOrigin += "-foreign"
				}
				value = other.WithID()
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
			switch {
			case strings.HasPrefix(kind, "contribution"):
				_, readErr = storage.ListPRContributions(ctx, contribution.RepoID)
				if err := storage.PutPRContribution(ctx, contribution); err == nil {
					t.Fatal("corrupt row overwritten")
				}
			case strings.HasPrefix(kind, "memory"):
				_, readErr = storage.ListPRContributionMemories(ctx, contribution.RepoID)
				if err := storage.PutPRContributionMemory(ctx, memory); err == nil {
					t.Fatal("corrupt memory overwritten")
				}
			default:
				_, readErr = storage.GetPRRefObservation(ctx, contribution.RepoID, contribution.GitOrigin, observation.ID)
			}
			if !errors.Is(readErr, domain.ErrIntegrity) {
				t.Fatal("corruption not detected", readErr)
			}
		})
	}
}
