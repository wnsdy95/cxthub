package memoryquery

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func contributionMemoryInputs(test *testing.T) (*projectionTestSource, domain.ContentHash, domain.BranchContext, []VerifiedContribution, []ContributionPin) {
	test.Helper()
	source, repo, capture := projectionSource()
	digest := domain.MemoryDigest{SnapshotID: capture, Summary: "PINNED"}
	hash, err := domain.MemoryDigestHash(digest)
	if err != nil {
		test.Fatal(err)
	}
	source.memories[hash] = digest
	record := ContributionRecord{ID: "contribution", RepoID: repo, DestinationBranchID: "main", SourceBranchID: "topic", Capture: capture, OriginalHead: "head", IntegrationSHA: "merge"}
	publication := domain.HistoryEvent{ID: "publication", RepoID: string(repo), BranchID: record.SourceBranchID, Kind: "publish", Target: capture, Source: capture, GitAfter: record.OriginalHead, MemoryHash: hash, MemorySource: capture, MemoryPinned: true}
	verified := []VerifiedContribution{{Record: record, Valid: true, Publications: map[string]domain.HistoryEvent{publication.ID: publication}}}
	pins := []ContributionPin{{ID: "pin", RepoID: repo, ContributionID: record.ID, PublicationID: publication.ID, MemoryHash: hash, MemorySource: capture, MemoryPinned: true}}
	base := source.snapshots[0].ID
	inclusion := domain.BranchContext{SnapshotID: base, SnapshotIDs: []domain.ContentHash{base, capture}, Roots: []domain.ContentHash{base, capture}, Merges: []domain.BranchContextMerge{{EventID: record.ID, DestinationBranchID: record.DestinationBranchID, Source: capture, MergeSHA: record.IntegrationSHA, State: "included"}}}
	return source, repo, inclusion, verified, pins
}

func TestContributionMemoryOwnerSelectsPinnedCausalTip(test *testing.T) {
	source, repo, inclusion, verified, pins := contributionMemoryInputs(test)
	originalSnapshots := append([]domain.Snapshot(nil), source.snapshots...)
	firstHash := pins[0].MemoryHash
	digest := domain.MemoryDigest{SnapshotID: pins[0].MemorySource, PreviousMemoryHash: firstHash, Summary: "PINNED SUPPLEMENT"}
	latestHash, err := domain.MemoryDigestHash(digest)
	if err != nil {
		test.Fatal(err)
	}
	source.memories[latestHash] = digest
	publication := verified[0].Publications[pins[0].PublicationID]
	publication.ID, publication.MemoryHash = "supplement", latestHash
	verified[0].Publications[publication.ID] = publication
	pins = append(pins, ContributionPin{ID: "supplement-pin", RepoID: repo, ContributionID: verified[0].Record.ID, PublicationID: publication.ID, MemoryHash: latestHash, MemorySource: publication.MemorySource, MemoryPinned: true})
	plan, err := PrepareVerifiedContributionMemory(projectionTestContext(), source, repo, inclusion, source.snapshots, verified, pins)
	if err != nil || len(plan.Versions) != 2 {
		test.Fatalf("causal preparation: %+v %v", plan, err)
	}
	for _, version := range plan.Versions {
		want := ContributionSuperseded
		if version.Hash == latestHash {
			want = ContributionSelected
			if version.Digest().SnapshotID != pins[0].MemorySource || version.ProjectionHash == "" || !strings.Contains(version.Digest().Summary, "PINNED SUPPLEMENT") {
				test.Fatalf("pinned lineage lost: %+v", version)
			}
		}
		if version.State != want {
			test.Fatalf("causal state: %+v want=%s", version, want)
		}
	}
	if !plan.Excluded(pins[0].MemorySource) || plan.Excluded(inclusion.SnapshotID) || source.reads != 2 || source.lists != 0 {
		test.Fatalf("bounded pinned reader or exclusion changed: reads=%d lists=%d", source.reads, source.lists)
	}
	if !reflect.DeepEqual(source.snapshots, originalSnapshots) || plan.Snapshots[1].MemoryHash != "" {
		test.Fatal("preparation mutated input or retained mutable pointer")
	}
	pins[0], pins[1] = pins[1], pins[0]
	reordered, err := PrepareVerifiedContributionMemory(projectionTestContext(), source, repo, inclusion, source.snapshots, verified, pins)
	if err != nil || !reflect.DeepEqual(reordered, plan) {
		test.Fatalf("pin arrival order changed plan: %+v %v", reordered, err)
	}
}

