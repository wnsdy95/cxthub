package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMigrationCatalogMatchesOnlyReleasedBundle(test *testing.T) {
	const source = "../../../../schemas/db/migrations"
	manifest, err := migrationManifest(context.Background(), source)
	if err != nil {
		test.Fatal(err)
	}
	if len(manifest) != 158 {
		test.Fatal("released source catalog must contain all 158 SQL files", len(manifest))
	}
	if err := verifyMigrationCatalog(manifest); err != nil {
		test.Fatal("complete source catalog does not match the compiled release digest", err)
	}
	if err := verifyMigrationCatalog(manifest[:155]); !errors.Is(err, ErrSchemaMismatch) {
		test.Fatal("pre-parser release bundle was accepted for Stage checkpoint readers", err)
	}
	for _, mutation := range []string{"altered", "incomplete", "extra", "renamed"} {
		test.Run(mutation, func(test *testing.T) {
			directory := test.TempDir()
			for _, migration := range manifest {
				if mutation == "incomplete" && migration.name == manifest[len(manifest)-1].name {
					continue
				}
				contents, err := os.ReadFile(filepath.Join(source, migration.name))
				if err != nil {
					test.Fatal(err)
				}
				name := migration.name
				if migration.name == manifest[0].name {
					if mutation == "altered" {
						contents = append(contents, []byte("\nSELECT 1;\n")...)
					} else if mutation == "renamed" {
						name = "0000_renamed.sql"
					}
				}
				if err := os.WriteFile(filepath.Join(directory, name), contents, 0600); err != nil {
					test.Fatal(err)
				}
			}
			if mutation == "extra" {
				if err := os.WriteFile(filepath.Join(directory, "9999_extra.sql"), []byte("SELECT 1;\n"), 0600); err != nil {
					test.Fatal(err)
				}
			}
			bundle, err := migrationManifest(context.Background(), directory)
			if err != nil {
				test.Fatal(err)
			}
			if err := verifyMigrationCatalog(bundle); !errors.Is(err, ErrSchemaMismatch) || !strings.Contains(err.Error(), "image") || !strings.Contains(err.Error(), "bundle") {
				test.Fatal("different bundle did not fail with an actionable image/bundle mismatch", err)
			}
		})
	}
}

func TestRuntimeStoreExposesVerificationWithoutMigrationApply(test *testing.T) {
	capabilities := reflect.TypeOf((*Store)(nil)).Elem()
	if _, present := capabilities.MethodByName("VerifyMigrations"); !present {
		test.Fatal("runtime store lost schema verification")
	}
	for _, method := range []string{"ApplyMigrations", "ApplyPublicMigrations"} {
		if _, present := capabilities.MethodByName(method); present {
			test.Fatal("runtime store exposes schema application", method)
		}
	}
}
