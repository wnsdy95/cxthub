//go:build postgres

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyByteCompatibilityPGStore struct {
	*legacyFindGenerationPGStore
}

func (storage *legacyByteCompatibilityPGStore) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	storage.cost.whole++
	return storage.PostgresStore.GetDoc(ctx, repo, hash)
}

func legacyByteRangePGSourceImage(test *testing.T, fixture docViewPGFixture) string {
	test.Helper()
	var image string
	err := fixture.pool.QueryRow(fixture.ctx, `SELECT jsonb_agg(jsonb_build_array(owned.kind,owned.hash,encode(source.bytes,'hex'),source.doc_range_source_version,owned.xmin::text,source.xmin::text) ORDER BY owned.kind,owned.hash)::text
 FROM repo_blobs owned JOIN blobs source ON source.hash=owned.hash WHERE owned.repo_id=$1`, fixture.repo).Scan(&image)
	if err != nil {
		test.Fatal(err)
	}
	return image
}

func TestPGLegacyByteRangeCrossesOriginalChunksWithoutManifest(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{{Kind: domain.EventToolResult, Output: strings.Repeat("x", 2*domain.ChunkTarget+128)}})
	before := legacyByteRangePGSourceImage(test, fixture)
	prepareLegacyFindGenerationPG(test, fixture, document.Hash)
	if after := legacyByteRangePGSourceImage(test, fixture); after != before {
		test.Fatal("byte directory preparation rewrote source, chunks or ownership")
	}
	observed := newLegacyFindGenerationPGStore(fixture)
	fixture.service.blobs = observed
	raw := docViewLegacyGenerationEventBytes(test, document, 0)
	for _, offset := range []int{0, domain.ChunkTarget - 32, domain.ChunkTarget + 11, len(raw) - 17} {
		observed.cost = legacyFindGenerationCosts{}
		fragment, err := fixture.service.ReadDocViewFragment(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, document.Hash, 0, offset, 64)
		end := min(offset+64, len(raw))
		if err != nil || fragment.JSON != string(raw[offset:end]) || fragment.Total != len(raw) || observed.cost.manifests != 0 || observed.cost.whole != 0 || observed.cost.indexes != 0 || observed.cost.pages != 1 || observed.cost.chunkLookups < 1 || observed.cost.chunkLookups > 2 || observed.cost.chunks != observed.cost.chunkLookups {
			test.Fatal("byte directory changed original range or used full-source IO", offset, fragment, observed.cost, err)
		}
	}
	if after := legacyByteRangePGSourceImage(test, fixture); after != before {
		test.Fatal("selected byte reads changed original source")
	}
}

