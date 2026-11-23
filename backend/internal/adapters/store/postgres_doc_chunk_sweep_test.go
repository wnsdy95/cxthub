//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type chunkSweepFixture struct {
	store     *PostgresStore
	ctx       context.Context
	repo      domain.ContentHash
	namespace string
	clock     time.Time
}

// The repository belongs to a namespace so that the storage account is metered.
func newChunkSweepFixture(t *testing.T) *chunkSweepFixture {
	t.Helper()
	st, ctx := chunkReusePG(t)
	fixture := &chunkSweepFixture{store: st, ctx: ctx, clock: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	st.chunkSweep.now = func() time.Time { return fixture.clock }
	now := time.Now().UTC()
	owner := domain.NewID("sweep")
	user := domain.User{ID: owner, Username: owner, Name: "Sweep", Email: owner + "@example.test"}
	if err := st.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	fixture.namespace = domain.NewID("ns_")
	if err := st.CreateNamespace(ctx, domain.Namespace{ID: fixture.namespace, Slug: owner, Kind: domain.NamespaceUser, UserID: user.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	fixture.repo = fixture.addRepo(t, "first")
	return fixture
}

func (fixture *chunkSweepFixture) addRepo(t *testing.T, name string) domain.ContentHash {
	t.Helper()
	st, ctx := fixture.store, fixture.ctx
	var owner, username string
	if err := st.pool.QueryRow(ctx, `SELECT user_id,slug FROM namespaces WHERE id=$1`, fixture.namespace).Scan(&owner, &username); err != nil {
		t.Fatal(err)
	}
	repository := domain.Repository{ID: domain.NewID("ws_"), Name: name, Slug: name, OwnerID: owner, OwnerUsername: username, OwnerNamespaceID: fixture.namespace, CreatedAt: time.Now().UTC()}
	if err := st.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: domain.HashContent([]byte(repository.ID)), RepositoryID: repository.ID, RemoteURL: "https://example.test/" + repository.ID, DefaultBranch: "main"}
	if _, err := st.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	return repo.ID
}

// A conversation of n 40 KiB events, unique to this fixture so that no other
// test's chunks are shared.
func (fixture *chunkSweepFixture) conversation(t *testing.T, label string, n int) domain.VerifiedSessionDoc {
	t.Helper()
	cir := chunkBigDoc(n)
	for i := range cir.Events {
		cir.Events[i].Blocks[0].Text = string(fixture.repo) + label + cir.Events[i].Blocks[0].Text
	}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: domain.HashContent(raw), CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func (fixture *chunkSweepFixture) plan(t *testing.T, doc domain.VerifiedSessionDoc) domain.DocChunkPlan {
	t.Helper()
	plan, chunked := domain.PlanDocChunks(doc.Bytes())
	if !chunked || len(plan.Order) < 2 {
		t.Fatal("fixture must span several chunks", len(plan.Order))
	}
	return plan
}

func (fixture *chunkSweepFixture) sweep(t *testing.T, repo domain.ContentHash) domain.DocChunkSweep {
	t.Helper()
	result, err := fixture.store.SweepOrphanDocChunks(fixture.ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (fixture *chunkSweepFixture) owned(t *testing.T, repo domain.ContentHash, chunks []domain.ContentHash) map[domain.ContentHash]bool {
	t.Helper()
	have, err := fixture.store.HasChunks(fixture.ctx, repo, chunks)
	if err != nil {
		t.Fatal(err)
	}
	owned := make(map[domain.ContentHash]bool, len(have))
	for _, hash := range have {
		owned[hash] = true
	}
	return owned
}

func (fixture *chunkSweepFixture) stored(t *testing.T, hash domain.ContentHash) bool {
	t.Helper()
	var present bool
	if err := fixture.store.pool.QueryRow(fixture.ctx, `SELECT EXISTS(SELECT 1 FROM blobs WHERE hash=$1)`, string(hash)).Scan(&present); err != nil {
		t.Fatal(err)
	}
	return present
}

func (fixture *chunkSweepFixture) usage(t *testing.T) domain.StorageUsage {
	t.Helper()
	usage, err := fixture.store.ReadStorageUsage(fixture.ctx, fixture.namespace, time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return usage
}

// The case the cleanup exists for: a conversation is captured, grows, is
// captured again, and the first capture is deleted. Its partial tail chunk is
// then named by nothing.
func TestPGDocChunkSweepRemovesSupersededTailAndKeepsGrownDocument(t *testing.T) {
	fixture := newChunkSweepFixture(t)
	st, ctx, repo := fixture.store, fixture.ctx, fixture.repo
	first, grown := fixture.conversation(t, "", 30), fixture.conversation(t, "", 45)
	firstPlan, grownPlan := fixture.plan(t, first), fixture.plan(t, grown)
	tail := firstPlan.Order[len(firstPlan.Order)-1]
	if _, shared := grownPlan.Bodies[tail]; shared {
		t.Fatal("fixture: the first capture's tail chunk must not survive growth")
	}
	for _, chunk := range firstPlan.Order[:len(firstPlan.Order)-1] {
		if _, shared := grownPlan.Bodies[chunk]; !shared {
			t.Fatal("fixture: closed chunks must be a prefix of the grown capture")
		}
	}
	for _, doc := range []domain.VerifiedSessionDoc{first, grown} {
		if _, err := st.PutVerifiedDoc(ctx, repo, doc); err != nil {
			t.Fatal(err)
		}
	}
	// While both manifests are stored every chunk is live.
	if result := fixture.sweep(t, repo); result.Orphans != 0 || result.Removed != 0 {
		t.Fatalf("live chunks reported as orphans: %+v", result)
	}
	if err := st.DeleteDoc(ctx, repo, first.Hash()); err != nil {
		t.Fatal(err)
	}
	before := fixture.usage(t)
	every := append(append([]domain.ContentHash(nil), firstPlan.Order...), grownPlan.Order...)

	// First sighting starts the grace period; nothing is deleted before it ends.
	if result := fixture.sweep(t, repo); result.Orphans != 1 || result.Waiting != 1 || result.Removed != 0 {
		t.Fatalf("first sighting: %+v", result)
	}
	fixture.clock = fixture.clock.Add(docChunkSweepGrace - time.Second)
	if result := fixture.sweep(t, repo); result.Orphans != 1 || result.Removed != 0 {
		t.Fatalf("inside the grace period: %+v", result)
	}
	if !fixture.owned(t, repo, every)[tail] || !fixture.stored(t, tail) {
		t.Fatal("tail chunk deleted inside the grace period")
	}

	fixture.clock = fixture.clock.Add(time.Second)
	result := fixture.sweep(t, repo)
	tailBytes := int64(len(docCompress(firstPlan.Bodies[tail])))
	if result.Orphans != 1 || result.Removed != 1 || result.Bodies != 1 || result.Bytes != tailBytes || result.Deferred != 0 {
		t.Fatalf("after the grace period: %+v, tail is %d stored bytes", result, tailBytes)
	}
	owned := fixture.owned(t, repo, every)
	if owned[tail] || fixture.stored(t, tail) {
		t.Fatal("superseded tail chunk survived the cleanup")
	}
	for _, chunk := range grownPlan.Order {
		if !owned[chunk] || !fixture.stored(t, chunk) {
			t.Fatal("cleanup removed a chunk of the grown document", chunk)
		}
		body, err := st.GetChunk(ctx, repo, chunk)
		if err != nil || !bytes.Equal(body, grownPlan.Bodies[chunk]) {
			t.Fatal("chunk of the grown document changed", chunk, err)
		}
	}
	if len(owned) != len(grownPlan.Bodies) {
		t.Fatal("repository owns chunks no manifest names", len(owned), len(grownPlan.Bodies))
	}

	// The grown document still reads back and verifies from its stored chunks.
	read, err := st.ReadVerifiedDoc(ctx, repo, grown.Hash())
	if err != nil || read.Hash() != grown.Hash() || !bytes.Equal(read.Bytes(), grown.Bytes()) {
		t.Fatal("grown document no longer reads back", err)
	}
	if _, err := st.VerifyStoredDoc(ctx, repo, grown.Hash()); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDoc(ctx, repo, grown.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if err := domain.ValidateSessionDocHash(got); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDoc(ctx, repo, first.Hash()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("deleted capture is readable again", err)
	}

	// The namespace is charged exactly the tail's stored bytes less, through
	// the ordinary metering trigger, and reconciliation finds nothing to fix.
	after := fixture.usage(t)
	if before.CurrentBytes-after.CurrentBytes != tailBytes {
		t.Fatalf("storage account moved by %d, want %d", before.CurrentBytes-after.CurrentBytes, tailBytes)
	}
	var reason string
	var delta int64
	if err := st.pool.QueryRow(ctx, `SELECT reason,delta_bytes FROM storage_usage_ledger WHERE namespace_id=$1 AND object_key=$2 ORDER BY seq DESC LIMIT 1`,
		fixture.namespace, "blob:"+string(tail)).Scan(&reason, &delta); err != nil || reason != "payload.delete" || delta != -tailBytes {
		t.Fatal("tail deletion was not metered", reason, delta, err)
	}
	if err := st.ReconcileStorageUsage(ctx, fixture.namespace); err != nil {
		t.Fatal(err)
	}
	reconciled := fixture.usage(t)
	if reconciled.CurrentBytes != after.CurrentBytes {
		t.Fatalf("cleanup left the storage account off by %d bytes", reconciled.CurrentBytes-after.CurrentBytes)
	}
	var corrections int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM storage_usage_ledger WHERE namespace_id=$1 AND reason LIKE 'reconcile%'`, fixture.namespace).Scan(&corrections); err != nil || corrections != 0 {
		t.Fatal("reconciliation had to correct the account after the cleanup", corrections, err)
	}

	// Storing the deleted capture again brings its tail back intact.
	if _, err := st.PutVerifiedDoc(ctx, repo, first); err != nil {
		t.Fatal(err)
	}
	if again, err := st.ReadVerifiedDoc(ctx, repo, first.Hash()); err != nil || !bytes.Equal(again.Bytes(), first.Bytes()) {
		t.Fatal("first capture could not be stored again after its tail was cleaned up", err)
	}
	if result := fixture.sweep(t, repo); result.Orphans != 0 {
		t.Fatalf("restored capture's chunks reported as orphans: %+v", result)
	}
}

// An upload stores its chunks before the manifest that names them exists.
// Chunks inside the grace period stay, and so do chunks a pending
// finalization job names, however old they are.
func TestPGDocChunkSweepKeepsUploadsInProgress(t *testing.T) {
	fixture := newChunkSweepFixture(t)
	st, ctx, repo := fixture.store, fixture.ctx, fixture.repo
	abandoned, pending := fixture.conversation(t, "abandoned", 20), fixture.conversation(t, "pending", 20)
	abandonedPlan, pendingPlan := fixture.plan(t, abandoned), fixture.plan(t, pending)
	for _, plan := range []domain.DocChunkPlan{abandonedPlan, pendingPlan} {
		if _, _, err := st.PutChunks(ctx, repo, plan.Bodies); err != nil {
			t.Fatal(err)
		}
	}
	job, err := domain.NewDocFinalizationJob(repo, pending.Hash(), pendingPlan.Manifest, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if job, err = st.EnqueueDocJob(ctx, job); err != nil || job.State != "waiting" {
		t.Fatal("finalization job was not accepted", job.State, err)
	}
	every := append(append([]domain.ContentHash(nil), abandonedPlan.Order...), pendingPlan.Order...)
	intact := func(when string, plans ...domain.DocChunkPlan) {
		t.Helper()
		owned := fixture.owned(t, repo, every)
		for _, plan := range plans {
			for _, chunk := range plan.Order {
				if !owned[chunk] || !fixture.stored(t, chunk) {
					t.Fatal("chunk of an upload in progress was deleted", when, chunk)
				}
			}
		}
	}

	// No manifest yet: only the job's chunks are named by anything.
	if result := fixture.sweep(t, repo); result.Orphans != len(abandonedPlan.Bodies) || result.Removed != 0 {
		t.Fatalf("first sighting: %+v", result)
	}
	intact("at first sighting", abandonedPlan, pendingPlan)
	fixture.clock = fixture.clock.Add(docChunkSweepGrace - time.Second)
	if result := fixture.sweep(t, repo); result.Removed != 0 || result.Waiting != len(abandonedPlan.Bodies) {
		t.Fatalf("inside the grace period: %+v", result)
	}
	intact("inside the grace period", abandonedPlan, pendingPlan)

	// Long after the grace period the pending job still keeps its chunks,
	// while the upload nobody finished is cleaned up.
	fixture.clock = fixture.clock.Add(48 * time.Hour)
	result := fixture.sweep(t, repo)
	if result.Orphans != len(abandonedPlan.Bodies) || result.Removed != len(abandonedPlan.Bodies) || result.Deferred != 0 {
		t.Fatalf("after the grace period: %+v", result)
	}
	intact("named by a pending job", pendingPlan)
	if owned := fixture.owned(t, repo, abandonedPlan.Order); len(owned) != 0 {
		t.Fatal("abandoned upload survived the grace period", len(owned))
	}
	if result := fixture.sweep(t, repo); result.Orphans != 0 || result.Removed != 0 {
		t.Fatalf("pending job's chunks reported as orphans: %+v", result)
	}

	// The job can still be completed from the chunks that were kept.
	if _, err := st.ClaimDocJob(ctx, repo, time.Now().UTC(), time.Minute); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.GetDocJob(ctx, repo, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteDocJob(ctx, claimed, pending, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if read, err := st.ReadVerifiedDoc(ctx, repo, pending.Hash()); err != nil || !bytes.Equal(read.Bytes(), pending.Bytes()) {
		t.Fatal("document finalized after the cleanup does not read back", err)
	}
	if result := fixture.sweep(t, repo); result.Orphans != 0 {
		t.Fatalf("finalized document's chunks reported as orphans: %+v", result)
	}
}

// A writer holds the advisory lock of every chunk it stores until it commits.
// The cleanup must skip such a chunk without waiting, and a chunk stored again
// after it was first seen must get a new grace period.
func TestPGDocChunkSweepSkipsChunksInUseAndRestartsGraceForNewGrants(t *testing.T) {
	fixture := newChunkSweepFixture(t)
	st, ctx, repo := fixture.store, fixture.ctx, fixture.repo
	body := []byte(t.Name() + string(repo))
	chunk := domain.HashContent(body)
	upload := func() {
		t.Helper()
		if _, _, err := st.PutChunks(ctx, repo, map[domain.ContentHash][]byte{chunk: body}); err != nil {
			t.Fatal(err)
		}
	}
	upload()
	if result := fixture.sweep(t, repo); result.Orphans != 1 || result.Removed != 0 {
		t.Fatalf("first sighting: %+v", result)
	}
	fixture.clock = fixture.clock.Add(docChunkSweepGrace)

	// An upload of the same chunk is in flight: its transaction has taken the
	// chunk's lock (as PutChunks does) and has not committed.
	writer, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	if err := (&rawRepresentationAttempt{}).lock(ctx, writer, "", []domain.ContentHash{chunk}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result := fixture.sweep(t, repo)
	if result.Removed != 0 || result.Deferred != 1 {
		t.Fatalf("chunk locked by a writer: %+v", result)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Fatal("cleanup waited for the writer", waited)
	}
	if !fixture.owned(t, repo, []domain.ContentHash{chunk})[chunk] || !fixture.stored(t, chunk) {
		t.Fatal("chunk in use was deleted")
	}
	// A reader that share-locks the body is respected the same way.
	if err := writer.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	reader, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback(context.Background())
	if _, err := reader.Exec(ctx, `SELECT 1 FROM blobs WHERE hash=$1 FOR SHARE`, string(chunk)); err != nil {
		t.Fatal(err)
	}
	fixture.clock = fixture.clock.Add(docChunkSweepRevisit)
	if result := fixture.sweep(t, repo); result.Removed != 0 || result.Deferred != 1 || !fixture.stored(t, chunk) {
		t.Fatalf("chunk body locked by a reader: %+v", result)
	}
	if err := reader.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// The grant is deleted and uploaded again: a new upload in progress.
	if _, err := st.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, string(repo), string(chunk)); err != nil {
		t.Fatal(err)
	}
	upload()
	if result := fixture.sweep(t, repo); result.Orphans != 1 || result.Removed != 0 || result.Waiting != 1 {
		t.Fatalf("new grant inherited the old grant's age: %+v", result)
	}
	fixture.clock = fixture.clock.Add(docChunkSweepGrace)
	if result := fixture.sweep(t, repo); result.Removed != 1 || result.Bodies != 1 {
		t.Fatalf("unlocked orphan after its own grace period: %+v", result)
	}
	if fixture.stored(t, chunk) {
		t.Fatal("orphan chunk body survived")
	}
}

// Between the first read and the deletion a writer may commit a manifest or a
// job that names an orphan. The deletion transaction looks again once it holds
// its locks, and it never trusts a grant that is not the row it observed.
func TestPGDocChunkSweepRechecksReferencesUnderItsLocks(t *testing.T) {
	fixture := newChunkSweepFixture(t)
	st, ctx, repo := fixture.store, fixture.ctx, fixture.repo
	st.chunkSweep.manifests = make(map[docChunkSweepSource][]uint64)
	observe := func(plan domain.DocChunkPlan) []docChunkOrphan {
		t.Helper()
		if _, _, err := st.PutChunks(ctx, repo, plan.Bodies); err != nil {
			t.Fatal(err)
		}
		grants, err := lockedDocChunkRows(ctx, st.pool, `SELECT hash,xmin::text FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=ANY($2::text[])`, string(repo), strs(plan.Order))
		if err != nil || len(grants) != len(plan.Bodies) {
			t.Fatal("uploaded chunks are not owned", len(grants), err)
		}
		orphans := make([]docChunkOrphan, 0, len(grants))
		for hash, xmin := range grants {
			orphans = append(orphans, docChunkOrphan{hash: hash, xmin: xmin})
		}
		return orphans
	}
	kept := func(when string, plan domain.DocChunkPlan, orphans []docChunkOrphan) {
		t.Helper()
		removed, bodies, _, _, err := st.removeOrphanDocChunks(ctx, repo, orphans)
		if err != nil || len(removed) != 0 || bodies != 0 {
			t.Fatal("stale orphan list deleted chunks", when, len(removed), bodies, err)
		}
		if owned := fixture.owned(t, repo, plan.Order); len(owned) != len(plan.Bodies) {
			t.Fatal("chunks lost", when, len(owned))
		}
	}

	// A manifest committed after the orphans were listed.
	published := fixture.conversation(t, "published", 20)
	publishedPlan := fixture.plan(t, published)
	stale := observe(publishedPlan)
	if _, err := st.PutVerifiedDoc(ctx, repo, published); err != nil {
		t.Fatal(err)
	}
	kept("named by a new manifest", publishedPlan, stale)
	if read, err := st.ReadVerifiedDoc(ctx, repo, published.Hash()); err != nil || !bytes.Equal(read.Bytes(), published.Bytes()) {
		t.Fatal("document published during the sweep does not read back", err)
	}

	// A job enqueued after the orphans were listed.
	queued := fixture.conversation(t, "queued", 20)
	queuedPlan := fixture.plan(t, queued)
	stale = observe(queuedPlan)
	job, err := domain.NewDocFinalizationJob(repo, queued.Hash(), queuedPlan.Manifest, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueDocJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	kept("named by a new job", queuedPlan, stale)

	// A grant that was deleted and stored again is not the row that was seen.
	reuploaded := fixture.conversation(t, "reuploaded", 20)
	reuploadedPlan := fixture.plan(t, reuploaded)
	stale = observe(reuploadedPlan)
	if _, err := st.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=ANY($2::text[])`, string(repo), strs(reuploadedPlan.Order)); err != nil {
		t.Fatal(err)
	}
	fresh := observe(reuploadedPlan)
	kept("stored again", reuploadedPlan, stale)

	// The same list is deleted once it describes the current, unnamed rows.
	removed, bodies, _, _, err := st.removeOrphanDocChunks(ctx, repo, fresh)
	if err != nil || len(removed) != len(reuploadedPlan.Bodies) || bodies != len(reuploadedPlan.Bodies) {
		t.Fatal("current orphans were not deleted", len(removed), bodies, err)
	}
}

// A writer that names a chunk while the cleanup is deleting it waits for the
// cleanup's lock and then stores the chunk again; its document is complete.
func TestPGDocChunkSweepWriterWaitsThenStoresChunksAgain(t *testing.T) {
	fixture := newChunkSweepFixture(t)
	st, ctx, repo := fixture.store, fixture.ctx, fixture.repo
	doc := fixture.conversation(t, "", 20)
	plan := fixture.plan(t, doc)
	if _, _, err := st.PutChunks(ctx, repo, plan.Bodies); err != nil {
		t.Fatal(err)
	}
	// The cleanup's deletion transaction, stopped before it commits.
	cleanup, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup.Rollback(context.Background())
	for _, chunk := range plan.Order {
		var locked bool
		if err := cleanup.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1,$2)`, rawRepresentationLockSpace, rawRepresentationLockKey(chunk)).Scan(&locked); err != nil || !locked {
			t.Fatal("cleanup could not lock an idle chunk", locked, err)
		}
	}
	if _, err := cleanup.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=ANY($2::text[])`, string(repo), strs(plan.Order)); err != nil {
		t.Fatal(err)
	}
	if _, err := cleanup.Exec(ctx, `DELETE FROM blobs WHERE hash=ANY($1::text[])`, strs(plan.Order)); err != nil {
		t.Fatal(err)
	}
	stored := make(chan error, 1)
	go func() {
		_, err := st.PutVerifiedDoc(ctx, repo, doc)
		stored <- err
	}()
	select {
	case err := <-stored:
		t.Fatal("writer did not wait for the cleanup's chunk locks", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := cleanup.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stored:
		if err != nil {
			t.Fatal("writer failed after the cleanup committed", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("writer still blocked after the cleanup committed")
	}
	if read, err := st.ReadVerifiedDoc(ctx, repo, doc.Hash()); err != nil || !bytes.Equal(read.Bytes(), doc.Bytes()) {
		t.Fatal("document stored across the cleanup is incomplete", err)
	}
	if result := fixture.sweep(t, repo); result.Orphans != 0 {
		t.Fatalf("stored document's chunks reported as orphans: %+v", result)
	}
}

// A visit deletes in several short transactions and stops starting new ones
// when its deadline is near; what is left stays due for the next visit.
func TestPGDocChunkSweepWorksInBoundedTransactions(t *testing.T) {
	fixture := newChunkSweepFixture(t)
	st, ctx, repo := fixture.store, fixture.ctx, fixture.repo
	chunks := make(map[domain.ContentHash][]byte)
	for index := 0; index < 5; index++ {
		body := []byte(fmt.Sprint(t.Name(), repo, index))
		chunks[domain.HashContent(body)] = body
	}
	if _, _, err := st.PutChunks(ctx, repo, chunks); err != nil {
		t.Fatal(err)
	}
	fixture.sweep(t, repo)
	fixture.clock = fixture.clock.Add(docChunkSweepGrace)

	// Too close to the deadline to start a transaction: nothing is deleted
	// and nothing is reported as an error.
	short, cancel := context.WithTimeout(ctx, docChunkSweepReserve-time.Second)
	result, err := st.SweepOrphanDocChunks(short, repo)
	cancel()
	if err != nil || result.Removed != 0 || result.Deferred != len(chunks) {
		t.Fatal("visit started a deletion it had no time for", result, err)
	}

	// One grant per transaction still clears the whole backlog in one visit,
	// and fast transactions let the batch grow again.
	st.chunkSweep.batch = 1
	fixture.clock = fixture.clock.Add(docChunkSweepRevisit)
	if result := fixture.sweep(t, repo); result.Removed != len(chunks) || result.Bodies != len(chunks) || result.Deferred != 0 {
		t.Fatalf("backlog larger than one transaction: %+v", result)
	}
	if st.chunkSweep.batch <= 1 || st.chunkSweep.batch > docChunkSweepBatch {
		t.Fatal("batch size did not adapt within its bounds", st.chunkSweep.batch)
	}
	if owned := fixture.owned(t, repo, mapKeys(chunks)); len(owned) != 0 {
		t.Fatal("orphans survived", len(owned))
	}
}

// More orphans than the worker tracks are worked off in turns. Every orphan
// still waits for its own grace period, counted from when it was first tracked.
func TestPGDocChunkSweepWorksOffABacklogLargerThanItTracks(t *testing.T) {
	fixture := newChunkSweepFixture(t)
	st, ctx, repo := fixture.store, fixture.ctx, fixture.repo
	st.chunkSweep.perRepo = 2
	chunks := make(map[domain.ContentHash][]byte)
	for index := 0; index < 5; index++ {
		body := []byte(fmt.Sprint(t.Name(), repo, index))
		chunks[domain.HashContent(body)] = body
	}
	if _, _, err := st.PutChunks(ctx, repo, chunks); err != nil {
		t.Fatal(err)
	}
	remaining := len(chunks)
	for turn := 0; remaining > 0; turn++ {
		if turn > len(chunks) {
			t.Fatal("backlog is not shrinking", remaining)
		}
		// First sighting of the next orphans: nothing is deleted, whatever
		// age the orphans tracked before them had reached.
		if result := fixture.sweep(t, repo); result.Orphans != remaining || result.Removed != 0 || result.Waiting != remaining {
			t.Fatalf("turn %d, first sighting: %+v", turn, result)
		}
		fixture.clock = fixture.clock.Add(docChunkSweepGrace)
		tracked := min(remaining, 2)
		result := fixture.sweep(t, repo)
		if result.Orphans != remaining || result.Removed != tracked || result.Waiting != remaining-tracked {
			t.Fatalf("turn %d, after the grace period: %+v", turn, result)
		}
		remaining -= tracked
		if owned := fixture.owned(t, repo, mapKeys(chunks)); len(owned) != remaining {
			t.Fatal("orphans the worker was not tracking were deleted", len(owned), remaining)
		}
		// With orphans left the repository is due again on the next tick.
		state, queued := st.chunkSweep.repos[repo]
		if queued != (remaining > 0) || (queued && !state.visit.Equal(fixture.clock.Add(docChunkSweepRevisit))) {
			t.Fatal("remaining backlog was not scheduled", queued, remaining)
		}
		fixture.clock = fixture.clock.Add(docChunkSweepRevisit)
	}
	if st.chunkSweep.tracked != 0 {
		t.Fatal("tracking count drifted", st.chunkSweep.tracked)
	}
}

func mapKeys(chunks map[domain.ContentHash][]byte) []domain.ContentHash {
	keys := make([]domain.ContentHash, 0, len(chunks))
	for hash := range chunks {
		keys = append(keys, hash)
	}
	return keys
}

// Chunk bodies are shared between repositories. Removing one repository's
// grant must leave the body, the other repository's access and the shared
// namespace's storage account alone.
func TestPGDocChunkSweepKeepsBodiesAnotherRepositoryOwns(t *testing.T) {
	fixture := newChunkSweepFixture(t)
	st, ctx, repo := fixture.store, fixture.ctx, fixture.repo
	other := fixture.addRepo(t, "second")
	doc := fixture.conversation(t, "", 20)
	plan := fixture.plan(t, doc)
	if _, err := st.PutVerifiedDoc(ctx, other, doc); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.PutChunks(ctx, repo, plan.Bodies); err != nil {
		t.Fatal(err)
	}
	before := fixture.usage(t)
	fixture.sweep(t, repo)
	fixture.clock = fixture.clock.Add(docChunkSweepGrace)
	result := fixture.sweep(t, repo)
	if result.Removed != len(plan.Bodies) || result.Bodies != 0 || result.Bytes != 0 {
		t.Fatalf("shared bodies: %+v", result)
	}
	if owned := fixture.owned(t, repo, plan.Order); len(owned) != 0 {
		t.Fatal("orphan grants survived", len(owned))
	}
	if owned := fixture.owned(t, other, plan.Order); len(owned) != len(plan.Bodies) {
		t.Fatal("cleanup of one repository removed another repository's chunks")
	}
	if read, err := st.ReadVerifiedDoc(ctx, other, doc.Hash()); err != nil || !bytes.Equal(read.Bytes(), doc.Bytes()) {
		t.Fatal("other repository's document no longer reads back", err)
	}
	// The other repository's sweep sees its own manifest and removes nothing.
	fixture.sweep(t, other)
	fixture.clock = fixture.clock.Add(docChunkSweepGrace)
	if result := fixture.sweep(t, other); result.Orphans != 0 || result.Removed != 0 {
		t.Fatalf("owning repository: %+v", result)
	}
	if after := fixture.usage(t); after.CurrentBytes != before.CurrentBytes {
		t.Fatalf("namespace still owns every body but was charged %d bytes differently", after.CurrentBytes-before.CurrentBytes)
	}
}

// A document object the cleanup cannot account for completely leaves its
// repository untouched: an unknown representation may name chunks in a way
// this code does not understand.
func TestPGDocChunkSweepRefusesRepositoriesItCannotClassify(t *testing.T) {
	fixture := newChunkSweepFixture(t)
	st, ctx, repo := fixture.store, fixture.ctx, fixture.repo
	body := []byte(t.Name() + string(repo))
	chunk := domain.HashContent(body)
	if _, _, err := st.PutChunks(ctx, repo, map[domain.ContentHash][]byte{chunk: body}); err != nil {
		t.Fatal(err)
	}
	fixture.sweep(t, repo)
	fixture.clock = fixture.clock.Add(docChunkSweepGrace)
	unknown := []byte(`{"descriptor":"future","parts":["` + string(chunk) + `"]}`)
	hash := domain.HashContent(unknown)
	if _, err := st.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(hash), docCompress(unknown)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, string(repo), string(hash)); err != nil {
		t.Fatal(err)
	}
	if result, err := st.SweepOrphanDocChunks(ctx, repo); !errors.Is(err, domain.ErrIntegrity) || result.Removed != 0 {
		t.Fatal("repository with an unknown document representation was swept", result, err)
	}
	if !fixture.owned(t, repo, []domain.ContentHash{chunk})[chunk] || !fixture.stored(t, chunk) {
		t.Fatal("chunk deleted beside an unknown document representation")
	}
	// Once the unknown object is gone the repository is swept as usual.
	if _, err := st.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, string(repo), string(hash)); err != nil {
		t.Fatal(err)
	}
	fixture.clock = fixture.clock.Add(docChunkSweepRevisit)
	if result := fixture.sweep(t, repo); result.Removed != 1 {
		t.Fatalf("after the unknown object was removed: %+v", result)
	}
}

// The worker entry point walks repositories that own chunks, comes back to a
// repository when its orphans are due, and reports when a pass is finished.
func TestPGDocChunkSweepNextVisitsDueRepositoriesAndEndsPasses(t *testing.T) {
	fixture := newChunkSweepFixture(t)
	st, ctx, repo := fixture.store, fixture.ctx, fixture.repo
	body := []byte(t.Name() + string(repo))
	chunk := domain.HashContent(body)
	if _, _, err := st.PutChunks(ctx, repo, map[domain.ContentHash][]byte{chunk: body}); err != nil {
		t.Fatal(err)
	}
	// Start the pass immediately before this repository; the test database
	// holds the repositories of every other test as well.
	st.chunkSweep.cursor = string(repo[:len(repo)-1])
	first, err := st.SweepNextDocChunks(ctx)
	if err != nil || first.Repo != repo || first.Orphans != 1 || first.Removed != 0 {
		t.Fatal("pass did not visit the repository", first, err)
	}
	if st.chunkSweep.cursor != string(repo) {
		t.Fatal("pass did not advance", st.chunkSweep.cursor)
	}
	// End the pass: nothing is visited until the next one is due, except a
	// repository whose tracked orphans reach the end of their grace period.
	st.chunkSweep.cursor = "sha256:" + strings.Repeat("g", 64)
	if idle, err := st.SweepNextDocChunks(ctx); err != nil || idle.Repo != "" {
		t.Fatal("pass did not end", idle, err)
	}
	if st.chunkSweep.cursor != "" || !st.chunkSweep.passAfter.Equal(fixture.clock.Add(docChunkSweepPass)) {
		t.Fatal("next pass was not scheduled", st.chunkSweep.cursor, st.chunkSweep.passAfter)
	}
	fixture.clock = fixture.clock.Add(docChunkSweepPass - time.Second)
	if idle, err := st.SweepNextDocChunks(ctx); err != nil || idle.Repo != "" {
		t.Fatal("pass restarted early or revisited a repository before its grace period", idle, err)
	}
	fixture.clock = fixture.clock.Add(docChunkSweepGrace - docChunkSweepPass + time.Second)
	st.chunkSweep.passAfter = fixture.clock.Add(time.Hour)
	due, err := st.SweepNextDocChunks(ctx)
	if err != nil || due.Repo != repo || due.Removed != 1 || due.Bodies != 1 {
		t.Fatal("due repository was not revisited", due, err)
	}
	if len(st.chunkSweep.repos) != 0 || st.chunkSweep.tracked != 0 {
		t.Fatal("cleaned repository is still tracked", len(st.chunkSweep.repos), st.chunkSweep.tracked)
	}
	if idle, err := st.SweepNextDocChunks(ctx); err != nil || idle.Repo != "" {
		t.Fatal("nothing was left to visit", idle, err)
	}
	// A caller's transaction cannot host the sweep's own lock order.
	err = st.WithinRepository(ctx, repo, func(ctx context.Context) error {
		_, err := st.SweepNextDocChunks(ctx)
		return err
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatal("sweep ran inside a caller's transaction", err)
	}
}

func TestDocChunkDependenciesClassifiesStoredDocuments(t *testing.T) {
	ctx := context.Background()
	cir := chunkBigDoc(20)
	canonical, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(canonical)
	plan, chunked := domain.PlanDocChunks(canonical)
	if !chunked {
		t.Fatal("fixture must be chunked")
	}
	manifest, err := json.Marshal(plan.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	same := func(got, want []domain.ContentHash) bool {
		return fmt.Sprint(got) == fmt.Sprint(want)
	}

	// A chunk manifest names its chunks, compressed or not.
	for _, stored := range [][]byte{docCompress(manifest), manifest} {
		if chunks, err := docChunkDependencies(ctx, hash, stored); err != nil || !same(chunks, plan.Manifest.Chunks) {
			t.Fatal("chunk manifest", len(chunks), err)
		}
	}
	// A conversation root names the chunks of its manifest.
	root, _, err := domain.ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	rootBytes, err := domain.CanonicalConversationManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	rootHash, err := domain.ConversationManifestHash(root)
	if err != nil {
		t.Fatal(err)
	}
	if chunks, err := docChunkDependencies(ctx, rootHash, docCompress(rootBytes)); err != nil || !same(chunks, conversationChunkOrder(root)) || len(chunks) == 0 {
		t.Fatal("conversation root", len(chunks), err)
	}
	if _, err := docChunkDependencies(ctx, hash, docCompress(rootBytes)); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("conversation root stored under another hash was accepted", err)
	}
	// A whole-body document names nothing, in canonical or legacy spelling.
	if chunks, err := docChunkDependencies(ctx, hash, docCompress(canonical)); err != nil || len(chunks) != 0 {
		t.Fatal("canonical whole body", len(chunks), err)
	}
	legacy, err := json.MarshalIndent(cir, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if chunks, err := docChunkDependencies(ctx, hash, docCompress(legacy)); err != nil || len(chunks) != 0 {
		t.Fatal("legacy whole body", len(chunks), err)
	}
	// Anything else is refused: wrong hash, unknown fields, trailing data,
	// undecodable bytes, or a body too large to decode here.
	for name, stored := range map[string][]byte{
		"another document's body": docCompress(bytes.Replace(canonical, []byte(`"claude"`), []byte(`"codex"`), 1)),
		"unknown descriptor":      docCompress([]byte(`{"descriptor":"future","parts":[]}`)),
		"trailing value":          docCompress(append(append([]byte(nil), canonical...), []byte(`{}`)...)),
		"corrupt frame":           {0x28, 0xb5, 0x2f, 0xfd, 0xff, 0xff},
	} {
		if _, err := docChunkDependencies(ctx, hash, stored); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal(name, "was classified", err)
		}
	}
	oversized := docCompress(bytes.Repeat([]byte(" "), docChunkSweepDocBytes+1))
	if _, err := docChunkDependencies(ctx, hash, oversized); !errors.Is(err, domain.ErrDocumentResourceLimit) {
		t.Fatal("oversized body was decoded", err)
	}
}
