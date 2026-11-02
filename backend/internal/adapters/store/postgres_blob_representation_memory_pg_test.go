//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type blobRepresentationMemoryFixturePG struct {
	digest domain.MemoryDigest
	hash   domain.ContentHash
	raw    []byte
	plan   domain.MemoryChunkPlan
}

func blobRepresentationMemoryPG(test *testing.T, snapshot domain.ContentHash, format string) blobRepresentationMemoryFixturePG {
	test.Helper()
	summaryUnit := "memory-summary:" + string(snapshot) + ":"
	fragmentUnit := "memory-fragment:" + string(snapshot) + ":"
	digest := domain.MemoryDigest{
		SnapshotID: snapshot,
		Summary:    strings.Repeat(summaryUnit, 1+(256<<10)/len(summaryUnit)),
		KeyFacts:   []string{"retain the complete wire identity", "résumé <facts> & \"quotes\""},
		OpenTasks:  []string{"preserve raw ownership", "preserve memory readability"},
		Provider:   domain.ProviderCodex,
		Fragments: []domain.MemoryFragment{
			{
				SourceSnapshot:     domain.HashContent([]byte("fragment-source:" + string(snapshot))),
				Summary:            strings.Repeat(fragmentUnit, 1+(256<<10)/len(fragmentUnit)),
				KeyFacts:           []string{"fragment facts remain scoped"},
				OpenTasks:          []string{"retain authoritative fragment tasks"},
				TasksAuthoritative: true,
			},
			{
				SourceSnapshot: domain.HashContent([]byte("second-source:" + string(snapshot))),
				Summary:        "second fragment\nwith an escaped \"summary\"",
			},
		},
	}
	if format != domain.MemoryChunkFormatV1 {
		digest.GraftCoverage = &domain.MemoryGraftCoverage{
			ProjectionVersion:  domain.MemoryProjectionVersion,
			ProjectionComplete: true,
			LineageFingerprint: domain.HashContent([]byte("lineage:" + string(snapshot))),
			GraftSeq:           7,
			GraftParents:       []domain.ContentHash{domain.HashContent([]byte("graft-parent:" + string(snapshot)))},
			PinnedSources:      []domain.ContentHash{digest.Fragments[0].SourceSnapshot},
		}
	}
	if format == domain.MemoryChunkFormatV3 || format == domain.MemoryChunkFormatV4 {
		digest.PreviousMemoryHash = domain.HashContent([]byte("previous-memory:" + string(snapshot)))
	}
	if format == domain.MemoryChunkFormatV4 {
		digest.ClaimsVersion = domain.MemoryClaimsVersion
		digest.Fragments[0].Claims = []domain.MemoryClaim{
			{Kind: "decision", Text: "Retain compatible raw bytes while another kind owns them."},
			{Kind: "rationale", Text: "A memory manifest is not the original portable chunk body."},
			{
				Kind: "code", Text: "Validate the current stored representation before granting raw ownership.",
				Code: &domain.MemoryCodeScope{
					Commit: strings.Repeat("a", 40), Parent: strings.Repeat("b", 40),
					Paths: []string{"backend/internal/adapters/store/postgres_store.go"},
				},
			},
		}
	}
	if err := validateMemoryDigestRefs(digest); err != nil {
		test.Fatal("invalid memory reference fixture", err)
	}
	if err := digest.ValidateMemoryClaims(); err != nil {
		test.Fatal("invalid memory claim fixture", err)
	}
	raw, err := json.Marshal(digest)
	if err != nil {
		test.Fatal(err)
	}
	hash, err := domain.MemoryDigestHash(digest)
	if err != nil || domain.HashContent(raw) != hash {
		test.Fatal("wire JSON must have the actual MemoryDigestHash", err)
	}
	if len(raw) < 512<<10 || len(raw) > domain.MaxPortableChunkBytes {
		test.Fatal("memory wire must exceed the chunking threshold and fit one legitimate portable raw chunk", len(raw))
	}
	plan, chunked, err := domain.PlanMemoryChunks(digest)
	if err != nil || !chunked || plan.Manifest.Format != format || len(plan.Manifest.SummaryChunks) < 2 || len(plan.Manifest.FragmentChunks) < 2 {
		test.Fatal("fixture must use the requested fragmented memory format", format, chunked, err)
	}
	assembled, err := domain.AssembleMemoryChunks(plan.Manifest, plan.Bodies)
	if err != nil || !reflect.DeepEqual(assembled, digest) {
		test.Fatal("memory component planning changed the complete digest", err)
	}
	return blobRepresentationMemoryFixturePG{digest: digest, hash: hash, raw: raw, plan: plan}
}

