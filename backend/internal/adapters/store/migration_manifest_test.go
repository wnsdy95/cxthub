package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrationManifestRequiresReadableNonemptyOrderedBundle(test *testing.T) {
	directory := test.TempDir()
	if _, err := migrationManifest(context.Background(), directory); !errors.Is(err, ErrSchemaMismatch) {
		test.Fatal("empty migration bundle accepted", err)
	}
	for _, name := range []string{"0002_second.sql", "0001_first.sql", "README.md"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0600); err != nil {
			test.Fatal(err)
		}
	}
	manifest, err := migrationManifest(context.Background(), directory)
	if err != nil || len(manifest) != 2 || manifest[0].name != "0001_first.sql" || manifest[1].name != "0002_second.sql" || manifest[0].checksum != fmt.Sprintf("%x", sha256.Sum256([]byte(manifest[0].name))) {
		test.Fatal("migration bundle lost exact bytes or stable name order", manifest, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := migrationManifest(ctx, directory); !errors.Is(err, context.Canceled) {
		test.Fatal("cancelled manifest load continued", err)
	}
}

func TestMigrationHistoryChecksCompleteSourceAndOwnerCompatibility(test *testing.T) {
	manifest := []schemaMigration{{name: "0001_first.sql", checksum: "first"}, {name: "0002_second.sql", checksum: "second"}}
	for _, scenario := range []struct {
		name     string
		applied  map[string]string
		complete bool
		valid    bool
	}{
		{name: "exact", applied: map[string]string{"0001_first.sql": "first", "0002_second.sql": "second"}, complete: true, valid: true},
		{name: "missing", applied: map[string]string{"0001_first.sql": "first"}, complete: true},
		{name: "pending owner", applied: map[string]string{"0001_first.sql": "first"}, valid: true},
		{name: "changed", applied: map[string]string{"0001_first.sql": "wrong"}},
		{name: "legacy checksum cannot serve", applied: map[string]string{"0001_first.sql": "", "0002_second.sql": "second"}, complete: true},
		{name: "public owner rejects missing proof", applied: map[string]string{"0001_first.sql": ""}},
		{name: "newer unknown cannot apply", applied: map[string]string{"9999_newer.sql": "newer"}},
		{name: "newer unknown cannot serve", applied: map[string]string{"0001_first.sql": "first", "0002_second.sql": "second", "9999_newer.sql": "newer"}, complete: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			err := migrationHistoryMatches(manifest, scenario.applied, scenario.complete)
			if scenario.valid && err != nil || !scenario.valid && !errors.Is(err, ErrSchemaMismatch) {
				test.Fatal("migration history accepted or rejected the wrong source", err)
			}
		})
	}
}
