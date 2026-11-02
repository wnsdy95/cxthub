package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func transientSpoolBudgetAssertUsage(test testing.TB, limit, used int64, files int) outbound.TransientSpoolBudgetUsage {
	test.Helper()
	usage := transientSpoolBudgetUsage()
	if usage.LimitBytes != limit || usage.UsedBytes != used || usage.OpenFiles != files || usage.PeakBytes < used {
		test.Fatalf("unexpected transient spool usage: got %+v, want limit=%d used=%d files=%d", usage, limit, used, files)
	}
	return usage
}

func transientSpoolBudgetConfigure(test *testing.T, limit int64) func() error {
	test.Helper()
	restore, err := configureTransientSpoolBudget(limit)
	if err != nil || restore == nil {
		test.Fatalf("configure transient spool budget: restore=%v error=%v", restore != nil, err)
	}
	test.Cleanup(func() {
		if err := restore(); err != nil {
			test.Errorf("restore transient spool budget: %v", err)
		}
	})
	transientSpoolBudgetAssertUsage(test, limit, 0, 0)
	return restore
}

func transientSpoolBudgetAllocation(test *testing.T) *transientSpoolAllocation {
	test.Helper()
	allocation, err := newTransientSpoolAllocation()
	if err != nil || allocation == nil {
		test.Fatalf("create transient spool allocation: %v", err)
	}
	test.Cleanup(func() { allocation.Close() })
	return allocation
}

func transientSpoolBudgetLegacy(test *testing.T) *legacyDocSpool {
	test.Helper()
	spool, err := newLegacyDocSpool()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { spool.Close() })
	return spool
}

func transientSpoolBudgetRange(test *testing.T) *docRangeSpool {
	test.Helper()
	spool, err := newDocRangeSpool()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { spool.Close() })
	return spool
}

func transientSpoolBudgetNormalization(test *testing.T) *legacyNormalizationSpool {
	test.Helper()
	storage, err := newLegacyNormalizationSpool(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { storage.Close() })
	return storage
}

func transientSpoolBudgetAssertFile(test testing.TB, file *os.File, expected string) {
	test.Helper()
	body, err := os.ReadFile(file.Name())
	if err != nil || string(body) != expected {
		test.Fatalf("unexpected spool contents: got %q, want %q, error=%v", body, expected, err)
	}
}

func transientSpoolBudgetAssertOverloaded(test testing.TB, err error) {
	test.Helper()
	if !errors.Is(err, domain.ErrResourceOverloaded) || errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrDocumentResourceLimit) {
		test.Fatalf("quota exhaustion changed error class: %v", err)
	}
}

func TestTransientSpoolBudgetReservationsAndInvalidReleases(test *testing.T) {
	transientSpoolBudgetConfigure(test, 12)
	first := transientSpoolBudgetAllocation(test)
	second := transientSpoolBudgetAllocation(test)
	if err := first.Reserve(8); err != nil {
		test.Fatal(err)
	}
	if err := second.Reserve(4); err != nil {
		test.Fatal(err)
	}
	peak := transientSpoolBudgetAssertUsage(test, 12, 12, 2).PeakBytes
	transientSpoolBudgetAssertOverloaded(test, first.Reserve(1))
	for _, invalid := range []int64{-1, math.MinInt64} {
		if err := first.Reserve(invalid); err == nil {
			test.Fatalf("accepted negative reservation %d", invalid)
		}
	}
	for _, invalid := range []int64{-1, math.MinInt64, 9, math.MaxInt64} {
		first.Release(invalid)
		transientSpoolBudgetAssertUsage(test, 12, 12, 2)
	}
	first.Release(0)
	if err := first.Reserve(0); err != nil {
		test.Fatal("zero-byte reservation failed", err)
	}
	first.Release(3)
	transientSpoolBudgetAssertUsage(test, 12, 9, 2)
	if err := second.Reserve(3); err != nil {
		test.Fatal("released capacity was not reusable", err)
	}
	first.Close()
	first.Close()
	first.Release(5)
	first.Release(math.MaxInt64)
	for _, amount := range []int64{0, 1} {
		if err := first.Reserve(amount); err == nil {
			test.Fatal("closed allocation accepted a reservation", amount)
		}
	}
	if usage := transientSpoolBudgetAssertUsage(test, 12, 7, 1); usage.PeakBytes < peak {
		test.Fatal("cleanup erased the observed peak", usage)
	}
	second.Release(7)
	second.Release(7)
	transientSpoolBudgetAssertUsage(test, 12, 0, 1)
	second.Close()
	transientSpoolBudgetAssertUsage(test, 12, 0, 0)
}

