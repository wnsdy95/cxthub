//go:build postgres

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docRootSourceGenerationPGStore struct {
	*store.PostgresStore
	pool            *pgxpool.Pool
	repo            domain.ContentHash
	chunk           domain.ContentHash
	directory       string
	claims          []domain.DocFinalizationJob
	prepared        []*docRootSourceGenerationPGPublication
	completionCalls int
	updates         int
}

type docRootSourceGenerationPGPublication struct {
	outbound.BoundedRootDocPublication
	storage            *docRootSourceGenerationPGStore
	job                domain.DocFinalizationJob
	paths              []string
	initialResources   outbound.RootPublicationResources
	resourceCalls      int
	completionCalls    int
	completionErr      error
	closeCalls         int
	closedResources    outbound.RootPublicationResources
	closedResourcesErr error
}

func (storage *docRootSourceGenerationPGStore) observe(ctx context.Context) error {
	if _, system := inbound.RepositoryActor(ctx); !system {
		return domain.ErrUnauthorized
	}
	permit, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit)
	if permit == nil || !permit.live.Load() {
		return errors.New("root source retry bypassed live worker admission")
	}
	return outbound.CheckDocumentIdentityCompatibility(ctx, domain.DocumentIdentityRootV1)
}

func (storage *docRootSourceGenerationPGStore) ClaimDocJob(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.DocFinalizationJob, error) {
	if err := storage.observe(ctx); err != nil {
		return domain.DocFinalizationJob{}, err
	}
	job, err := storage.PostgresStore.ClaimDocJob(ctx, repo, now, lease)
	if err == nil {
		storage.claims = append(storage.claims, job)
	}
	return job, err
}

func (storage *docRootSourceGenerationPGStore) PrepareRootDocJob(ctx context.Context, job domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
	if err := storage.observe(ctx); err != nil {
		return nil, err
	}
	if !storage.InReadOnlyTransaction(ctx) {
		return nil, errors.New("root source retry bypassed the actual preparation snapshot")
	}
	prepared, err := storage.PostgresStore.PrepareRootDocJob(ctx, job)
	if err != nil {
		return prepared, err
	}
	bounded, ok := prepared.(outbound.BoundedRootDocPublication)
	if !ok {
		return nil, errors.Join(errors.New("real root preparation lost resource accounting"), prepared.Close())
	}
	resources, err := bounded.RetainedResources()
	if err != nil {
		return nil, errors.Join(err, prepared.Close())
	}
	paths, err := filepath.Glob(filepath.Join(storage.directory, "cxthub-doc-range-*"))
	if err != nil || len(paths) != 2 {
		return nil, errors.Join(fmt.Errorf("root preparation must retain original and derived spools: paths=%v error=%v", paths, err), prepared.Close())
	}
	var spoolBytes int64
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, errors.Join(err, prepared.Close())
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() <= 0 {
			return nil, errors.Join(errors.New("root preparation did not retain private publication spools"), prepared.Close())
		}
		spoolBytes += info.Size()
	}
	if spoolBytes != resources.SpoolBytes || resources.HeapBytes <= 0 {
		return nil, errors.Join(errors.New("root preparation did not account for both publication spools"), prepared.Close())
	}
	publication := &docRootSourceGenerationPGPublication{
		BoundedRootDocPublication: bounded,
		storage:                   storage,
		job:                       job,
		paths:                     paths,
		initialResources:          resources,
	}
	storage.prepared = append(storage.prepared, publication)
	return publication, nil
}

func (publication *docRootSourceGenerationPGPublication) RetainedResources() (outbound.RootPublicationResources, error) {
	publication.resourceCalls++
	return publication.BoundedRootDocPublication.RetainedResources()
}

