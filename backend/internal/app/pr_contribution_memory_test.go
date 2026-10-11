package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type prMemoryFixture struct {
	worker   *GitHubConnections
	storage  *store.FSStore
	remote   *githubHistoryRemote
	repo     domain.Repo
	ordinary domain.HistoryEvent
	main     domain.Ref
	next     int
}

func newPRMemoryFixture(t *testing.T) *prMemoryFixture {
	t.Helper()
	worker, storage, remote, repo, ordinary, main := contributionFixture(t)
	return &prMemoryFixture{worker: worker, storage: storage, remote: remote, repo: repo, ordinary: ordinary, main: main}
}

func (fixture *prMemoryFixture) digest(t *testing.T, previous domain.ContentHash, texts ...string) domain.ContentHash {
	t.Helper()
	claims := []domain.MemoryClaim{}
	for _, text := range texts {
		claims = append(claims, domain.MemoryClaim{Kind: "code", Text: text, Code: &domain.MemoryCodeScope{Commit: fixture.ordinary.GitAfter, Paths: []string{"file"}}})
	}
	digest := domain.MemoryDigest{SnapshotID: fixture.ordinary.Target, PreviousMemoryHash: previous, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: fixture.ordinary.Target, Claims: claims}}}
	hash, err := fixture.storage.PutMemory(systemTestContext(), fixture.repo.ID, digest)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func (fixture *prMemoryFixture) publish(t *testing.T, hash domain.ContentHash, pinned bool) domain.HistoryEvent {
	t.Helper()
	fixture.next++
	publication := prPublication(fixture.ordinary)
	publication.ID = fmt.Sprintf("%032x", 700+fixture.next)
	publication.MemoryHash, publication.MemoryPinned = hash, pinned
	if err := fixture.worker.core.RecordHistory(systemTestContext(), publication); err != nil {
		t.Fatal(err)
	}
	return publication
}

func (fixture *prMemoryFixture) reconcile(t *testing.T) {
	t.Helper()
	for range 2 {
		if err := fixture.worker.reconcilePRHistory(systemTestContext(), fixture.remote, fixture.storage, historyWork(t, fixture.worker, fixture.storage, fixture.repo)); err != nil {
			t.Fatal(err)
		}
	}
}

func (fixture *prMemoryFixture) project(t *testing.T) (prContributionMemoryPlan, domain.MemoryProjection) {
	t.Helper()
	plan, projection, err := fixture.projectResult(t)
	if err != nil {
		t.Fatal(err)
	}
	return plan, projection
}