func assertBlobRepresentationMemoryPG(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, fixture blobRepresentationMemoryFixturePG) {
	test.Helper()
	digest, err := storage.GetMemory(ctx, repo, fixture.hash)
	if err != nil || !reflect.DeepEqual(digest, fixture.digest) {
		test.Fatal("cross-kind reuse changed memory summary, facts, tasks, provider, fragments, graft coverage, causal parent, or claims", err)
	}
	raw, err := json.Marshal(digest)
	if err != nil || !bytes.Equal(raw, fixture.raw) {
		test.Fatal("cross-kind reuse changed the original memory wire bytes", err)
	}
	hash, err := domain.MemoryDigestHash(digest)
	if err != nil || hash != fixture.hash {
		test.Fatal("cross-kind reuse changed MemoryDigestHash", hash, err)
	}
	captured := memoryWitnessPGCapture(test, storage, ctx, repo, fixture.hash)
	if captured.Digest == nil || !reflect.DeepEqual(*captured.Digest, fixture.digest) {
		test.Fatal("bounded memory capture lost the shared representation")
	}
}

func putBlobRepresentationRawPG(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, fixture blobRepresentationMemoryFixturePG, replay bool) {
	test.Helper()
	wantStored, wantDeduped := 1, 0
	if replay {
		wantStored, wantDeduped = 0, 1
	}
	stored, deduped, err := storage.PutChunks(ctx, repo, map[domain.ContentHash][]byte{fixture.hash: fixture.raw})
	if err != nil || stored != wantStored || deduped != wantDeduped {
		test.Fatal("valid portable raw grant disagreed with the shared memory representation", stored, deduped, err)
	}
	raw, err := storage.GetChunk(ctx, repo, fixture.hash)
	if err != nil || !bytes.Equal(raw, fixture.raw) || domain.HashContent(raw) != fixture.hash {
		test.Fatal("raw chunk owner did not receive the original content-addressed wire bytes", err)
	}
}

