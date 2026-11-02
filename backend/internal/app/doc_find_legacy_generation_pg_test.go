//go:build postgres

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyFindGenerationPGStore struct {
	*store.PostgresStore
	cost     legacyFindGenerationCosts
	pages    map[domain.ContentHash]int
	bindings map[domain.ContentHash]domain.DocReadGenerationBinding
}

type legacyFindGenerationPGRead struct {
	outbound.DocRangeGenerationRead
	storage *legacyFindGenerationPGStore
	hash    domain.ContentHash
}

func (storage *legacyFindGenerationPGStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	storage.cost.opens++
	read, err := storage.PostgresStore.OpenDocRangeGeneration(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	raw, _ := read.Descriptor().Encode()
	storage.cost.metadataBytes += len(raw)
	storage.bindings[hash] = read.Binding()
	return &legacyFindGenerationPGRead{DocRangeGenerationRead: read, storage: storage, hash: hash}, nil
}

func (read *legacyFindGenerationPGRead) ReadPage(ctx context.Context, position int) ([]domain.DocRangeEntry, error) {
	read.storage.cost.pages++
	read.storage.pages[read.hash]++
	entries, err := read.DocRangeGenerationRead.ReadPage(ctx, position)
	raw, _ := json.Marshal(entries)
	read.storage.cost.metadataBytes += len(raw)
	return entries, err
}

func (read *legacyFindGenerationPGRead) PageHash(ctx context.Context, position int) (domain.ContentHash, error) {
	read.storage.cost.directories++
	return read.DocRangeGenerationRead.PageHash(ctx, position)
}

func (read *legacyFindGenerationPGRead) Chunk(ctx context.Context, position int) (domain.ConversationManifestChunk, error) {
	read.storage.cost.chunkLookups++
	chunk, err := read.DocRangeGenerationRead.Chunk(ctx, position)
	raw, _ := json.Marshal(chunk)
	read.storage.cost.metadataBytes += len(raw)
	return chunk, err
}

func (storage *legacyFindGenerationPGStore) DocReadIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocReadIndex, error) {
	storage.cost.indexes++
	return domain.DocReadIndex{}, domain.ErrIntegrity
}

func (storage *legacyFindGenerationPGStore) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	storage.cost.whole++
	return domain.SessionDoc{}, domain.ErrIntegrity
}

func (storage *legacyFindGenerationPGStore) GetDocManifest(ctx context.Context, repo, hash domain.ContentHash) (domain.DocChunkManifest, error) {
	storage.cost.manifests++
	manifest, err := storage.PostgresStore.GetDocManifest(ctx, repo, hash)
	raw, _ := json.Marshal(manifest)
	storage.cost.manifestBytes += len(raw)
	return manifest, err
}

func (storage *legacyFindGenerationPGStore) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	storage.cost.chunks++
	raw, err := storage.PostgresStore.GetChunk(ctx, repo, hash)
	storage.cost.chunkBytes += len(raw)
	return raw, err
}

func (storage *legacyFindGenerationPGStore) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	storage.cost.chunks++
	raw, err := storage.PostgresStore.ReadConversationChunk(ctx, repo, hash, length)
	storage.cost.chunkBytes += len(raw)
	return raw, err
}

func (storage *legacyFindGenerationPGStore) DocRangeBoundary(ctx context.Context, repo, hash, base domain.ContentHash) (int, error) {
	storage.cost.boundaries++
	return storage.PostgresStore.DocRangeBoundary(ctx, repo, hash, base)
}

func prepareLegacyFindGenerationPG(test *testing.T, fixture docViewPGFixture, hash domain.ContentHash) {
	test.Helper()
	if _, err := fixture.storage.EnqueueDocRangeJob(fixture.ctx, fixture.repo, hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		generation, err := fixture.storage.OpenDocRangeGeneration(read, fixture.repo, hash)
		if err != nil {
			return err
		}
		if generation.Descriptor().Identity() != domain.DocumentIdentityLegacy || generation.Binding().Validate() != nil {
			return domain.ErrIntegrity
		}
		return nil
	}); err != nil {
		test.Fatal("legacy generation was not prepared by the existing worker", err)
	}
}

func prepareLegacyFindPairPG(test *testing.T, fixture docViewPGFixture, hash, base domain.ContentHash, inherited int) {
	test.Helper()
	if _, err := fixture.storage.EnqueueDocRangeBoundary(fixture.ctx, fixture.repo, hash, base, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	for attempt := 0; attempt < 12; attempt++ {
		if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
			test.Fatal(err)
		}
		err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
			prefix, err := fixture.storage.DocRangeBoundary(read, fixture.repo, hash, base)
			if err == nil && prefix != inherited {
				return domain.ErrIntegrity
			}
			return err
		})
		if err == nil {
			return
		}
		if !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
			test.Fatal(err)
		}
	}
	test.Fatal("legacy pair did not complete bounded durable prefix preparation")
}

