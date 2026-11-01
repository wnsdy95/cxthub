//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type pgPreparedRootIndexesNativeObject struct {
	hash      domain.ContentHash
	position  int
	rangePage bool
	raw       []byte
}

type pgPreparedRootIndexesNativeExpected struct {
	header            []byte
	rangeDescriptor   []byte
	visibleDescriptor []byte
	items             []pgPreparedRootIndexesNativeObject
}

func pgPreparedRootIndexesNativeOracle(test *testing.T, prepared *preparedStreamingRootDoc) pgPreparedRootIndexesNativeExpected {
	test.Helper()
	ctx := context.Background()
	_, header, err := verifiedRangeHeader(prepared.root.ranges)
	if err != nil {
		test.Fatal(err)
	}
	proof, objects, err := domain.BuildVerifiedDocRangeGeneration(ctx, prepared.root.ranges)
	if err != nil {
		test.Fatal(err)
	}
	rangeDescriptor, err := proof.Descriptor().Encode()
	if err != nil {
		test.Fatal(err)
	}
	expected := pgPreparedRootIndexesNativeExpected{header: header, rangeDescriptor: rangeDescriptor}
	derived := 0
	for _, object := range objects {
		expected.items = append(expected.items, pgPreparedRootIndexesNativeObject{hash: object.Hash, position: derived, raw: bytes.Clone(object.Bytes)})
		derived++
	}
	visible, err := domain.BuildDocVisibleGenerationWithRangePages(ctx, proof, func(emission context.Context, object domain.DocRangeObject) error {
		expected.items = append(expected.items, pgPreparedRootIndexesNativeObject{hash: object.Hash, position: derived, raw: bytes.Clone(object.Bytes)})
		derived++
		return emission.Err()
	}, func(emission context.Context, position int, object domain.DocRangeObject) error {
		expected.items = append(expected.items, pgPreparedRootIndexesNativeObject{hash: object.Hash, position: position, rangePage: true, raw: bytes.Clone(object.Bytes)})
		return emission.Err()
	})
	if err != nil {
		test.Fatal(err)
	}
	expected.visibleDescriptor, err = visible.Encode()
	if err != nil {
		test.Fatal(err)
	}
	return expected
}

func pgPreparedRootIndexesNativePublication(test *testing.T, events int) *pgStreamingRootPublication {
	test.Helper()
	publication := pgRootWitnessPublicationFixture(test, events)
	if publication.indexes == nil {
		indexes, err := preparePGRootIndexes(context.Background(), publication.preparedStreamingRootDoc)
		if err != nil {
			test.Fatal(err)
		}
		publication.indexes = indexes
	}
	if publication.indexes == nil || publication.indexes.spool == nil || publication.indexes.spool.file == nil {
		test.Fatal("native index preparation returned no owned spool")
	}
	spool := publication.indexes.spool
	test.Cleanup(func() { _ = spool.Close() })
	return publication
}