func TestTransientSpoolBudgetReservationOverflow(test *testing.T) {
	for _, configured := range []bool{false, true} {
		name := "embedded"
		if configured {
			name = "configured"
		}
		test.Run(name, func(test *testing.T) {
			limit := int64(0)
			if configured {
				limit = math.MaxInt64
				transientSpoolBudgetConfigure(test, limit)
			}
			first := transientSpoolBudgetAllocation(test)
			second := transientSpoolBudgetAllocation(test)
			if err := first.Reserve(math.MaxInt64); err != nil {
				test.Fatal("maximum representable reservation rejected", err)
			}
			for _, allocation := range []*transientSpoolAllocation{first, second} {
				if err := allocation.Reserve(1); err == nil {
					test.Fatal("overflowing aggregate reservation accepted")
				}
				transientSpoolBudgetAssertUsage(test, limit, math.MaxInt64, 2)
			}
			first.Release(math.MaxInt64)
			if err := second.Reserve(1); err != nil {
				test.Fatal("overflow poisoned later reservations", err)
			}
			transientSpoolBudgetAssertUsage(test, limit, 1, 2)
		})
	}
}

func TestTransientSpoolBudgetConfigurationRejectsInvalidAndOverlappingLimits(test *testing.T) {
	for _, limit := range []int64{0, -1, math.MinInt64} {
		restore, err := configureTransientSpoolBudget(limit)
		if restore != nil {
			test.Cleanup(func() { restore() })
		}
		if err == nil {
			test.Fatal("accepted nonpositive explicit limit", limit)
		}
		transientSpoolBudgetAssertUsage(test, 0, 0, 0)
	}
	transientSpoolBudgetConfigure(test, 11)
	for _, limit := range []int64{11, 12} {
		restore, err := configureTransientSpoolBudget(limit)
		if restore != nil {
			test.Cleanup(func() { restore() })
		}
		if err == nil {
			test.Fatal("accepted overlapping explicit configuration", limit)
		}
		transientSpoolBudgetAssertUsage(test, 11, 0, 0)
	}
}

func TestTransientSpoolBudgetEmbeddedFilesFenceConfiguration(test *testing.T) {
	for _, kind := range []string{"allocation", "legacy", "range", "normalization"} {
		test.Run(kind, func(test *testing.T) {
			var closeAllocation func()
			files := 1
			switch kind {
			case "allocation":
				allocation := transientSpoolBudgetAllocation(test)
				closeAllocation = allocation.Close
			case "legacy":
				spool := transientSpoolBudgetLegacy(test)
				closeAllocation = func() { spool.Close() }
			case "range":
				spool := transientSpoolBudgetRange(test)
				closeAllocation = func() { spool.Close() }
			case "normalization":
				storage := transientSpoolBudgetNormalization(test)
				closeAllocation = func() { storage.Close() }
				files = domain.LegacyNormalizationSlots
			}
			transientSpoolBudgetAssertUsage(test, 0, 0, files)
			restore, err := configureTransientSpoolBudget(8)
			if restore != nil {
				test.Cleanup(func() { restore() })
			}
			if err == nil {
				test.Fatal("configured over a pre-existing empty allocation", kind)
			}
			transientSpoolBudgetAssertUsage(test, 0, 0, files)
			closeAllocation()
			transientSpoolBudgetConfigure(test, 8)
		})
	}
}

func TestTransientSpoolBudgetFSStoresSharePrivateOwner(test *testing.T) {
	var first outbound.TransientSpoolBudgetStore = NewFSStore(test.TempDir())
	var second outbound.TransientSpoolBudgetStore = NewFSStore(test.TempDir())
	restore, err := first.ConfigureTransientSpoolBudget(9)
	if err != nil || restore == nil {
		test.Fatal("FS capability rejected configuration", err)
	}
	test.Cleanup(func() {
		if err := restore(); err != nil {
			test.Error(err)
		}
	})
	spool := transientSpoolBudgetLegacy(test)
	if err := spool.Append(context.Background(), []byte("shared")); err != nil {
		test.Fatal(err)
	}
	usage := transientSpoolBudgetAssertUsage(test, 9, 6, 1)
	if first.TransientSpoolBudgetUsage() != usage || second.TransientSpoolBudgetUsage() != usage {
		test.Fatal("FS instances do not expose the process owner")
	}
	if peerRestore, err := second.ConfigureTransientSpoolBudget(99); err == nil {
		if peerRestore != nil {
			test.Cleanup(func() { peerRestore() })
		}
		test.Fatal("second FS instance bypassed active configuration")
	}
	allocation := transientSpoolBudgetAllocation(test)
	transientSpoolBudgetAssertOverloaded(test, allocation.Reserve(4))
	if err := allocation.Reserve(3); err != nil {
		test.Fatal(err)
	}
	usage = transientSpoolBudgetAssertUsage(test, 9, 9, 2)
	if first.TransientSpoolBudgetUsage() != usage || second.TransientSpoolBudgetUsage() != usage {
		test.Fatal("public usage omitted private reservations")
	}
	allocation.Close()
	if err := spool.Close(); err != nil {
		test.Fatal(err)
	}
	if err := restore(); err != nil {
		test.Fatal(err)
	}
	transientSpoolBudgetAssertUsage(test, 0, 0, 0)
}

