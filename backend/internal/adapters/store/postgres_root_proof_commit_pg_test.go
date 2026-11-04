//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func rootProofCommitFixturePG(test *testing.T) rootReaderFixture {
	test.Helper()
	nonce := test.Name() + time.Now().String()
	first := strings.Repeat(nonce+"a", 1+domain.ConversationManifestChunkBytes/(len(nonce)+1))[:domain.ConversationManifestChunkBytes]
	second := strings.Repeat(nonce+"b", 1+domain.ConversationManifestChunkBytes/(len(nonce)+1))[:domain.ConversationManifestChunkBytes]
	fixture := uniqueRootPublicationFixturePG(test, first+second)
	if len(fixture.bodies) != 3 || len(fixture.manifest.Chunks) != 3 {
		test.Fatal("proof commit fixture must contain three distinct dependencies", len(fixture.bodies), len(fixture.manifest.Chunks))
	}
	return fixture
}

func rootProofCommitSourcesPG(test *testing.T, storage *PostgresStore, ctx context.Context, fixture rootReaderFixture) map[domain.ContentHash]rootSourceWitnessStatePG {
	test.Helper()
	sources := make(map[domain.ContentHash]rootSourceWitnessStatePG, len(fixture.bodies))
	for hash := range fixture.bodies {
		sources[hash] = readRootSourceWitnessStatePG(test, storage, ctx, hash)
	}
	return sources
}

func assertRootProofCommitWitnessPG(test *testing.T, publication outbound.PreparedRootDocPublication, job domain.DocFinalizationJob, sources map[domain.ContentHash]rootSourceWitnessStatePG) {
	test.Helper()
	prepared, ok := publication.(*pgStreamingRootPublication)
	if !ok || prepared == nil {
		test.Fatal("real root preparation did not return its streaming publication")
	}
	if err := prepared.source.matches(job, prepared.root.canonical); err != nil || len(prepared.source.chunks) != len(sources) {
		test.Fatal("preparation did not bind the complete source generation witness", err)
	}
	for hash, source := range sources {
		captured, exists := prepared.source.dependency(hash)
		if !exists || captured.hash != hash || captured.version != source.version || captured.incarnation != source.incarnation {
			test.Fatal("preparation did not capture the exact dependency version and incarnation", hash, captured)
		}
	}
}

func assertRootProofCommitSourceMetadataPG(test *testing.T, trace *rootPublicationBatchTracePG) {
	test.Helper()
	if trace == nil {
		test.Fatal("final publication did not enter its observed repository transaction")
	}
	for _, query := range trace.queries {
		if query.payload {
			continue
		}
		if !strings.Contains(query.sql, "b.doc_range_source_version") ||
			!strings.Contains(query.sql, "incarnation.incarnation::text") ||
			!strings.Contains(query.sql, "FOR SHARE OF rb,b,incarnation") {
			test.Fatal("final retention omitted source metadata or a required dependency lock", query.sql)
		}
	}
}

