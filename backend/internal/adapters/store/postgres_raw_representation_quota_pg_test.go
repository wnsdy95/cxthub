//go:build postgres

package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type rawRepresentationQuotaFixturePG struct {
	hash           domain.ContentHash
	raw            []byte
	assertReadable func()
}

func rawRepresentationQuotaBytesPG(test *testing.T, size int) []byte {
	test.Helper()
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		test.Fatal(err)
	}
	return body
}

func rawRepresentationQuotaObjectPG(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, kind string, ownerGrowth bool) rawRepresentationQuotaFixturePG {
	test.Helper()
	if kind == "document" {
		text := strings.Repeat("retained quota document "+string(repo), 4096)
		if ownerGrowth {
			text = hex.EncodeToString(rawRepresentationQuotaBytesPG(test, 32<<10))
		}
		fixture := uniqueRootPublicationFixturePG(test, text)
		document := domain.SessionDoc{Hash: domain.HashContent(fixture.canonical), CIR: fixture.cir}
		if _, err := storage.PutDoc(ctx, repo, document); err != nil {
			test.Fatal("publish the original document", err)
		}
		manifest, err := storage.GetDocManifest(ctx, repo, document.Hash)
		if err != nil {
			test.Fatal(err)
		}
		return rawRepresentationQuotaFixturePG{
			hash: document.Hash,
			raw:  fixture.canonical,
			assertReadable: func() {
				assertRepresentationOrdersManifestPG(test, storage, ctx, repo, document, manifest)
			},
		}
	}
	fixture := blobRepresentationMemoryPG(test, repo, domain.MemoryChunkFormatV4)
	if ownerGrowth {
		fixture.digest.Summary = hex.EncodeToString(rawRepresentationQuotaBytesPG(test, 64<<10))
		var err error
		fixture.raw, err = json.Marshal(fixture.digest)
		if err != nil {
			test.Fatal(err)
		}
		fixture.hash, err = domain.MemoryDigestHash(fixture.digest)
		if err != nil {
			test.Fatal(err)
		}
		var chunked bool
		fixture.plan, chunked, err = domain.PlanMemoryChunks(fixture.digest)
		if err != nil || !chunked || fixture.plan.Manifest.Format != domain.MemoryChunkFormatV4 {
			test.Fatal("memory growth fixture must retain its real component manifest", chunked, err)
		}
	}
	if hash, err := storage.PutMemory(ctx, repo, fixture.digest); err != nil || hash != fixture.hash {
		test.Fatal("publish the original memory", hash, err)
	}
	return rawRepresentationQuotaFixturePG{
		hash: fixture.hash,
		raw:  fixture.raw,
		assertReadable: func() {
			assertBlobRepresentationMemoryPG(test, storage, ctx, repo, fixture)
		},
	}
}

func rawRepresentationQuotaUsagePG(test *testing.T, storage *PostgresStore, ctx context.Context, namespace string) domain.StorageUsage {
	test.Helper()
	usage, err := storage.ReadStorageUsage(ctx, namespace, time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		test.Fatal(err)
	}
	return usage
}