func pgPreparedRootIndexesNativeAssert(test *testing.T, prepared *preparedStreamingRootDoc, indexes *pgPreparedRootIndexes, expected pgPreparedRootIndexesNativeExpected) int64 {
	test.Helper()
	if indexes == nil || indexes.spool == nil || indexes.spool.file == nil || indexes.spool.writer == nil {
		test.Fatal("prepared indexes have no readable spool")
	}
	if indexes.spool == prepared.spool || indexes.spool.file.Name() == prepared.spool.file.Name() {
		test.Fatal("index preparation reused or replaced the verified source spool")
	}
	if indexes.spool.writer.Buffered() != 0 {
		test.Fatal("index preparation returned before flushing its serialized objects")
	}
	if indexes.document != prepared.job.DocHash || indexes.rangeHash != domain.HashContent(expected.rangeDescriptor) || indexes.visibleHash != domain.HashContent(expected.visibleDescriptor) {
		test.Fatal("prepared descriptor hashes do not bind the verified document")
	}
	if !bytes.Equal(indexes.header, expected.header) || !bytes.Equal(indexes.rangeDescriptor, expected.rangeDescriptor) || !bytes.Equal(indexes.visibleDescriptor, expected.visibleDescriptor) {
		test.Fatal("prepared header or descriptors differ from the existing domain builders")
	}
	ranges, err := domain.DecodeDocRangeGeneration(indexes.document, indexes.rangeHash, indexes.rangeDescriptor)
	if err != nil {
		test.Fatal("prepared range descriptor is not independently decodable", err)
	}
	visible, err := domain.DecodeDocVisibleGeneration(indexes.document, indexes.rangeHash, indexes.visibleHash, indexes.visibleDescriptor)
	if err != nil {
		test.Fatal("prepared visible descriptor is not independently decodable", err)
	}
	expectedDerived := 0
	for _, expectedObject := range expected.items {
		if !expectedObject.rangePage {
			expectedDerived++
		}
	}
	if len(indexes.items) != len(expected.items) || len(indexes.spool.offsets) != expectedDerived+1 || indexes.spool.offsets[0] != 0 {
		test.Fatalf("prepared object sequence or derived-only spool changed: items=%d offsets=%d expected items=%d derived=%d", len(indexes.items), len(indexes.spool.offsets), len(expected.items), expectedDerived)
	}
	ctx := context.Background()
	header := prepared.root.ranges.Header()
	seenPages := make([]bool, len(header.Pages))
	objects := make(map[domain.ContentHash][]byte)
	reference := make(map[domain.ContentHash][]byte)
	derived := 0
	var totalBytes int64
	for ordinal, expectedObject := range expected.items {
		item := indexes.items[ordinal]
		if item.hash != expectedObject.hash || item.rangePage != expectedObject.rangePage || item.position != expectedObject.position {
			test.Fatalf("object emission order or source/derived ordinal changed at item %d", ordinal)
		}
		spool := indexes.spool
		if item.rangePage {
			spool = prepared.spool
		} else if item.position != derived {
			test.Fatal("derived object ordinals are not consecutive", ordinal, item.position, derived)
		}
		raw, err := spool.Read(ctx, item.position)
		if err != nil || !bytes.Equal(raw, expectedObject.raw) || domain.HashContent(raw) != item.hash {
			test.Fatalf("referenced source or derived object %d differs from the builder bytes or hash: %v", ordinal, err)
		}
		if previous, exists := objects[item.hash]; exists && !bytes.Equal(previous, raw) {
			test.Fatal("a repeated object hash changed bytes", ordinal)
		}
		objects[item.hash] = raw
		reference[expectedObject.hash] = expectedObject.raw
		if !item.rangePage {
			derived++
			totalBytes += int64(len(raw))
			if indexes.spool.offsets[derived] != totalBytes {
				test.Fatal("derived spool offsets do not preserve exact object boundaries", ordinal)
			}
		}
		if item.rangePage {
			if item.position < 0 || item.position >= len(seenPages) || seenPages[item.position] {
				test.Fatal("prepared range page has a duplicate or invalid ordinal", item.position)
			}
			seenPages[item.position] = true
			source, err := prepared.root.ranges.Page(ctx, item.position)
			if err != nil || !bytes.Equal(raw, source) || item.hash != header.Pages[item.position] {
				test.Fatal("prepared range page differs from the verified source", item.position, err)
			}
		}
	}
	if derived != expectedDerived {
		test.Fatal("derived spool contains copied source pages or missing derived objects", derived, expectedDerived)
	}
	load := func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, exists := objects[hash]
		if !exists {
			return nil, domain.ErrNotFound
		}
		return raw, nil
	}
	for position, seen := range seenPages {
		if !seen {
			test.Fatal("verified range page was not emitted", position)
		}
		hash, err := domain.ResolveDocRangeLeaf(ctx, ranges.EventPages, position, load)
		if err != nil || hash != header.Pages[position] {
			test.Fatal("prepared range directory changed page order", position, err)
		}
	}
	for _, mode := range []string{"all", "chat", "prompts"} {
		if visible.Events == 0 {
			continue
		}
		for _, position := range []int{0, visible.Events / 2, visible.Events - 1} {
			unit, ordinal, err := domain.ReadDocVisibleUnit(ctx, visible, mode, position, true, load)
			want, expectedOrdinal, expectedErr := domain.ReadDocVisibleUnit(ctx, visible, mode, position, true, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
				return reference[hash], nil
			})
			if err != nil || expectedErr != nil || ordinal != expectedOrdinal || unit != want {
				test.Fatal("prepared visible tree changed native event order", mode, position, unit, ordinal, err)
			}
		}
	}
	info, err := indexes.spool.file.Stat()
	if err != nil {
		test.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != totalBytes || indexes.spool.writer.Buffered() != 0 {
		test.Fatal("prepared spool is not private, complete, and fully flushed", info, totalBytes)
	}
	return totalBytes
}