func (fixture *prMemoryFixture) projectResult(t *testing.T) (prContributionMemoryPlan, domain.MemoryProjection, error) {
	t.Helper()
	ctx := systemTestContext()
	service := fixture.worker.core
	history, err := service.ListHistory(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := fixture.storage.ListSnapshots(ctx, fixture.repo.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := service.newCodeEvidence(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	inclusion, err := service.branchContext(ctx, fixture.main, strings.Repeat("e", 40), snapshots, history, evidence)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := service.contributionsForEvidence(ctx, fixture.repo.ID, history, evidence)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.preparePRContributionMemory(ctx, fixture.repo.ID, inclusion, snapshots, verified)
	if err != nil {
		t.Fatal(err)
	}
	rows := []domain.Snapshot{}
	for _, snapshot := range plan.Snapshots {
		if !slices.Contains(inclusion.SnapshotIDs, snapshot.ID) {
			continue
		}
		snapshot.GraftParents = slices.DeleteFunc(append([]domain.ContentHash{}, snapshot.GraftParents...), func(id domain.ContentHash) bool { return !slices.Contains(inclusion.SnapshotIDs, id) })
		rows = append(rows, snapshot)
	}
	root := hh("pr-memory-test-projection")
	rows = append(rows, domain.Snapshot{RepoID: fixture.repo.ID, ID: root, Parents: inclusion.Roots})
	projection, err := ProjectMemory(ctx, branchProjectionSource{serviceProjectionSource{service.meta, service.blobs}, rows}, fixture.repo.ID, root)
	if err != nil {
		t.Fatal(err)
	}
	projection.Digest.SnapshotID, projection.Inclusion = inclusion.SnapshotID, &inclusion
	projection, err = plan.Merge(ctx, projection)
	return plan, projection, err
}

func TestPRContributionMemoryPinsCausalSupplementsWithoutLatestPointer(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	ctx := systemTestContext()
	first := fixture.digest(t, "", "removed claim", "retained claim")
	intermediate := fixture.digest(t, first, "removed intermediate claim", "retained claim")
	later := fixture.digest(t, intermediate, "retained claim")
	unpublished := fixture.digest(t, later, "unpublished mutable memory")
	if err := fixture.storage.CompareAndSwapSnapshotMemory(ctx, fixture.repo.ID, fixture.ordinary.Target, "", unpublished); err != nil {
		t.Fatal(err)
	}
	firstPublication := fixture.publish(t, first, true)
	fixture.reconcile(t)
	plan, initial := fixture.project(t)
	if len(plan.Versions) != 1 || plan.Versions[0].Hash != first || !initial.Digest.HasMemoryClaims() {
		t.Fatalf("original pin: %+v", plan.Versions)
	}
	secondPublication := fixture.publish(t, later, true)
	fixture.reconcile(t)
	history, _ := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
	refs, _ := fixture.storage.ListRefs(ctx, fixture.repo.ID)
	snapshots, _ := fixture.storage.ListSnapshots(ctx, fixture.repo.ID, "")
	plan, projected := fixture.project(t)
	if len(plan.Versions) != 2 || initial.StateHash == projected.StateHash {
		t.Fatal("supplement lost version provenance")
	}
	states := map[domain.ContentHash]prContributionMemoryState{}
	publications := map[string]bool{}
	for _, version := range plan.Versions {
		states[version.Hash] = version.State
		for _, pin := range version.Pins {
			publications[pin.PublicationID] = true
		}
	}
	if states[first] != "superseded" || states[later] != "selected" || !publications[firstPublication.ID] || !publications[secondPublication.ID] {
		t.Fatalf("causal pins: %+v %+v", states, publications)
	}
	items, err := effectiveMemoryItems(projected.Digest, "claims")
	if err != nil || len(items) != 1 || items[0].Text != "retained claim" {
		t.Fatalf("old or mutable claims re-entered: %+v %v", items, err)
	}
	afterHistory, _ := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
	afterRefs, _ := fixture.storage.ListRefs(ctx, fixture.repo.ID)
	afterSnapshots, _ := fixture.storage.ListSnapshots(ctx, fixture.repo.ID, "")
	if !reflect.DeepEqual(history, afterHistory) || !reflect.DeepEqual(refs, afterRefs) || !reflect.DeepEqual(snapshots, afterSnapshots) {
		t.Fatal("query wrote history, refs or memory pointers")
	}
}

func TestPRContributionMemoryEmptyAndUnpinnedNeverFollowPointer(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprint(pinned), func(t *testing.T) {
			fixture := newPRMemoryFixture(t)
			hash := fixture.digest(t, "", "MUST NOT IMPORT")
			if err := fixture.storage.CompareAndSwapSnapshotMemory(systemTestContext(), fixture.repo.ID, fixture.ordinary.Target, "", hash); err != nil {
				t.Fatal(err)
			}
			fixture.publish(t, "", pinned)
			fixture.reconcile(t)
			plan, projected, err := fixture.projectResult(t)
			if projected.Digest.HasMemoryClaims() || projected.Digest.Summary != "" {
				t.Fatalf("mutable fallback: %+v", projected.Digest)
			}
			if pinned && (err != nil || len(plan.Versions) != 1 || plan.Versions[0].State != "empty") {
				t.Fatalf("explicit empty pin lost: %+v", plan.Versions)
			}
			if !pinned && (!errors.Is(err, domain.ErrConflict) || len(plan.Versions) != 1 || plan.Versions[0].State != prMemoryUnavailable) {
				t.Fatalf("unpinned publication not explicitly unavailable: %+v %v", plan.Versions, err)
			}
			_, queryErr := fixture.worker.core.QueryBranchMemory(systemTestContext(), fixture.repo.ID, "main", fixture.main.Target, strings.Repeat("e", 40))
			if (pinned && queryErr != nil) || (!pinned && !errors.Is(queryErr, domain.ErrConflict)) {
				t.Fatalf("public query did not distinguish unavailable from empty: %v", queryErr)
			}
		})
	}
}

func TestPRContributionMemoryIncomparableVersionsAreReviewMetadataOnly(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	first := fixture.digest(t, "", strings.Repeat("first conflicting claim ", 300))
	second := fixture.digest(t, "", strings.Repeat("second conflicting claim ", 300))
	fixture.publish(t, first, true)
	fixture.publish(t, second, true)
	fixture.reconcile(t)
	plan, projected, err := fixture.projectResult(t)
	if !errors.Is(err, domain.ErrConflict) || len(plan.Versions) != 2 || projected.Digest.HasMemoryClaims() || projected.Digest.Summary != "" {
		t.Fatal("incomparable claims silently selected or applied")
	}
	for _, version := range plan.Versions {
		if version.State != "review" || len(version.Pins) != 1 {
			t.Fatalf("missing review provenance: %+v", version)
		}
		metadata, err := json.Marshal(version)
		if err != nil || len(metadata) > 2048 || strings.Contains(string(metadata), "conflicting claim") || !strings.Contains(string(metadata), string(version.Source)) || !strings.Contains(string(metadata), string(version.Hash)) || !strings.Contains(string(metadata), version.Pins[0].PublicationID) {
			t.Fatalf("review metadata unbounded or lost pin provenance: %s %v", metadata, err)
		}
	}
	for _, content := range []string{"", "claims", "prompt"} {
		items, err := effectiveMemoryItems(projected.Digest, content)
		if err != nil || len(items) != 0 {
			t.Fatalf("unresolved text entered %q delivery: %+v %v", content, items, err)
		}
	}
	againPlan, again, againErr := fixture.projectResult(t)
	if !errors.Is(againErr, domain.ErrConflict) || !reflect.DeepEqual(plan.Versions, againPlan.Versions) || !reflect.DeepEqual(projected, again) {
		t.Fatal("projection not deterministic")
	}
	for _, content := range []string{"all", "claims", "prompt"} {
		_, err := fixture.worker.core.QueryEffectiveMemory(systemTestContext(), fixture.repo.ID, domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{Branch: "main", SnapshotID: fixture.main.Target, CodeCommit: strings.Repeat("e", 40)}, Content: content})
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("public %s query silently omitted unresolved variants: %v", content, err)
		}
	}
	stored, err := fixture.worker.core.QueryEffectiveMemory(systemTestContext(), fixture.repo.ID, domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: fixture.ordinary.Target, MemoryHash: first, CodeCommit: strings.Repeat("e", 40)}, Content: "claims"})
	if err != nil || stored.Total != 1 {
		t.Fatalf("explicit stored hash became unavailable: %+v %v", stored, err)
	}
}

