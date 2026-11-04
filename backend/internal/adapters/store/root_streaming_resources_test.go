package store

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func preparedRootResourcesFixture(t *testing.T, events int) *preparedStreamingRootDoc {
	t.Helper()
	rangeSpoolTempDir(t)
	fixture := rangeFixture(t, events)
	job := rootPublicationJob(t, domain.HashContent([]byte(t.Name())), fixture).Claim(time.Now().UTC(), time.Minute)
	prepared, err := prepareStreamingRootJob(context.Background(), job, &domain.CanonicalDocVerifier{}, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		return fixture.bodies[hash], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Close() })
	return prepared
}

type embeddedRootResourcePublication struct {
	*preparedStreamingRootDoc
}

func (*embeddedRootResourcePublication) Complete(context.Context, domain.DocFinalizationJob, time.Time) error {
	return nil
}

var _ outbound.BoundedRootDocPublication = (*embeddedRootResourcePublication)(nil)

func TestStreamingRootResourcesReportActualSpoolAndMetadata(t *testing.T) {
	for _, events := range []int{0, 1, domain.DocRangePageEvents + 1} {
		prepared := preparedRootResourcesFixture(t, events)
		resources, err := prepared.RetainedResources()
		if err != nil {
			t.Fatal(err)
		}
		info, err := prepared.spool.file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		header, err := prepared.root.ranges.RetainedHeaderBytes()
		if err != nil {
			t.Fatal(err)
		}
		minimum := header + int64(unsafe.Sizeof(*prepared)) + int64(cap(prepared.job.RootManifest)) +
			int64(cap(prepared.root.canonical)) + int64(cap(prepared.root.manifest.Envelope)) +
			int64(cap(prepared.root.manifest.Chunks))*int64(unsafe.Sizeof(domain.ConversationManifestChunk{})) +
			int64(cap(prepared.spool.offsets))*8 + int64(prepared.spool.writer.Size())
		if resources.HeapBytes < minimum || resources.HeapBytes >= 16<<20 || resources.SpoolBytes != info.Size() ||
			resources.SpoolBytes != prepared.spool.offsets[len(prepared.spool.offsets)-1] || (resources.SpoolBytes == 0) != (events == 0) {
			t.Fatalf("resources=%+v metadata minimum=%d actual spool=%d events=%d", resources, minimum, info.Size(), events)
		}
		if _, enabled := any(&fsStreamingRootPublication{prepared: prepared}).(outbound.BoundedRootDocPublication); enabled {
			t.Fatal("FS publication unexpectedly exposed bounded root resources")
		}
		if err := prepared.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStreamingRootResourcesChargeOwnedCapacitiesAndStrings(t *testing.T) {
	for _, part := range []string{"root-manifest", "legacy-envelope", "legacy-chunks", "canonical", "envelope", "chunks", "offsets", "job-id", "repo-id", "doc-hash", "legacy-format", "state", "reason", "manifest-identity", "manifest-format"} {
		t.Run(part, func(t *testing.T) {
			prepared := preparedRootResourcesFixture(t, 1)
			before, err := prepared.RetainedResources()
			if err != nil {
				t.Fatal(err)
			}
			const extra = 4096
			grow := func(prior []byte) []byte {
				grown := make([]byte, len(prior), cap(prior)+extra)
				copy(grown, prior)
				return grown
			}
			minimumIncrease := int64(extra)
			switch part {
			case "root-manifest":
				prepared.job.RootManifest = grow(prepared.job.RootManifest)
			case "legacy-envelope":
				prepared.job.Manifest.Envelope = grow(prepared.job.Manifest.Envelope)
			case "legacy-chunks":
				chunks := make([]domain.ContentHash, 2)
				chunks[0], chunks[1] = domain.ContentHash(strings.Repeat("a", extra)), domain.ContentHash(strings.Repeat("b", extra))
				prepared.job.Manifest.Chunks = chunks[:1]
				minimumIncrease = 2 * extra
			case "canonical":
				prepared.root.canonical = grow(prepared.root.canonical)
			case "envelope":
				prepared.root.manifest.Envelope = grow(prepared.root.manifest.Envelope)
			case "chunks":
				prior := prepared.root.manifest.Chunks
				grown := make([]domain.ConversationManifestChunk, cap(prior)+1)
				copy(grown, prior)
				grown[len(grown)-1].Hash = domain.ContentHash(strings.Repeat("c", extra))
				prepared.root.manifest.Chunks = grown[:len(prior)]
			case "offsets":
				prior := prepared.spool.offsets
				grown := make([]int64, len(prior), cap(prior)+extra)
				copy(grown, prior)
				prepared.spool.offsets = grown
				minimumIncrease = 8 * extra
			case "job-id":
				prepared.job.ID += strings.Repeat("i", extra)
			case "repo-id":
				prepared.job.RepoID += domain.ContentHash(strings.Repeat("r", extra))
			case "doc-hash":
				prepared.job.DocHash += domain.ContentHash(strings.Repeat("h", extra))
			case "legacy-format":
				prepared.job.Manifest.Format += strings.Repeat("f", extra)
			case "state":
				prepared.job.State += strings.Repeat("s", extra)
			case "reason":
				prepared.job.Reason += strings.Repeat("e", extra)
			case "manifest-identity":
				prepared.root.manifest.Identity += strings.Repeat("i", extra)
			case "manifest-format":
				prepared.root.manifest.ChunkFormat += strings.Repeat("f", extra)
			}
			after, err := prepared.RetainedResources()
			if err != nil || after.HeapBytes-before.HeapBytes < minimumIncrease || after.SpoolBytes != before.SpoolBytes {
				t.Fatalf("uncharged metadata: before=%+v after=%+v minimum increase=%d error=%v", before, after, minimumIncrease, err)
			}
		})
	}
}

func TestStreamingRootResourcesDoNotEnforceAdmissionLimits(t *testing.T) {
	prepared := preparedRootResourcesFixture(t, 1)
	manifest := make([]byte, len(prepared.job.RootManifest), 9<<20)
	copy(manifest, prepared.job.RootManifest)
	prepared.job.RootManifest = manifest
	resources, err := prepared.RetainedResources()
	if err != nil || resources.HeapBytes <= 16<<20 {
		t.Fatal("large valid metadata capacity was rejected or undercounted", resources, err)
	}
	const spoolBytes = 65 << 20
	if err := prepared.spool.file.Truncate(spoolBytes); err != nil {
		t.Fatal(err)
	}
	pages := spoolBytes / domain.MaxDocRangePageBytes
	prepared.spool.offsets = make([]int64, pages+1)
	for position := range prepared.spool.offsets {
		prepared.spool.offsets[position] = int64(position) * domain.MaxDocRangePageBytes
	}
	prepared.root.manifest.EventCount = int64(pages * domain.DocRangePageEvents)
	resources, err = prepared.RetainedResources()
	if err != nil || resources.SpoolBytes != spoolBytes {
		t.Fatal("large spool was rejected or undercounted", resources, err)
	}
}

func TestStreamingRootResourcesRejectInvalidSpool(t *testing.T) {
	for _, mode := range []string{"closed-file", "non-regular-file", "nil-writer", "missing-offsets", "nonzero-start", "negative-end", "duplicate-offset", "oversized-page", "wrong-page-count", "truncated", "extended", "buffered", "invalid-header", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			prepared := preparedRootResourcesFixture(t, 1)
			var mutationErr error
			switch mode {
			case "closed-file":
				mutationErr = prepared.spool.file.Close()
			case "non-regular-file":
				original := prepared.spool.file
				var directory *os.File
				directory, mutationErr = os.Open(t.TempDir())
				if mutationErr == nil {
					prepared.spool.file = directory
					t.Cleanup(func() {
						_ = directory.Close()
						prepared.spool.file = original
					})
				}
			case "nil-writer":
				prepared.spool.writer = nil
			case "missing-offsets":
				prepared.spool.offsets = nil
			case "nonzero-start":
				prepared.spool.offsets[0] = 1
			case "negative-end":
				prepared.spool.offsets[1] = -1
			case "duplicate-offset":
				prepared.spool.offsets[1] = 0
			case "oversized-page":
				prepared.spool.offsets[1] = math.MaxInt64
			case "wrong-page-count":
				prepared.root.manifest.EventCount = 0
			case "truncated":
				mutationErr = prepared.spool.file.Truncate(0)
			case "extended":
				mutationErr = prepared.spool.file.Truncate(prepared.spool.offsets[1] + 1)
			case "buffered":
				_, mutationErr = prepared.spool.writer.WriteString("pending")
			case "invalid-header":
				prepared.root.ranges = domain.VerifiedDocRangeIndex{}
			case "legacy":
				prepared.job.DocIdentity = domain.DocumentIdentityLegacy
			}
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			if resources, err := prepared.RetainedResources(); err == nil || resources != (outbound.RootPublicationResources{}) {
				t.Fatal("invalid spool yielded a resource capability", resources, err)
			}
		})
	}
	for _, prepared := range []*preparedStreamingRootDoc{nil, {}, {spool: &docRangeSpool{}}} {
		if resources, err := prepared.RetainedResources(); err == nil || resources != (outbound.RootPublicationResources{}) {
			t.Fatal("nil preparation yielded resources", resources, err)
		}
	}
}