func TestPGPreparedRootIndexesNativeBuilderParity(test *testing.T) {
	for _, events := range []int{0, 1, 129, domain.DocRangePageEvents*domain.DocRangeDirectoryFanout + 1} {
		test.Run(fmt.Sprint(events), func(test *testing.T) {
			prepared := preparedRootResourcesFixture(test, events)
			expected := pgPreparedRootIndexesNativeOracle(test, prepared)
			indexes, err := preparePGRootIndexes(context.Background(), prepared)
			if err != nil || indexes == nil || indexes.spool == nil {
				test.Fatal("native index preparation failed", err)
			}
			defer indexes.spool.Close()
			witness, err := newPGRootSourceWitness(prepared, pgRootWitnessFixtureSources(prepared))
			if err != nil {
				test.Fatal(err)
			}
			publication := &pgStreamingRootPublication{preparedStreamingRootDoc: prepared, source: witness, indexes: indexes}
			test.Cleanup(func() { _ = publication.Close() })
			spoolBytes := pgPreparedRootIndexesNativeAssert(test, publication.preparedStreamingRootDoc, publication.indexes, expected)
			if events > domain.DocRangePageEvents*domain.DocRangeDirectoryFanout && len(publication.root.ranges.Header().Pages) <= domain.DocRangeDirectoryFanout {
				test.Fatal("large native fixture did not cross the range directory fanout")
			}
			source, err := publication.spool.file.Stat()
			if err != nil {
				test.Fatal(err)
			}
			resources, err := publication.RetainedResources()
			if err != nil || resources.HeapBytes <= 0 || resources.HeapBytes >= 16<<20 || resources.SpoolBytes != source.Size()+spoolBytes || resources.SpoolBytes >= 64<<20 {
				test.Fatal("publication omitted a native spool or exceeded the bounded fixture budget", resources, source.Size(), spoolBytes, err)
			}
		})
	}
}