func seedLegacyFindGenerationPairPG(test *testing.T, fixture docViewPGFixture, count int, rootBase bool) (domain.SessionDoc, domain.ContentHash) {
	test.Helper()
	events := make([]domain.CIREvent, count)
	for index := range events {
		events[index] = historyMessage(domain.RoleUser, fmt.Sprintf("source row %d %s", index, fixture.repo))
		events[index].Seq = index
	}
	events[count-1] = historyMessage(domain.RoleAssistant, strings.Repeat("Needle🙂 ", 45))
	doc := seedLegacyFindPGDocument(test, fixture, events)
	prepareLegacyFindGenerationPG(test, fixture, doc.Hash)
	var base domain.ContentHash
	if rootBase {
		base = seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: doc.CIR.Envelope, Events: append([]domain.CIREvent(nil), doc.CIR.Events[:count-2]...)})
	} else {
		base = seedLegacyFindPGDocument(test, fixture, doc.CIR.Events[:count-2]).Hash
		prepareLegacyFindGenerationPG(test, fixture, base)
	}
	prepareLegacyFindPairPG(test, fixture, doc.Hash, base, count-2)
	return doc, base
}

func newLegacyFindGenerationPGStore(fixture docViewPGFixture) *legacyFindGenerationPGStore {
	return &legacyFindGenerationPGStore{PostgresStore: fixture.storage, pages: map[domain.ContentHash]int{}, bindings: map[domain.ContentHash]domain.DocReadGenerationBinding{}}
}

func TestPGLegacyFindGenerationSelectedMetadataDoesNotScaleWithHistory(test *testing.T) {
	for _, rootBase := range []bool{false, true} {
		var costs []legacyFindGenerationCosts
		for _, count := range []int{2048, 20480} {
			test.Run(fmt.Sprintf("root_base=%t/events=%d", rootBase, count), func(test *testing.T) {
				fixture := newDocViewPGFixture(test)
				doc, base := seedLegacyFindGenerationPairPG(test, fixture, count, rootBase)
				observed := newLegacyFindGenerationPGStore(fixture)
				fixture.service.blobs = observed
				ctx := outbound.WithDocReadOnly(fixture.ctx)
				before := legacyFindPGImage(test, fixture)
				request := domain.DocFindPageRequest{Legacy: true, Query: "needle", Base: base, From: count - 2, To: count, Total: count, Inherited: count - 2}
				first, err := fixture.service.FindDocPage(ctx, fixture.repo, doc.Hash, request)
				if err != nil || first.Projection != domain.DocLegacyFindProjection || first.Hash != doc.Hash || first.Generation != "" || first.NextCursor == nil || len(first.Matches) != domain.DocFindPageMatches {
					test.Fatal("prepared legacy find lost its immutable selection", first, err)
				}
				cost := observed.cost
				if cost.opens != 2 || cost.pages != 1 || observed.pages[base] != 0 || cost.directories != 0 || cost.boundaries != 1 || cost.manifests != 0 || cost.manifestBytes != 0 || cost.chunkLookups < 1 || cost.chunkLookups > 2 || cost.chunks < 1 || cost.chunks > 2 || cost.chunkBytes > 2*domain.ChunkTarget || cost.indexes != 0 || cost.whole != 0 {
					test.Fatal("warm find replayed the base or loaded full legacy metadata", cost, observed.pages)
				}
				costs = append(costs, cost)
				test.Logf("events=%d root_base=%t selected find port cost=%+v (directory SQL loads are adapter-internal)", count, rootBase, cost)
				matches := append([]domain.DocFindMatch(nil), first.Matches...)
				request = domain.DocFindPageRequest{Legacy: true, Query: "needle", Cursor: *first.NextCursor}
				for attempt := 0; attempt < 4; attempt++ {
					page, err := fixture.service.FindDocPage(ctx, fixture.repo, doc.Hash, request)
					if err != nil || page.Hash != doc.Hash || page.Generation != "" {
						test.Fatal("prepared legacy continuation changed identity", page, err)
					}
					matches = append(matches, page.Matches...)
					if page.NextCursor == nil {
						break
					}
					request.Cursor = *page.NextCursor
				}
				if len(matches) != 45 {
					test.Fatal("prepared continuation lost matches", len(matches))
				}
				match := matches[0]
				if match.Index != count-1 || match.SourceRange == nil || match.RevealCursor != "" {
					test.Fatal("prepared match invented a generation or lost source coordinates", match)
				}
				observed.cost = legacyFindGenerationCosts{}
				fragment, err := fixture.service.ReadDocFindFragment(ctx, fixture.repo, doc.Hash, domain.DocFindFragmentRequest{Legacy: true, Index: match.Index, Total: count, Offset: match.SourceRange.From, Budget: 64})
				if err != nil || fragment.Hash != doc.Hash || fragment.Generation != "" || fragment.Projection != domain.DocLegacyFindFragmentProjection || !strings.Contains(fragment.JSON, "Needle") || observed.cost.opens != 1 || observed.cost.pages != 1 || observed.cost.manifests != 0 || observed.cost.manifestBytes != 0 || observed.cost.chunkLookups < 1 || observed.cost.chunkLookups > 2 || observed.cost.chunks > 2 || observed.cost.indexes != 0 || observed.cost.whole != 0 {
					test.Fatal("prepared reveal lost original bytes or selected metadata bounds", fragment, observed.cost, err)
				}
				if after := legacyFindPGImage(test, fixture); before != after {
					test.Fatal("warm find/continuation/reveal mutated persisted source or projections")
				}
			})
		}
		if len(costs) == 2 && (costs[1].metadataBytes >= 2*costs[0].metadataBytes || costs[1].manifestBytes != 0 || costs[0].manifestBytes != 0) {
			test.Fatal("selected metadata scaled with history or the archival manifest cost was hidden", costs)
		}
	}
}

