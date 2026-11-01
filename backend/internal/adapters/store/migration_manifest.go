package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const releasedMigrationCatalogSHA256 = "ceefc26566f79599aa121c84e5fa128de97a2eb4d057457388dd1cfd3c966db8"

var ErrSchemaMismatch = errors.New("database schema is incompatible")

type schemaMigration struct {
	name     string
	checksum string
}

func verifyMigrationCatalog(manifest []schemaMigration) error {
	catalog := append([]schemaMigration(nil), manifest...)
	sort.Slice(catalog, func(left, right int) bool { return catalog[left].name < catalog[right].name })
	hasher := sha256.New()
	for _, migration := range catalog {
		_, _ = fmt.Fprintf(hasher, "%s\x00%s\n", migration.name, migration.checksum)
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if digest != releasedMigrationCatalogSHA256 {
		return fmt.Errorf("%w: migration bundle does not match this image's released catalog (expected %s, got %s); use the complete SQL bundle shipped with this image", ErrSchemaMismatch, releasedMigrationCatalogSHA256, digest)
	}
	return nil
}

func migrationManifest(ctx context.Context, directory string) ([]schemaMigration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: migration bundle is missing: %w", ErrSchemaMismatch, err)
		}
		return nil, err
	}
	var manifest []schemaMigration
	buffer := make([]byte, 32<<10)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf("%w: migration %q is not a regular file", ErrSchemaMismatch, entry.Name())
		}
		checksum, err := migrationFileChecksum(ctx, filepath.Join(directory, entry.Name()), buffer)
		if err != nil {
			return nil, err
		}
		manifest = append(manifest, schemaMigration{name: entry.Name(), checksum: checksum})
	}
	if len(manifest) == 0 {
		return nil, fmt.Errorf("%w: the migration bundle is empty", ErrSchemaMismatch)
	}
	return manifest, nil
}

func migrationFileChecksum(ctx context.Context, path string, buffer []byte) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		count, err := file.Read(buffer)
		if count > 0 {
			_, _ = hasher.Write(buffer[:count])
		}
		if errors.Is(err, io.EOF) {
			return hex.EncodeToString(hasher.Sum(nil)), nil
		}
		if err != nil {
			return "", err
		}
	}
}

func migrationHistoryMatches(manifest []schemaMigration, applied map[string]string, complete bool) error {
	expected := make(map[string]string, len(manifest))
	for _, migration := range manifest {
		expected[migration.name] = migration.checksum
		checksum, present := applied[migration.name]
		if !present {
			if complete {
				return fmt.Errorf("%w: migration %q is not applied", ErrSchemaMismatch, migration.name)
			}
			continue
		}
		if checksum != migration.checksum {
			return fmt.Errorf("%w: migration %q checksum is missing or differs", ErrSchemaMismatch, migration.name)
		}
	}
	for name := range applied {
		if _, known := expected[name]; !known {
			return fmt.Errorf("%w: database contains migration %q absent from this image", ErrSchemaMismatch, name)
		}
	}
	return nil
}

func manifestContains(manifest []schemaMigration, name string) bool {
	for _, migration := range manifest {
		if migration.name == name {
			return true
		}
	}
	return false
}

func migrationApplyHistory(manifest []schemaMigration, applied map[string]string, public bool) map[string]string {
	if public {
		return applied
	}
	provided := make(map[string]string, len(manifest))
	for _, migration := range manifest {
		if checksum, present := applied[migration.name]; present && checksum != "" {
			provided[migration.name] = checksum
		}
	}
	return provided
}