func TestContributionMemoryOwnerKeepsAmbiguityAndEmptyStates(test *testing.T) {
	for _, mode := range []string{"explicit empty", "unpinned empty", "missing pin", "missing object", "incomplete chain", "incomparable", "independent missing pin"} {
		test.Run(mode, func(test *testing.T) {
			source, repo, inclusion, verified, pins := contributionMemoryInputs(test)
			publication := verified[0].Publications[pins[0].PublicationID]
			want := ContributionUnavailable
			wantConflict := true
			switch mode {
			case "explicit empty", "unpinned empty":
				publication.MemoryHash, pins[0].MemoryHash = "", ""
				if mode == "explicit empty" {
					want, wantConflict = ContributionEmpty, false
				} else {
					publication.MemoryPinned, pins[0].MemoryPinned = false, false
				}
				verified[0].Publications[publication.ID] = publication
			case "missing pin", "independent missing pin":
				pins = nil
				if mode == "independent missing pin" {
					source.snapshots[0].Parents = []domain.ContentHash{verified[0].Record.Capture}
					wantConflict = false
				}
			case "missing object":
				delete(source.memories, pins[0].MemoryHash)
			case "incomplete chain":
				digest := source.memories[pins[0].MemoryHash]
				digest.PreviousMemoryHash = projectionTestHash("missing causal parent")
				hash, err := domain.MemoryDigestHash(digest)
				if err != nil {
					test.Fatal(err)
				}
				source.memories[hash] = digest
				publication.MemoryHash, pins[0].MemoryHash = hash, hash
				verified[0].Publications[publication.ID] = publication
				want = ContributionReview
			case "incomparable":
				digest := domain.MemoryDigest{SnapshotID: publication.MemorySource, Summary: "INCOMPARABLE"}
				hash, err := domain.MemoryDigestHash(digest)
				if err != nil {
					test.Fatal(err)
				}
				source.memories[hash] = digest
				publication.ID, publication.MemoryHash = "incomparable", hash
				verified[0].Publications[publication.ID] = publication
				pins = append(pins, ContributionPin{ID: "incomparable-pin", RepoID: repo, ContributionID: verified[0].Record.ID, PublicationID: publication.ID, MemoryHash: hash, MemorySource: publication.MemorySource, MemoryPinned: true})
				want = ContributionReview
			}
			plan, err := PrepareVerifiedContributionMemory(projectionTestContext(), source, repo, inclusion, source.snapshots, verified, pins)
			if err != nil || len(plan.Versions) == 0 {
				test.Fatalf("state preparation: %+v %v", plan, err)
			}
			for _, version := range plan.Versions {
				if version.State != want || version.ProjectionHash != "" || version.Digest().Summary != "" {
					test.Fatalf("uncertain or empty pin imported memory: %+v", version)
				}
			}
			result, err := plan.Merge(projectionTestContext(), domain.MemoryProjection{})
			if errors.Is(err, domain.ErrConflict) != wantConflict || err != nil && !wantConflict || result.Digest.Summary != "" {
				test.Fatalf("conflict or empty merge changed: %+v %v", result, err)
			}
			if mode == "missing pin" && (len(plan.Versions[0].MissingPins) != 1 || plan.Versions[0].MissingPins[0].PublicationID != publication.ID) {
				test.Fatal("missing publication witness omitted")
			}
			if mode == "independent missing pin" && (!plan.Versions[0].Independent || plan.Excluded(verified[0].Record.Capture)) {
				test.Fatal("ordinary lineage lost independent authority")
			}
		})
	}
}