func TestPGBlobRepresentationMemoryRawContentBothOrders(test *testing.T) {
	for _, format := range []string{domain.MemoryChunkFormatV1, domain.MemoryChunkFormatV2, domain.MemoryChunkFormatV3, domain.MemoryChunkFormatV4} {
		test.Run(format, func(test *testing.T) {
			for _, order := range []string{"memory-first", "raw-first"} {
				test.Run(order, func(test *testing.T) {
					storage, ctx, memoryRepo := catalogPG(test)
					peerRepo := memorySourceRepoPG(test, ctx, storage, "peer-memory")
					rawRepo := memorySourceRepoPG(test, ctx, storage, "raw-owner")
					fixture := blobRepresentationMemoryPG(test, memoryRepo, format)
					var original rootSourceWitnessStatePG
					if order == "raw-first" {
						putBlobRepresentationRawPG(test, storage, ctx, rawRepo, fixture, false)
						original = readRootSourceWitnessStatePG(test, storage, ctx, fixture.hash)
					}
					for _, owner := range []domain.ContentHash{memoryRepo, peerRepo} {
						hash, err := storage.PutMemory(ctx, owner, fixture.digest)
						if err != nil || hash != fixture.hash {
							test.Fatal("valid PutMemory failed at a shared raw-content hash", hash, err)
						}
						assertBlobRepresentationMemoryPG(test, storage, ctx, owner, fixture)
						if order == "raw-first" {
							assertRootSourceWitnessStatePG(test, storage, ctx, fixture.hash, original)
						}
					}
					if order == "memory-first" {
						original = readRootSourceWitnessStatePG(test, storage, ctx, fixture.hash)
						decoded, err := docDecompress(original.body)
						if err != nil {
							test.Fatal(err)
						}
						manifest, recognized, err := domain.ParseMemoryChunkManifest(decoded)
						if err != nil || !recognized || manifest.Format != format || !reflect.DeepEqual(manifest, fixture.plan.Manifest) {
							test.Fatal("PutMemory must first store the real requested component manifest", recognized, err)
						}
						putBlobRepresentationRawPG(test, storage, ctx, rawRepo, fixture, false)
						converted := readRootSourceWitnessStatePG(test, storage, ctx, fixture.hash)
						decoded, err = docDecompress(converted.body)
						if err != nil || !bytes.Equal(decoded, fixture.raw) || converted.version != original.version+1 || converted.incarnation != original.incarnation {
							test.Fatal("raw grant must restore compatible original bytes with one source change and no new incarnation", err)
						}
					}
					for _, owner := range []domain.ContentHash{memoryRepo, peerRepo} {
						assertBlobRepresentationMemoryPG(test, storage, ctx, owner, fixture)
					}
					if _, err := storage.GetMemory(ctx, rawRepo, fixture.hash); !errors.Is(err, domain.ErrNotFound) {
						test.Fatal("raw grant leaked memory-kind ownership", err)
					}
					if _, err := storage.GetChunk(ctx, memoryRepo, fixture.hash); !errors.Is(err, domain.ErrNotFound) {
						test.Fatal("memory grant leaked raw-kind ownership", err)
					}
					source := readRootSourceWitnessStatePG(test, storage, ctx, fixture.hash)
					hashes := []domain.ContentHash{fixture.hash}
					for component := range fixture.plan.Bodies {
						hashes = append(hashes, component)
					}
					before := blobRepresentationMemoryImagePG(test, storage, ctx, rawRepo, hashes)
					generations := make(map[domain.ContentHash]domain.RepositoryMemorySourceGeneration)
					for _, owner := range []domain.ContentHash{memoryRepo, peerRepo, rawRepo} {
						generations[owner] = memorySourceGenerationPG(test, ctx, storage, owner)
					}
					for range 2 {
						for _, owner := range []domain.ContentHash{memoryRepo, peerRepo} {
							hash, err := storage.PutMemory(ctx, owner, fixture.digest)
							if err != nil || hash != fixture.hash {
								test.Fatal("memory replay changed the shared hash", hash, err)
							}
							assertBlobRepresentationMemoryPG(test, storage, ctx, owner, fixture)
						}
						putBlobRepresentationRawPG(test, storage, ctx, rawRepo, fixture, true)
						assertRootSourceWitnessStatePG(test, storage, ctx, fixture.hash, source)
						if after := blobRepresentationMemoryImagePG(test, storage, ctx, rawRepo, hashes); after != before {
							test.Fatal("replay changed shared source bytes, generations, incarnations, or ownership")
						}
						for _, generation := range generations {
							memorySourceAdvancePG(test, ctx, storage, generation, 0)
						}
					}
				})
			}
		})
	}
}