func TestPGLegacyFindGenerationSourceAndBasesFailClosed(test *testing.T) {
	for _, rootBase := range []bool{false, true} {
		for _, target := range []string{"source", "base"} {
			for _, mutation := range []string{"ownership", "descriptor corruption", "wrong hash", "wrong identity"} {
				test.Run(fmt.Sprintf("root_base=%t/%s/%s", rootBase, target, mutation), func(test *testing.T) {
					fixture := newDocViewPGFixture(test)
					doc, base := seedLegacyFindGenerationPairPG(test, fixture, 5, rootBase)
					observed := newLegacyFindGenerationPGStore(fixture)
					fixture.service.blobs = observed
					ctx := outbound.WithDocReadOnly(fixture.ctx)
					initial := domain.DocFindPageRequest{Legacy: true, Query: "needle", Base: base, From: 3, To: 5, Total: 5, Inherited: 3}
					first, err := fixture.service.FindDocPage(ctx, fixture.repo, doc.Hash, initial)
					if err != nil || first.NextCursor == nil {
						test.Fatal("missing warm continuation fixture", first, err)
					}
					continuation := domain.DocFindPageRequest{Legacy: true, Query: "needle", Cursor: *first.NextCursor}
					updated := doc.Hash
					if target == "base" {
						updated = base
					}
					want := domain.ErrIntegrity
					if mutation == "ownership" {
						want = domain.ErrNotFound
						_, err = fixture.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, updated)
					} else {
						var raw []byte
						if err := fixture.pool.QueryRow(fixture.ctx, `SELECT descriptor FROM doc_range_generations_v2 WHERE doc_hash=$1`, updated).Scan(&raw); err != nil {
							test.Fatal(err)
						}
						var generation domain.DocRangeGeneration
						if err := json.Unmarshal(raw, &generation); err != nil {
							test.Fatal(err)
						}
						switch mutation {
						case "descriptor corruption":
							raw = []byte("corrupt selected descriptor")
						case "wrong hash":
							generation.Hash = h('9')
							raw, err = json.Marshal(generation)
						case "wrong identity":
							if generation.Identity() == domain.DocumentIdentityLegacy {
								generation.Version, generation.SourceIdentity = 2, nil
								generation.ChunkPages = domain.DocRangeDirectoryRoot{Hash: h('8'), Count: 1}
							} else {
								identity := domain.DocumentIdentityLegacy
								generation.Version, generation.SourceIdentity = 3, &identity
								generation.ChunkPages = domain.DocRangeDirectoryRoot{}
							}
							raw, err = generation.Encode()
						}
						if err != nil {
							test.Fatal(err)
						}
						_, err = fixture.pool.Exec(fixture.ctx, `UPDATE doc_range_generations_v2 SET descriptor=$2,generation_hash=$3 WHERE doc_hash=$1`, updated, raw, domain.HashContent(raw))
					}
					if err != nil {
						test.Fatal(err)
					}
					for _, selected := range []domain.DocFindPageRequest{initial, continuation} {
						out, err := fixture.service.FindDocPage(ctx, fixture.repo, doc.Hash, selected)
						if !errors.Is(err, want) || !reflect.DeepEqual(out, domain.DocFindPage{}) || observed.cost.indexes != 0 || observed.cost.whole != 0 {
							test.Fatal("warm source/base failure was bypassed", out, observed.cost, err)
						}
					}
					if target == "source" {
						fragment, err := fixture.service.ReadDocFindFragment(ctx, fixture.repo, doc.Hash, domain.DocFindFragmentRequest{Legacy: true, Index: 4, Total: 5, Budget: 64})
						if !errors.Is(err, want) || fragment != (domain.DocFindFragmentPage{}) || observed.cost.indexes != 0 || observed.cost.whole != 0 {
							test.Fatal("warm source reveal bypassed a current failure", fragment, observed.cost, err)
						}
					}
				})
			}
		}
	}
}

