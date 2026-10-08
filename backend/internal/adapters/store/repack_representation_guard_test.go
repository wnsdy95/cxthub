package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestRepackRejectsUnknownDescriptorBeforeNormalization(t *testing.T) {
	for _, format := range []string{"future", "cxt-doc-chunks-v999"} {
		t.Run(format, func(t *testing.T) {
			st := NewFSStore(t.TempDir())
			repo := domain.HashContent([]byte(t.Name()))
			f := rootFixture(t, "valid CIR inside unknown descriptor")
			hash := domain.HashContent(f.canonical)
			body := []byte("future descriptor dependency")
			chunk := domain.HashContent(body)
			raw := append([]byte(fmt.Sprintf(`{"format":%q,"chunks":[%q],`, format, chunk)), f.canonical[1:]...)
			if err := writeAtomic(st.docPath(repo, hash), docCompress(raw)); err != nil {
				t.Fatal(err)
			}
			if err := writeAtomic(st.chunkPath(repo, chunk), docCompress(body)); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(st.chunkPath(repo, chunk), old, old); err != nil {
				t.Fatal(err)
			}
			before := rootFSImage(t, st.repoDir(repo))
			if converted, _, err := st.RepackDocs(); err == nil || converted != 0 {
				t.Fatalf("unknown descriptor converted=%d error=%v", converted, err)
			}
			after := rootFSImage(t, st.repoDir(repo))
			if len(before) != len(after) {
				t.Fatal("rejected repack changed object set")
			}
			for path, value := range before {
				if after[path] != value {
					t.Fatal("rejected repack changed source or dependency")
				}
			}
		})
	}
}

func TestRepackAcceptsKnownNoncanonicalLegacyCIR(t *testing.T) {
	st := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	f := rootFixture(t, "noncanonical legacy control")
	hash := domain.HashContent(f.canonical)
	raw, err := json.MarshalIndent(f.cir, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(st.docPath(repo, hash), docCompress(raw)); err != nil {
		t.Fatal(err)
	}
	if converted, _, err := st.RepackDocs(); err != nil || converted != 1 {
		t.Fatalf("known legacy converted=%d error=%v", converted, err)
	}
	doc, err := st.GetDoc(context.Background(), repo, hash)
	if err != nil {
		t.Fatal(err)
	}
	got, err := domain.CanonicalBytes(doc.CIR)
	if err != nil || !bytes.Equal(got, f.canonical) {
		t.Fatal("legacy content/identity changed", err)
	}
}
