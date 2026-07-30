package boundary

import (
	"testing"
	"time"
)

func TestBoundaryTimestampPreservesSubsecondOrdering(t *testing.T) {
	start := time.Now().UTC()
	root := t.TempDir()
	if err := Record(root, Boundary{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	b, ok := Load(root)
	if !ok {
		t.Fatal("recorded boundary did not load")
	}
	at, err := time.Parse(time.RFC3339, b.At)
	if err != nil {
		t.Fatal(err)
	}
	if !at.After(start) {
		t.Fatalf("boundary timestamp %s is not after start %s", at, start)
	}
}
