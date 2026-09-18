package storage

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"testing"
)

func TestAppendCaptureDocRetainsCanonicalIdentityAndChunks(t *testing.T) {
	ctx := context.Background()
	s := NewFileStore(t.TempDir())
	full := bigDoc(30)
	prior := full
	prior.Events = full.Events[:29]
	base, err := s.PutDoc(ctx, domain.SessionDoc{CIR: prior})
	if err != nil {
		t.Fatal(err)
	}
	beforeRaw, err := readCxtFile(s.objectPath("docs", base))
	if err != nil {
		t.Fatal(err)
	}
	beforeRaw, _ = docDecompress(beforeRaw)
	before, _ := chunkcas.ParseManifest(beforeRaw)
	delta := full
	delta.Events = full.Events[29:]
	delta.Envelope.ContextTokens = 123
	full.Envelope.ContextTokens = 123
	got, err := s.AppendCaptureDoc(ctx, base, delta)
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := domain.CanonicalBytes(full)
	if got != domain.HashContent(canonical) {
		t.Fatal("hash changed")
	}
	if _, err := s.GetDoc(ctx, got); err != nil {
		t.Fatal(err)
	}
	raw, _ := readCxtFile(s.objectPath("docs", got))
	raw, _ = docDecompress(raw)
	after, _ := chunkcas.ParseManifest(raw)
	for i, ch := range before.Chunks[:len(before.Chunks)-1] {
		if after.Chunks[i] != ch {
			t.Fatal("closed chunk changed")
		}
	}
	if err := writeAtomic(s.objectPath("chunks", before.Chunks[0]), docCompress([]byte("corrupt"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendCaptureDoc(ctx, base, delta); err == nil {
		t.Fatal("corrupt prefix accepted")
	}
}