func TestPGRootProofCommitMatchingGenerationSkipsDependencyPayload(test *testing.T) {
	for _, scenario := range []string{"empty", "three-distinct-dependencies"} {
		test.Run(scenario, func(test *testing.T) {
			directory := rangeSpoolTempDir(test)
			storage, ctx := chunkReusePG(test)
			ctx = rootPublicationContext(ctx)
			repo, namespace := rootPGRepo(test, ctx, storage)
			var fixture rootReaderFixture
			if scenario == "empty" {
				fixture = uniqueRootPublicationFixturePG(test, "")
			} else {
				fixture = rootProofCommitFixturePG(test)
			}
			job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, repo)
			sources := rootProofCommitSourcesPG(test, storage, ctx, fixture)
			before := rootPGImage(test, ctx, storage, repo, namespace)
			publication := prepareRootStreamingPG(test, storage, ctx, job, directory)
			assertRootProofCommitWitnessPG(test, publication, job, sources)
			assertRootStreamingUnpublishedPG(test, storage, ctx, job)
			if after := rootPGImage(test, ctx, storage, repo, namespace); after != before {
				test.Fatal("preparation changed archive, ownership, accounting, indexes, or receipt")
			}
			if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
				traced, trace := rootPublicationBatchTraceContextPG(bound)
				if err := publication.Complete(traced, job, time.Now()); err != nil {
					return err
				}
				assertRootProofCommitSourceMetadataPG(test, trace)
				assertRootPublicationBatchQueriesPG(test, trace, conversationChunkOrder(fixture.manifest), 0)
				current, err := storage.GetDocJob(bound, repo, job.ID)
				if err != nil || current.State != "completed" || current.DocumentRef() != job.DocumentRef() || current.Version != job.Version || !current.LeaseUntil.IsZero() {
					test.Fatal("metadata-only finalization did not stage its fenced completion receipt", current, err)
				}
				assertRootStreamingUnpublishedPG(test, storage, ctx, job)
				if len(fixture.manifest.Chunks) != 0 {
					assertRootPublicationBatchLockedPG(test, storage, ctx, repo, fixture.manifest.Chunks[len(fixture.manifest.Chunks)-1].Hash)
				}
				return nil
			}); err != nil {
				test.Fatal("matching source generation did not commit", err)
			}
			assertRootSourceWitnessPublishedPG(test, storage, ctx, job, fixture)
			for hash, source := range sources {
				assertRootSourceWitnessStatePG(test, storage, ctx, hash, source)
			}
			replay, err := storage.EnqueueDocJob(ctx, rootPublicationJob(test, repo, fixture))
			if err != nil || replay.State != "completed" || replay.DocumentRef() != job.DocumentRef() || replay.Version != job.Version {
				test.Fatal("metadata-only publication lost its durable completion receipt", replay, err)
			}
			if err := publication.Close(); err != nil {
				test.Fatal(err)
			}
			assertRangeSpoolsRemoved(test, directory)
		})
	}
}

