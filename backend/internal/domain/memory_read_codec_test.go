package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

func memoryReadTestItems(count int) []EffectiveMemoryItem {
	items := make([]EffectiveMemoryItem, count)
	for index := range items {
		items[index] = EffectiveMemoryItem{
			ID: HashContent(fmt.Appendf(nil, "original item %d", index)), SourceSnapshot: HashContent([]byte("source snapshot")),
			Kind: "legacy_summary", Text: fmt.Sprintf("historical text %d", index), TextHash: HashContent([]byte("complete historical text")),
			Part: index, MemoryClaimAssessment: MemoryClaimAssessment{State: "review", Reason: "untyped_historical_text"},
		}
	}
	return items
}

func memoryReadTestHeader(test *testing.T, items []EffectiveMemoryItem) MemoryReadHeader {
	test.Helper()
	header := MemoryReadHeader{
		Version: MemoryReadVersion, Rules: MemoryReadRules, CaptureID: HashContent([]byte("capture")),
		Source: RepositoryMemorySourceGeneration{
			Version: RepositoryMemorySourceVersion, Epoch: "23456789-1234-4321-9876-123456789abc", Sequence: 9007199254740995,
			Graph: RepositoryViewSourceCheckpoint{
				Version: RepositoryViewSourceVersion, Scope: RepositoryGraphSourceScope,
				RepoID: HashContent([]byte("repository")), Epoch: "12345678-1234-4321-9876-123456789abc", Sequence: 9007199254740993,
			},
		},
		Origin: "https://example.invalid/owner/repo",
		View: EffectiveMemoryPage{
			Selection: EffectiveMemorySelection{SnapshotID: HashContent([]byte("selected snapshot")), CodeCommit: strings.Repeat("a", 40)},
			StateHash: HashContent([]byte("state")), LineageHash: HashContent([]byte("lineage")), Total: len(items),
			Revision: RepositoryRevision{Graph: 9007199254740997, Pending: 1, Evidence: 9007199254740999},
		},
	}
	memoryReadTestDelivery(test, &header, items)
	return header
}

func memoryReadTestDelivery(test *testing.T, header *MemoryReadHeader, items []EffectiveMemoryItem) {
	test.Helper()
	digest, err := EffectiveMemoryDeliveryStateHash(context.Background(), header.Source.Graph.RepoID, header.Origin, header.View, items)
	if err != nil {
		test.Fatal(err)
	}
	header.View.DeliveryStateHash = digest
}

func memoryReadTestBuild(test *testing.T, header MemoryReadHeader, items []EffectiveMemoryItem) MemoryReadBundle {
	test.Helper()
	bundle, err := BuildMemoryReadBundle(context.Background(), header, items)
	if err != nil {
		test.Fatal(err)
	}
	if err := ValidateMemoryReadBundle(context.Background(), bundle); err != nil {
		test.Fatal("built bundle failed validation", err)
	}
	return bundle
}

func memoryReadTestCopy[Value any](test *testing.T, value Value) Value {
	test.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	var copied Value
	if err := json.Unmarshal(raw, &copied); err != nil {
		test.Fatal(err)
	}
	return copied
}