func TestPRContributionMemoryLateAmbiguityWithdrawsSupplement(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	fixture.publish(t, fixture.digest(t, "", "historical claim"), true)
	fixture.reconcile(t)
	_, before := fixture.project(t)
	if !before.Digest.HasMemoryClaims() {
		t.Fatal("missing initial supplement")
	}
	conflict := fixture.ordinary
	conflict.ID, conflict.Kind, conflict.BranchID = strings.Repeat("9", 32), "position", "reused-identity"
	if err := fixture.worker.core.RecordHistory(systemTestContext(), conflict); err != nil {
		t.Fatal(err)
	}
	plan, after := fixture.project(t)
	if len(plan.Versions) != 0 || after.Digest.HasMemoryClaims() {
		t.Fatal("late ambiguity did not fail closed")
	}
}

func TestPRContributionMemoryPinsValidateScopeAndBudget(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	fixture.publish(t, fixture.digest(t, "", "claim"), true)
	fixture.reconcile(t)
	ctx := systemTestContext()
	mutable := fixture.digest(t, "", "MUTABLE POINTER")
	if err := fixture.storage.CompareAndSwapSnapshotMemory(ctx, fixture.repo.ID, fixture.ordinary.Target, "", mutable); err != nil {
		t.Fatal(err)
	}
	history, _ := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
	snapshots, _ := fixture.storage.ListSnapshots(ctx, fixture.repo.ID, "")
	evidence, _ := fixture.worker.core.newCodeEvidence(ctx, fixture.repo.ID)
	inclusion, err := fixture.worker.core.branchContext(ctx, fixture.main, strings.Repeat("e", 40), snapshots, history, evidence)
	if err != nil {
		t.Fatal(err)
	}
	verified, _ := fixture.worker.core.contributionsForEvidence(ctx, fixture.repo.ID, history, evidence)
	pins, _ := fixture.storage.ListPRContributionMemories(ctx, fixture.repo.ID)
	for _, mode := range []string{"publication mismatch", "cycle", "limit", "canceled", "duplicate", "missing pin"} {
		t.Run(mode, func(t *testing.T) {
			input := append([]domain.PRContributionMemory{}, pins...)
			rows := append([]domain.Snapshot{}, snapshots...)
			bound := ctx
			want := domain.ErrIntegrity
			switch mode {
			case "publication mismatch":
				input[0].MemoryHash = hh("other digest")
				input[0] = input[0].WithID()
			case "cycle":
				for index := range rows {
					if rows[index].ID == fixture.ordinary.Target {
						rows[index].Parents = []domain.ContentHash{rows[index].ID}
					}
				}
			case "limit":
				input = make([]domain.PRContributionMemory, maxProjectionSnapshots+1)
				want = domain.ErrMemoryProjectionLimit
			case "canceled":
				var cancel context.CancelFunc
				bound, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "duplicate":
				input = append(input, input[0])
				want = nil
			case "missing pin":
				input = nil
				want = nil
			}
			plan, err := fixture.worker.core.prepareVerifiedPRContributionMemory(bound, fixture.repo.ID, inclusion, rows, verified, input)
			if !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
			if mode == "duplicate" && (len(plan.Versions) != 1 || len(plan.Versions[0].Pins) != 1) {
				t.Fatal("duplicate witness duplicated memory")
			}
			if mode == "missing pin" {
				if len(plan.Versions) != 1 || plan.Versions[0].State != prMemoryUnavailable || !plan.excluded[fixture.ordinary.Target] {
					t.Fatal("missing pin not explicitly unavailable")
				}
				if _, err := plan.Merge(ctx, domain.MemoryProjection{}); !errors.Is(err, domain.ErrConflict) {
					t.Fatalf("missing pin silently omitted: %v", err)
				}
				for _, snapshot := range plan.Snapshots {
					if snapshot.ID == fixture.ordinary.Target && snapshot.MemoryHash != "" {
						t.Fatal("missing pin followed snapshot pointer")
					}
				}
			}
		})
	}
}

