//go:build postgres

package store

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestRootSourceFunctionDefinitionsMatchLatestReleasedSQL(test *testing.T) {
	definitions := rootSourceFunctionDefinitionsPG()
	pattern := regexp.MustCompile(`(?s)CREATE (?:OR REPLACE )?FUNCTION (cxt_(?:advance_doc_range_source_version|assign_blob_incarnation|preserve_blob_incarnation|initialize_blob_incarnation|capture_catalog))\(\).*?AS \$\$(.*?)\$\$`)
	paths, err := filepath.Glob("../../../../schemas/db/migrations/*.sql")
	if err != nil || len(paths) == 0 {
		test.Fatal("read the complete current SQL bundle", err)
	}
	current := make(map[string]string)
	for _, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			test.Fatal(err)
		}
		for _, match := range pattern.FindAllSubmatch(source, -1) {
			digest := sha256.Sum256(match[2])
			current[string(match[1])] = hex.EncodeToString(digest[:])
		}
	}
	if len(current) != len(definitions) {
		test.Fatal("source contract does not cover the released function set", current)
	}
	for name, digest := range definitions {
		if current[name] != digest {
			test.Fatal("source proof contract differs from the last released SQL definition", name, current[name], digest)
		}
	}
}
