//go:build monorepo

package domain

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestConversationManifestWireAndFixtureParity(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Go tests run in the package directory, including builds using -trimpath.
	root := filepath.Clean(filepath.Join(dir, "..", "..", ".."))
	// Verification caches are module-local. Keep the wire format, builder and
	// identical semantic fixture suite aligned while allowing different readers.
	for _, name := range []string{"conversation_manifest.go", "conversation_manifest_build.go", "conversation_manifest_test.go"} {
		backend, err := os.ReadFile(filepath.Join(root, "backend", "internal", "domain", name))
		if err != nil {
			t.Fatal(err)
		}
		cli, err := os.ReadFile(filepath.Join(root, "cli", "internal", "domain", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(backend, cli) {
			t.Fatalf("independent wire/fixture drift: %s", name)
		}
	}
}