func TestStreamingRootResourcesCloseClearsMetadataAndPreservesErrors(t *testing.T) {
	for _, mode := range []string{"success", "close-error", "remove-error"} {
		t.Run(mode, func(t *testing.T) {
			prepared := preparedRootResourcesFixture(t, 1)
			job := prepared.job
			path := prepared.spool.file.Name()
			reservedBytes := prepared.spool.quota.bytes
			if mode == "close-error" {
				if err := prepared.spool.file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "remove-error" {
				if err := os.Rename(path, path+".moved"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					closeErr := prepared.spool.file.Close()
					removeErr := os.RemoveAll(path)
					retainedErr := os.Remove(path + ".moved")
					if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) || removeErr != nil || retainedErr != nil && !os.IsNotExist(retainedErr) {
						t.Error("failed to reclaim retained spool fixture", closeErr, removeErr, retainedErr)
						return
					}
					prepared.spool.quota.Close()
				})
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "blocker"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			first := prepared.Close()
			if (first == nil) != (mode == "success") || prepared.Close() != first {
				t.Fatal("close lost idempotence or error", first)
			}
			if !reflect.DeepEqual(prepared.job, domain.DocFinalizationJob{}) || !reflect.DeepEqual(prepared.root, preparedRootDoc{}) || prepared.spool.offsets != nil || prepared.spool.writer != nil {
				t.Fatal("close retained owned metadata")
			}
			if mode != "remove-error" {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("close did not remove spool", err)
				}
				transientSpoolBudgetAssertUsage(t, 0, 0, 0)
			} else {
				retained, err := os.Stat(path + ".moved")
				if err != nil || retained.Size() != reservedBytes || reservedBytes <= 0 {
					t.Fatal("removal failure did not retain the reserved spool bytes", retained, err)
				}
				transientSpoolBudgetAssertUsage(t, 0, reservedBytes, 1)
				if restore, err := configureTransientSpoolBudget(1); !errors.Is(err, domain.ErrConflict) || restore != nil {
					if restore != nil {
						t.Cleanup(func() { restore() })
					}
					t.Fatal("configuration ignored the retained spool owner", err)
				}
			}
			if resources, err := prepared.RetainedResources(); err == nil || resources != (outbound.RootPublicationResources{}) {
				t.Fatal("closed preparation yielded resources", resources, err)
			}
			if err := prepared.matches(job); err == nil {
				t.Fatal("closed preparation matched a job")
			}
		})
		transientSpoolBudgetAssertUsage(t, 0, 0, 0)
	}
	transientSpoolBudgetConfigure(t, 1)
}