func TestPGPreparedRootIndexesNativeMixedEventBranches(test *testing.T) {
	rangeSpoolTempDir(test)
	fixture := rangeFixture(test, 0)
	fixture.cir.Envelope.CIRVersion = "2"
	pattern := []domain.CIREvent{
		{Kind: domain.EventToolResult, CallID: "leading", Output: "leading work"},
		{Kind: domain.EventReasoning},
		{Kind: domain.EventTurn, Role: domain.RoleAssistant},
		{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "[cxt seed] Branch-switch context: inherited"}}},
		{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "native prompt \ud55c <&>"}}},
		{Kind: domain.EventToolCall, CallID: "read", ToolName: "read", Input: map[string]any{"path": "native"}},
		{Kind: domain.EventToolResult, CallID: "read", Output: "tool result"},
		{Kind: domain.EventReasoning, RedactedSummary: " \t\n\u3000"},
		{Kind: domain.EventReasoning, RedactedSummary: "visible work"},
		{Kind: domain.EventMessage, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: "native answer"}}},
		{Kind: domain.EventMessage, Role: domain.RoleUser, CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: "compact summary"}}},
		{Kind: domain.EventCompaction, Replacement: []domain.CIREvent{}, ReplacementComplete: true},
		{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: " \ufeff[cxt] This session was resumed from a branch context seed. inherited"}}},
	}
	for position := 0; position < 3*domain.DocRangePageEvents+7; position++ {
		event := pattern[position%len(pattern)]
		event.Seq = position + 1
		fixture.cir.Events = append(fixture.cir.Events, event)
	}
	var err error
	fixture.manifest, fixture.bodies, err = domain.ConversationManifestForCIR(fixture.cir)
	if err != nil {
		test.Fatal(err)
	}
	fixture.hash, err = domain.ConversationManifestHash(fixture.manifest)
	if err != nil {
		test.Fatal(err)
	}
	fixture.manifestBytes, err = domain.CanonicalConversationManifest(fixture.manifest)
	if err != nil {
		test.Fatal(err)
	}
	job := rootPublicationJob(test, domain.HashContent([]byte(test.Name())), fixture).Claim(time.Now().UTC(), time.Minute)
	prepared, err := prepareStreamingRootJob(context.Background(), job, &domain.CanonicalDocVerifier{}, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		return fixture.bodies[hash], nil
	})
	if err != nil {
		test.Fatal(err)
	}
	defer prepared.Close()
	expected := pgPreparedRootIndexesNativeOracle(test, prepared)
	indexes, err := preparePGRootIndexes(context.Background(), prepared)
	if err != nil || indexes == nil || indexes.spool == nil {
		test.Fatal("branch-heavy native preparation failed", err)
	}
	defer indexes.spool.Close()
	pgPreparedRootIndexesNativeAssert(test, prepared, indexes, expected)
	visible, err := domain.DecodeDocVisibleGeneration(indexes.document, indexes.rangeHash, indexes.visibleHash, indexes.visibleDescriptor)
	if err != nil || visible.Rendered >= visible.Events || visible.Views["chat"].Units >= visible.Views["all"].Units || visible.Views["prompts"].Units >= visible.Views["chat"].Units {
		test.Fatal("mixed fixture did not exercise empty reasoning, folding, and prompt branches", visible, err)
	}
}

func pgPreparedRootIndexesNativeAssertUsage(test *testing.T, beforeUsed int64, beforeFiles int) {
	test.Helper()
	after := transientSpoolBudgetUsage()
	if after.UsedBytes != beforeUsed || after.OpenFiles != beforeFiles {
		test.Fatalf("index preparation leaked spool reservations: used=%d/%d files=%d/%d", after.UsedBytes, beforeUsed, after.OpenFiles, beforeFiles)
	}
}

func pgPreparedRootIndexesNativeAssertOnlySource(test *testing.T, source *docRangeSpool) {
	test.Helper()
	entries, err := os.ReadDir(filepath.Dir(source.file.Name()))
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(source.file.Name()) {
		test.Fatal("failed index preparation leaked an output spool or removed the source", entries)
	}
}

type pgPreparedRootIndexesNativeContext struct {
	context.Context
	check func()
}

func (observed *pgPreparedRootIndexesNativeContext) Err() error {
	if err := observed.Context.Err(); err != nil {
		return err
	}
	observed.check()
	return observed.Context.Err()
}

