package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// TestDocCompressionRoundTripAndLegacy fixes two invariants of doc at-rest compression:
// (1) newly written docs are stored using zstd and GetDoc round-trip matches the original, (2) legacy uncompressed
// (JSON plain text) files are also read verbatim via magic detection — mixable without migration.
func TestDocCompressionRoundTripAndLegacy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := NewFSStore(dir)
	repo := ph("r")

	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{SessionOriginID: "sess-1", SourceModel: "claude-fable-5"}}}
	canonical, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(canonical)
	if _, err := st.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(st.docPath(repo, doc.Hash))
	if err != nil {
		t.Fatal(err)
	}
	if !(len(raw) >= 4 && raw[0] == 0x28 && raw[1] == 0xB5 && raw[2] == 0x2F && raw[3] == 0xFD) {
		t.Fatalf("new doc not stored using zstd (first bytes %x)", raw[:4])
	}
	got, err := st.GetDoc(ctx, repo, doc.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.CIR.Envelope.SessionOriginID != "sess-1" || got.CIR.Envelope.SourceModel != "claude-fable-5" {
		t.Fatalf("compression round-trip mismatch: %+v", got.CIR.Envelope)
	}

	// legacy uncompressed file — existing storage must be readable without migration.
	legacy := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{SessionOriginID: "sess-legacy"}}}
	legacyCanonical, err := domain.CanonicalBytes(legacy.CIR)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Hash = domain.HashContent(legacyCanonical)
	lb, _ := json.Marshal(legacy.CIR)
	lp := st.docPath(repo, legacy.Hash)
	if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lp, lb, 0o644); err != nil {
		t.Fatal(err)
	}
	got2, err := st.GetDoc(ctx, repo, legacy.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if got2.CIR.Envelope.SessionOriginID != "sess-legacy" {
		t.Fatalf("failed to read legacy uncompressed doc: %+v", got2.CIR.Envelope)
	}
}