func TestPRContributionMemoryAssessmentUsesExactOriginalAndFileEvidence(t *testing.T) {
	fixture := newEffectiveFixture(t)
	ctx := systemTestContext()
	publication := fixture.publish(t, fixture.a, 8, "rewritten-source")
	record := (domain.PRContribution{RepoID: fixture.repo, GitOrigin: "https://github.com/example/effective-memory", ProviderRepositoryID: 1, PullRequestID: 2,
		PR:                  domain.PullRequestMerge{Number: 1, HeadBranch: "rewritten-source", BaseBranch: "main", HeadSHA: effectiveOID(9), MergeSHA: effectiveOID(2)},
		DestinationBranchID: "main", SourceBranchID: publication.BranchID, Capture: fixture.a, OriginalHead: publication.GitAfter}).WithID()
	history, _ := fixture.svc.ListHistory(ctx, fixture.repo)
	for _, test := range []struct {
		name     string
		code     int
		valid    bool
		source   domain.ContentHash
		original string
		want     string
	}{
		{"applied", 3, true, fixture.a, publication.GitAfter, "applied"},
		{"reverted", 4, true, fixture.a, publication.GitAfter, "inactive"},
		{"partial revert", 6, true, fixture.a, publication.GitAfter, "review"},
		{"missing evidence", 99, true, fixture.a, publication.GitAfter, "review"},
		{"unselected merge", 1, true, fixture.a, publication.GitAfter, "inactive"},
		{"late ambiguity", 3, false, fixture.a, publication.GitAfter, "review"},
		{"different capture", 3, true, fixture.b, publication.GitAfter, "review"},
		{"different head", 3, true, fixture.a, effectiveOID(9), "review"},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence, _ := fixture.svc.newCodeEvidence(ctx, fixture.repo)
			candidate := record
			candidate.Capture, candidate.OriginalHead = test.source, test.original
			if err := evidence.bindContributionHistory(fixture.repo, history); err != nil {
				t.Fatal(err)
			}
			evidence.contributionsLoaded = true
			evidence.contributions = []verifiedPRContribution{{record: candidate, valid: test.valid, publications: map[string]domain.HistoryEvent{publication.ID: publication}}}
			resolver, err := newMemoryIntegration(fixture.svc, evidence, fixture.repo, history)
			if err != nil {
				t.Fatal(err)
			}
			item := domain.EffectiveMemoryItem{SourceSnapshot: fixture.a, Kind: "code", Text: "rewritten feature", Code: &domain.MemoryCodeScope{Commit: publication.GitAfter, Paths: []string{"a", "b"}}}
			got, err := resolver.assess(ctx, item, effectiveOID(test.code), domain.RepositoryRevision{})
			if err != nil || got.State != test.want {
				t.Fatalf("assessment: %+v %v", got, err)
			}
			if test.name == "applied" && (got.IntegrationReceipt != record.ID || len(got.Paths) != 2) {
				t.Fatal("missing contribution receipt or skipped file checks")
			}
		})
	}
}

func TestPRContributionMemoryCoalescedCaptureKeepsEveryPublicationPin(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	ctx := systemTestContext()
	first := fixture.digest(t, "", "old claim")
	latest := fixture.digest(t, first, "new claim")
	fixture.publish(t, first, true)
	fixture.reconcile(t)
	ordinary := fixture.ordinary
	ordinary.ID, ordinary.Kind, ordinary.GitAfter = strings.Repeat("6", 32), "position", strings.Repeat("b", 40)
	ordinary.Source = ordinary.Target
	if err := fixture.worker.core.RecordHistory(ctx, ordinary); err != nil {
		t.Fatal(err)
	}
	fixture.ordinary = ordinary
	fixture.publish(t, latest, true)
	fixture.reconcile(t)
	contributions, err := fixture.storage.ListPRContributions(ctx, fixture.repo.ID)
	if err != nil || len(contributions) != 2 {
		t.Fatalf("missing coalesced contribution fixture: %+v %v", contributions, err)
	}
	plan, projection := fixture.project(t)
	if len(plan.Versions) != 2 || len(projection.Inclusion.Merges) != 1 {
		t.Fatalf("pins tied to representative event ID: %+v %+v", plan.Versions, projection.Inclusion)
	}
	items, err := effectiveMemoryItems(projection.Digest, "claims")
	if err != nil || len(items) != 1 || items[0].Text != "new claim" {
		t.Fatalf("old coalesced capture memory won: %+v %v", items, err)
	}
}