func TestPGRawRepresentationQuotaBatchAtomic(test *testing.T) {
	for _, kind := range []string{"document", "memory"} {
		for _, scope := range []string{"same-repo-companion-growth", "cross-namespace-existing-owner-growth"} {
			test.Run(kind+"/"+scope, func(test *testing.T) {
				storage, ctx := chunkReusePG(test)
				ownerRepo, ownerNamespace := rootPGRepo(test, ctx, storage)
				ownerGrowth := scope == "cross-namespace-existing-owner-growth"
				fixture := rawRepresentationQuotaObjectPG(test, storage, ctx, ownerRepo, kind, ownerGrowth)
				fixture.assertReadable()
				source := readRootSourceWitnessStatePG(test, storage, ctx, fixture.hash)
				decoded, err := docDecompress(source.body)
				if err != nil {
					test.Fatal(err)
				}
				if kind == "document" {
					if _, recognized := domain.ParseDocChunkManifest(decoded); !recognized {
						test.Fatal("quota fixture must start from an actual stored document manifest")
					}
				} else if _, recognized, err := domain.ParseMemoryChunkManifest(decoded); err != nil || !recognized {
					test.Fatal("quota fixture must start from an actual stored memory manifest", err)
				}
				compressedRaw := docCompress(fixture.raw)
				if len(compressedRaw) == len(source.body) || ownerGrowth && len(compressedRaw) <= len(source.body) {
					test.Fatal("raw promotion must change the metered representation size")
				}
				rawRepo, rawNamespace := ownerRepo, ownerNamespace
				usage := rawRepresentationQuotaUsagePG(test, storage, ctx, ownerNamespace)
				capBytes := usage.CurrentBytes
				if ownerGrowth {
					rawRepo, rawNamespace = rootPGRepo(test, ctx, storage)
				} else {
					if len(compressedRaw) > 16<<10 {
						test.Fatal("promotion alone must fit the companion case's small allowance")
					}
					capBytes += 16 << 10
				}
				if err := storage.ConfigureStoragePolicy(ctx, ownerNamespace, domain.NewID("quota_"), 0,
					domain.StoragePolicy{Plan: "free", IncludedBytes: capBytes}, "test-operator", "enforce raw promotion quota"); err != nil {
					test.Fatal(err)
				}
				companion := rawRepresentationQuotaBytesPG(test, 128<<10)
				companionHash := domain.HashContent(companion)
				if !ownerGrowth && int64(len(docCompress(companion))) <= capBytes {
					test.Fatal("the unrelated new chunk must exceed the enforced namespace cap")
				}
				hashes := []domain.ContentHash{fixture.hash, companionHash}
				image := func() string {
					var accounts string
					if err := storage.pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(account) ORDER BY namespace_id),'[]'::jsonb)::text
 FROM storage_accounts account WHERE namespace_id=ANY($1::text[])`, []string{ownerNamespace, rawNamespace}).Scan(&accounts); err != nil {
						test.Fatal(err)
					}
					return accounts + representationOrdersBlobImagePG(test, storage, ctx, hashes) +
						rootPGImage(test, ctx, storage, ownerRepo, ownerNamespace) +
						rootPGImage(test, ctx, storage, rawRepo, rawNamespace)
				}
				before := image()
				stored, deduped, err := storage.PutChunks(ctx, rawRepo, map[domain.ContentHash][]byte{
					fixture.hash:  fixture.raw,
					companionHash: companion,
				})
				if !errors.Is(err, domain.ErrStorageLimit) || stored != 0 || deduped != 0 {
					test.Fatal("ordinary raw promotion exempted namespace growth from its real quota", stored, deduped, err)
				}
				if after := image(); after != before {
					test.Fatal("quota rejection changed raw bytes, source identity, ownership, accounting, ledger, indexes, or receipts")
				}
				assertRootSourceWitnessStatePG(test, storage, ctx, fixture.hash, source)
				if owned, err := storage.HasChunks(ctx, rawRepo, hashes); err != nil || len(owned) != 0 {
					test.Fatal("quota rejection leaked a raw ownership grant", owned, err)
				}
				for _, hash := range hashes {
					if body, err := storage.GetChunk(ctx, rawRepo, hash); !errors.Is(err, domain.ErrNotFound) || body != nil {
						test.Fatal("quota rejection exposed partial raw content", hash, err)
					}
				}
				fixture.assertReadable()
				if ownerGrowth {
					return
				}
				small := []byte("within-cap:" + string(ownerRepo))
				smallHash := domain.HashContent(small)
				stored, deduped, err = storage.PutChunks(ctx, rawRepo, map[domain.ContentHash][]byte{fixture.hash: fixture.raw, smallHash: small})
				if err != nil || stored != 2 || deduped != 0 {
					test.Fatal("a within-cap promotion and companion must succeed after rollback", stored, deduped, err)
				}
				for hash, expected := range map[domain.ContentHash][]byte{fixture.hash: fixture.raw, smallHash: small} {
					if body, err := storage.GetChunk(ctx, rawRepo, hash); err != nil || !bytes.Equal(body, expected) {
						test.Fatal("successful within-cap raw content is unreadable", hash, err)
					}
				}
				if usage := rawRepresentationQuotaUsagePG(test, storage, ctx, ownerNamespace); usage.CurrentBytes > capBytes || usage.State != "active" {
					test.Fatal("within-cap promotion exceeded the actual storage policy", usage)
				}
				var bindings int
				if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM storage_usage_ledger
 WHERE namespace_id=$1 AND object_key=$2 AND reason='payload.raw-binding'`, ownerNamespace, "blob:"+string(fixture.hash)).Scan(&bindings); err != nil || bindings != 1 {
					test.Fatal("ordinary representation changes must retain their non-reconciliation reason", bindings, err)
				}
				fixture.assertReadable()
			})
		}
	}
}