func TestPGLegacyByteRangeChecksSelectedDirectoryAndOwnership(test *testing.T) {
	for _, damage := range []string{"directory", "ownership", "selected bytes", "oversized stored bytes", "expanded compressed bytes"} {
		test.Run(damage, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{historyMessage(domain.RoleUser, "selected legacy bytes remain current "+string(fixture.repo))})
			prepareLegacyFindGenerationPG(test, fixture, document.Hash)
			var selected domain.ConversationManifestChunk
			var page domain.ContentHash
			if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
				reader, err := fixture.storage.OpenDocRangeGeneration(read, fixture.repo, document.Hash)
				if err != nil {
					return err
				}
				if reader.Descriptor().Version != 4 {
					return fmt.Errorf("expected byte generation v4, got %d", reader.Descriptor().Version)
				}
				selected, err = reader.Chunk(read, 0)
				if err != nil {
					return fmt.Errorf("selected original chunk: %w", err)
				}
				page, err = domain.ResolveDocRangeLeaf(read, reader.Descriptor().ChunkPages, 0, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
					var raw []byte
					err := fixture.pool.QueryRow(ctx, `SELECT bytes FROM doc_range_objects_v2 WHERE doc_hash=$1 AND hash=$2`, document.Hash, hash).Scan(&raw)
					return raw, err
				})
				return err
			}); err != nil {
				test.Fatal(err)
			}
			want := domain.ErrIntegrity
			switch damage {
			case "directory":
				if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE doc_range_objects_v2 SET bytes=$3 WHERE doc_hash=$1 AND hash=$2`, document.Hash, page, []byte("corrupt selected chunk directory")); err != nil {
					test.Fatal(err)
				}
			case "ownership":
				want = domain.ErrNotFound
				if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, fixture.repo, selected.Hash); err != nil {
					test.Fatal(err)
				}
			case "selected bytes":
				if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, selected.Hash, bytes.Repeat([]byte{'x'}, int(selected.Bytes))); err != nil {
					test.Fatal(err)
				}
			case "oversized stored bytes":
				want = domain.ErrDocumentResourceLimit
				if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, selected.Hash, bytes.Repeat([]byte{'x'}, 2*int(selected.Bytes)+65)); err != nil {
					test.Fatal(err)
				}
			case "expanded compressed bytes":
				encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
				if err != nil {
					test.Fatal(err)
				}
				raw := encoder.EncodeAll(bytes.Repeat([]byte{'x'}, 2*domain.ChunkTarget), nil)
				encoder.Close()
				if len(raw) > 2*int(selected.Bytes)+64 {
					test.Fatal("compressed fixture does not reach the bounded decoder", len(raw), selected.Bytes)
				}
				if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, selected.Hash, raw); err != nil {
					test.Fatal(err)
				}
			}
			observed := newLegacyFindGenerationPGStore(fixture)
			fixture.service.blobs = observed
			fragment, err := fixture.service.ReadDocViewFragment(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, document.Hash, 0, 0, 64)
			if !errors.Is(err, want) || fragment != (domain.DocViewFragment{}) || observed.cost.manifests != 0 || observed.cost.whole != 0 || observed.cost.indexes != 0 {
				test.Fatal("selected byte failure used fallback or exposed content", fragment, observed.cost, err)
			}
			if (damage == "oversized stored bytes" || damage == "expanded compressed bytes") && observed.cost.chunkBytes != 0 {
				test.Fatal("oversized selected bytes escaped the bounded decoder", observed.cost)
			}
		})
	}
}

func TestPGLegacyByteRangePublicationHasReaderFirstFuse(test *testing.T) {
	for _, mode := range []string{"reader-only old generation", "publication enabled", "reader-only new generation"} {
		test.Run(mode, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{historyMessage(domain.RoleUser, "reader-first rollout")})
			fixture.storage.ConfigureLegacyByteRangePublication(mode != "reader-only old generation")
			prepareLegacyFindGenerationPG(test, fixture, document.Hash)
			fixture.storage.ConfigureLegacyByteRangePublication(false)
			observed := newLegacyFindGenerationPGStore(fixture)
			fixture.service.blobs = observed
			page, err := fixture.service.ReadDocViewEvents(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, document.Hash, "", 0, 1, "chat")
			if err != nil || len(page.Events) != 1 || page.Hash != document.Hash || page.DocIdentity != domain.DocumentIdentityLegacy {
				test.Fatal("disabling publication disabled the compatible reader", page, err)
			}
			manifests := 0
			if mode == "reader-only old generation" {
				manifests = 1
			}
			if observed.cost.manifests != manifests || observed.cost.whole != 0 || observed.cost.indexes != 0 {
				test.Fatal("publication fuse did not preserve reader-first behavior", mode, observed.cost)
			}
		})
	}
}

func TestPGLegacyByteRangeKeepsOldMetadataAndMonolithicCompatibilityExplicit(test *testing.T) {
	for _, monolithic := range []bool{false, true} {
		name := "old metadata"
		if monolithic {
			name = "monolithic"
		}
		test.Run(name, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			fixture.storage.ConfigureLegacyByteRangePublication(false)
			document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{historyMessage(domain.RoleUser, "original archival representation")})
			verified, err := domain.VerifySessionDoc(document)
			if err != nil {
				test.Fatal(err)
			}
			if monolithic {
				raw, err := domain.CanonicalBytes(document.CIR)
				if err != nil {
					test.Fatal(err)
				}
				if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, document.Hash, raw); err != nil {
					test.Fatal(err)
				}
			}
			before := legacyByteRangePGSourceImage(test, fixture)
			prepareLegacyFindGenerationPG(test, fixture, document.Hash)
			if !monolithic {
				generation, _, err := domain.BuildLegacyDocRangeGeneration(fixture.ctx, verified)
				if err != nil {
					test.Fatal(err)
				}
				raw, err := generation.Encode()
				if err != nil {
					test.Fatal(err)
				}
				if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE doc_range_generations_v2 SET generation_hash=$2,descriptor=$3 WHERE doc_hash=$1`, document.Hash, domain.HashContent(raw), raw); err != nil {
					test.Fatal(err)
				}
			}
			var raw []byte
			if err := fixture.pool.QueryRow(fixture.ctx, `SELECT descriptor FROM doc_range_generations_v2 WHERE doc_hash=$1`, document.Hash).Scan(&raw); err != nil {
				test.Fatal(err)
			}
			var generation domain.DocRangeGeneration
			if json.Unmarshal(raw, &generation) != nil || generation.Version != 3 {
				test.Fatal("compatibility fixture incorrectly advertises a byte directory", generation)
			}
			observed := &legacyByteCompatibilityPGStore{legacyFindGenerationPGStore: newLegacyFindGenerationPGStore(fixture)}
			fixture.service.blobs = observed
			fragment, err := fixture.service.ReadDocViewFragment(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, document.Hash, 0, 0, 64)
			expected := docViewLegacyGenerationEventBytes(test, document, 0)
			whole := 0
			if monolithic {
				whole = 1
			}
			if err != nil || fragment.JSON != string(expected[:min(len(expected), 64)]) || observed.cost.pages != 1 || observed.cost.manifests != 1 || observed.cost.chunkLookups != 0 || observed.cost.whole != whole || observed.cost.indexes != 0 {
				test.Fatal("old archive compatibility was lost or hidden as bounded bytes", fragment, observed.cost, err)
			}
			if after := legacyByteRangePGSourceImage(test, fixture); after != before {
				test.Fatal("preparation or compatibility silently re-encoded original bytes")
			}
		})
	}
}