func TestTransientSpoolBudgetDrainRestoresOnlyAfterLastAllocation(test *testing.T) {
	restore := transientSpoolBudgetConfigure(test, 12)
	first := transientSpoolBudgetLegacy(test)
	second := transientSpoolBudgetRange(test)
	empty := transientSpoolBudgetAllocation(test)
	ctx := context.Background()
	if err := first.Append(ctx, []byte("legacy")); err != nil {
		test.Fatal(err)
	}
	if err := second.Append(ctx, []byte("page")); err != nil {
		test.Fatal(err)
	}
	if err := restore(); err == nil {
		test.Fatal("restored configuration before open files drained")
	}
	transientSpoolBudgetAssertUsage(test, 12, 10, 3)
	for _, attempt := range []func() error{
		func() error {
			allocation, err := newTransientSpoolAllocation()
			if allocation != nil {
				allocation.Close()
			}
			return err
		},
		func() error {
			spool, err := newLegacyDocSpool()
			if spool != nil {
				spool.Close()
			}
			return err
		},
		func() error {
			spool, err := newDocRangeSpool()
			if spool != nil {
				spool.Close()
			}
			return err
		},
		func() error {
			spool, err := newLegacyNormalizationSpool(ctx)
			if spool != nil {
				spool.Close()
			}
			return err
		},
	} {
		transientSpoolBudgetAssertOverloaded(test, attempt())
		transientSpoolBudgetAssertUsage(test, 12, 10, 3)
	}
	if err := second.Append(ctx, []byte("xy")); err != nil {
		test.Fatal("draining prevented an existing allocation from reserving", err)
	}
	transientSpoolBudgetAssertUsage(test, 12, 12, 3)
	if err := first.Close(); err != nil {
		test.Fatal(err)
	}
	transientSpoolBudgetAssertUsage(test, 12, 6, 2)
	if err := restore(); err == nil {
		test.Fatal("restored with the final buffered file still open")
	}
	if err := second.Flush(ctx); err != nil {
		test.Fatal("draining prevented existing work from flushing", err)
	}
	if err := second.Close(); err != nil {
		test.Fatal(err)
	}
	transientSpoolBudgetAssertUsage(test, 12, 0, 1)
	if err := restore(); err == nil {
		test.Fatal("restored over an empty but still open allocation")
	}
	empty.Close()
	transientSpoolBudgetAssertUsage(test, 0, 0, 0)
	newRestore := transientSpoolBudgetConfigure(test, 5)
	if err := restore(); err != nil {
		test.Fatal("old restore was not idempotent", err)
	}
	transientSpoolBudgetAssertUsage(test, 5, 0, 0)
	if err := newRestore(); err != nil {
		test.Fatal(err)
	}
}

func TestTransientSpoolBudgetLegacyAndBufferedRangeShareExactLimit(test *testing.T) {
	transientSpoolBudgetConfigure(test, 10)
	legacy := transientSpoolBudgetLegacy(test)
	ranged := transientSpoolBudgetRange(test)
	ctx := context.Background()
	if err := legacy.Append(ctx, []byte("legacy")); err != nil {
		test.Fatal(err)
	}
	if err := ranged.Append(ctx, []byte("page")); err != nil {
		test.Fatal(err)
	}
	transientSpoolBudgetAssertUsage(test, 10, 10, 2)
	if ranged.writer.Buffered() != 4 {
		test.Fatal("fixture did not stage a buffered reservation")
	}
	transientSpoolBudgetAssertFile(test, ranged.file, "")
	transientSpoolBudgetAssertOverloaded(test, legacy.Append(ctx, []byte("x")))
	transientSpoolBudgetAssertOverloaded(test, ranged.Append(ctx, []byte("x")))
	if legacy.size != 6 || len(ranged.offsets) != 2 || ranged.writer.Buffered() != 4 {
		test.Fatal("overload changed committed size, page index, or buffer")
	}
	transientSpoolBudgetAssertFile(test, legacy.file, "legacy")
	transientSpoolBudgetAssertFile(test, ranged.file, "")
	if err := ranged.Flush(ctx); err != nil {
		test.Fatal("flush double-reserved already staged bytes", err)
	}
	transientSpoolBudgetAssertUsage(test, 10, 10, 2)
	transientSpoolBudgetAssertFile(test, ranged.file, "page")
	page, err := ranged.Read(ctx, 0)
	if err != nil || string(page) != "page" {
		test.Fatal("rejected append damaged original page", string(page), err)
	}
	if err := legacy.Close(); err != nil {
		test.Fatal(err)
	}
	transientSpoolBudgetAssertUsage(test, 10, 4, 1)
	if err := ranged.Append(ctx, []byte("123456")); err != nil {
		test.Fatal("closing another spool did not recover capacity", err)
	}
	page, err = ranged.Read(ctx, 1)
	if err != nil || string(page) != "123456" {
		test.Fatal("recovered page was not readable", string(page), err)
	}
	transientSpoolBudgetAssertUsage(test, 10, 10, 1)
	if err := ranged.Close(); err != nil {
		test.Fatal(err)
	}
	if err := ranged.Close(); err != nil {
		test.Fatal("range close was not idempotent", err)
	}
	transientSpoolBudgetAssertUsage(test, 10, 0, 0)
}