func TestPRContributionMemoryInheritedPinKeepsItsOwnRoot(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	ctx := systemTestContext()
	source := prSnapshot(t, fixture.storage, fixture.repo.ID, "separate inherited memory")
	hash, err := fixture.storage.PutMemory(ctx, fixture.repo.ID, domain.MemoryDigest{SnapshotID: source, Summary: "PINNED INHERITED ROOT"})
	if err != nil {
		t.Fatal(err)
	}
	publication := prPublication(fixture.ordinary)
	publication.MemoryPinned, publication.MemorySource, publication.MemoryHash = true, source, hash
	if err := fixture.worker.core.RecordHistory(ctx, publication); err != nil {
		t.Fatal(err)
	}
	fixture.reconcile(t)
	plan, projected := fixture.project(t)
	if len(plan.Versions) != 1 || plan.Versions[0].Source != source || !strings.Contains(projected.Digest.Summary, "PINNED INHERITED ROOT") {
		t.Fatalf("inherited pin relabeled or lost: %+v %+v", plan.Versions, projected.Digest)
	}
}

func TestPRContributionMemoryIncompleteChainCannotPickWinner(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	ctx := systemTestContext()
	hash, err := fixture.storage.PutMemory(ctx, fixture.repo.ID, domain.MemoryDigest{SnapshotID: fixture.ordinary.Target, PreviousMemoryHash: hh("unavailable parent"), Summary: "partial causal variant"})
	if err != nil {
		t.Fatal(err)
	}
	fixture.publish(t, hash, true)
	fixture.reconcile(t)
	plan, projected, err := fixture.projectResult(t)
	if !errors.Is(err, domain.ErrConflict) || len(plan.Versions) != 1 || plan.Versions[0].State != "review" || projected.Digest.Summary != "" || projected.Digest.HasMemoryClaims() {
		t.Fatalf("missing parent was treated as complete: %+v", plan.Versions)
	}
}

func TestPRContributionMemoryMergeUsesInclusionOrderAndRemovesStaleFragments(t *testing.T) {
	oldest, contribution, current := hh("oldest"), hh("contribution"), hh("current")
	plan := prContributionMemoryPlan{
		excluded: map[domain.ContentHash]bool{contribution: true},
		order:    map[domain.ContentHash]int{oldest: 2, contribution: 1, current: 0},
		Versions: []prContributionMemoryVersion{{Source: contribution, Hash: hh("pin"), State: "selected", digest: domain.MemoryDigest{SnapshotID: contribution, Summary: "PINNED"}}},
	}
	base := domain.MemoryProjection{StateHash: hh("projection"), Found: true, Digest: domain.MemoryDigest{SnapshotID: current, Fragments: []domain.MemoryFragment{
		{SourceSnapshot: oldest, Summary: "OLDEST"}, {SourceSnapshot: contribution, Summary: "STALE MUTABLE FALLBACK"}, {SourceSnapshot: current, Summary: "CURRENT"},
	}}}
	result, err := plan.Merge(systemTestContext(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Digest.Fragments) != 3 || result.Digest.Fragments[0].SourceSnapshot != oldest || result.Digest.Fragments[1].SourceSnapshot != contribution || result.Digest.Fragments[2].SourceSnapshot != current || strings.Contains(result.Digest.Summary, "STALE") {
		t.Fatalf("inclusion order or pin lost: %+v", result.Digest)
	}
	if base.Digest.Fragments[1].Summary != "STALE MUTABLE FALLBACK" {
		t.Fatal("projection input mutated")
	}
}

type prMemoryMissingBlob struct {
	outbound.BlobStore
	missing domain.ContentHash
}

func (blobs prMemoryMissingBlob) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if hash == blobs.missing {
		return domain.MemoryDigest{}, domain.ErrNotFound
	}
	return blobs.BlobStore.GetMemory(ctx, repo, hash)
}

func TestPRContributionMemoryMissingBlobNeverFollowsPointer(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	missing := fixture.digest(t, "", "pinned but unavailable")
	mutable := fixture.digest(t, "", "MUTABLE FALLBACK")
	fixture.publish(t, missing, true)
	fixture.reconcile(t)
	if err := fixture.storage.CompareAndSwapSnapshotMemory(systemTestContext(), fixture.repo.ID, fixture.ordinary.Target, "", mutable); err != nil {
		t.Fatal(err)
	}
	fixture.worker.core.blobs = prMemoryMissingBlob{BlobStore: fixture.worker.core.blobs, missing: missing}
	plan, result, err := fixture.projectResult(t)
	if !errors.Is(err, domain.ErrConflict) || len(plan.Versions) != 1 || plan.Versions[0].State != prMemoryUnavailable || result.Digest.Summary != "" || result.Digest.HasMemoryClaims() {
		t.Fatalf("missing immutable pin fell back: %+v %+v", plan.Versions, result)
	}
	if _, err := fixture.worker.core.QueryBranchMemory(systemTestContext(), fixture.repo.ID, "main", fixture.main.Target, strings.Repeat("e", 40)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("public query silently lost missing pin: %v", err)
	}
}