func TestContributionMemoryOwnerRejectsInvalidScopeTopologyAndBounds(test *testing.T) {
	for _, mode := range []string{"canceled", "foreign snapshot", "duplicate snapshot", "foreign record", "foreign pin", "merge mismatch", "publication mismatch", "cycle", "pin limit", "verified limit", "edge limit", "metadata limit", "wrong pinned source"} {
		test.Run(mode, func(test *testing.T) {
			source, repo, inclusion, verified, pins := contributionMemoryInputs(test)
			ctx := projectionTestContext()
			want := domain.ErrIntegrity
			switch mode {
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "foreign snapshot":
				source.snapshots[0].RepoID = projectionTestHash("foreign")
			case "duplicate snapshot":
				source.snapshots = append(source.snapshots, source.snapshots[0])
			case "foreign record":
				verified[0].Record.RepoID = projectionTestHash("foreign")
			case "foreign pin":
				pins[0].RepoID = projectionTestHash("foreign")
			case "merge mismatch":
				inclusion.Merges[0].MergeSHA = "other integration"
			case "publication mismatch":
				pins[0].MemoryHash = projectionTestHash("other pin")
			case "cycle":
				source.snapshots[1].Parents = []domain.ContentHash{source.snapshots[1].ID}
			case "pin limit":
				pins = make([]ContributionPin, MaxProjectionSnapshots+1)
				want = domain.ErrMemoryProjectionLimit
			case "verified limit":
				verified = make([]VerifiedContribution, MaxProjectionSnapshots+1)
				want = domain.ErrMemoryProjectionLimit
			case "edge limit":
				source.snapshots[0].Parents = make([]domain.ContentHash, MaxProjectionEdges+1)
				want = domain.ErrMemoryProjectionLimit
			case "metadata limit":
				pins[0].ID = strings.Repeat("p", MaxContributionMetadataBytes+1)
				want = domain.ErrMemoryProjectionLimit
			case "wrong pinned source":
				digest := domain.MemoryDigest{SnapshotID: inclusion.SnapshotID, Summary: "FOREIGN SOURCE"}
				hash, err := domain.MemoryDigestHash(digest)
				if err != nil {
					test.Fatal(err)
				}
				source.memories[hash] = digest
				publication := verified[0].Publications[pins[0].PublicationID]
				publication.MemoryHash, pins[0].MemoryHash = hash, hash
				verified[0].Publications[publication.ID] = publication
			}
			if _, err := PrepareVerifiedContributionMemory(ctx, source, repo, inclusion, source.snapshots, verified, pins); !errors.Is(err, want) {
				test.Fatalf("got %v want %v", err, want)
			}
			if mode == "canceled" && (source.reads != 0 || source.lists != 0) {
				test.Fatal("canceled preparation touched source")
			}
		})
	}
}