func TestTransientSpoolBudgetCanceledAndExpiredOperationsDoNotWrite(test *testing.T) {
	transientSpoolBudgetConfigure(test, 8)
	legacy := transientSpoolBudgetLegacy(test)
	ranged := transientSpoolBudgetRange(test)
	ctx := context.Background()
	if err := legacy.Append(ctx, []byte("keep")); err != nil {
		test.Fatal(err)
	}
	if err := ranged.Append(ctx, []byte("page")); err != nil {
		test.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	expired, expire := context.WithTimeout(ctx, 0)
	defer expire()
	for _, selected := range []context.Context{canceled, expired} {
		want := selected.Err()
		for _, operation := range []func() error{
			func() error { return legacy.Append(selected, []byte("blocked")) },
			func() error { return legacy.Truncate(selected) },
			func() error { return ranged.Append(selected, []byte("blocked")) },
			func() error { return ranged.Flush(selected) },
			func() error {
				_, err := ranged.Read(selected, 0)
				return err
			},
		} {
			if err := operation(); !errors.Is(err, want) || errors.Is(err, domain.ErrResourceOverloaded) {
				test.Fatalf("cancellation/deadline lost precedence: want=%v got=%v", want, err)
			}
			transientSpoolBudgetAssertUsage(test, 8, 8, 2)
		}
	}
	if legacy.size != 4 || ranged.writer.Buffered() != 4 || len(ranged.offsets) != 2 {
		test.Fatal("canceled operation changed spool state")
	}
	transientSpoolBudgetAssertFile(test, legacy.file, "keep")
	transientSpoolBudgetAssertFile(test, ranged.file, "")
}

type transientSpoolBudgetCancelContext struct {
	context.Context
	cancel     context.CancelFunc
	checkpoint int
	checks     int
}

func (selected *transientSpoolBudgetCancelContext) Err() error {
	selected.checks++
	if selected.checks >= selected.checkpoint {
		selected.cancel()
	}
	return selected.Context.Err()
}

func TestTransientSpoolBudgetCancellationAfterWriteRetainsAcceptedBytes(test *testing.T) {
	transientSpoolBudgetConfigure(test, 8)
	legacy := transientSpoolBudgetLegacy(test)
	ranged := transientSpoolBudgetRange(test)
	legacyContext, cancelLegacy := context.WithCancel(context.Background())
	defer cancelLegacy()
	legacyCanceled := &transientSpoolBudgetCancelContext{Context: legacyContext, cancel: cancelLegacy, checkpoint: 2}
	if err := legacy.Append(legacyCanceled, []byte("keep")); !errors.Is(err, context.Canceled) {
		test.Fatal("post-write legacy cancellation was ignored", err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 4, 2)
	transientSpoolBudgetAssertFile(test, legacy.file, "keep")
	rangeContext, cancelRange := context.WithCancel(context.Background())
	defer cancelRange()
	rangeCanceled := &transientSpoolBudgetCancelContext{Context: rangeContext, cancel: cancelRange, checkpoint: 2}
	if err := ranged.Append(rangeCanceled, []byte("page")); !errors.Is(err, context.Canceled) {
		test.Fatal("post-buffer cancellation was ignored", err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 8, 2)
	if legacy.size != 4 || ranged.writer.Buffered() != 4 || len(ranged.offsets) != 2 {
		test.Fatal("post-write cancellation lost accepted bytes or page metadata")
	}
	transientSpoolBudgetAssertOverloaded(test, legacy.Append(context.Background(), []byte("x")))
	page, err := ranged.Read(context.Background(), 0)
	if err != nil || string(page) != "page" {
		test.Fatal("accepted canceled page was not retained", string(page), err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 8, 2)
}

func TestTransientSpoolBudgetNormalizationCancellationCleansPartialCreation(test *testing.T) {
	transientSpoolBudgetConfigure(test, 8)
	for _, scenario := range []struct {
		name       string
		checkpoint int
	}{
		{name: "one-open-slot", checkpoint: 2},
		{name: "all-open-slots", checkpoint: domain.LegacyNormalizationSlots + 1},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			directory := test.TempDir()
			for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
				test.Setenv(name, directory)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			selected := &transientSpoolBudgetCancelContext{Context: ctx, cancel: cancel, checkpoint: scenario.checkpoint}
			storage, err := newLegacyNormalizationSpool(selected)
			if storage != nil {
				storage.Close()
			}
			if !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrResourceOverloaded) || errors.Is(err, domain.ErrPreparedDocRangeObject) {
				test.Fatal("partial normalization cancellation changed error class", err)
			}
			transientSpoolBudgetAssertUsage(test, 8, 0, 0)
			remaining, err := os.ReadDir(directory)
			if err != nil || len(remaining) != 0 {
				test.Fatal("partial normalization construction retained files", remaining, err)
			}
		})
	}
}

func TestTransientSpoolBudgetCreationFailureRollsBackRegistration(test *testing.T) {
	transientSpoolBudgetConfigure(test, 8)
	missing := filepath.Join(test.TempDir(), "absent")
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		test.Setenv(name, missing)
	}
	legacy, err := newLegacyDocSpool()
	if legacy != nil {
		legacy.Close()
	}
	if err == nil || !os.IsNotExist(err) {
		test.Fatal("legacy creation did not reach the missing temp directory", err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 0, 0)
	ranged, err := newDocRangeSpool()
	if ranged != nil {
		ranged.Close()
	}
	if err == nil || !os.IsNotExist(err) {
		test.Fatal("range creation did not reach the missing temp directory", err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 0, 0)
	storage, err := newLegacyNormalizationSpool(context.Background())
	if storage != nil {
		storage.Close()
	}
	if err == nil || !errors.Is(err, domain.ErrPreparedDocRangeObject) {
		test.Fatal("normalization creation lost its private error", err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 0, 0)
}

type transientSpoolBudgetPartialWriter struct {
	file    *os.File
	limit   int
	failure error
	calls   int
}

func (writer *transientSpoolBudgetPartialWriter) Write(body []byte) (int, error) {
	writer.calls++
	written, err := writer.file.Write(body[:min(writer.limit, len(body))])
	if err != nil {
		return written, err
	}
	return written, writer.failure
}

func TestTransientSpoolBudgetRangePartialWriteReleasesOnlyUnwrittenBytes(test *testing.T) {
	transientSpoolBudgetConfigure(test, 8)
	ranged := transientSpoolBudgetRange(test)
	writer := &transientSpoolBudgetPartialWriter{file: ranged.file, limit: 2, failure: io.ErrClosedPipe}
	ranged.writer = bufio.NewWriterSize(writer, 2)
	ctx := context.Background()
	if err := ranged.Append(ctx, []byte("abcdef")); !errors.Is(err, io.ErrClosedPipe) {
		test.Fatal("partial append error changed class", err)
	}
	if writer.calls != 1 || len(ranged.offsets) != 1 {
		test.Fatal("failed partial page became readable", writer.calls, ranged.offsets)
	}
	transientSpoolBudgetAssertFile(test, ranged.file, "ab")
	transientSpoolBudgetAssertUsage(test, 8, 2, 1)
	legacy := transientSpoolBudgetLegacy(test)
	if err := legacy.Append(ctx, []byte("123456")); err != nil {
		test.Fatal("unwritten reservation was not released", err)
	}
	transientSpoolBudgetAssertOverloaded(test, legacy.Append(ctx, []byte("x")))
	if err := legacy.Close(); err != nil {
		test.Fatal(err)
	}
	if err := ranged.Append(ctx, []byte("later")); !errors.Is(err, io.ErrClosedPipe) || writer.calls != 1 {
		test.Fatal("failed writer was retried or hid its original error", err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 2, 1)
	if err := ranged.Close(); err != nil {
		test.Fatal(err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 0, 0)
}

func TestTransientSpoolBudgetRangeShortFlushRetainsBufferedReservations(test *testing.T) {
	transientSpoolBudgetConfigure(test, 8)
	ranged := transientSpoolBudgetRange(test)
	writer := &transientSpoolBudgetPartialWriter{file: ranged.file, limit: 2}
	ranged.writer = bufio.NewWriterSize(writer, 8)
	ctx := context.Background()
	if err := ranged.Append(ctx, []byte("abcdef")); err != nil {
		test.Fatal(err)
	}
	if err := ranged.Flush(ctx); !errors.Is(err, io.ErrShortWrite) {
		test.Fatal("flush hid its short write", err)
	}
	if ranged.writer.Buffered() != 4 || writer.calls != 1 {
		test.Fatal("short flush lost the remaining staged bytes")
	}
	transientSpoolBudgetAssertFile(test, ranged.file, "ab")
	transientSpoolBudgetAssertUsage(test, 8, 6, 1)
	if err := ranged.Append(ctx, []byte("x")); !errors.Is(err, io.ErrShortWrite) {
		test.Fatal("later append hid sticky flush error", err)
	}
	if _, err := ranged.Read(ctx, 0); !errors.Is(err, io.ErrShortWrite) {
		test.Fatal("read published a partially flushed page", err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 6, 1)
	if err := ranged.Close(); err != nil || writer.calls != 1 {
		test.Fatal("close flushed a failed buffer", err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 0, 0)
}

func TestTransientSpoolBudgetLegacyWriteFailureRollsBackReservation(test *testing.T) {
	transientSpoolBudgetConfigure(test, 8)
	legacy := transientSpoolBudgetLegacy(test)
	ctx := context.Background()
	if err := legacy.Append(ctx, []byte("keep")); err != nil {
		test.Fatal(err)
	}
	if err := legacy.file.Close(); err != nil {
		test.Fatal(err)
	}
	if err := legacy.Append(ctx, []byte("lost")); !errors.Is(err, os.ErrClosed) || errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("closed backing file did not preserve write error", err)
	}
	if legacy.size != 4 {
		test.Fatal("failed write changed logical size", legacy.size)
	}
	transientSpoolBudgetAssertUsage(test, 8, 4, 1)
	transientSpoolBudgetAssertFile(test, legacy.file, "keep")
	peer := transientSpoolBudgetLegacy(test)
	if err := peer.Append(ctx, []byte("peer")); err != nil {
		test.Fatal("failed write leaked its unused reservation", err)
	}
	if err := legacy.Close(); !errors.Is(err, os.ErrClosed) {
		test.Fatal("close error was hidden", err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 4, 1)
	if _, err := os.Stat(legacy.file.Name()); !os.IsNotExist(err) {
		test.Fatal("failed close did not remove its file", err)
	}
	if err := legacy.Close(); !errors.Is(err, os.ErrClosed) {
		test.Fatal("idempotent close changed the stored error", err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 4, 1)
}

func TestTransientSpoolBudgetNormalizationResetReleasesOnlyItsSlot(test *testing.T) {
	transientSpoolBudgetConfigure(test, 10)
	storage := transientSpoolBudgetNormalization(test)
	peer := transientSpoolBudgetRange(test)
	ctx := context.Background()
	if err := storage.Append(ctx, 0, []byte("abcdef")); err != nil {
		test.Fatal(err)
	}
	if err := peer.Append(ctx, []byte("page")); err != nil {
		test.Fatal(err)
	}
	transientSpoolBudgetAssertOverloaded(test, storage.Append(ctx, 1, []byte("x")))
	transientSpoolBudgetAssertUsage(test, 10, 10, domain.LegacyNormalizationSlots+1)
	resources, err := storage.RetainedResources()
	if err != nil || resources.SpoolBytes != 6 {
		test.Fatal("normalization resources omitted actual slot bytes", resources, err)
	}
	if err := storage.Reset(ctx, 0); err != nil {
		test.Fatal(err)
	}
	transientSpoolBudgetAssertUsage(test, 10, 4, domain.LegacyNormalizationSlots+1)
	transientSpoolBudgetAssertFile(test, storage.spools[0].file, "")
	if err := storage.Reset(ctx, 0); err != nil {
		test.Fatal("empty reset was not idempotent", err)
	}
	if err := storage.Append(ctx, 1, []byte("123456")); err != nil {
		test.Fatal("successful truncate did not recover capacity", err)
	}
	body, err := storage.Read(ctx, 1, 0, 6)
	if err != nil || string(body) != "123456" {
		test.Fatal("replacement scratch data was not readable", string(body), err)
	}
	transientSpoolBudgetAssertUsage(test, 10, 10, domain.LegacyNormalizationSlots+1)
	if err := storage.Close(); err != nil {
		test.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		test.Fatal("normalization close was not idempotent", err)
	}
	transientSpoolBudgetAssertUsage(test, 10, 4, 1)
	if err := peer.Append(ctx, []byte("123456")); err != nil {
		test.Fatal("normalization cleanup did not release slot reservations", err)
	}
	transientSpoolBudgetAssertUsage(test, 10, 10, 1)
}

func TestTransientSpoolBudgetNormalizationResetRejectsCanceledReadonlyAndInvalidSlots(test *testing.T) {
	transientSpoolBudgetConfigure(test, 6)
	storage := transientSpoolBudgetNormalization(test)
	ctx := context.Background()
	if err := storage.Append(ctx, 0, []byte("retain")); err != nil {
		test.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := storage.Reset(canceled, 0); !errors.Is(err, context.Canceled) {
		test.Fatal("canceled reset changed error class", err)
	}
	for _, slot := range []int{-1, domain.LegacyNormalizationSlots} {
		if err := storage.Reset(ctx, slot); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("invalid slot changed error class", slot, err)
		}
	}
	storage.readOnly = true
	if err := storage.Reset(ctx, 0); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("read-only reset accepted mutation", err)
	}
	if err := storage.Append(ctx, 0, []byte("x")); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("read-only append lost its original error", err)
	}
	transientSpoolBudgetAssertUsage(test, 6, 6, domain.LegacyNormalizationSlots)
	transientSpoolBudgetAssertFile(test, storage.spools[0].file, "retain")
	storage.readOnly = false
	if err := storage.Reset(ctx, 0); err != nil {
		test.Fatal(err)
	}
	transientSpoolBudgetAssertUsage(test, 6, 0, domain.LegacyNormalizationSlots)
}

func TestTransientSpoolBudgetFailedTruncateDoesNotReleaseCapacity(test *testing.T) {
	transientSpoolBudgetConfigure(test, 8)
	storage := transientSpoolBudgetNormalization(test)
	peer := transientSpoolBudgetLegacy(test)
	ctx := context.Background()
	if err := storage.Append(ctx, 0, []byte("retain")); err != nil {
		test.Fatal(err)
	}
	if err := peer.Append(ctx, []byte("ok")); err != nil {
		test.Fatal(err)
	}
	spool := storage.spools[0]
	readOnly, err := os.Open(spool.file.Name())
	if err != nil {
		test.Fatal(err)
	}
	if err := spool.file.Close(); err != nil {
		readOnly.Close()
		test.Fatal(err)
	}
	spool.file = readOnly
	if err := storage.Reset(ctx, 0); !errors.Is(err, domain.ErrPreparedDocRangeObject) || errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("truncate failure did not preserve private error class", err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 8, domain.LegacyNormalizationSlots+1)
	transientSpoolBudgetAssertFile(test, spool.file, "retain")
	if spool.size != 6 {
		test.Fatal("failed truncate changed logical size", spool.size)
	}
	transientSpoolBudgetAssertOverloaded(test, peer.Append(ctx, []byte("x")))
	if err := storage.Close(); err != nil {
		test.Fatal(err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 2, 1)
}

func TestTransientSpoolBudgetLegacyTruncateReusesFileAndCapacity(test *testing.T) {
	transientSpoolBudgetConfigure(test, 8)
	spool := transientSpoolBudgetLegacy(test)
	ctx := context.Background()
	if err := spool.Append(ctx, []byte("original")); err != nil {
		test.Fatal(err)
	}
	path := spool.file.Name()
	if err := spool.Truncate(ctx); err != nil {
		test.Fatal(err)
	}
	if spool.size != 0 || spool.file.Name() != path {
		test.Fatal("truncate replaced the allocation or retained its old size")
	}
	transientSpoolBudgetAssertUsage(test, 8, 0, 1)
	transientSpoolBudgetAssertFile(test, spool.file, "")
	if err := spool.Append(ctx, []byte("new")); err != nil {
		test.Fatal(err)
	}
	body, err := spool.Read(ctx, 0, 3)
	if err != nil || string(body) != "new" {
		test.Fatal("append after truncate retained old bytes", string(body), err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 3, 1)
	if err := spool.Close(); err != nil {
		test.Fatal(err)
	}
	if err := spool.Truncate(ctx); err == nil {
		test.Fatal("closed spool accepted truncation")
	}
	transientSpoolBudgetAssertUsage(test, 8, 0, 0)
}

func TestTransientSpoolBudgetNotExistCleanupReleasesReservations(test *testing.T) {
	transientSpoolBudgetConfigure(test, 8)
	ctx := context.Background()
	for _, kind := range []string{"legacy", "range"} {
		test.Run(kind, func(test *testing.T) {
			var file *os.File
			var closeSpool func() error
			if kind == "legacy" {
				spool := transientSpoolBudgetLegacy(test)
				file, closeSpool = spool.file, spool.Close
				if err := spool.Append(ctx, []byte("gone")); err != nil {
					test.Fatal(err)
				}
			} else {
				spool := transientSpoolBudgetRange(test)
				file, closeSpool = spool.file, spool.Close
				if err := spool.Append(ctx, []byte("gone")); err != nil {
					test.Fatal(err)
				}
			}
			transientSpoolBudgetAssertUsage(test, 8, 4, 1)
			if err := os.Remove(file.Name()); err != nil {
				test.Fatal(err)
			}
			transientSpoolBudgetAssertUsage(test, 8, 4, 1)
			if err := closeSpool(); err != nil {
				test.Fatal("missing file was not treated as removed", err)
			}
			if err := closeSpool(); err != nil {
				test.Fatal("missing-file cleanup was not idempotent", err)
			}
			transientSpoolBudgetAssertUsage(test, 8, 0, 0)
		})
	}
}

func TestTransientSpoolBudgetRemovalFailureRetainsOrphanReservation(test *testing.T) {
	restore := transientSpoolBudgetConfigure(test, 8)
	spool := transientSpoolBudgetLegacy(test)
	peer := transientSpoolBudgetLegacy(test)
	ctx := context.Background()
	if err := spool.Append(ctx, []byte("orphan")); err != nil {
		test.Fatal(err)
	}
	if err := peer.Append(ctx, []byte("ok")); err != nil {
		test.Fatal(err)
	}
	path := spool.file.Name()
	retained := path + "-retained"
	if err := os.Rename(path, retained); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		removeErr := os.RemoveAll(path)
		retainedErr := os.Remove(retained)
		if removeErr != nil || retainedErr != nil && !os.IsNotExist(retainedErr) {
			test.Error("failed to remove retained fixture files", removeErr, retainedErr)
			return
		}
		spool.quota.Close()
	})
	if err := os.Mkdir(path, 0700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "blocked"), []byte("occupied"), 0600); err != nil {
		test.Fatal(err)
	}
	if err := spool.Close(); err == nil {
		test.Fatal("close hid a failed removal")
	}
	if err := spool.Close(); err == nil {
		test.Fatal("repeated close hid the original removal failure")
	}
	transientSpoolBudgetAssertUsage(test, 8, 8, 2)
	retainedBytes, err := os.ReadFile(retained)
	if err != nil || string(retainedBytes) != "orphan" {
		test.Fatal("removal fixture did not retain the backing bytes", string(retainedBytes), err)
	}
	transientSpoolBudgetAssertOverloaded(test, peer.Append(ctx, []byte("x")))
	if err := restore(); err == nil {
		test.Fatal("restore disregarded an orphaned allocation")
	}
	if err := peer.Close(); err != nil {
		test.Fatal(err)
	}
	transientSpoolBudgetAssertUsage(test, 8, 6, 1)
	if err := os.RemoveAll(path); err != nil {
		test.Fatal(err)
	}
	if err := os.Remove(retained); err != nil {
		test.Fatal(err)
	}
	spool.quota.Close()
	transientSpoolBudgetAssertUsage(test, 0, 0, 0)
}

func TestTransientSpoolBudgetFormatValidationStillPrecedesReservation(test *testing.T) {
	transientSpoolBudgetConfigure(test, 1)
	spool := transientSpoolBudgetRange(test)
	ctx := context.Background()
	if err := spool.Append(ctx, []byte("x")); err != nil {
		test.Fatal(err)
	}
	for _, body := range [][]byte{nil, bytes.Repeat([]byte("x"), domain.MaxDocRangePageBytes+1)} {
		if err := spool.Append(ctx, body); !errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrResourceOverloaded) {
			test.Fatal("quota masked pre-existing page validation", err)
		}
		transientSpoolBudgetAssertUsage(test, 1, 1, 1)
	}
	if _, err := spool.Read(ctx, 1); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("quota masked invalid page index", err)
	}
	if spool.writer.Buffered() != 1 || len(spool.offsets) != 2 {
		test.Fatal("invalid page operation flushed or changed staged data")
	}
	transientSpoolBudgetAssertFile(test, spool.file, "")
}

func TestTransientSpoolBudgetProofOverloadDoesNotMintOrCacheReference(test *testing.T) {
	want, plan := legacyProofStreamFixture(test)
	stored, err := json.Marshal(plan.Manifest)
	if err != nil {
		test.Fatal(err)
	}
	directory := test.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		test.Setenv(name, directory)
	}
	transientSpoolBudgetConfigure(test, 1)
	var cache docProofCache
	repo := domain.HashContent([]byte(test.Name()))
	reads := 0
	proof, err := cache.verifyStored(context.Background(), repo, want, stored, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		reads++
		return plan.Bodies[hash], nil
	})
	transientSpoolBudgetAssertOverloaded(test, err)
	if proof.Valid() || len(cache.proofs) != 0 || reads != 1 {
		test.Fatal("overloaded proof retained authority, cache state, or performed extra reads", proof, reads)
	}
	transientSpoolBudgetAssertUsage(test, 1, 0, 0)
	remaining, err := os.ReadDir(directory)
	if err != nil || len(remaining) != 0 {
		test.Fatal("overloaded proof leaked temporary files", remaining, err)
	}
}

func TestTransientSpoolBudgetConcurrentReservationsRespectAggregateLimit(test *testing.T) {
	const contenders = 32
	const limit = 16
	transientSpoolBudgetConfigure(test, limit)
	allocations := make([]*transientSpoolAllocation, contenders)
	for index := range allocations {
		allocations[index] = transientSpoolBudgetAllocation(test)
	}
	start := make(chan struct{})
	release := make(chan struct{})
	results := make(chan error, contenders)
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	var workers sync.WaitGroup
	defer func() {
		releaseAll()
		workers.Wait()
	}()
	for _, allocation := range allocations {
		workers.Add(1)
		go func(allocation *transientSpoolAllocation) {
			defer workers.Done()
			defer allocation.Close()
			<-start
			results <- allocation.Reserve(1)
			<-release
		}(allocation)
	}
	close(start)
	accepted := 0
	for range contenders {
		if err := <-results; err == nil {
			accepted++
		} else if !errors.Is(err, domain.ErrResourceOverloaded) {
			test.Error("concurrent reservation changed overload class", err)
		}
	}
	if accepted != limit {
		test.Errorf("accepted %d simultaneous bytes, want %d", accepted, limit)
	}
	transientSpoolBudgetAssertUsage(test, limit, limit, contenders)
	releaseAll()
	workers.Wait()
	transientSpoolBudgetAssertUsage(test, limit, 0, 0)
}

func TestTransientSpoolBudgetConcurrentReleaseAndCloseDoNotUndercountPeer(test *testing.T) {
	transientSpoolBudgetConfigure(test, 64)
	shared := transientSpoolBudgetAllocation(test)
	peer := transientSpoolBudgetAllocation(test)
	if err := peer.Reserve(8); err != nil {
		test.Fatal(err)
	}
	start := make(chan struct{})
	failures := make(chan outbound.TransientSpoolBudgetUsage, 33)
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for range 64 {
				if err := shared.Reserve(1); err == nil {
					shared.Release(1)
				}
				usage := transientSpoolBudgetUsage()
				if usage.UsedBytes < 8 || usage.UsedBytes > 40 || usage.OpenFiles < 1 || usage.OpenFiles > 2 {
					failures <- usage
					return
				}
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		for range 64 {
			shared.Close()
			shared.Release(math.MaxInt64)
			shared.Release(-1)
		}
	}()
	close(start)
	workers.Wait()
	close(failures)
	for usage := range failures {
		test.Error("concurrent cleanup undercounted peer or corrupted aggregate usage", usage)
	}
	transientSpoolBudgetAssertUsage(test, 64, 8, 1)
	peer.Close()
	transientSpoolBudgetAssertUsage(test, 64, 0, 0)
}