func TestPRContributionMemoryInvalidHeadDoesNotPoisonValidAlternative(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	ctx := systemTestContext()
	original := fixture.ordinary
	invalidHash := fixture.digest(t, "", "INVALID HEAD PIN")
	fixture.publish(t, invalidHash, true)
	fixture.reconcile(t)
	alternative := original
	alternative.ID, alternative.Kind, alternative.GitAfter = strings.Repeat("6", 32), "position", strings.Repeat("b", 40)
	alternative.Source, alternative.CreatedAt = alternative.Target, time.Unix(240, 0).UTC()
	if err := fixture.worker.core.RecordHistory(ctx, alternative); err != nil {
		t.Fatal(err)
	}
	fixture.ordinary = alternative
	validHash := fixture.digest(t, "", "VALID ALTERNATIVE PIN")
	validPublication := fixture.publish(t, validHash, true)
	fixture.reconcile(t)
	conflict := original
	conflict.ID, conflict.Kind, conflict.BranchID = strings.Repeat("9", 32), "position", "ambiguous-original"
	if err := fixture.worker.core.RecordHistory(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	plan, projected := fixture.project(t)
	if len(plan.Versions) != 1 || plan.Versions[0].Hash != validHash || plan.Versions[0].State != prMemorySelected || len(plan.Versions[0].Pins) != 1 || plan.Versions[0].Pins[0].PublicationID != validPublication.ID {
		t.Fatalf("ambiguous head poisoned valid pin: %+v", plan.Versions)
	}
	items, err := effectiveMemoryItems(projected.Digest, "claims")
	if err != nil || len(items) != 1 || items[0].Text != "VALID ALTERNATIVE PIN" {
		t.Fatalf("invalid pin survived: %+v %v", items, err)
	}
}

func (fixture *prMemoryFixture) scanMemoryCode(t *testing.T) string {
	t.Helper()
	selected := strings.Repeat("f", 40)
	entry := domain.GitEntry{OID: effectiveOID(102), Mode: "100644"}
	reader := &scanReader{deltas: map[string]domain.GitCommitDelta{}}
	for code, parent := range map[string]string{fixture.ordinary.GitAfter: strings.Repeat("1", 40), strings.Repeat("9", 40): strings.Repeat("1", 40), selected: strings.Repeat("e", 40)} {
		reader.deltas[code] = domain.GitCommitDelta{Commit: code, Parent: parent, Parents: []string{parent}, Complete: true, Changes: []domain.GitPathChange{{Path: "file", After: entry}}}
	}
	scans, err := NewGitScans(fixture.worker.core, reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{fixture.ordinary.GitAfter, strings.Repeat("9", 40), selected} {
		if _, err := scans.ObservePush(systemTestContext(), fixture.repo.GitRemoteURL, "refs/heads/memory-proof", "", code, false, "memory-"+code); err != nil {
			t.Fatal(err)
		}
	}
	sourceJob := domain.NewGitScan(fixture.repo.ID, fixture.repo.GitRemoteURL, fixture.ordinary.GitAfter, time.Now())
	if err := scans.RetryScan(systemTestContext(), fixture.repo.ID, sourceJob.ID); err != nil {
		t.Fatal(err)
	}
	if err := scans.Process(systemTestContext(), 100); err != nil {
		t.Fatal(err)
	}
	for code := range reader.deltas {
		if _, err := fixture.storage.GetGitCommitTree(systemTestContext(), fixture.repo.ID, fixture.repo.GitRemoteURL, code); err != nil {
			t.Fatalf("missing memory code tree %s: %v", code, err)
		}
	}
	return selected
}

func TestPRContributionMemoryPublicQueriesUsePinnedTipAndFileEvidence(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	ctx := systemTestContext()
	selected := fixture.scanMemoryCode(t)
	first := fixture.digest(t, "", "removed claim", "retained claim")
	intermediate := fixture.digest(t, first, "unpublished intermediate", "retained claim")
	latest := fixture.digest(t, intermediate, "retained claim")
	mutable := fixture.digest(t, latest, "MUTABLE POINTER")
	if err := fixture.storage.CompareAndSwapSnapshotMemory(ctx, fixture.repo.ID, fixture.ordinary.Target, "", mutable); err != nil {
		t.Fatal(err)
	}
	fixture.publish(t, first, true)
	fixture.reconcile(t)
	publication := fixture.publish(t, latest, true)
	fixture.reconcile(t)
	history, _ := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
	refs, _ := fixture.storage.ListRefs(ctx, fixture.repo.ID)
	snapshots, _ := fixture.storage.ListSnapshots(ctx, fixture.repo.ID, "")
	calls := len(fixture.remote.calls)
	for _, test := range []struct{ code, state string }{{selected, "applied"}, {strings.Repeat("e", 40), "inactive"}} {
		projection, err := fixture.worker.core.QueryBranchMemory(ctx, fixture.repo.ID, "main", fixture.main.Target, test.code)
		if err != nil {
			t.Fatal(err)
		}
		for _, content := range []string{"all", "claims", "prompt"} {
			page, err := fixture.worker.core.QueryEffectiveMemory(ctx, fixture.repo.ID, domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{Branch: "main", SnapshotID: fixture.main.Target, CodeCommit: test.code}, Content: content})
			if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Text != "retained claim" || page.Items[0].State != test.state || page.Items[0].IntegrationReceipt == "" || len(page.Items[0].Paths) != 1 || !slices.Contains(page.Items[0].PublicationIDs, publication.ID) {
				t.Fatalf("public %s/%s projection: %+v %v", test.code, content, page, err)
			}
			if page.LineageHash != projection.StateHash || !reflect.DeepEqual(page.Inclusion, projection.Inclusion) {
				t.Fatal("branch and effective query projection drift")
			}
		}
	}
	past, err := fixture.worker.core.QueryBranchMemory(ctx, fixture.repo.ID, "main", fixture.main.Target, strings.Repeat("1", 40))
	if err != nil || past.Digest.HasMemoryClaims() || past.Digest.Summary != "" {
		t.Fatalf("merge not selected but pin included: %+v %v", past, err)
	}
	afterHistory, _ := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
	afterRefs, _ := fixture.storage.ListRefs(ctx, fixture.repo.ID)
	afterSnapshots, _ := fixture.storage.ListSnapshots(ctx, fixture.repo.ID, "")
	if !reflect.DeepEqual(history, afterHistory) || !reflect.DeepEqual(refs, afterRefs) || !reflect.DeepEqual(snapshots, afterSnapshots) || calls != len(fixture.remote.calls) {
		t.Fatal("public memory query wrote state or fetched remote evidence")
	}
}

func TestPRContributionMemoryUnrelatedInheritedClaimsKeepTheirOwnProof(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprint(pinned), func(t *testing.T) {
			fixture := newPRMemoryFixture(t)
			ctx := systemTestContext()
			selected := fixture.scanMemoryCode(t)
			unrelated := prSnapshot(t, fixture.storage, fixture.repo.ID, "unrelated inherited source")
			observation := fixture.ordinary
			observation.ID, observation.BranchID, observation.Branch, observation.GitAfter = strings.Repeat("6", 32), "unrelated", "unrelated", strings.Repeat("9", 40)
			observation.Kind, observation.Source, observation.Target = "position", unrelated, unrelated
			if err := fixture.worker.core.RecordHistory(ctx, observation); err != nil {
				t.Fatal(err)
			}
			publishPRSource(t, fixture.worker.core, observation)
			digest := domain.MemoryDigest{SnapshotID: fixture.ordinary.Target, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{
				{SourceSnapshot: fixture.ordinary.Target, Claims: []domain.MemoryClaim{{Kind: "code", Text: "own code", Code: &domain.MemoryCodeScope{Commit: fixture.ordinary.GitAfter, Paths: []string{"file"}}}}},
				{SourceSnapshot: unrelated, Claims: []domain.MemoryClaim{{Kind: "code", Text: "unrelated code", Code: &domain.MemoryCodeScope{Commit: observation.GitAfter, Paths: []string{"file"}}}}},
			}}
			if pinned {
				digest.GraftCoverage = &domain.MemoryGraftCoverage{ProjectionVersion: domain.MemoryProjectionVersion, PinnedSources: []domain.ContentHash{unrelated}}
			}
			hash, err := fixture.storage.PutMemory(ctx, fixture.repo.ID, digest)
			if err != nil {
				t.Fatal(err)
			}
			fixture.publish(t, hash, true)
			fixture.reconcile(t)
			page, err := fixture.worker.core.QueryEffectiveMemory(ctx, fixture.repo.ID, domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{Branch: "main", SnapshotID: fixture.main.Target, CodeCommit: selected}, Content: "claims"})
			if err != nil {
				t.Fatal(err)
			}
			own := itemByText(t, page, "own code")
			if own.State != "applied" || own.SourceSnapshot != fixture.ordinary.Target {
				t.Fatalf("own proof lost: %+v", own)
			}
			if pinned {
				inherited := itemByText(t, page, "unrelated code")
				if page.Total != 2 || inherited.State == "applied" || inherited.IntegrationReceipt != "" || inherited.SourceSnapshot != unrelated {
					t.Fatalf("unrelated claim borrowed PR proof: %+v", inherited)
				}
			} else if page.Total != 1 {
				t.Fatalf("unrelated unpinned source survived projection: %+v", page)
			}
		})
	}
}