func TestPGRootProofCommitChangedGenerationPreservesCurrentByteFallback(test *testing.T) {
	for _, scenario := range []struct {
		name string
		want error
	}{
		{name: "identical-byte-update", want: domain.ErrConflict},
		{name: "recompressed-current-body", want: domain.ErrConflict},
		{name: "same-hash-version-zero-new-incarnation", want: domain.ErrConflict},
		{name: "corrupt-changed-body", want: domain.ErrIntegrity},
		{name: "valid-change-before-other-corruption", want: domain.ErrIntegrity},
		{name: "missing-grant", want: domain.ErrNotFound},
		{name: "missing-grant-and-corrupt-generation", want: domain.ErrNotFound},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			directory := rangeSpoolTempDir(test)
			storage, ctx := chunkReusePG(test)
			ctx = rootPublicationContext(ctx)
			repo, namespace := rootPGRepo(test, ctx, storage)
			fixture := rootProofCommitFixturePG(test)
			job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, repo)
			original := rootProofCommitSourcesPG(test, storage, ctx, fixture)
			publication := prepareRootStreamingPG(test, storage, ctx, job, directory)
			assertRootProofCommitWitnessPG(test, publication, job, original)
			hash := fixture.manifest.Chunks[len(fixture.manifest.Chunks)-1].Hash
			switch scenario.name {
			case "identical-byte-update":
				if tag, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash); err != nil || tag.RowsAffected() != 1 {
					test.Fatal("advance source generation without changing valid bytes", err)
				}
			case "recompressed-current-body":
				if tag, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, fixture.bodies[hash]); err != nil || tag.RowsAffected() != 1 {
					test.Fatal("retain valid logical bytes in a different stored representation", err)
				}
			case "same-hash-version-zero-new-incarnation":
				if original[hash].version != 0 {
					test.Fatal("new incarnation fixture requires the original source version zero", original[hash].version)
				}
				transaction, err := storage.pool.Begin(ctx)
				if err != nil {
					test.Fatal(err)
				}
				defer rollbackPG(transaction)
				if tag, err := transaction.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, hash); err != nil || tag.RowsAffected() != 1 {
					test.Fatal("remove original dependency grant", err)
				}
				if tag, err := transaction.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, hash); err != nil || tag.RowsAffected() != 1 {
					test.Fatal("remove original dependency source", err)
				}
				if _, err := transaction.Exec(ctx, `INSERT INTO blobs(hash,bytes,doc_range_source_version) VALUES($1,$2,0)`, hash, original[hash].body); err != nil {
					test.Fatal("replace source with identical stored bytes and version", err)
				}
				if _, err := transaction.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, repo, hash); err != nil {
					test.Fatal(err)
				}
				if err := transaction.Commit(ctx); err != nil {
					test.Fatal(err)
				}
			case "corrupt-changed-body", "valid-change-before-other-corruption", "missing-grant-and-corrupt-generation":
				if scenario.name == "valid-change-before-other-corruption" {
					first := fixture.manifest.Chunks[0].Hash
					if tag, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, first); err != nil || tag.RowsAffected() != 1 {
						test.Fatal("advance an earlier valid dependency generation", err)
					}
				}
				corrupt := bytes.Clone(fixture.bodies[hash])
				corrupt[len(corrupt)-1] ^= 1
				if tag, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, docCompress(corrupt)); err != nil || tag.RowsAffected() != 1 {
					test.Fatal("corrupt current logical bytes without changing logical length", err)
				}
			}
			if errors.Is(scenario.want, domain.ErrNotFound) {
				if tag, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, hash); err != nil || tag.RowsAffected() != 1 {
					test.Fatal("revoke the prepared dependency grant", err)
				}
			}
			changed := rootProofCommitSourcesPG(test, storage, ctx, fixture)
			if scenario.name == "same-hash-version-zero-new-incarnation" {
				if changed[hash].version != original[hash].version || changed[hash].incarnation == original[hash].incarnation || !bytes.Equal(changed[hash].body, original[hash].body) {
					test.Fatal("replacement did not isolate an incarnation change at the same version and bytes")
				}
			} else if scenario.name != "missing-grant" && (changed[hash].version != original[hash].version+1 || changed[hash].incarnation != original[hash].incarnation) {
				test.Fatal("body update did not isolate a source version change")
			}
			before := rootPGImage(test, ctx, storage, repo, namespace)
			var trace *rootPublicationBatchTracePG
			err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
				var traced context.Context
				traced, trace = rootPublicationBatchTraceContextPG(bound)
				return publication.Complete(traced, job, time.Now())
			})
			if !errors.Is(err, scenario.want) {
				test.Fatalf("final current-byte fallback returned %v, want %v", err, scenario.want)
			}
			for _, other := range []error{domain.ErrConflict, domain.ErrIntegrity, domain.ErrNotFound} {
				if other != scenario.want && errors.Is(err, other) {
					test.Fatal("final publication lost its error priority", err)
				}
			}
			assertRootProofCommitSourceMetadataPG(test, trace)
			if errors.Is(scenario.want, domain.ErrNotFound) {
				if len(trace.queries) != 1 {
					test.Fatal("missing grant must stop during metadata retention", len(trace.queries))
				}
				query := trace.queries[0]
				wanted := conversationChunkOrder(fixture.manifest)
				slices.Sort(wanted)
				if query.payload || query.bytes != 0 || query.maxBody != 0 || query.decodeErr != nil || query.rows < 1 || query.rows > len(wanted)-1 || !slices.Equal(query.wanted, strs(wanted)) {
					test.Fatal("missing ownership fetched a body or skipped the complete source metadata request", query.rows, query.bytes, query.decodeErr)
				}
			} else {
				requested := assertRootPublicationBatchQueriesPG(test, trace, conversationChunkOrder(fixture.manifest), 1)
				slices.Sort(requested)
				wanted := conversationChunkOrder(fixture.manifest)
				slices.Sort(wanted)
				if !slices.Equal(requested, strs(wanted)) {
					test.Fatal("changed generation did not retain the existing complete current-byte fallback", requested, wanted)
				}
			}
			assertRootStreamingUnpublishedPG(test, storage, ctx, job)
			if after := rootPGImage(test, ctx, storage, repo, namespace); after != before {
				test.Fatal("failed final publication changed ownership, archive, accounting, indexes, or receipt")
			}
			for hash, source := range changed {
				assertRootSourceWitnessStatePG(test, storage, ctx, hash, source)
			}
			if err := publication.Close(); err != nil {
				test.Fatal(err)
			}
			assertRangeSpoolsRemoved(test, directory)
		})
	}
}