func (publication *docRootSourceGenerationPGPublication) Complete(ctx context.Context, job domain.DocFinalizationJob, now time.Time) error {
	storage := publication.storage
	if err := storage.observe(ctx); err != nil {
		return err
	}
	if storage.InTransaction(ctx) || !reflect.DeepEqual(job, publication.job) {
		return errors.New("root completion did not leave its preparation snapshot with the exact claim")
	}
	current, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
	if err != nil {
		return err
	}
	if !current.Fences(job, time.Now().UTC()) {
		return errors.New("root source mutation requires the actual current live lease")
	}
	storage.completionCalls++
	publication.completionCalls++
	if storage.completionCalls == 1 {
		updated, err := storage.pool.Exec(ctx, `UPDATE blobs source SET bytes=source.bytes
 WHERE source.hash=$2 AND EXISTS(SELECT 1 FROM repo_blobs owned
 WHERE owned.repo_id=$1 AND owned.kind='chunk' AND owned.hash=source.hash)`, storage.repo, storage.chunk)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return errors.New("identical-byte update did not change exactly one owned dependency")
		}
		storage.updates++
	}
	publication.completionErr = publication.BoundedRootDocPublication.Complete(ctx, job, now)
	return publication.completionErr
}

func (publication *docRootSourceGenerationPGPublication) Close() error {
	publication.closeCalls++
	err := publication.BoundedRootDocPublication.Close()
	publication.closedResources, publication.closedResourcesErr = publication.BoundedRootDocPublication.RetainedResources()
	return err
}

type docRootSourceGenerationPGChunk struct {
	stored      []byte
	version     int64
	incarnation string
}

func readDocRootSourceGenerationPGChunk(test *testing.T, ctx context.Context, pool *pgxpool.Pool, repo, hash domain.ContentHash) docRootSourceGenerationPGChunk {
	test.Helper()
	var image docRootSourceGenerationPGChunk
	if err := pool.QueryRow(ctx, `SELECT source.bytes,source.doc_range_source_version,incarnation.incarnation::text
 FROM repo_blobs owned JOIN blobs source ON source.hash=owned.hash
 JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
 WHERE owned.repo_id=$1 AND owned.kind='chunk' AND owned.hash=$2`, repo, hash).
		Scan(&image.stored, &image.version, &image.incarnation); err != nil {
		test.Fatal("read the owned dependency source", err)
	}
	return image
}