func TestMemoryReadRoundTripAndOwnership(test *testing.T) {
	items := memoryReadTestItems(130)
	items[0] = EffectiveMemoryItem{
		ID: items[0].ID, SourceSnapshot: items[0].SourceSnapshot, Kind: "code", Text: "preserve <scope> & \"evidence\" \ud55c\uae00 😀",
		Code:                  &MemoryCodeScope{Commit: strings.Repeat("b", 40), Parent: strings.Repeat("c", 40), Paths: []string{"src/main.go"}},
		MemoryClaimAssessment: MemoryClaimAssessment{State: "applied", Reason: "declared_scope_matches"},
		PublicationIDs:        []string{"publication-id", "another-publication"}, IntegrationReceipt: "opaque-receipt-id",
		Paths: []CodePathState{{Path: "src/main.go", State: "applied", Before: GitEntry{}, After: GitEntry{OID: strings.Repeat("d", 40), Mode: "100755"}, Selected: GitEntry{OID: strings.Repeat("d", 40), Mode: "100755"}}},
	}
	header := memoryReadTestHeader(test, items)
	header.View.Selection.Branch = "main"
	header.View.Inclusion = &BranchContext{
		BranchID: "main-identity", SnapshotID: header.View.Selection.SnapshotID, CodeCommit: header.View.Selection.CodeCommit, Reason: "selected",
		Merges: []BranchContextMerge{{EventID: "merge-id", Source: items[0].SourceSnapshot, Before: header.View.Selection.SnapshotID, Order: -1, Reason: "review"}},
		Roots:  []ContentHash{items[0].SourceSnapshot}, SnapshotIDs: []ContentHash{header.View.Selection.SnapshotID, items[0].SourceSnapshot},
	}
	header.Dependencies = []MemoryReadDependency{{Hash: HashContent([]byte("first")), Available: true}, {Hash: HashContent([]byte("second")), Available: false}}
	memoryReadTestDelivery(test, &header, items)
	originalHeader, originalItems := memoryReadTestCopy(test, header), memoryReadTestCopy(test, items)
	bundle := memoryReadTestBuild(test, header, items)
	if !reflect.DeepEqual(header, originalHeader) || !reflect.DeepEqual(items, originalItems) {
		test.Fatal("build mutated its inputs")
	}
	if len(bundle.Chunks) != 3 || bundle.Header.Chunks[0].Count != 64 || bundle.Header.Chunks[1].Start != 64 || bundle.Header.Chunks[2].Count != 2 {
		test.Fatal("unexpected ordered chunk index", bundle.Header.Chunks)
	}
	var decoded []EffectiveMemoryItem
	for ordinal, chunk := range bundle.Chunks {
		part, err := DecodeMemoryReadChunk(context.Background(), bundle.Header, ordinal, chunk.Bytes)
		if err != nil {
			test.Fatal(err)
		}
		decoded = append(decoded, part...)
	}
	if !reflect.DeepEqual(decoded, items) {
		test.Fatal("round trip lost fields, source identifiers, or item order")
	}
	rawHeader, err := MemoryReadHeaderBytes(context.Background(), bundle.Header)
	if err != nil {
		test.Fatal(err)
	}
	canonical, err := canonicalJSON(bundle.Header)
	if err != nil || !bytes.Equal(rawHeader, canonical) || bundle.ID != HashContent(canonical) {
		test.Fatal("identity is not the canonical header hash", err)
	}
	slices.Reverse(header.Dependencies)
	rebuilt := memoryReadTestBuild(test, header, items)
	if !reflect.DeepEqual(bundle, rebuilt) {
		test.Fatal("dependency input order affected deterministic output")
	}
	header.Dependencies[0].Available = !header.Dependencies[0].Available
	header.View.Inclusion.Merges[0].EventID = "mutated"
	header.View.Inclusion.Roots[0] = HashContent([]byte("mutated"))
	header.View.Inclusion.SnapshotIDs[0] = HashContent([]byte("mutated"))
	items[0].Code.Paths[0] = "mutated"
	items[0].Paths[0].Reason = "mutated"
	items[0].PublicationIDs[0] = "mutated"
	if err := ValidateMemoryReadBundle(context.Background(), bundle); err != nil {
		test.Fatal("bundle retained aliases to caller-owned memory", err)
	}
	bundle.Chunks[0].Bytes[0] = '!'
	if err := ValidateMemoryReadBundle(context.Background(), rebuilt); err != nil {
		test.Fatal("separate builds share chunk buffers", err)
	}
}

func TestMemoryReadLegacyAndReviewVariants(test *testing.T) {
	kinds := []string{"code", "decision", "rationale", "legacy_text", "legacy_summary", "legacy_fact", "legacy_task"}
	items := memoryReadTestItems(len(kinds))
	for index, kind := range kinds {
		items[index].Kind = kind
	}
	items[0].Code = &MemoryCodeScope{Commit: strings.Repeat("1", 40), Parent: strings.Repeat("2", 40), Paths: []string{"reviewable\\scope"}}
	items[0].Reason = "declared_scope_invalid"
	items[0].Paths = []CodePathState{{Path: "file", State: "unknown", Reason: "incomplete_ancestry"}}
	items[1].State, items[2].State = "retained", "retained"
	items[1].ID = items[2].ID
	for _, content := range []string{"", "claims", "prompt"} {
		header := memoryReadTestHeader(test, items)
		header.View.Content = content
		memoryReadTestDelivery(test, &header, items)
		bundle := memoryReadTestBuild(test, header, items)
		decoded, err := DecodeMemoryReadChunk(context.Background(), bundle.Header, 0, bundle.Chunks[0].Bytes)
		if err != nil || !reflect.DeepEqual(decoded, items) {
			test.Fatal("codec changed projection semantics or deduplicated source IDs", err)
		}
	}
}