func blobRepresentationMemoryImagePG(test *testing.T, storage *PostgresStore, ctx context.Context, rawRepo domain.ContentHash, hashes []domain.ContentHash) string {
	test.Helper()
	var image string
	if err := storage.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'blobs',(SELECT jsonb_agg(jsonb_build_array(body.hash,md5(body.bytes),body.doc_range_source_version,body.xmin::text,source.incarnation::text) ORDER BY body.hash)
 FROM blobs body LEFT JOIN blob_source_incarnations source ON source.hash=body.hash WHERE body.hash=ANY($1::text[])),
 'grants',(SELECT jsonb_agg(jsonb_build_array(repo_id,kind,hash,xmin::text) ORDER BY repo_id,kind,hash)
 FROM repo_blobs WHERE hash=ANY($1::text[]) OR repo_id=$2))::text`, strs(hashes), string(rawRepo)).Scan(&image); err != nil {
		test.Fatal("read representation rollback image", err)
	}
	return image
}

func TestPGBlobRepresentationMemoryRawGrantRejectsBrokenManifest(test *testing.T) {
	for _, fault := range []string{"missing-summary", "missing-fragment", "corrupt-summary", "corrupt-fragment", "manifest-hash", "manifest-format"} {
		test.Run(fault, func(test *testing.T) {
			storage, ctx, memoryRepo := catalogPG(test)
			peerRepo := memorySourceRepoPG(test, ctx, storage, "broken-peer-memory")
			rawRepo := memorySourceRepoPG(test, ctx, storage, "rejected-raw-owner")
			fixture := blobRepresentationMemoryPG(test, memoryRepo, domain.MemoryChunkFormatV4)
			for _, owner := range []domain.ContentHash{memoryRepo, peerRepo} {
				if hash, err := storage.PutMemory(ctx, owner, fixture.digest); err != nil || hash != fixture.hash {
					test.Fatal("create valid memory before fault injection", hash, err)
				}
				assertBlobRepresentationMemoryPG(test, storage, ctx, owner, fixture)
			}
			original := readRootSourceWitnessStatePG(test, storage, ctx, fixture.hash)
			decoded, err := docDecompress(original.body)
			if err != nil {
				test.Fatal(err)
			}
			manifest, recognized, err := domain.ParseMemoryChunkManifest(decoded)
			if err != nil || !recognized || manifest.Format != domain.MemoryChunkFormatV4 {
				test.Fatal("fault fixture must start from a real stored memory manifest", recognized, err)
			}
			component := manifest.SummaryChunks[len(manifest.SummaryChunks)-1]
			if strings.HasSuffix(fault, "fragment") {
				component = manifest.FragmentChunks[len(manifest.FragmentChunks)-1]
			}
			switch {
			case strings.HasPrefix(fault, "missing-"):
				transaction := catalogTxPG(test, ctx, storage)
				catalogExecPG(test, ctx, transaction, `DELETE FROM repo_blobs WHERE hash=$1`, string(component))
				catalogExecPG(test, ctx, transaction, `DELETE FROM blobs WHERE hash=$1`, string(component))
				if err := transaction.Commit(ctx); err != nil {
					test.Fatal(err)
				}
			case strings.HasPrefix(fault, "corrupt-"):
				catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(component), docCompress([]byte("corrupt-memory-component:"+string(memoryRepo))))
			case fault == "manifest-hash":
				manifest.OpenTasks = []string{"substituted task must not be hidden by the incoming original raw body"}
				catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(fixture.hash), memoryCaptureTestJSON(test, manifest))
			case fault == "manifest-format":
				manifest.Format = "cxt-memory-chunks-v99"
				catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(fixture.hash), memoryCaptureTestJSON(test, manifest))
			}
			for _, owner := range []domain.ContentHash{memoryRepo, peerRepo} {
				digest, err := storage.GetMemory(ctx, owner, fixture.hash)
				if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(digest, domain.MemoryDigest{}) {
					test.Fatal("broken stored manifest must fail closed before raw reuse", err)
				}
			}
			companion := []byte("valid-companion:" + string(memoryRepo))
			companionHash := domain.HashContent(companion)
			hashes := []domain.ContentHash{fixture.hash, companionHash}
			for component := range fixture.plan.Bodies {
				hashes = append(hashes, component)
			}
			before := blobRepresentationMemoryImagePG(test, storage, ctx, rawRepo, hashes)
			generations := make(map[domain.ContentHash]domain.RepositoryMemorySourceGeneration)
			for _, owner := range []domain.ContentHash{memoryRepo, peerRepo, rawRepo} {
				generations[owner] = memorySourceGenerationPG(test, ctx, storage, owner)
			}
			for range 2 {
				stored, deduped, err := storage.PutChunks(ctx, rawRepo, map[domain.ContentHash][]byte{
					fixture.hash:  fixture.raw,
					companionHash: companion,
				})
				if !errors.Is(err, domain.ErrIntegrity) || stored != 0 || deduped != 0 {
					test.Fatal("raw reuse must validate current manifest components instead of repairing from the incoming raw body", stored, deduped, err)
				}
				if after := blobRepresentationMemoryImagePG(test, storage, ctx, rawRepo, hashes); after != before {
					test.Fatal("failed raw grant changed source bytes, generations, incarnations, ownership, or a companion blob")
				}
				for _, hash := range []domain.ContentHash{fixture.hash, companionHash} {
					if raw, err := storage.GetChunk(ctx, rawRepo, hash); !errors.Is(err, domain.ErrNotFound) || raw != nil {
						test.Fatal("failed raw batch left a partial ownership grant", hash, err)
					}
				}
				for _, generation := range generations {
					memorySourceAdvancePG(test, ctx, storage, generation, 0)
				}
			}
		})
	}
}
