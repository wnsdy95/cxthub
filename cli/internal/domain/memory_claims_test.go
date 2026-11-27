package domain

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/testutil"
)

func TestTypedMemoryWirePreservesSourceClaims(t *testing.T) {
	raw, err := os.ReadFile(testutil.RepositoryPath(t, "schemas", "testdata", "memory-claims-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var digest MemoryDigest
	if err := json.Unmarshal(raw, &digest); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(digest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, bytes.TrimSpace(raw)) {
		t.Fatalf("memory claims lost during round trip:\n%s", encoded)
	}
}