func TestPGPreparedRootIndexesNativeCancellationCleansOutputOnly(test *testing.T) {
	for _, stage := range []string{"before-start", "after-allocation", "after-write"} {
		test.Run(stage, func(test *testing.T) {
			prepared := preparedRootResourcesFixture(test, 3*domain.DocRangePageEvents+1)
			expected := pgPreparedRootIndexesNativeOracle(test, prepared)
			before := transientSpoolBudgetUsage()
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed := false
			ctx := &pgPreparedRootIndexesNativeContext{Context: base, check: func() {
				if stage == "after-allocation" {
					if transientSpoolBudgetUsage().OpenFiles > before.OpenFiles {
						observed = true
						cancel()
					}
					return
				}
				if stage != "after-write" {
					return
				}
				entries, err := os.ReadDir(filepath.Dir(prepared.spool.file.Name()))
				if err != nil {
					test.Fatal(err)
				}
				for _, entry := range entries {
					if entry.Name() == filepath.Base(prepared.spool.file.Name()) {
						continue
					}
					info, err := entry.Info()
					if err != nil {
						test.Fatal(err)
					}
					if info.Size() > 0 {
						observed = true
						cancel()
					}
				}
			}}
			if stage == "before-start" {
				observed = true
				cancel()
			}
			indexes, err := preparePGRootIndexes(ctx, prepared)
			if indexes != nil && indexes.spool != nil {
				defer indexes.spool.Close()
			}
			if !observed || indexes != nil || !errors.Is(err, context.Canceled) {
				test.Fatal("cancellation returned a prepared capability or missed its checkpoint", stage, observed, indexes != nil, err)
			}
			pgPreparedRootIndexesNativeAssertUsage(test, before.UsedBytes, before.OpenFiles)
			pgPreparedRootIndexesNativeAssertOnlySource(test, prepared.spool)
			if err := prepared.matches(prepared.job); err != nil {
				test.Fatal("canceling index preparation invalidated its source", err)
			}
			retried, err := preparePGRootIndexes(context.Background(), prepared)
			if err != nil || retried == nil || retried.spool == nil {
				test.Fatal("canceled index preparation could not reuse its verified source", err)
			}
			defer retried.spool.Close()
			pgPreparedRootIndexesNativeAssert(test, prepared, retried, expected)
		})
	}
}

func TestPGPreparedRootIndexesNativeFailuresCleanOutput(test *testing.T) {
	for _, damage := range []string{"nil-preparation", "empty-preparation", "unverified-index", "closed-source", "truncated-source", "corrupt-source", "spool-create"} {
		test.Run(damage, func(test *testing.T) {
			prepared := preparedRootResourcesFixture(test, 129)
			source := prepared.spool
			before := transientSpoolBudgetUsage()
			want := error(nil)
			switch damage {
			case "nil-preparation":
				prepared = nil
				want = domain.ErrIntegrity
			case "empty-preparation":
				prepared = &preparedStreamingRootDoc{}
				want = domain.ErrIntegrity
			case "unverified-index":
				prepared.root.ranges = domain.VerifiedDocRangeIndex{}
				want = domain.ErrIntegrity
			case "closed-source":
				if err := source.file.Close(); err != nil {
					test.Fatal(err)
				}
			case "truncated-source":
				if err := source.file.Truncate(0); err != nil {
					test.Fatal(err)
				}
			case "corrupt-source":
				if _, err := source.file.WriteAt([]byte("!"), 0); err != nil {
					test.Fatal(err)
				}
				want = domain.ErrIntegrity
			case "spool-create":
				missing := filepath.Join(filepath.Dir(source.file.Name()), "missing")
				for _, variable := range []string{"TMPDIR", "TMP", "TEMP"} {
					test.Setenv(variable, missing)
				}
				want = os.ErrNotExist
			}
			indexes, err := preparePGRootIndexes(context.Background(), prepared)
			if indexes != nil && indexes.spool != nil {
				defer indexes.spool.Close()
			}
			if indexes != nil || err == nil || want != nil && !errors.Is(err, want) {
				test.Fatal("failed preparation yielded indexes or lost the failure class", damage, indexes != nil, err, want)
			}
			pgPreparedRootIndexesNativeAssertUsage(test, before.UsedBytes, before.OpenFiles)
			pgPreparedRootIndexesNativeAssertOnlySource(test, source)
		})
	}
}