func TestPRContributionMemoryLegacyCompletionCannotBypassMatchingLedgerPin(t *testing.T) {
	for _, natural := range []bool{false, true} {
		t.Run(fmt.Sprintf("natural-main-ancestor=%t", natural), func(t *testing.T) {
			fixture := newPRMemoryFixture(t)
			ctx := systemTestContext()
			selected := fixture.scanMemoryCode(t)
			pinned := fixture.digest(t, "", "PUBLISHED PIN")
			unpublished := fixture.digest(t, pinned, "UNPUBLISHED POINTER")
			fixture.publish(t, pinned, true)
			fixture.reconcile(t)
			if err := fixture.storage.CompareAndSwapSnapshotMemory(ctx, fixture.repo.ID, fixture.ordinary.Target, "", unpublished); err != nil {
				t.Fatal(err)
			}
			records, err := fixture.storage.ListPRContributions(ctx, fixture.repo.ID)
			if err != nil || len(records) != 1 {
				t.Fatalf("missing contribution fixture: %+v %v", records, err)
			}
			receipt := domain.HistoryEvent{ID: strings.Repeat("7", 32), RepoID: string(fixture.repo.ID), Kind: "pr-merge", Branch: "main", BranchID: fixture.main.BranchID, SourceBranchID: fixture.ordinary.BranchID, Source: fixture.ordinary.Target, Target: fixture.ordinary.Target, SharedTarget: fixture.ordinary.Source, PR: &records[0].PR, CreatedAt: time.Unix(300, 0).UTC()}
			if err := fixture.storage.ApplyHistoryEvent(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			completion := receipt
			completionKey := sha256.Sum256([]byte(receipt.ID + ":completed"))
			completion.ID, completion.PRCompleted = fmt.Sprintf("%x", completionKey[:16]), true
			if err := fixture.storage.ApplyHistoryEvent(ctx, completion); err != nil {
				t.Fatal(err)
			}
			if natural {
				previous := fixture.main.Target
				fixture.main.Target = prSnapshot(t, fixture.storage, fixture.repo.ID, "main naturally continues source", fixture.ordinary.Target)
				if err := fixture.storage.CompareAndSwapRef(ctx, fixture.repo.ID, fixture.main, previous); err != nil {
					t.Fatal(err)
				}
			}
			history, _ := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
			refs, _ := fixture.storage.ListRefs(ctx, fixture.repo.ID)
			snapshots, _ := fixture.storage.ListSnapshots(ctx, fixture.repo.ID, "")
			projection, err := fixture.worker.core.QueryBranchMemory(ctx, fixture.repo.ID, "main", fixture.main.Target, selected)
			if err != nil {
				t.Fatal(err)
			}
			if projection.Inclusion == nil || len(projection.Inclusion.Merges) != 1 || projection.Inclusion.Merges[0].EventID != completion.ID {
				t.Fatalf("fixture did not retain legacy representative: %+v", projection.Inclusion)
			}
			want := "PUBLISHED PIN"
			if natural {
				want = "UNPUBLISHED POINTER"
			}
			items, err := effectiveMemoryItems(projection.Digest, "claims")
			if err != nil || len(items) != 1 || items[0].Text != want {
				t.Fatalf("legacy representative bypassed pin or masked natural source: %+v %v", items, err)
			}
			page, err := fixture.worker.core.QueryEffectiveMemory(ctx, fixture.repo.ID, domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{Branch: "main", SnapshotID: fixture.main.Target, CodeCommit: selected}, Content: "claims"})
			if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Text != want || page.Items[0].State != "applied" || page.Items[0].SourceSnapshot != fixture.ordinary.Target || page.LineageHash != projection.StateHash {
				t.Fatalf("effective query ignored semantic pin membership: %+v %v", page, err)
			}
			plan, _ := fixture.project(t)
			if len(plan.Versions) != 1 || plan.Versions[0].Hash != pinned || plan.Versions[0].Independent != natural {
				t.Fatalf("incorrect independent-source classification: %+v", plan.Versions)
			}
			afterHistory, _ := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
			afterRefs, _ := fixture.storage.ListRefs(ctx, fixture.repo.ID)
			afterSnapshots, _ := fixture.storage.ListSnapshots(ctx, fixture.repo.ID, "")
			if !reflect.DeepEqual(history, afterHistory) || !reflect.DeepEqual(refs, afterRefs) || !reflect.DeepEqual(snapshots, afterSnapshots) {
				t.Fatal("query changed accepted history, refs or memory pointers")
			}
		})
	}
}