func TestPGLegacyFindGenerationFreshBindingsAndSelectedBytes(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	doc := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{historyMessage(domain.RoleUser, strings.Repeat("needle ", 45)+string(fixture.repo))})
	prepareLegacyFindGenerationPG(test, fixture, doc.Hash)
	observed := newLegacyFindGenerationPGStore(fixture)
	fixture.service.blobs = observed
	initial := domain.DocFindPageRequest{Legacy: true, Query: "needle", To: 1, Total: 1}
	first, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, doc.Hash, initial)
	if err != nil || first.NextCursor == nil {
		test.Fatal(first, err)
	}
	previous := observed.bindings[doc.Hash]
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, doc.Hash); err != nil {
		test.Fatal(err)
	}
	fixture.service.blobs = fixture.storage
	prepareLegacyFindGenerationPG(test, fixture, doc.Hash)
	fixture.service.blobs = observed
	page, err := fixture.service.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, doc.Hash, domain.DocFindPageRequest{Legacy: true, Query: "needle", Cursor: *first.NextCursor})
	if err != nil || len(page.Matches) != domain.DocFindPageMatches || page.Generation != "" || page.Hash != doc.Hash || observed.bindings[doc.Hash] == previous || observed.cost.indexes != 0 || observed.cost.whole != 0 {
		test.Fatal("immutable legacy cursor reused a stale metadata binding", page, observed.bindings, observed.cost, err)
	}
	before := observed.cost
	denied := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		return bound, domain.ErrForbidden
	})
	if _, err := fixture.service.FindDocPage(denied, fixture.repo, doc.Hash, initial); !errors.Is(err, domain.ErrForbidden) || before != observed.cost {
		test.Fatal("prepared find reused old authorization", observed.cost, err)
	}
	manifest, err := fixture.storage.GetDocManifest(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, doc.Hash)
	if err != nil || len(manifest.Chunks) != 1 {
		test.Fatal("missing selected chunk fixture", manifest, err)
	}
	raw, err := fixture.storage.GetChunk(fixture.ctx, fixture.repo, manifest.Chunks[0])
	if err != nil {
		test.Fatal(err)
	}
	var storedBefore []byte
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT bytes FROM blobs WHERE hash=$1`, manifest.Chunks[0]).Scan(&storedBefore); err != nil {
		test.Fatal(err)
	}
	damaged := bytes.Clone(raw)
	damaged[0] ^= 1
	fixture.service.blobs = fixture.storage
	test.Run("selected chunk corruption", func(test *testing.T) {
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, manifest.Chunks[0], damaged); err != nil {
			test.Fatal(err)
		}
		test.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			restored, err := fixture.pool.Exec(cleanup, `UPDATE blobs SET bytes=$2 WHERE hash=$1 AND bytes=$3`, manifest.Chunks[0], storedBefore, damaged)
			if err != nil {
				test.Error("restore selected chunk", err)
			} else if restored.RowsAffected() != 1 {
				test.Error("selected chunk changed before fixture restoration")
			}
		})
		page, err := fixture.service.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, doc.Hash, initial)
		if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
			test.Fatal("warm find served corrupt current source bytes", page, err)
		}
		fragment, err := fixture.service.ReadDocFindFragment(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, doc.Hash, domain.DocFindFragmentRequest{Legacy: true, Index: 0, Total: 1, Budget: 64})
		if !errors.Is(err, domain.ErrIntegrity) || fragment != (domain.DocFindFragmentPage{}) {
			test.Fatal("warm reveal served corrupt current source bytes", fragment, err)
		}
	})
	var storedAfter []byte
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT bytes FROM blobs WHERE hash=$1`, manifest.Chunks[0]).Scan(&storedAfter); err != nil || !bytes.Equal(storedAfter, storedBefore) {
		test.Fatal("corruption fixture did not restore the exact stored representation", err)
	}
	peer := newDocViewPGFixture(test)
	reused := seedLegacyFindPGDocument(test, peer, doc.CIR.Events)
	reusedManifest, err := peer.storage.GetDocManifest(outbound.WithDocReadOnly(peer.ctx), peer.repo, reused.Hash)
	if err != nil || len(reusedManifest.Chunks) != 1 || reusedManifest.Chunks[0] != manifest.Chunks[0] {
		test.Fatal("restored corruption fixture could not safely reuse its chunk in another repository", reusedManifest, err)
	}
}
