package storage

import (
	"context"
	"os"
	"reflect"
	"testing"
)

func TestRootDocumentWarmInspectionRemainsPassive(t *testing.T) {
	s := NewFileStore(t.TempDir())
	ref, manifest, bodies := seedRootDoc(t, s, sampleCIR("inspection semantic reuse"))
	before := inspectionDiskState(t, s.storeDir())
	for i := 0; i < 2; i++ {
		if err := s.inspectDocReference(context.Background(), ref, nil); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(before, inspectionDiskState(t, s.storeDir())) {
		t.Fatal("warm inspection created or changed store files")
	}
	// A previously proven root cannot authorize a chunk outside this store even
	// when that other file contains exactly the expected compressed bytes.
	hash := manifest.Chunks[0].Hash
	outside := t.TempDir() + "/chunk"
	if err := os.WriteFile(outside, docCompress(bodies[hash]), 0o600); err != nil {
		t.Fatal(err)
	}
	path := s.objectPath("chunks", hash)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	before = inspectionDiskState(t, s.storeDir())
	if err := s.inspectDocReference(context.Background(), ref, nil); err == nil {
		t.Fatal("warm semantic proof bypassed current file ownership")
	}
	if !reflect.DeepEqual(before, inspectionDiskState(t, s.storeDir())) {
		t.Fatal("failed warm inspection changed store files")
	}
}
