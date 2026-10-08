package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
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

func assertRootCacheFirstEvent(t *testing.T, doc domain.VerifiedSessionDoc, f rootReaderFixture) {
	t.Helper()
	assertRootProof(t, doc, f)
	index, err := doc.ReadIndex()
	if err != nil || len(index.Events) != 2 {
		t.Fatal("two-event index", err)
	}
	first := index.Events[0]
	if first.Offset+first.Length >= domain.ConversationManifestChunkBytes {
		t.Fatal("first event overlaps the tail chunk")
	}
	raw, err := doc.EventStreamRange(context.Background(), first.Offset, first.Length)
	if err != nil {
		t.Fatal(err)
	}
	event, err := domain.DecodeIndexedEvent(raw, first)
	if err != nil || !reflect.DeepEqual(event, f.cir.Events[0]) {
		t.Fatal("first-event read", err)
	}
}

func TestRootFSWarmCacheRejectsUnusedTailWithoutWrites(t *testing.T) {
	for _, mode := range []string{"corrupt", "missing"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			st := NewFSStore(t.TempDir())
			repo := domain.HashContent([]byte(t.Name()))
			f := rootCacheTailFixture(t)
			seedRootFS(t, st, repo, f)
			before := rootFSImage(t, st.dataDir)
			for i := 0; i < 2; i++ {
				doc, err := st.ReadVerifiedDoc(ctx, repo, f.hash)
				if err != nil {
					t.Fatal(err)
				}
				assertRootCacheFirstEvent(t, doc, f)
			}
			results := make(chan error, 4)
			for range 4 {
				go func() {
					doc, err := st.ReadVerifiedDoc(ctx, repo, f.hash)
					if err == nil && (!doc.Valid() || doc.DocumentRef() != (domain.DocumentRef{Hash: f.hash, Identity: domain.DocumentIdentityRootV1})) {
						err = errors.New("concurrent warm read lost root identity")
					}
					results <- err
				}()
			}
			for range 4 {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(before, rootFSImage(t, st.dataDir)) {
				t.Fatal("warm read wrote durable state")
			}
			tail := f.manifest.Chunks[len(f.manifest.Chunks)-1].Hash
			want := domain.ErrNotFound
			if mode == "corrupt" {
				body := bytes.Clone(f.bodies[tail])
				body[len(body)-1] ^= 1
				if err := writeAtomic(st.chunkPath(repo, tail), docCompress(body)); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrIntegrity
			} else if err := os.Remove(st.chunkPath(repo, tail)); err != nil {
				t.Fatal(err)
			}
			before = rootFSImage(t, st.dataDir)
			for _, reader := range []*FSStore{st, NewFSStore(st.dataDir)} {
				if doc, err := reader.ReadVerifiedDoc(ctx, repo, f.hash); !errors.Is(err, want) || doc.Valid() {
					t.Fatal("invalid tail returned body proof", err)
				}
				if ref, err := reader.VerifyStoredDoc(ctx, repo, f.hash); !errors.Is(err, want) || ref.Valid() {
					t.Fatal("invalid tail returned reference proof", err)
				}
				if len(reader.docProofs.proofs) != 0 {
					t.Fatal("root entered legacy physical cache")
				}
			}
			if !reflect.DeepEqual(before, rootFSImage(t, st.dataDir)) {
				t.Fatal("failed read repaired dependencies or wrote durable state")
			}
		})
	}
}