func TestPGRawRepresentationQuotaExplicitReconciliation(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	repo, namespace := rootPGRepo(test, ctx, storage)
	body := rawRepresentationQuotaBytesPG(test, 1024)
	hash := domain.HashContent(body)
	if _, _, err := storage.PutChunks(ctx, repo, map[domain.ContentHash][]byte{hash: body}); err != nil {
		test.Fatal(err)
	}
	original := rawRepresentationQuotaUsagePG(test, storage, ctx, namespace)
	if original.CurrentBytes <= 1 {
		test.Fatal("reconciliation fixture must retain more than the reduced cap")
	}
	if err := storage.ConfigureStoragePolicy(ctx, namespace, domain.NewID("quota_"), 0,
		domain.StoragePolicy{Plan: "free", IncludedBytes: 1}, "test-operator", "retain existing bytes above a reduced cap"); err != nil {
		test.Fatal(err)
	}
	transaction := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, transaction, `UPDATE storage_usage_objects SET bytes=1 WHERE namespace_id=$1`, namespace)
	catalogExecPG(test, ctx, transaction, `UPDATE storage_accounts SET current_bytes=1 WHERE namespace_id=$1`, namespace)
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if err := storage.ReconcileStorageUsage(ctx, namespace); err != nil {
		test.Fatal("explicit operator reconciliation must repair undercounted retained bytes above the cap", err)
	}
	repaired := rawRepresentationQuotaUsagePG(test, storage, ctx, namespace)
	if repaired.CurrentBytes != original.CurrentBytes || repaired.State != "read_only" || len(repaired.Entries) == 0 || repaired.Entries[0].Reason != "reconcile" {
		test.Fatal("explicit reconciliation lost its corrective accounting semantics", repaired)
	}
	if err := storage.ReconcileStorageUsage(ctx, namespace); err != nil {
		test.Fatal(err)
	}
	if replay := rawRepresentationQuotaUsagePG(test, storage, ctx, namespace); replay.CurrentBytes != repaired.CurrentBytes || len(replay.Entries) != len(repaired.Entries) {
		test.Fatal("explicit reconciliation appended duplicate corrections", replay)
	}
	companion := []byte("not-an-operator-reconciliation:" + string(repo))
	companionHash := domain.HashContent(companion)
	hashes := []domain.ContentHash{hash, companionHash}
	before := rootPGImage(test, ctx, storage, repo, namespace) + representationOrdersBlobImagePG(test, storage, ctx, hashes)
	stored, deduped, err := storage.PutChunks(ctx, repo, map[domain.ContentHash][]byte{companionHash: companion})
	if !errors.Is(err, domain.ErrStorageLimit) || stored != 0 || deduped != 0 {
		test.Fatal("explicit reconciliation exempted a later ordinary upload", stored, deduped, err)
	}
	if after := rootPGImage(test, ctx, storage, repo, namespace) + representationOrdersBlobImagePG(test, storage, ctx, hashes); after != before {
		test.Fatal("rejected post-reconciliation growth left partial changes")
	}
	if actual, err := storage.GetChunk(ctx, repo, hash); err != nil || !bytes.Equal(actual, body) {
		test.Fatal("reconciliation or quota rejection made retained content unreadable", err)
	}
}