func docRootSourceGenerationPGCounts(test *testing.T, ctx context.Context, pool *pgxpool.Pool, repo, hash domain.ContentHash) [8]int {
	test.Helper()
	var counts [8]int
	if err := pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM blobs WHERE hash=$2),
 (SELECT count(*) FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2),
 (SELECT count(*) FROM doc_range_indexes_v1 WHERE hash=$2),
 (SELECT count(*) FROM doc_range_pages_v1 WHERE doc_hash=$2),
 (SELECT count(*) FROM doc_range_generations_v2 WHERE doc_hash=$2),
 (SELECT count(*) FROM doc_range_objects_v2 WHERE doc_hash=$2),
 (SELECT count(*) FROM doc_finalization_jobs WHERE repo_id=$1),
 (SELECT count(*) FROM doc_finalization_jobs WHERE repo_id=$1 AND state='completed')`, repo, hash).
		Scan(&counts[0], &counts[1], &counts[2], &counts[3], &counts[4], &counts[5], &counts[6], &counts[7]); err != nil {
		test.Fatal("read root publication and receipt counts", err)
	}
	return counts
}

func assertDocRootSourceGenerationPGClosed(test *testing.T, storage *docRootSourceGenerationPGStore, baseline outbound.TransientSpoolBudgetUsage) {
	test.Helper()
	for attempt, publication := range storage.prepared {
		if publication.closeCalls != 1 || publication.resourceCalls == 0 || publication.completionCalls != 1 ||
			publication.initialResources.HeapBytes <= 0 || publication.initialResources.SpoolBytes <= 0 ||
			publication.closedResources != (outbound.RootPublicationResources{}) || publication.closedResourcesErr == nil {
			test.Fatal("worker did not close and release its accounted root preparation", attempt, publication)
		}
		for _, path := range publication.paths {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				test.Fatal("root attempt retained a private spool", attempt, err)
			}
		}
	}
	if files, err := os.ReadDir(storage.directory); err != nil || len(files) != 0 {
		test.Fatal("root attempt leaked private temporary files", files, err)
	}
	usage := storage.TransientSpoolBudgetUsage()
	if usage.LimitBytes != baseline.LimitBytes || usage.UsedBytes != baseline.UsedBytes || usage.OpenFiles != baseline.OpenFiles {
		test.Fatal("root attempt leaked shared spool reservations", baseline, usage)
	}
}

func TestPGDocRootWorkerAutomaticallyRepreparesChangedChunkSource(test *testing.T) {
	if !conversationRootReleaseReady {
		test.Skip("requires released binary or explicitly authorized private Go readiness overlay")
	}
	ctx, gate := admissionTestContext(resourceWaitLimit)
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	ctx = inbound.WithDocumentIdentities(inbound.WithSystemActor(ctx), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	test.Setenv("CXT_TEST_DSN", a11IsolatedPGDSN(test, ctx))
	service, actual, repo := collaborationPG(test)
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), service.DocumentIdentitiesSupported())
	team := makeTeamFixture(test, actual)
	if _, err := actual.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: team.repository.ID}); err != nil {
		test.Fatal(err)
	}
	if err := service.ConfigureConversationRootPublication(true); err != nil {
		test.Fatal(err)
	}
	identity := domain.DocumentIdentityRootV1
	if _, err := service.PatchRepoProfile(ctx, repo, inbound.RepoProfilePatch{RequiredDocIdentity: &identity}); err != nil {
		test.Fatal(err)
	}
	representation, bodies := a11RootRepresentation(test, "transparent worker source-generation retry")
	manifest, err := representation.ConversationManifest()
	if err != nil || len(manifest.Chunks) == 0 {
		test.Fatal("root retry fixture has no owned dependency", err)
	}
	canonicalManifest := bytes.Clone(representation.RootManifest)
	chunks := make([]inbound.ChunkObject, 0, len(bodies))
	for hash, body := range bodies {
		chunks = append(chunks, inbound.ChunkObject{Hash: hash, Data: body})
	}
	if _, err := service.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: chunks}); err != nil {
		test.Fatal(err)
	}
	accepted, err := service.SubmitDocFinalization(ctx, repo, representation)
	if err != nil || accepted.State != "waiting" || accepted.ValidateFor(repo, representation) != nil {
		test.Fatal("actual root admission did not accept the fixture", accepted, err)
	}
	queued, err := actual.GetDocJob(ctx, repo, accepted.ID)
	if err != nil || queued.Attempts != 0 || queued.Version != 0 || queued.State != "waiting" {
		test.Fatal("root admission bypassed the durable waiting receipt", queued, err)
	}
	pool, err := pgxpool.New(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	directory := test.TempDir()
	for _, variable := range []string{"TMPDIR", "TMP", "TEMP"} {
		test.Setenv(variable, directory)
	}
	storage := &docRootSourceGenerationPGStore{
		PostgresStore: actual,
		pool:          pool,
		repo:          repo,
		chunk:         manifest.Chunks[0].Hash,
		directory:     directory,
	}
	test.Cleanup(func() {
		for _, publication := range storage.prepared {
			if publication.closeCalls == 0 {
				test.Error("actual finalizer abandoned a prepared root without closing it")
				if err := publication.Close(); err != nil {
					test.Error(err)
				}
			}
		}
		assertResourcesReleased(test, gate)
	})
	workers, err := NewDocumentWorkers(DocumentWorkersConfig{
		Metadata: storage, Blobs: storage, FindFields: storage, Display: storage,
		Verifier: &domain.CanonicalDocVerifier{}, Profile: documentquery.VerifiedRoots,
	})
	if err != nil {
		test.Fatal(err)
	}
	before := make(map[domain.ContentHash]docRootSourceGenerationPGChunk, len(bodies))
	for hash := range bodies {
		before[hash] = readDocRootSourceGenerationPGChunk(test, ctx, pool, repo, hash)
	}
	assertChunksPreserved := func() {
		test.Helper()
		for hash, body := range bodies {
			image := readDocRootSourceGenerationPGChunk(test, ctx, pool, repo, hash)
			original := before[hash]
			version := original.version
			if hash == storage.chunk {
				version++
			}
			if !bytes.Equal(image.stored, original.stored) || image.version != version || image.incarnation != original.incarnation {
				test.Fatal("retry changed bytes, incarnation or an unrelated dependency generation", hash, image.version, version)
			}
			current, err := actual.ReadConversationChunk(ctx, repo, hash, int64(len(body)))
			if err != nil || !bytes.Equal(current, body) || domain.HashContent(current) != hash {
				test.Fatal("retry changed the repository-owned logical chunk", hash, err)
			}
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1 AND kind='chunk'`, repo).Scan(&count); err != nil || count != len(bodies) {
			test.Fatal("retry changed dependency ownership", count, err)
		}
	}
	baseline := actual.TransientSpoolBudgetUsage()
	unpublished := [8]int{0, 0, 0, 0, 0, 0, 1, 0}
	if counts := docRootSourceGenerationPGCounts(test, ctx, pool, repo, representation.Hash); counts != unpublished {
		test.Fatal("acceptance published before worker execution", counts)
	}
	if err := workers.Finalization.Process(ctx, 1); !errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrValidation) {
		test.Fatal("identical-byte dependency generation change must be a retryable completion conflict", err)
	}
	if len(storage.claims) != 1 || len(storage.prepared) != 1 || storage.completionCalls != 1 || storage.updates != 1 ||
		!errors.Is(storage.prepared[0].completionErr, domain.ErrConflict) {
		test.Fatal("first actual claim did not prepare, mutate once and delegate to real completion", storage.claims, storage.completionCalls, storage.updates)
	}
	retrying, err := actual.GetDocJob(ctx, repo, accepted.ID)
	if err != nil || retrying.State != "retrying" || retrying.Reason != "temporary_failure" || retrying.Attempts != 1 ||
		retrying.Version != storage.claims[0].Version || !retrying.LeaseUntil.IsZero() ||
		!retrying.NextAttempt.Equal(retrying.UpdatedAt.Add(2*time.Second)) || retrying.DocumentRef() != representation.DocumentRef() ||
		!bytes.Equal(retrying.RootManifest, canonicalManifest) {
		test.Fatal("source change became rejection, lost its manifest or bypassed existing retry scheduling", retrying, err)
	}
	if counts := docRootSourceGenerationPGCounts(test, ctx, pool, repo, representation.Hash); counts != unpublished {
		test.Fatal("stale root preparation escaped atomic publication rollback", counts)
	}
	assertDocRootSourceGenerationPGClosed(test, storage, baseline)
	assertResourcesReleased(test, gate)
	assertChunksPreserved()
	if err := service.ConfigureConversationRootPublication(false); err != nil {
		test.Fatal(err)
	}
	if _, err := service.SubmitDocFinalization(ctx, repo, representation); !errors.Is(err, domain.ErrRootPublicationDisabled) {
		test.Fatal("accepted-job retry unexpectedly required or reopened public admission", err)
	}
	if current, err := actual.GetDocJob(ctx, repo, accepted.ID); err != nil || !reflect.DeepEqual(current, retrying) {
		test.Fatal("blocked resubmission repaired or reset the durable retry", current, err)
	}
	if delay := time.Until(retrying.NextAttempt); delay > 0 {
		if delay > 3*time.Second {
			test.Fatal("unexpected existing retry deadline", delay)
		}
		timer := time.NewTimer(delay + time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			test.Fatal(ctx.Err())
		}
	}
	if err := workers.Finalization.Process(ctx, 1); err != nil {
		test.Fatal("next naturally due claim did not freshly prepare and publish", err)
	}
	if len(storage.claims) != 2 || len(storage.prepared) != 2 || storage.completionCalls != 2 || storage.updates != 1 ||
		slices.ContainsFunc(storage.prepared[0].paths, func(path string) bool {
			return slices.Contains(storage.prepared[1].paths, path)
		}) || storage.prepared[1].completionErr != nil {
		test.Fatal("retry reused the stale preparation or repeated the dependency update", storage.claims, storage.completionCalls, storage.updates)
	}
	for attempt, claim := range storage.claims {
		if claim.State != "running" || claim.ID != accepted.ID || claim.RepoID != repo || claim.DocumentRef() != representation.DocumentRef() ||
			claim.Version != queued.Version+int64(attempt)+1 || claim.Attempts != attempt+1 || !claim.LeaseUntil.After(claim.UpdatedAt) ||
			!reflect.DeepEqual(storage.prepared[attempt].job, claim) || !bytes.Equal(claim.RootManifest, canonicalManifest) {
			test.Fatal("retry did not retain actual claim fencing and the accepted root identity", attempt, claim)
		}
	}
	completed, err := actual.GetDocJob(ctx, repo, accepted.ID)
	if err != nil || completed.State != "completed" || completed.Reason != "" || completed.Attempts != 2 ||
		completed.Version != retrying.Version+1 || !completed.LeaseUntil.IsZero() || completed.DocumentRef() != representation.DocumentRef() ||
		!bytes.Equal(completed.RootManifest, canonicalManifest) {
		test.Fatal("fresh preparation did not complete the original exact receipt", completed, err)
	}
	assertDocRootSourceGenerationPGClosed(test, storage, baseline)
	assertResourcesReleased(test, gate)
	assertChunksPreserved()
	counts := docRootSourceGenerationPGCounts(test, ctx, pool, repo, representation.Hash)
	pages := int((manifest.EventCount + domain.DocRangePageEvents - 1) / domain.DocRangePageEvents)
	if counts[5] <= 0 || counts != ([8]int{1, 1, 1, pages, 1, counts[5], 1, 1}) {
		test.Fatal("retry did not publish exactly one root, owned index and completed receipt", counts)
	}
	proof, err := actual.ReadVerifiedDoc(ctx, repo, representation.Hash)
	if err != nil || proof.DocumentRef() != representation.DocumentRef() {
		test.Fatal("completed retry lacks a current verified root", err)
	}
	verifiedManifest, ok := proof.ConversationManifest()
	if !ok {
		test.Fatal("completed retry lost its root manifest")
	}
	canonical, err := domain.CanonicalConversationManifest(verifiedManifest)
	if err != nil || !bytes.Equal(canonical, canonicalManifest) {
		test.Fatal("retry changed the canonical manifest", err)
	}
	if hash, err := domain.ConversationManifestHash(verifiedManifest); err != nil || hash != representation.Hash {
		test.Fatal("retry changed the logical root hash", hash, err)
	}
	if err := actual.WithinReadSnapshot(ctx, func(read context.Context) error {
		generation, err := actual.OpenDocRangeGeneration(read, repo, representation.Hash)
		if err != nil {
			return err
		}
		descriptor := generation.Descriptor()
		if descriptor.Validate(representation.Hash) != nil || descriptor.Identity() != identity ||
			descriptor.EventCount != manifest.EventCount || descriptor.StreamBytes != manifest.StreamBytes || descriptor.ChunkCount() != len(manifest.Chunks) {
			return errors.New("retry published an index for a different root manifest")
		}
		var events int64
		for position := 0; position < descriptor.EventPages.Count; position++ {
			entries, err := generation.ReadPage(read, position)
			if err != nil {
				return err
			}
			events += int64(len(entries))
		}
		if events != manifest.EventCount {
			return errors.New("retry published an incomplete event index")
		}
		for position, expected := range manifest.Chunks {
			chunk, err := generation.Chunk(read, position)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(chunk, expected) {
				return errors.New("retry index changed an accepted dependency")
			}
		}
		return nil
	}); err != nil {
		test.Fatal("fresh retry did not atomically publish the complete usable range index", err)
	}
	if err := workers.Finalization.Process(ctx, 1); err != nil {
		test.Fatal("completed retry was not quiescent", err)
	}
	if current, err := actual.GetDocJob(ctx, repo, accepted.ID); err != nil || !reflect.DeepEqual(current, completed) ||
		len(storage.claims) != 2 || len(storage.prepared) != 2 || storage.updates != 1 {
		test.Fatal("idle worker changed or duplicated the completed receipt", current, err)
	}
	if after := docRootSourceGenerationPGCounts(test, ctx, pool, repo, representation.Hash); after != counts {
		test.Fatal("idle worker duplicated root publication", counts, after)
	}
	if service.RootPublicationEnabled() {
		test.Fatal("transparent retry reopened new root admission")
	}
	assertDocRootSourceGenerationPGClosed(test, storage, baseline)
	assertResourcesReleased(test, gate)
}
