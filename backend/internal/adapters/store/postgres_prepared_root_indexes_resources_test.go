//go:build postgres

package store

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func pgPreparedRootIndexesResourcePublication(test *testing.T, prepared *preparedStreamingRootDoc) *pgStreamingRootPublication {
	test.Helper()
	publication := &pgStreamingRootPublication{preparedStreamingRootDoc: prepared}
	test.Cleanup(func() { _ = publication.Close() })
	var err error
	publication.source, err = newPGRootSourceWitness(prepared, pgRootWitnessFixtureSources(prepared))
	if err != nil {
		test.Fatal(err)
	}
	publication.indexes, err = preparePGRootIndexes(context.Background(), prepared)
	if err != nil {
		test.Fatal(err)
	}
	if publication.indexes == nil || publication.indexes.spool == nil {
		test.Fatal("preparation omitted its owned derived index spool")
	}
	if err := publication.matches(prepared.job); err != nil {
		test.Fatal("prepared publication did not retain valid source and index bindings", err)
	}
	return publication
}

func pgPreparedRootIndexesResourceFixture(test *testing.T, events int) *pgStreamingRootPublication {
	test.Helper()
	return pgPreparedRootIndexesResourcePublication(test, preparedRootResourcesFixture(test, events))
}

func pgPreparedRootIndexesResourceSpoolInfo(test *testing.T, spool *docRangeSpool) os.FileInfo {
	test.Helper()
	if spool == nil || spool.file == nil || spool.writer == nil || len(spool.offsets) == 0 {
		test.Fatal("prepared index spool is missing live owned resources")
	}
	info, err := spool.file.Stat()
	if err != nil {
		test.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || spool.writer.Buffered() != 0 ||
		spool.offsets[0] != 0 || info.Size() != spool.offsets[len(spool.offsets)-1] {
		test.Fatal("prepared index spool is not private, flushed and completely indexed", info, spool.offsets)
	}
	return info
}

func pgPreparedRootIndexesResourceFailure(test *testing.T, ctx context.Context, prepared *preparedStreamingRootDoc, expected error) {
	test.Helper()
	indexes, err := preparePGRootIndexes(ctx, prepared)
	if indexes != nil {
		publication := &pgStreamingRootPublication{preparedStreamingRootDoc: prepared, indexes: indexes}
		test.Cleanup(func() { _ = publication.Close() })
	}
	if indexes != nil || !errors.Is(err, expected) {
		test.Fatalf("failed preparation returned owned indexes or changed its error: indexes=%t error=%v want=%v", indexes != nil, err, expected)
	}
}

func pgPreparedRootIndexesResourceSourcePreserved(test *testing.T, prepared *preparedStreamingRootDoc, before outbound.TransientSpoolBudgetUsage, original []byte) {
	test.Helper()
	transientSpoolBudgetAssertUsage(test, before.LimitBytes, before.UsedBytes, before.OpenFiles)
	current, err := os.ReadFile(prepared.spool.file.Name())
	if err != nil || !bytes.Equal(current, original) {
		test.Fatal("index preparation changed or removed the caller-owned verified page spool", err)
	}
	if err := prepared.matches(prepared.job); err != nil {
		test.Fatal("index preparation closed its caller-owned root", err)
	}
	if resources, err := prepared.RetainedResources(); err != nil || resources.SpoolBytes != int64(len(original)) {
		test.Fatal("index preparation invalidated caller-owned resource reporting", resources, err)
	}
	entries, err := os.ReadDir(filepath.Dir(prepared.spool.file.Name()))
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(prepared.spool.file.Name()) {
		test.Fatal("failed index preparation leaked a derived spool", entries, err)
	}
}

func TestPGPreparedRootIndexesResourcesReportActualSourceAndDerivedSpools(test *testing.T) {
	for _, events := range []int{0, 1, domain.DocRangePageEvents + 1, 3*domain.DocRangePageEvents + 1} {
		test.Run(fmt.Sprintf("events-%d", events), func(test *testing.T) {
			prepared := preparedRootResourcesFixture(test, events)
			original := prepared.spool
			originalInfo := pgPreparedRootIndexesResourceSpoolInfo(test, original)
			originalBytes, err := os.ReadFile(original.file.Name())
			if err != nil {
				test.Fatal(err)
			}
			base, err := prepared.RetainedResources()
			if err != nil {
				test.Fatal(err)
			}
			publication := pgPreparedRootIndexesResourcePublication(test, prepared)
			indexes := publication.indexes
			derived := indexes.spool
			derivedInfo := pgPreparedRootIndexesResourceSpoolInfo(test, derived)
			if prepared.spool != original || derived == original || os.SameFile(originalInfo, derivedInfo) {
				test.Fatal("index preparation replaced or aliased the original verified page spool")
			}
			currentBytes, err := os.ReadFile(original.file.Name())
			if err != nil || !bytes.Equal(currentBytes, originalBytes) {
				test.Fatal("index preparation rewrote the original verified range pages", err)
			}
			if indexes.document != prepared.job.DocHash || len(indexes.header) == 0 || indexes.headerHash != domain.HashContent(indexes.header) || len(indexes.rangeDescriptor) == 0 ||
				len(indexes.visibleDescriptor) == 0 || indexes.rangeHash != domain.HashContent(indexes.rangeDescriptor) ||
				indexes.visibleHash != domain.HashContent(indexes.visibleDescriptor) {
				test.Fatal("prepared indexes omitted serialized headers, descriptors or their bindings")
			}
			minimum := base.HeapBytes + int64(unsafe.Sizeof(*publication)) + int64(unsafe.Sizeof(*indexes)) +
				int64(unsafe.Sizeof(*derived)) + int64(unsafe.Sizeof(*derived.file)) + int64(unsafe.Sizeof(*derived.writer)) +
				int64(cap(indexes.header)+cap(indexes.rangeDescriptor)+cap(indexes.visibleDescriptor)) +
				int64(cap(indexes.items))*int64(unsafe.Sizeof(pgRootIndexItem{})) +
				int64(cap(derived.offsets))*int64(unsafe.Sizeof(int64(0))) + int64(derived.writer.Size()) +
				int64(len(indexes.document)+len(indexes.headerHash)+len(indexes.rangeHash)+len(indexes.visibleHash)+len(derived.file.Name()))
			for _, item := range indexes.items[:cap(indexes.items)] {
				minimum += int64(len(item.hash))
			}
			resources, err := publication.RetainedResources()
			if err != nil || resources.HeapBytes < minimum || resources.HeapBytes >= 16<<20 ||
				resources.SpoolBytes != originalInfo.Size()+derivedInfo.Size() || resources.SpoolBytes >= 64<<20 {
				test.Fatal("publication omitted owned prepared index resources", resources, minimum, originalInfo.Size(), derivedInfo.Size(), err)
			}
			usage := transientSpoolBudgetUsage()
			if usage.UsedBytes != resources.SpoolBytes || usage.OpenFiles != 2 {
				test.Fatal("reported source and derived spool bytes differ from process reservations", resources, usage)
			}
		})
	}
}

func TestPGPreparedRootIndexesReuseOriginalRangePagesWithoutDerivedCopies(test *testing.T) {
	publication := pgPreparedRootIndexesResourceFixture(test, 3*domain.DocRangePageEvents+1)
	indexes := publication.indexes
	header := publication.root.ranges.Header()
	rangePages := make([]bool, len(header.Pages))
	derivedObjects := make([]bool, len(indexes.spool.offsets)-1)
	pageHashes := make(map[domain.ContentHash]bool, len(header.Pages))
	for _, hash := range header.Pages {
		pageHashes[hash] = true
	}
	rangeOrdinal, derivedOrdinal := 0, 0
	for _, item := range indexes.items {
		spool := indexes.spool
		if item.rangePage {
			spool = publication.spool
			if item.position != rangeOrdinal || item.position >= len(rangePages) || rangePages[item.position] || header.Pages[item.position] != item.hash {
				test.Fatal("range page reference lost its original position or was duplicated", item)
			}
			rangePages[item.position] = true
			rangeOrdinal++
		} else {
			if item.position != derivedOrdinal || item.position >= len(derivedObjects) || derivedObjects[item.position] || pageHashes[item.hash] {
				test.Fatal("derived spool contains a duplicate original range page or object position", item)
			}
			derivedObjects[item.position] = true
			derivedOrdinal++
		}
		raw, err := spool.Read(context.Background(), item.position)
		if err != nil || domain.HashContent(raw) != item.hash {
			test.Fatal("prepared item does not reference its owned source bytes", item, err)
		}
	}
	for position, retained := range rangePages {
		if !retained {
			test.Fatal("prepared items omitted an original verified range page", position)
		}
	}
	for position, retained := range derivedObjects {
		if !retained {
			test.Fatal("derived spool retained unreferenced object bytes", position)
		}
	}
}

func TestPGPreparedRootIndexesResourcesChargeMetadataCapacitiesAndStrings(test *testing.T) {
	for _, part := range []string{"header", "range-descriptor", "visible-descriptor", "items", "spare-item-hash", "offsets", "writer-buffer", "file-name"} {
		test.Run(part, func(test *testing.T) {
			publication := pgPreparedRootIndexesResourceFixture(test, domain.DocRangePageEvents+1)
			indexes := publication.indexes
			if part == "spare-item-hash" {
				prior := indexes.items
				grown := make([]pgRootIndexItem, len(prior), cap(prior)+1)
				copy(grown, prior)
				indexes.items = grown
			}
			before, err := publication.RetainedResources()
			if err != nil {
				test.Fatal(err)
			}
			const extra = 4096
			grow := func(prior []byte) []byte {
				grown := make([]byte, len(prior), cap(prior)+extra)
				copy(grown, prior)
				return grown
			}
			minimumIncrease := int64(extra)
			switch part {
			case "header":
				indexes.header = grow(indexes.header)
			case "range-descriptor":
				indexes.rangeDescriptor = grow(indexes.rangeDescriptor)
			case "visible-descriptor":
				indexes.visibleDescriptor = grow(indexes.visibleDescriptor)
			case "items":
				prior := indexes.items
				grown := make([]pgRootIndexItem, len(prior), cap(prior)+extra)
				copy(grown, prior)
				indexes.items = grown
				minimumIncrease *= int64(unsafe.Sizeof(pgRootIndexItem{}))
			case "spare-item-hash":
				retained := indexes.items[:cap(indexes.items)]
				retained[len(retained)-1].hash = domain.ContentHash(strings.Repeat("h", extra))
			case "offsets":
				prior := indexes.spool.offsets
				grown := make([]int64, len(prior), cap(prior)+extra)
				copy(grown, prior)
				indexes.spool.offsets = grown
				minimumIncrease *= int64(unsafe.Sizeof(int64(0)))
			case "writer-buffer":
				indexes.spool.writer = bufio.NewWriterSize(indexes.spool.file, indexes.spool.writer.Size()+extra)
			case "file-name":
				previous := indexes.spool.file.Name()
				longer := filepath.Join(filepath.Dir(previous), strings.Repeat("p", 64)+filepath.Base(previous))
				if err := indexes.spool.file.Close(); err != nil {
					test.Fatal(err)
				}
				if err := os.Rename(previous, longer); err != nil {
					test.Fatal(err)
				}
				file, err := os.OpenFile(longer, os.O_RDWR, 0600)
				if err != nil {
					test.Fatal(err)
				}
				indexes.spool.file = file
				indexes.spool.writer = bufio.NewWriterSize(file, indexes.spool.writer.Size())
				minimumIncrease = int64(len(longer) - len(previous))
			}
			if err := publication.matches(publication.job); err != nil {
				test.Fatal("resource accounting mutation changed a live publication binding", part, err)
			}
			after, err := publication.RetainedResources()
			if err != nil || after.HeapBytes-before.HeapBytes < minimumIncrease || after.SpoolBytes != before.SpoolBytes {
				test.Fatal("prepared index metadata was not fully charged", before, after, minimumIncrease, err)
			}
		})
	}
}

func TestPGPreparedRootIndexesResourcesRejectChangedLiveBindings(test *testing.T) {
	for _, binding := range []string{"document", "header", "header-hash", "range-descriptor", "range-hash", "visible-descriptor", "visible-hash", "derived-item-hash", "range-page-hash"} {
		test.Run(binding, func(test *testing.T) {
			publication := pgPreparedRootIndexesResourceFixture(test, domain.DocRangePageEvents+1)
			indexes := publication.indexes
			expected := domain.ErrIntegrity
			switch binding {
			case "document":
				indexes.document = domain.HashContent([]byte("another prepared root document"))
				expected = domain.ErrConflict
			case "header":
				indexes.header[0] ^= 1
			case "header-hash":
				indexes.headerHash += "h"
			case "range-descriptor":
				indexes.rangeDescriptor[0] ^= 1
			case "range-hash":
				indexes.rangeHash += "h"
			case "visible-descriptor":
				indexes.visibleDescriptor[0] ^= 1
			case "visible-hash":
				indexes.visibleHash += "h"
			case "derived-item-hash", "range-page-hash":
				mutated := false
				for position, item := range indexes.items {
					if item.rangePage == (binding == "range-page-hash") {
						indexes.items[position].hash += domain.ContentHash(strings.Repeat("h", 4096))
						mutated = true
						break
					}
				}
				if !mutated {
					test.Fatal("binding fixture omitted the requested source or derived item", binding)
				}
			}
			if err := indexes.matches(publication.job.DocHash, len(publication.spool.offsets)-1); !errors.Is(err, expected) {
				test.Fatal("prepared indexes accepted a changed live binding", binding, err, expected)
			}
			if err := publication.matches(publication.job); !errors.Is(err, expected) {
				test.Fatal("publication accepted a changed live index binding", binding, err, expected)
			}
			if resources, err := publication.RetainedResources(); !errors.Is(err, expected) || resources != (outbound.RootPublicationResources{}) {
				test.Fatal("changed live binding yielded retained resource reporting", binding, resources, err, expected)
			}
		})
	}
}

func TestPGPreparedRootIndexesResourcesRejectInvalidSpoolStatsAndMetadata(test *testing.T) {
	for _, owner := range []string{"source", "derived"} {
		for _, damage := range []string{"nil-file", "closed-file", "directory", "nil-writer", "missing-offsets", "nonzero-start", "negative-end", "duplicate-offset", "oversized-object", "truncated", "extended", "buffered"} {
			test.Run(owner+"/"+damage, func(test *testing.T) {
				publication := pgPreparedRootIndexesResourceFixture(test, domain.DocRangePageEvents+1)
				spool := publication.spool
				if owner == "derived" {
					spool = publication.indexes.spool
				}
				originalFile, originalWriter, originalOffsets := spool.file, spool.writer, spool.offsets
				test.Cleanup(func() {
					spool.file, spool.writer, spool.offsets = originalFile, originalWriter, originalOffsets
				})
				var mutationErr error
				switch damage {
				case "nil-file":
					spool.file = nil
				case "closed-file":
					mutationErr = spool.file.Close()
				case "directory":
					var directory *os.File
					directory, mutationErr = os.Open(test.TempDir())
					if mutationErr == nil {
						spool.file = directory
						test.Cleanup(func() { _ = directory.Close() })
					}
				case "nil-writer":
					spool.writer = nil
				case "missing-offsets":
					spool.offsets = nil
				case "nonzero-start":
					spool.offsets[0] = 1
				case "negative-end":
					spool.offsets[len(spool.offsets)-1] = -1
				case "duplicate-offset":
					spool.offsets[1] = 0
				case "oversized-object":
					maximum := domain.MaxDocRangePageBytes
					if owner == "derived" {
						maximum = domain.MaxDocVisibleObjectBytes
					}
					spool.offsets[1] = int64(maximum) + 1
				case "truncated":
					mutationErr = spool.file.Truncate(0)
				case "extended":
					mutationErr = spool.file.Truncate(spool.offsets[len(spool.offsets)-1] + 1)
				case "buffered":
					_, mutationErr = spool.writer.WriteString("pending")
				}
				if mutationErr != nil {
					test.Fatal(mutationErr)
				}
				if resources, err := publication.RetainedResources(); err == nil || resources != (outbound.RootPublicationResources{}) {
					test.Fatal("invalid source or derived spool yielded publication resources", resources, err)
				}
			})
		}
	}
	for _, damage := range []string{"missing-indexes", "missing-derived-spool"} {
		test.Run(damage, func(test *testing.T) {
			publication := pgPreparedRootIndexesResourceFixture(test, 1)
			indexes, spool := publication.indexes, publication.indexes.spool
			test.Cleanup(func() { publication.indexes, indexes.spool = indexes, spool })
			if damage == "missing-indexes" {
				publication.indexes = nil
			} else {
				publication.indexes.spool = nil
			}
			if resources, err := publication.RetainedResources(); err == nil || resources != (outbound.RootPublicationResources{}) {
				test.Fatal("incomplete index ownership yielded publication resources", resources, err)
			}
		})
	}
}

func TestPGPreparedRootIndexesQuotaFailureReleasesOnlyNewReservationsAndAllowsRetry(test *testing.T) {
	for _, partial := range []bool{false, true} {
		test.Run(fmt.Sprintf("partial-%t", partial), func(test *testing.T) {
			const limit = 64 << 20
			transientSpoolBudgetConfigure(test, limit)
			prepared := preparedRootResourcesFixture(test, domain.DocRangePageEvents+1)
			original, err := os.ReadFile(prepared.spool.file.Name())
			if err != nil {
				test.Fatal(err)
			}
			allowance := int64(0)
			if partial {
				_, objects, err := domain.BuildVerifiedDocRangeGeneration(context.Background(), prepared.root.ranges)
				if err != nil || len(objects) < 2 || len(objects[0].Bytes) == 0 {
					test.Fatal("quota fixture requires at least two derived objects", len(objects), err)
				}
				allowance = int64(len(objects[0].Bytes))
			}
			blocker := transientSpoolBudgetAllocation(test)
			if err := blocker.Reserve(limit - transientSpoolBudgetUsage().UsedBytes - allowance); err != nil {
				test.Fatal(err)
			}
			before := transientSpoolBudgetUsage()
			pgPreparedRootIndexesResourceFailure(test, context.Background(), prepared, domain.ErrResourceOverloaded)
			pgPreparedRootIndexesResourceSourcePreserved(test, prepared, before, original)
			if usage := transientSpoolBudgetUsage(); partial && usage.PeakBytes != limit {
				test.Fatal("quota failure did not exercise cleanup after a successful derived reservation", usage)
			}
			blocker.Close()
			publication := pgPreparedRootIndexesResourcePublication(test, prepared)
			resources, err := publication.RetainedResources()
			if err != nil || resources.SpoolBytes <= int64(len(original)) {
				test.Fatal("released quota or caller-owned pages could not be reused", resources, err)
			}
			transientSpoolBudgetAssertUsage(test, limit, resources.SpoolBytes, 2)
			if err := publication.Close(); err != nil {
				test.Fatal(err)
			}
			transientSpoolBudgetAssertUsage(test, limit, 0, 0)
			assertRangeSpoolsRemoved(test, filepath.Dir(prepared.spool.file.Name()))
		})
	}
}

type pgPreparedRootIndexesResourceCancellation struct {
	context.Context
	initial  int64
	cancel   context.CancelFunc
	observed bool
}

func (observed *pgPreparedRootIndexesResourceCancellation) Err() error {
	if err := observed.Context.Err(); err != nil {
		return err
	}
	if transientSpoolBudgetUsage().UsedBytes > observed.initial {
		observed.observed = true
		observed.cancel()
	}
	return observed.Context.Err()
}

func TestPGPreparedRootIndexesCancellationPreservesCallerSpoolAndReleasesDerivedResources(test *testing.T) {
	for _, partial := range []bool{false, true} {
		test.Run(fmt.Sprintf("partial-%t", partial), func(test *testing.T) {
			transientSpoolBudgetConfigure(test, 64<<20)
			prepared := preparedRootResourcesFixture(test, domain.DocRangePageEvents+1)
			original, err := os.ReadFile(prepared.spool.file.Name())
			if err != nil {
				test.Fatal(err)
			}
			before := transientSpoolBudgetUsage()
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed := &pgPreparedRootIndexesResourceCancellation{Context: base, initial: before.UsedBytes, cancel: cancel}
			if !partial {
				cancel()
			}
			pgPreparedRootIndexesResourceFailure(test, observed, prepared, context.Canceled)
			if observed.observed != partial {
				test.Fatal("cancellation did not occur at the requested derived reservation boundary", observed.observed)
			}
			pgPreparedRootIndexesResourceSourcePreserved(test, prepared, before, original)
			if err := prepared.Close(); err != nil {
				test.Fatal(err)
			}
			transientSpoolBudgetAssertUsage(test, before.LimitBytes, 0, 0)
			assertRangeSpoolsRemoved(test, filepath.Dir(prepared.spool.file.Name()))
		})
	}
}

func TestPGPreparedRootIndexesLatePageFailureRemovesPartialDerivedSpool(test *testing.T) {
	transientSpoolBudgetConfigure(test, 64<<20)
	prepared := preparedRootResourcesFixture(test, domain.DocRangePageEvents+1)
	if _, err := prepared.spool.file.WriteAt([]byte{0}, prepared.spool.offsets[1]); err != nil {
		test.Fatal(err)
	}
	original, err := os.ReadFile(prepared.spool.file.Name())
	if err != nil {
		test.Fatal(err)
	}
	before := transientSpoolBudgetUsage()
	pgPreparedRootIndexesResourceFailure(test, context.Background(), prepared, domain.ErrIntegrity)
	pgPreparedRootIndexesResourceSourcePreserved(test, prepared, before, original)
	if usage := transientSpoolBudgetUsage(); usage.PeakBytes <= before.UsedBytes {
		test.Fatal("late page failure did not exercise partial derived spool cleanup", usage)
	}
	if err := prepared.Close(); err != nil {
		test.Fatal(err)
	}
	transientSpoolBudgetAssertUsage(test, before.LimitBytes, 0, 0)
}

func TestPGPreparedRootIndexesCreateFailureReturnsNewFileReservation(test *testing.T) {
	transientSpoolBudgetConfigure(test, 64<<20)
	prepared := preparedRootResourcesFixture(test, 1)
	original, err := os.ReadFile(prepared.spool.file.Name())
	if err != nil {
		test.Fatal(err)
	}
	before := transientSpoolBudgetUsage()
	missing := filepath.Join(test.TempDir(), "missing")
	test.Setenv("TMPDIR", missing)
	test.Setenv("TMP", missing)
	test.Setenv("TEMP", missing)
	pgPreparedRootIndexesResourceFailure(test, context.Background(), prepared, os.ErrNotExist)
	pgPreparedRootIndexesResourceSourcePreserved(test, prepared, before, original)
	if err := prepared.Close(); err != nil {
		test.Fatal(err)
	}
	transientSpoolBudgetAssertUsage(test, before.LimitBytes, 0, 0)
}

func TestPGPreparedRootIndexesPublicationCloseReleasesBothSpoolsAndPreservesErrors(test *testing.T) {
	for _, mode := range []string{"success", "source-close-error", "derived-close-error", "both-close-errors"} {
		test.Run(mode, func(test *testing.T) {
			transientSpoolBudgetConfigure(test, 64<<20)
			publication := pgPreparedRootIndexesResourceFixture(test, domain.DocRangePageEvents+1)
			indexes := publication.indexes
			source, derived := publication.spool, indexes.spool
			sourcePath, derivedPath := source.file.Name(), derived.file.Name()
			job := publication.job
			for position, spool := range []*docRangeSpool{source, derived} {
				if mode == "both-close-errors" || position == 0 && mode == "source-close-error" || position == 1 && mode == "derived-close-error" {
					if err := spool.file.Close(); err != nil {
						test.Fatal(err)
					}
				}
			}
			first := publication.Close()
			if (first == nil) != (mode == "success") {
				test.Fatal("publication close lost a source or derived close error", first)
			}
			if first != nil {
				if !errors.Is(first, os.ErrClosed) ||
					(mode == "source-close-error" || mode == "both-close-errors") && !strings.Contains(first.Error(), sourcePath) ||
					(mode == "derived-close-error" || mode == "both-close-errors") && !strings.Contains(first.Error(), derivedPath) {
					test.Fatal("publication close did not preserve each spool's original close error", first)
				}
			}
			second := publication.Close()
			if (second == nil) != (first == nil) || first != nil && second.Error() != first.Error() {
				test.Fatal("repeated publication close changed its original result", first, second)
			}
			if indexes.header != nil || indexes.rangeDescriptor != nil || indexes.visibleDescriptor != nil || indexes.items != nil ||
				!reflect.DeepEqual(publication.source, pgRootSourceWitness{}) || !reflect.DeepEqual(publication.job, domain.DocFinalizationJob{}) ||
				!reflect.DeepEqual(publication.root, preparedRootDoc{}) {
				test.Fatal("closed publication retained root, witness or prepared index metadata")
			}
			for _, spool := range []*docRangeSpool{source, derived} {
				if spool.writer != nil {
					test.Fatal("closed publication retained a spool buffer")
				}
				if _, err := spool.file.Stat(); !errors.Is(err, os.ErrClosed) {
					test.Fatal("publication close left a spool handle open", err)
				}
				if _, err := os.Stat(spool.file.Name()); !errors.Is(err, os.ErrNotExist) {
					test.Fatal("publication close left a spool file behind", err)
				}
				if _, err := spool.Read(context.Background(), 0); !errors.Is(err, os.ErrClosed) {
					test.Fatal("closed publication left a spool readable", err)
				}
			}
			if source.offsets != nil {
				test.Fatal("closed root retained its verified page offsets")
			}
			if err := publication.matches(job); err == nil {
				test.Fatal("closed publication still matched its original job")
			}
			if resources, err := publication.RetainedResources(); err == nil || resources != (outbound.RootPublicationResources{}) {
				test.Fatal("closed publication reported retained resources", resources, err)
			}
			transientSpoolBudgetAssertUsage(test, 64<<20, 0, 0)
			assertRangeSpoolsRemoved(test, filepath.Dir(sourcePath))
		})
	}
}