func TestContributionMemoryOwnerProjectsBranchWithOriginalWireHashes(test *testing.T) {
	source, repo, inclusion, verified, pins := contributionMemoryInputs(test)
	blocked := projectionTestHash("outside inclusion")
	source.snapshots[0].GraftParents = []domain.ContentHash{blocked}
	source.snapshots[1].GraftParents = []domain.ContentHash{blocked}
	plan, err := PrepareVerifiedContributionMemory(projectionTestContext(), source, repo, inclusion, source.snapshots, verified, pins)
	if err != nil {
		test.Fatal(err)
	}
	key, err := json.Marshal(struct {
		Version   int
		Inclusion domain.BranchContext
	}{1, inclusion})
	if err != nil {
		test.Fatal(err)
	}
	synthetic := domain.HashContent(key)
	rows := append([]domain.Snapshot(nil), plan.Snapshots...)
	for index := range rows {
		rows[index].GraftParents = []domain.ContentHash{}
	}
	rows = append(rows, domain.Snapshot{ID: synthetic, RepoID: repo, Parents: inclusion.Roots})
	base, err := ProjectMemory(projectionTestContext(), contributionProjectionSource{source: source, rows: rows}, repo, synthetic)
	if err != nil {
		test.Fatal(err)
	}
	raw, err := json.Marshal(struct {
		Version  int
		Base     domain.ContentHash
		Versions []ContributionVersion
	}{1, base.StateHash, plan.Versions})
	if err != nil {
		test.Fatal(err)
	}
	result, err := ProjectBranchWithPlan(projectionTestContext(), source, repo, inclusion, plan)
	if err != nil || !result.Found || result.StateHash != domain.HashContent(raw) || result.Digest.SnapshotID != inclusion.SnapshotID || !reflect.DeepEqual(result.Inclusion, &inclusion) {
		test.Fatalf("branch projection identity changed: %+v %v", result, err)
	}
	if !strings.Contains(result.Digest.Summary, "BASE DECISION") || !strings.Contains(result.Digest.Summary, "PINNED") || strings.Contains(result.Digest.Summary, "TIP DECISION") || result.Digest.PreviousMemoryHash != "" || result.Digest.GraftCoverage != nil {
		test.Fatalf("mutable or outside-inclusion memory leaked: %+v", result.Digest)
	}
	if source.lists != 0 || source.snapshots[0].GraftParents[0] != blocked || source.snapshots[1].GraftParents[0] != blocked {
		test.Fatal("branch projection rediscovered snapshots or mutated graft input")
	}
	wire, err := json.Marshal(plan.Versions)
	if err != nil || strings.Contains(string(wire), "PINNED") || !strings.Contains(string(wire), "\"source_snapshot\"") || !strings.Contains(string(wire), "\"memory_hash\"") {
		test.Fatalf("private digest or version wire changed: %s %v", wire, err)
	}
	ctx, cancel := context.WithCancel(projectionTestContext())
	cancel()
	reads := source.reads
	if _, err := ProjectBranchWithPlan(ctx, source, repo, inclusion, plan); !errors.Is(err, context.Canceled) || source.reads != reads {
		test.Fatalf("canceled branch projection touched source: %v", err)
	}
}

func TestPRContributionMemoryMergeUsesInclusionOrderAndRemovesStaleFragments(test *testing.T) {
	oldest, contribution, current := projectionTestHash("oldest"), projectionTestHash("contribution"), projectionTestHash("current")
	plan := ContributionPlan{
		excluded: map[domain.ContentHash]bool{contribution: true},
		order:    map[domain.ContentHash]int{oldest: 2, contribution: 1, current: 0},
		Versions: []ContributionVersion{{Source: contribution, Hash: projectionTestHash("pin"), State: "selected", digest: domain.MemoryDigest{SnapshotID: contribution, Summary: "PINNED"}}},
	}
	base := domain.MemoryProjection{StateHash: projectionTestHash("projection"), Found: true, Digest: domain.MemoryDigest{SnapshotID: current, Fragments: []domain.MemoryFragment{
		{SourceSnapshot: oldest, Summary: "OLDEST"}, {SourceSnapshot: contribution, Summary: "STALE MUTABLE FALLBACK"}, {SourceSnapshot: current, Summary: "CURRENT"},
	}}}
	result, err := plan.Merge(projectionTestContext(), base)
	if err != nil {
		test.Fatal(err)
	}
	if len(result.Digest.Fragments) != 3 || result.Digest.Fragments[0].SourceSnapshot != oldest || result.Digest.Fragments[1].SourceSnapshot != contribution || result.Digest.Fragments[2].SourceSnapshot != current || strings.Contains(result.Digest.Summary, "STALE") {
		test.Fatalf("inclusion order or pin lost: %+v", result.Digest)
	}
	if base.Digest.Fragments[1].Summary != "STALE MUTABLE FALLBACK" {
		test.Fatal("projection input mutated")
	}
}