func TestMemoryReadRejectsChunkAndHeaderTampering(test *testing.T) {
	items := memoryReadTestItems(130)
	original := memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
	cases := []struct {
		name   string
		mutate func(*MemoryReadBundle)
	}{
		{"missing", func(bundle *MemoryReadBundle) { bundle.Chunks = bundle.Chunks[:2] }},
		{"extra", func(bundle *MemoryReadBundle) { bundle.Chunks = append(bundle.Chunks, bundle.Chunks[0]) }},
		{"reordered", func(bundle *MemoryReadBundle) {
			bundle.Chunks[0], bundle.Chunks[1] = bundle.Chunks[1], bundle.Chunks[0]
		}},
		{"modified bytes", func(bundle *MemoryReadBundle) { bundle.Chunks[0].Bytes[10] ^= 1 }},
		{"modified chunk hash", func(bundle *MemoryReadBundle) { bundle.Chunks[0].Hash = HashContent(nil) }},
		{"modified index hash", func(bundle *MemoryReadBundle) { bundle.Header.Chunks[0].Hash = HashContent(nil) }},
		{"missing index", func(bundle *MemoryReadBundle) { bundle.Header.Chunks = nil }},
		{"gap", func(bundle *MemoryReadBundle) { bundle.Header.Chunks[1].Start++ }},
		{"overlap", func(bundle *MemoryReadBundle) { bundle.Header.Chunks[1].Start-- }},
		{"wrong size", func(bundle *MemoryReadBundle) { bundle.Header.Chunks[0].Bytes++ }},
		{"wrong total", func(bundle *MemoryReadBundle) { bundle.Header.View.Total-- }},
		{"wrong root", func(bundle *MemoryReadBundle) { bundle.ID = HashContent(nil) }},
		{"empty root", func(bundle *MemoryReadBundle) { bundle.ID = "" }},
		{"capture", func(bundle *MemoryReadBundle) { bundle.Header.CaptureID = HashContent(nil) }},
		{"origin", func(bundle *MemoryReadBundle) { bundle.Header.Origin += "/changed" }},
		{"revision", func(bundle *MemoryReadBundle) { bundle.Header.View.Revision.Graph++ }},
		{"generation", func(bundle *MemoryReadBundle) { bundle.Header.Source.Sequence++ }},
	}
	for _, entry := range cases {
		test.Run(entry.name, func(test *testing.T) {
			bundle := memoryReadTestCopy(test, original)
			entry.mutate(&bundle)
			if err := ValidateMemoryReadBundle(context.Background(), bundle); !errors.Is(err, ErrIntegrity) {
				test.Fatal("tampered bundle was accepted", err)
			}
		})
	}
	if decoded, err := DecodeMemoryReadChunk(context.Background(), original.Header, 0, original.Chunks[1].Bytes); !errors.Is(err, ErrIntegrity) || decoded != nil {
		test.Fatal("wrong ordinal body accepted", err)
	}
	for _, ordinal := range []int{-1, len(original.Chunks)} {
		if decoded, err := DecodeMemoryReadChunk(context.Background(), original.Header, ordinal, original.Chunks[0].Bytes); !errors.Is(err, ErrIntegrity) || decoded != nil {
			test.Fatal("invalid ordinal accepted", err)
		}
	}
	selected, err := DecodeMemoryReadChunk(context.Background(), original.Header, 2, original.Chunks[2].Bytes)
	if err != nil || !reflect.DeepEqual(selected, items[128:]) {
		test.Fatal("selected chunk required unrelated bodies", err)
	}
}

func TestMemoryReadDeliveryBindingAndEmptyProjection(test *testing.T) {
	emptyHeader := memoryReadTestHeader(test, nil)
	empty := memoryReadTestBuild(test, emptyHeader, nil)
	if len(empty.Chunks) != 0 || len(empty.Header.Chunks) != 0 || empty.ID == "" {
		test.Fatal("empty projection is not a complete indexed bundle")
	}
	items := memoryReadTestItems(1)
	header := memoryReadTestHeader(test, items)
	if bundle, err := BuildMemoryReadBundle(context.Background(), header, nil); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(bundle, MemoryReadBundle{}) {
		test.Fatal("partial projection accepted as empty", err)
	}
	original := memoryReadTestBuild(test, header, items)
	if original.ID == empty.ID {
		test.Fatal("empty and nonempty identities collided")
	}
	items[0].Reason = "modified assessment"
	if _, err := BuildMemoryReadBundle(context.Background(), header, items); !errors.Is(err, ErrIntegrity) {
		test.Fatal("build accepted a stale delivery proof", err)
	}
	modified := memoryReadTestCopy(test, original)
	modified.Chunks[0].Bytes, _ = json.Marshal(items)
	modified.Chunks[0].Hash = HashContent(modified.Chunks[0].Bytes)
	modified.Header.Chunks[0].Hash = modified.Chunks[0].Hash
	modified.Header.Chunks[0].Bytes = len(modified.Chunks[0].Bytes)
	raw, err := MemoryReadHeaderBytes(context.Background(), modified.Header)
	if err != nil {
		test.Fatal(err)
	}
	modified.ID = HashContent(raw)
	if err := ValidateMemoryReadBundle(context.Background(), modified); !errors.Is(err, ErrIntegrity) {
		test.Fatal("rehashed chunk/header bypassed delivery semantics", err)
	}
	changed := original.Header
	changed.View.Revision.Graph++
	changed.View.StateHash = HashContent([]byte("new cursor state"))
	changed.Source.Sequence++
	rebuilt := memoryReadTestBuild(test, changed, memoryReadTestItems(1))
	if rebuilt.Header.View.DeliveryStateHash != original.Header.View.DeliveryStateHash || rebuilt.ID == original.ID {
		test.Fatal("cursor and generation changes confused root and semantic proof")
	}
}

func TestMemoryReadConcurrentBuildAndDecode(test *testing.T) {
	items := memoryReadTestItems(65)
	header := memoryReadTestHeader(test, items)
	bundle := memoryReadTestBuild(test, header, items)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 3 {
				built, err := BuildMemoryReadBundle(context.Background(), header, items)
				if err != nil || built.ID != bundle.ID {
					test.Error("concurrent deterministic build", err)
					return
				}
				if err := ValidateMemoryReadBundle(context.Background(), bundle); err != nil {
					test.Error("concurrent validation", err)
				}
			}
		})
	}
	workers.Wait()
}
