package store

import (
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// The last chunk belongs only to the second event, outside a first-event page.
func rootCacheTailFixture(t *testing.T, salt ...string) rootReaderFixture {
	t.Helper()
	f := rootFixture(t, "first event")
	tile := t.Name() + strings.Join(salt, " ") + " tail "
	f.cir.Events = append(f.cir.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: 2, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat(tile, domain.ConversationManifestChunkBytes/len(tile)+1)}}})
	var err error
	f.manifest, f.bodies, err = domain.ConversationManifestForCIR(f.cir)
	if err != nil {
		t.Fatal(err)
	}
	f.hash, err = domain.ConversationManifestHash(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	f.manifestBytes, err = domain.CanonicalConversationManifest(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	f.canonical, err = domain.CanonicalBytes(f.cir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.manifest.Chunks) < 2 || f.manifest.Chunks[0].Hash == f.manifest.Chunks[len(f.manifest.Chunks)-1].Hash {
		t.Fatal("fixture needs an independent tail chunk")
	}
	return f
}