func TestPGPreparedRootIndexesNativeQuotaFailureReleasesPartialOutput(test *testing.T) {
	const limit = 64 << 20
	transientSpoolBudgetConfigure(test, limit)
	prepared := preparedRootResourcesFixture(test, 129)
	expected := pgPreparedRootIndexesNativeOracle(test, prepared)
	derived := 0
	var firstDerivedBytes int64
	for _, expectedObject := range expected.items {
		if expectedObject.rangePage {
			continue
		}
		if derived == 0 {
			firstDerivedBytes = int64(len(expectedObject.raw))
		}
		derived++
	}
	if derived < 2 {
		test.Fatal("quota fixture has no partial output boundary")
	}
	held := transientSpoolBudgetAllocation(test)
	usage := transientSpoolBudgetUsage()
	if err := held.Reserve(limit - usage.UsedBytes - firstDerivedBytes); err != nil {
		test.Fatal(err)
	}
	before := transientSpoolBudgetUsage()
	indexes, err := preparePGRootIndexes(context.Background(), prepared)
	if indexes != nil && indexes.spool != nil {
		defer indexes.spool.Close()
	}
	if indexes != nil || !errors.Is(err, domain.ErrResourceOverloaded) || errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrDocumentResourceLimit) {
		test.Fatal("partial output quota exhaustion changed class or returned a capability", indexes != nil, err)
	}
	pgPreparedRootIndexesNativeAssertUsage(test, before.UsedBytes, before.OpenFiles)
	pgPreparedRootIndexesNativeAssertOnlySource(test, prepared.spool)
	if transientSpoolBudgetUsage().PeakBytes <= before.UsedBytes {
		test.Fatal("quota fixture failed before reserving any partial output")
	}
	held.Close()
	retried, err := preparePGRootIndexes(context.Background(), prepared)
	if err != nil || retried == nil || retried.spool == nil {
		test.Fatal("released quota could not be reused for the same verified root", err)
	}
	defer retried.spool.Close()
	pgPreparedRootIndexesNativeAssert(test, prepared, retried, expected)
}

func TestPGPreparedRootIndexesNativePublicationCloseOwnsBothSpools(test *testing.T) {
	for _, mode := range []string{"success", "source-close-error", "index-close-error"} {
		test.Run(mode, func(test *testing.T) {
			before := transientSpoolBudgetUsage()
			publication := pgPreparedRootIndexesNativePublication(test, 129)
			source, indexes := publication.spool, publication.indexes.spool
			sourcePath, indexPath := source.file.Name(), indexes.file.Name()
			job := publication.job
			if mode == "source-close-error" {
				if err := source.file.Close(); err != nil {
					test.Fatal(err)
				}
			}
			if mode == "index-close-error" {
				if err := indexes.file.Close(); err != nil {
					test.Fatal(err)
				}
			}
			first := publication.Close()
			if (first == nil) != (mode == "success") || publication.Close() != first || first != nil && !errors.Is(first, os.ErrClosed) {
				test.Fatal("publication close lost an original spool error or idempotence", mode, first)
			}
			for _, path := range []string{sourcePath, indexPath} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					test.Fatal("publication close leaked an owned spool", path, err)
				}
			}
			if source.writer != nil || indexes.writer != nil || source.offsets != nil {
				test.Fatal("publication close retained live spool state or source metadata")
			}
			if publication.indexes != nil && (len(publication.indexes.items) != 0 || len(publication.indexes.header) != 0 || len(publication.indexes.rangeDescriptor) != 0 || len(publication.indexes.visibleDescriptor) != 0) {
				test.Fatal("publication close retained prepared index payloads")
			}
			if !reflect.DeepEqual(publication.source, pgRootSourceWitness{}) || publication.matches(job) == nil {
				test.Fatal("closed publication retained its source witness or matched the old job")
			}
			if resources, err := publication.RetainedResources(); err == nil || resources.HeapBytes != 0 || resources.SpoolBytes != 0 {
				test.Fatal("closed publication retained a resource capability", resources, err)
			}
			pgPreparedRootIndexesNativeAssertUsage(test, before.UsedBytes, before.OpenFiles)
			assertRangeSpoolsRemoved(test, filepath.Dir(sourcePath))
		})
	}
}