func TestStreamingRootResourceEstimateOverflow(t *testing.T) {
	for _, input := range []struct{ initial, count, width int64 }{
		{0, -1, 1}, {0, 1, 0}, {0, 1, -1}, {-1, 1, 1},
		{0, math.MaxInt64, 1}, {0, 2, math.MaxInt64}, {math.MaxInt64 - 64, 1, 1},
	} {
		estimate := rootPublicationHeapEstimate{bytes: input.initial}
		estimate.allocation(input.count, input.width)
		if !errors.Is(estimate.err, domain.ErrIntegrity) || estimate.bytes != input.initial {
			t.Fatal("overflow mutated or accepted estimate", input, estimate)
		}
		estimate.allocation(1, 1)
		if !errors.Is(estimate.err, domain.ErrIntegrity) || estimate.bytes != input.initial {
			t.Fatal("invalid estimate recovered", estimate)
		}
	}
	estimate := rootPublicationHeapEstimate{}
	estimate.allocation((math.MaxInt64-64)/2, 1)
	if estimate.err != nil || estimate.bytes != math.MaxInt64-1 {
		t.Fatal("largest supported allocation", estimate)
	}
	estimate.allocation(1, 1)
	if !errors.Is(estimate.err, domain.ErrIntegrity) {
		t.Fatal("sum overflow accepted", estimate)
	}
}
