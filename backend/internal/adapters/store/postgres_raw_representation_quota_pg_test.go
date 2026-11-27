//go:build postgres

package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func rawRepresentationQuotaBytesPG(test *testing.T, size int) []byte {
	test.Helper()
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		test.Fatal(err)
	}
	return body
}

func rawRepresentationQuotaUsagePG(test *testing.T, storage *PostgresStore, ctx context.Context, namespace string) domain.StorageUsage {
	test.Helper()
	usage, err := storage.ReadStorageUsage(ctx, namespace, time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		test.Fatal(err)
	}
	return usage
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

func TestPGLegacyManifestQuotaPreservesBodyFallback(test *testing.T) {
	for _, scope := range []string{"same-namespace", "shared-namespace"} {
		test.Run(scope, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			owner, namespace := rootPGRepo(test, ctx, storage)
			peer, peerNamespace := rootPGRepo(test, ctx, storage)
			document := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{{
				Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser,
				Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("retained archive ", 1024) + string(owner)}},
			}}}}
			raw, err := domain.CanonicalBytes(document.CIR)
			if err != nil {
				test.Fatal(err)
			}
			document.Hash = domain.HashContent(raw)
			plan, chunked := domain.PlanDocChunks(raw)
			if !chunked {
				test.Fatal("retained legacy fixture must admit a real chunk plan")
			}
			if _, err := storage.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, document.Hash, docCompress(raw)); err != nil {
				test.Fatal(err)
			}
			if _, err := storage.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$3),($2,'doc',$3)`, owner, peer, document.Hash); err != nil {
				test.Fatal(err)
			}
			if err := storage.ConfigureStoragePolicy(ctx, namespace, domain.NewID("quota_"), 0,
				domain.StoragePolicy{Plan: "free", IncludedBytes: 1}, "test-operator", "retain existing legacy body"); err != nil {
				test.Fatal(err)
			}
			if usage := rawRepresentationQuotaUsagePG(test, storage, ctx, namespace); usage.CurrentBytes <= 1 || usage.State != "read_only" {
				test.Fatal("retained legacy fixture must exceed its unchanged reduced cap", usage)
			}
			companion := []byte("ordinary growth remains rejected:" + string(owner))
			companionHash := domain.HashContent(companion)
			hashes := append([]domain.ContentHash{document.Hash, companionHash}, plan.Order...)
			image := func() string {
				var accounts string
				if err := storage.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(account) ORDER BY namespace_id)::text
 FROM storage_accounts account WHERE namespace_id=ANY($1::text[])`, []string{namespace, peerNamespace}).Scan(&accounts); err != nil {
					test.Fatal(err)
				}
				return accounts + representationOrdersBlobImagePG(test, storage, ctx, hashes) +
					rootPGImage(test, ctx, storage, owner, namespace) + rootPGImage(test, ctx, storage, peer, peerNamespace)
			}
			before := image()
			requester := owner
			if scope == "shared-namespace" {
				requester = peer
			}
			manifest, err := storage.GetDocManifest(ctx, requester, document.Hash)
			if !errors.Is(err, domain.ErrNotFound) || manifest.Format != "" || len(manifest.Envelope) != 0 || len(manifest.Chunks) != 0 {
				test.Fatal("quota-rejected lazy repack must expose only the existing body fallback", manifest, err)
			}
			if image() != before {
				test.Fatal("unavailable manifest left source, ownership, accounting, ledger, or index changes")
			}
			for _, reader := range []domain.ContentHash{owner, peer} {
				actual, err := storage.GetDoc(ctx, reader, document.Hash)
				if err != nil || actual.Hash != document.Hash {
					test.Fatal("quota blocked an existing document owner", reader, err)
				}
				canonical, err := domain.CanonicalBytes(actual.CIR)
				if err != nil || !bytes.Equal(canonical, raw) {
					test.Fatal("quota fallback changed original canonical bytes", reader, err)
				}
			}
			nestedPrepared := false
			err = storage.WithinRepository(ctx, requester, func(bound context.Context) error {
				manifest, err := storage.GetDocManifest(bound, requester, document.Hash)
				nestedPrepared = err == nil && sameDocManifest(manifest, plan.Manifest)
				return err
			})
			if !nestedPrepared || !errors.Is(err, domain.ErrStorageLimit) || image() != before {
				test.Fatal("borrowed transaction lost its outer quota failure or atomic rollback", nestedPrepared, err)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := storage.GetDocManifest(canceled, requester, document.Hash); !errors.Is(err, context.Canceled) || image() != before {
				test.Fatal("manifest fallback changed cancellation or retained effects", err)
			}
			foreign, _ := rootPGRepo(test, ctx, storage)
			if _, err := storage.GetDocManifest(ctx, foreign, document.Hash); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("manifest fallback exposed a foreign document", err)
			}
			if _, err := storage.GetDoc(ctx, foreign, document.Hash); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("legacy body fallback bypassed current ownership", err)
			}
			stored, deduped, err := storage.PutChunks(ctx, owner, map[domain.ContentHash][]byte{companionHash: companion})
			if !errors.Is(err, domain.ErrStorageLimit) || stored != 0 || deduped != 0 || image() != before {
				test.Fatal("legacy fallback exempted ordinary writes or retained partial effects", stored, deduped, err)
			}
		})
	}
}
