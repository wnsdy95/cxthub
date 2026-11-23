//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

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