func TestPGRootProofCommitMatchingWitnessStillChecksAuthorityAndFence(test *testing.T) {
	for _, scenario := range []string{"caller-version", "caller-wire", "expired-current-lease", "reclaimed-current-job", "current-compatibility"} {
		test.Run(scenario, func(test *testing.T) {
			directory := rangeSpoolTempDir(test)
			storage, ctx := chunkReusePG(test)
			ctx = rootPublicationContext(ctx)
			repo, namespace := rootPGRepo(test, ctx, storage)
			fixture := rangeFixture(test, domain.DocRangePageEvents+1)
			job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, repo)
			sources := rootProofCommitSourcesPG(test, storage, ctx, fixture)
			publication := prepareRootStreamingPG(test, storage, ctx, job, directory)
			assertRootProofCommitWitnessPG(test, publication, job, sources)
			candidate, completeCtx := job, ctx
			want := domain.ErrConflict
			switch scenario {
			case "caller-version":
				candidate.Version++
			case "caller-wire":
				candidate.RootManifest = append(bytes.Clone(job.RootManifest), '\n')
				want = domain.ErrConversationManifest
			case "expired-current-lease":
				current := job
				current.LeaseUntil = time.Now().Add(-time.Second)
				if err := storage.writeDocJob(ctx, current); err != nil {
					test.Fatal(err)
				}
			case "reclaimed-current-job":
				current, err := storage.ClaimDocJob(ctx, repo, job.LeaseUntil.Add(time.Second), time.Minute)
				if err != nil || current.ID != job.ID || current.Version != job.Version+1 {
					test.Fatal("fixture did not advance the authoritative job fence", current, err)
				}
			case "current-compatibility":
				completeCtx = outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityLegacy}, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
				want = domain.ErrDocumentIdentityUpgradeRequired
			}
			before := rootPGImage(test, ctx, storage, repo, namespace)
			var trace *rootPublicationBatchTracePG
			err := storage.WithinRepository(completeCtx, repo, func(bound context.Context) error {
				var traced context.Context
				traced, trace = rootPublicationBatchTraceContextPG(bound)
				return publication.Complete(traced, candidate, time.Now())
			})
			if !errors.Is(err, want) {
				test.Fatalf("matching dependency witness bypassed current authority: got %v, want %v", err, want)
			}
			if trace == nil || len(trace.queries) != 0 {
				test.Fatal("rejected authority or fence reached dependency retention")
			}
			assertRootStreamingUnpublishedPG(test, storage, ctx, job)
			if after := rootPGImage(test, ctx, storage, repo, namespace); after != before {
				test.Fatal("rejected authority or fence changed durable publication state")
			}
			for hash, source := range sources {
				assertRootSourceWitnessStatePG(test, storage, ctx, hash, source)
			}
		})
	}
}

func TestPGRootProofCommitRetentionMetadataBoundary(test *testing.T) {
	for _, count := range []int{64, 65} {
		test.Run(fmt.Sprintf("dependencies-%d", count), func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			repo, order, _ := seedRootPublicationBatchPG(test, storage, ctx, count, 128)
			locked := slices.Clone(order)
			slices.Sort(locked)
			source := pgRootSourceWitness{chunks: make([]pgRootDependencySource, 0, len(locked))}
			for _, hash := range locked {
				state := readRootSourceWitnessStatePG(test, storage, ctx, hash)
				source.chunks = append(source.chunks, pgRootDependencySource{hash: hash, version: state.version, incarnation: state.incarnation})
			}
			if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
				traced, trace := rootPublicationBatchTraceContextPG(bound)
				read, current, err := storage.retainedRootChunkReaderWithSource(traced, repo, order, &source)
				if err != nil || read == nil || !current {
					test.Fatal("exact dependency tuples did not establish retained source equality", current, err)
				}
				assertRootProofCommitSourceMetadataPG(test, trace)
				assertRootPublicationBatchQueriesPG(test, trace, order, 0)
				if len(trace.queries) != (count+63)/64 || trace.queries[0].rows != 64 || count == 65 && trace.queries[1].rows != 1 {
					test.Fatal("source retention changed the finite 64-row metadata boundary", len(trace.queries))
				}
				assertRootPublicationBatchLockedPG(test, storage, ctx, repo, order[0])
				return nil
			}); err != nil {
				test.Fatal(err)
			}
			if tag, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, order[0]); err != nil || tag.RowsAffected() != 1 {
				test.Fatal("metadata-only dependency body lock survived outer commit", err)
			}
			if tag, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, order[0]); err != nil || tag.RowsAffected() != 1 {
				test.Fatal("metadata-only dependency grant lock survived outer commit", err)
			}
		})
	}
}
