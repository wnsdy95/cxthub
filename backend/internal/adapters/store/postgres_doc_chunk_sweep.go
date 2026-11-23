//go:build postgres

package store

// Orphan document chunk cleanup for PostgreSQL: the counterpart of
// FSStore.repackRepo's mark and sweep.
//
// A repository owns a chunk through a repo_blobs row of kind 'chunk'. Deleting
// a document (DeleteDoc) removes the manifest only, so the chunks nothing else
// names stay behind: typically the partial tail of an earlier capture of a
// conversation that has since grown. The sweep removes those grants and then
// the chunk bodies no repository owns any more.
//
// A chunk is live while any of these names it:
//   - the stored manifest of a document the repository owns,
//   - the stored manifest of a document one of its snapshots points at,
//   - a finalization job that is waiting, running or retrying.
//
// An upload in progress has stored chunks whose manifest does not exist yet.
// Those survive because an orphan is deleted only after this process has seen
// the same grant unreferenced for a whole grace period. The period starts when
// the grant is first seen, which is after its transaction committed. It is kept
// in memory: a restart only delays the cleanup.
//
// Deletion runs in one repository transaction (WithinRepository), the lock
// every job enqueue and job completion takes first. Inside it the sweep pins
// the storage account the metering trigger will update, then takes the raw
// representation advisory locks of the candidate chunks, the same locks PutDoc
// and PutChunks take before they touch a chunk, and finally the rows. Those
// three steps never wait: a candidate another transaction is using is left for
// a later visit. With the locks held the live set is computed again, so a
// manifest or job committed in the meantime keeps its chunks.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const (
	// docChunkSweepGrace is how long a chunk must stay unreferenced before it
	// is deleted. It has to outlast the longest upload: chunks arrive first
	// and the manifest that names them last.
	docChunkSweepGrace = time.Hour
	// docChunkSweepPass spaces out full passes over the repositories. A pass
	// only discovers orphans; a repository is revisited when its own grace
	// period ends, whatever the pass is doing.
	docChunkSweepPass = time.Hour
	// docChunkSweepRevisit is the shortest time before a repository whose
	// orphans were in use is looked at again.
	docChunkSweepRevisit = time.Minute
	// One transaction deletes at most docChunkSweepBatch grants. Deleting a
	// chunk body makes PostgreSQL check every table that references blobs, and
	// snapshots has no index on memory_hash, so the cost of one body grows
	// with the deployment. The batch therefore starts small and is resized to
	// keep the transaction, and with it the repository lock, near
	// docChunkSweepHold.
	docChunkSweepBatch      = 256
	docChunkSweepFirstBatch = 16
	docChunkSweepHold       = time.Second
	// A visit does not start another transaction this close to its deadline.
	docChunkSweepReserve = 5 * time.Second
	// Orphans tracked per repository and per process. Untracked orphans are
	// picked up once tracked ones have been deleted.
	docChunkSweepRepoTracked = 16384
	docChunkSweepTracked     = 65536
	// docChunkSweepCachedRefs bounds the cached manifest chunk references.
	docChunkSweepCachedRefs = 1 << 20
	// docChunkSweepDocBytes bounds a stored document object the sweep decodes.
	// Manifests are far smaller; a larger legacy whole-body document leaves
	// its repository unswept rather than decoded in a serving process.
	docChunkSweepDocBytes = 16 << 20
	// A lock wait here means a writer is using what the sweep wants. Give up
	// before the deadlock detector (deadlock_timeout, 1s by default) could
	// pick that writer as its victim.
	docChunkSweepLockTimeout = `SET LOCAL lock_timeout='500ms'`
)

var _ outbound.DocChunkMaintenance = (*PostgresStore)(nil)

// docChunkSweepSource identifies the exact bytes of a stored document object:
// an update advances the version, delete and re-insert changes the incarnation.
type docChunkSweepSource struct {
	hash        domain.ContentHash
	incarnation string
	version     int64
}

type docChunkObservation struct {
	xmin  uint32
	first time.Time
}

type docChunkSweepRepo struct {
	chunks map[domain.ContentHash]docChunkObservation
	visit  time.Time
}

type docChunkSweeper struct {
	run       sync.Mutex
	now       func() time.Time
	grace     time.Duration
	perRepo   int
	cursor    string
	passAfter time.Time
	repos     map[domain.ContentHash]*docChunkSweepRepo
	tracked   int
	manifests map[docChunkSweepSource][]uint64
	refs      int
	batch     int
}

func (sweeper *docChunkSweeper) clock() time.Time {
	if sweeper.now != nil {
		return sweeper.now()
	}
	return time.Now()
}

func (sweeper *docChunkSweeper) gracePeriod() time.Duration {
	if sweeper.grace > 0 {
		return sweeper.grace
	}
	return docChunkSweepGrace
}

func (sweeper *docChunkSweeper) repoLimit() int {
	if sweeper.perRepo > 0 {
		return sweeper.perRepo
	}
	return docChunkSweepRepoTracked
}

// docChunkLiveID is the first 64 bits of a chunk digest. The live set holds
// these instead of whole hashes; two chunks sharing a prefix can only keep an
// orphan, never delete a live chunk.
func docChunkLiveID(hash domain.ContentHash) (uint64, error) {
	if err := validateHash(hash); err != nil {
		return 0, fmt.Errorf("%w: invalid chunk reference", domain.ErrIntegrity)
	}
	var prefix [8]byte
	if _, err := hex.Decode(prefix[:], []byte(hexOf(hash)[:16])); err != nil {
		return 0, fmt.Errorf("%w: invalid chunk reference", domain.ErrIntegrity)
	}
	return binary.BigEndian.Uint64(prefix[:]), nil
}

// docChunkDependencies returns the chunks a stored document object names. It
// classifies the object the way readers do and refuses anything it cannot
// account for completely: an unknown descriptor may depend on chunks without
// this code knowing how it names them.
func docChunkDependencies(ctx context.Context, hash domain.ContentHash, raw []byte) ([]domain.ContentHash, error) {
	root, recognized, err := storedConversationManifest(ctx, raw)
	if err != nil {
		return nil, err
	}
	if recognized {
		want, err := domain.ConversationManifestHash(root)
		if err != nil || want != hash {
			return nil, fmt.Errorf("%w: stored conversation root does not match its hash", domain.ErrIntegrity)
		}
		return conversationChunkOrder(root), nil
	}
	body, err := boundedLegacyObject(ctx, raw, docChunkSweepDocBytes)
	if err != nil {
		return nil, err
	}
	if manifest, chunked := domain.ParseDocChunkManifest(body); chunked {
		return manifest.Chunks, nil
	}
	// A whole-body document names no chunks, but only a body that decodes as
	// exactly one CIR document and hashes to its name is known to be one.
	var document domain.CIRDocument
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("%w: stored document is not a known representation", domain.ErrIntegrity)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("%w: stored document is not a known representation", domain.ErrIntegrity)
	}
	if domain.HashContent(body) != hash {
		canonical, err := domain.CanonicalBytes(document)
		if err != nil || domain.HashContent(canonical) != hash {
			return nil, fmt.Errorf("%w: stored document does not match its hash", domain.ErrIntegrity)
		}
	}
	return nil, ctx.Err()
}

// liveDocChunks returns the chunks the repository's stored manifests and
// pending finalization jobs name. Any failure leaves the repository unswept.
func (s *PostgresStore) liveDocChunks(ctx context.Context, db pgDatabase, repo domain.ContentHash) (map[uint64]struct{}, error) {
	live := make(map[uint64]struct{})
	mark := func(chunks []domain.ContentHash) error {
		for _, chunk := range chunks {
			id, err := docChunkLiveID(chunk)
			if err != nil {
				return err
			}
			live[id] = struct{}{}
		}
		return nil
	}
	rows, err := db.Query(ctx, `SELECT payload FROM doc_finalization_jobs
 WHERE repo_id=$1 AND state IN ('waiting','running','retrying')`, string(repo))
	if err != nil {
		return nil, err
	}
	var payloads [][]byte
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			rows.Close()
			return nil, err
		}
		payloads = append(payloads, payload)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, payload := range payloads {
		var job domain.DocFinalizationJob
		if err := json.Unmarshal(payload, &job); err != nil {
			return nil, fmt.Errorf("%w: pending finalization job is unreadable", domain.ErrIntegrity)
		}
		chunks, err := publicationJobChunks(job)
		if err != nil {
			return nil, fmt.Errorf("pending finalization job %s: %w", job.ID, err)
		}
		if err := mark(chunks); err != nil {
			return nil, err
		}
	}

	// A snapshot's document counts even if the repository's grant for it is
	// gone: its manifest outlives DeleteDoc and may be stored again.
	rows, err = db.Query(ctx, `SELECT b.hash,COALESCE(i.incarnation::text,''),b.doc_range_source_version,octet_length(b.bytes)
 FROM (SELECT hash FROM repo_blobs WHERE repo_id=$1 AND kind='doc'
       UNION SELECT doc_hash FROM snapshots WHERE repo_id=$1) roots
 JOIN blobs b ON b.hash=roots.hash
 LEFT JOIN blob_source_incarnations i ON i.hash=b.hash
 ORDER BY b.hash`, string(repo))
	if err != nil {
		return nil, err
	}
	sweeper := &s.chunkSweep
	var missing []string
	var batchBytes int64
	var batches [][]string
	flush := func() {
		if len(missing) > 0 {
			batches = append(batches, missing)
			missing, batchBytes = nil, 0
		}
	}
	for rows.Next() {
		var source docChunkSweepSource
		var size int64
		if err := rows.Scan(&source.hash, &source.incarnation, &source.version, &size); err != nil {
			rows.Close()
			return nil, err
		}
		if cached, ok := sweeper.manifests[source]; ok && source.incarnation != "" {
			for _, id := range cached {
				live[id] = struct{}{}
			}
			continue
		}
		if size > 2*docChunkSweepDocBytes {
			rows.Close()
			return nil, fmt.Errorf("document %s: %w", source.hash, domain.ErrDocumentResourceLimit)
		}
		if len(missing) >= 64 || batchBytes+size > 4<<20 {
			flush()
		}
		missing = append(missing, string(source.hash))
		batchBytes += size
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	flush()
	for _, batch := range batches {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, err := db.Query(ctx, `SELECT b.hash,COALESCE(i.incarnation::text,''),b.doc_range_source_version,b.bytes
 FROM blobs b LEFT JOIN blob_source_incarnations i ON i.hash=b.hash
 WHERE b.hash=ANY($1::text[])`, batch)
		if err != nil {
			return nil, err
		}
		type object struct {
			source docChunkSweepSource
			raw    []byte
		}
		var objects []object
		for rows.Next() {
			var next object
			if err := rows.Scan(&next.source.hash, &next.source.incarnation, &next.source.version, &next.raw); err != nil {
				rows.Close()
				return nil, err
			}
			objects = append(objects, next)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		// A document deleted since the first query no longer names anything.
		for _, object := range objects {
			chunks, err := docChunkDependencies(ctx, object.source.hash, object.raw)
			if err != nil {
				return nil, fmt.Errorf("document %s: %w", object.source.hash, err)
			}
			ids := make([]uint64, 0, len(chunks))
			for _, chunk := range chunks {
				id, err := docChunkLiveID(chunk)
				if err != nil {
					return nil, fmt.Errorf("document %s: %w", object.source.hash, err)
				}
				ids = append(ids, id)
				live[id] = struct{}{}
			}
			if object.source.incarnation != "" {
				if _, replaced := sweeper.manifests[object.source]; !replaced {
					sweeper.refs += len(ids)
				}
				sweeper.manifests[object.source] = ids
			}
		}
	}
	return live, ctx.Err()
}

// SweepOrphanDocChunks visits one repository: it records the chunk grants no
// manifest or pending job names, and deletes the ones this process has now
// seen unreferenced for the whole grace period.
func (s *PostgresStore) SweepOrphanDocChunks(ctx context.Context, repo domain.ContentHash) (domain.DocChunkSweep, error) {
	if err := validateHash(repo); err != nil {
		return domain.DocChunkSweep{}, err
	}
	if s.InTransaction(ctx) {
		// The sweep owns its transactions and their lock order.
		return domain.DocChunkSweep{}, domain.ErrConflict
	}
	s.chunkSweep.run.Lock()
	defer s.chunkSweep.run.Unlock()
	return s.sweepOrphanDocChunks(ctx, repo)
}

// SweepNextDocChunks visits the next repository that needs it: first one whose
// tracked orphans have reached the end of their grace period, otherwise the
// next repository of the pass over all repositories that own chunks. A zero
// Repo means there is nothing to visit until the next pass is due.
func (s *PostgresStore) SweepNextDocChunks(ctx context.Context) (domain.DocChunkSweep, error) {
	if s.InTransaction(ctx) {
		return domain.DocChunkSweep{}, domain.ErrConflict
	}
	sweeper := &s.chunkSweep
	sweeper.run.Lock()
	defer sweeper.run.Unlock()
	now := sweeper.clock()
	var due domain.ContentHash
	for repo, state := range sweeper.repos {
		if !state.visit.After(now) && (due == "" || repo < due) {
			due = repo
		}
	}
	if due != "" {
		return s.sweepOrphanDocChunks(ctx, due)
	}
	if now.Before(sweeper.passAfter) {
		return domain.DocChunkSweep{}, nil
	}
	var next string
	err := s.pool.QueryRow(ctx, `SELECT repo_id FROM repo_blobs WHERE kind='chunk' AND repo_id>$1 ORDER BY repo_id LIMIT 1`, sweeper.cursor).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		sweeper.cursor, sweeper.passAfter = "", now.Add(docChunkSweepPass)
		return domain.DocChunkSweep{}, nil
	}
	if err != nil {
		return domain.DocChunkSweep{}, err
	}
	// Advance first: a repository that cannot be swept must not stop the pass.
	sweeper.cursor = next
	return s.sweepOrphanDocChunks(ctx, domain.ContentHash(next))
}

type docChunkOrphan struct {
	hash domain.ContentHash
	xmin uint32
}

func (s *PostgresStore) sweepOrphanDocChunks(ctx context.Context, repo domain.ContentHash) (domain.DocChunkSweep, error) {
	sweeper := &s.chunkSweep
	result := domain.DocChunkSweep{Repo: repo}
	if sweeper.repos == nil {
		sweeper.repos = make(map[domain.ContentHash]*docChunkSweepRepo)
	}
	if sweeper.manifests == nil || sweeper.refs > docChunkSweepCachedRefs {
		// Nothing is evicted during a visit, so the check under the locks finds
		// every manifest the first read parsed.
		sweeper.manifests, sweeper.refs = make(map[docChunkSweepSource][]uint64), 0
	}
	now := sweeper.clock()
	if state := sweeper.repos[repo]; state != nil {
		// A visit that fails is not repeated before the next pass would be.
		state.visit = now.Add(docChunkSweepPass)
	}
	live, err := s.liveDocChunks(ctx, s.pool, repo)
	if err != nil {
		return result, fmt.Errorf("repository %s: %w", repo, err)
	}
	rows, err := s.pool.Query(ctx, `SELECT hash,xmin::text FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' ORDER BY hash`, string(repo))
	if err != nil {
		return result, err
	}
	var orphans []docChunkOrphan
	for rows.Next() {
		var hash domain.ContentHash
		var inserted string
		if err := rows.Scan(&hash, &inserted); err != nil {
			rows.Close()
			return result, err
		}
		id, err := docChunkLiveID(hash)
		if err != nil {
			continue // Not a chunk any manifest can name; leave it alone.
		}
		if _, referenced := live[id]; referenced {
			continue
		}
		result.Orphans++
		if len(orphans) < sweeper.repoLimit() {
			xmin, err := strconv.ParseUint(inserted, 10, 32)
			if err != nil {
				rows.Close()
				return result, fmt.Errorf("%w: unreadable row version", domain.ErrIntegrity)
			}
			orphans = append(orphans, docChunkOrphan{hash: hash, xmin: uint32(xmin)})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, err
	}

	// Keep the first sighting of a grant that is still the same row; a grant
	// deleted and stored again is a new upload and starts a new grace period.
	previous := sweeper.repos[repo]
	state := &docChunkSweepRepo{chunks: make(map[domain.ContentHash]docChunkObservation, len(orphans))}
	if previous != nil {
		sweeper.tracked -= len(previous.chunks)
	}
	grace := sweeper.gracePeriod()
	var due []docChunkOrphan
	for _, orphan := range orphans {
		seen := docChunkObservation{xmin: orphan.xmin, first: now}
		if previous != nil {
			if earlier, ok := previous.chunks[orphan.hash]; ok && earlier.xmin == orphan.xmin {
				seen = earlier
			}
		}
		if seen.first.Equal(now) && sweeper.tracked+len(state.chunks) >= docChunkSweepTracked {
			continue
		}
		state.chunks[orphan.hash] = seen
		if now.Sub(seen.first) >= grace {
			due = append(due, orphan)
		}
	}
	sweeper.tracked += len(state.chunks)
	delete(sweeper.repos, repo)
	if len(state.chunks) > 0 {
		sweeper.repos[repo] = state
	}

	if sweeper.batch < 1 {
		sweeper.batch = docChunkSweepFirstBatch
	}
	for start := 0; start < len(due); {
		if ctx.Err() != nil {
			break
		}
		if deadline, bounded := ctx.Deadline(); bounded && time.Until(deadline) < docChunkSweepReserve {
			break // The rest stays due and is deleted on the next visit.
		}
		batch := due[start:min(start+sweeper.batch, len(due))]
		start += len(batch)
		removed, bodies, size, held, err := s.removeOrphanDocChunks(ctx, repo, batch)
		switch {
		case held > docChunkSweepHold:
			sweeper.batch = max(sweeper.batch/2, 1)
		case err == nil && held < docChunkSweepHold/4:
			sweeper.batch = min(sweeper.batch*2, docChunkSweepBatch)
		}
		for _, hash := range removed {
			if _, ok := state.chunks[hash]; ok {
				delete(state.chunks, hash)
				sweeper.tracked--
			}
		}
		result.Removed += len(removed)
		result.Bodies += bodies
		result.Bytes += size
		if err != nil {
			// An interrupted visit continues on the next tick; any other
			// failure is not repeated before the next pass would be.
			state.visit = now.Add(docChunkSweepPass)
			if ctx.Err() != nil {
				state.visit = now.Add(docChunkSweepRevisit)
			}
			if len(state.chunks) == 0 {
				delete(sweeper.repos, repo)
			}
			result.Deferred = len(due) - result.Removed
			result.Waiting = result.Orphans - len(due)
			return result, fmt.Errorf("repository %s: %w", repo, err)
		}
	}
	result.Deferred = len(due) - result.Removed
	result.Waiting = result.Orphans - len(due)
	if len(state.chunks) == 0 {
		delete(sweeper.repos, repo)
		if result.Removed > 0 && result.Orphans > result.Removed {
			// Tracking was full. This visit made room, so start the grace
			// period of the remaining orphans on the next tick instead of
			// waiting for the next pass.
			state.visit = now.Add(docChunkSweepRevisit)
			sweeper.repos[repo] = state
		}
		return result, nil
	}
	// Come back when the earliest tracked orphan is due; orphans left in use
	// by a writer are retried after a short pause instead of in a loop.
	state.visit = now.Add(docChunkSweepRevisit)
	if result.Deferred == 0 {
		earliest := now
		for _, seen := range state.chunks {
			if seen.first.Before(earliest) {
				earliest = seen.first
			}
		}
		if at := earliest.Add(grace); at.After(state.visit) {
			state.visit = at
		}
	}
	return result, nil
}

// removeOrphanDocChunks deletes the repository's grants for the given orphans
// and the chunk bodies that are left without any grant. It returns the grants
// it removed; the rest were in use or referenced again and stay. held is how
// long the transaction had the repository lock before it committed.
func (s *PostgresStore) removeOrphanDocChunks(ctx context.Context, repo domain.ContentHash, orphans []docChunkOrphan) (removed []domain.ContentHash, bodies int, size int64, held time.Duration, err error) {
	err = s.WithinRepository(ctx, repo, func(ctx context.Context) error {
		removed, bodies, size = nil, 0, 0
		locked := time.Now()
		defer func() { held = time.Since(locked) }()
		tx := s.db(ctx)
		// From here on this transaction holds the repository lock, so it must
		// not wait long for anything.
		if _, err := tx.Exec(ctx, docChunkSweepLockTimeout); err != nil {
			return err
		}
		// Deleting a grant updates the owning namespace's storage account.
		// Take that row now, in the order writers reach it (repository rows,
		// then the account), so the sweep never waits for it while it holds
		// the chunk locks a metered writer may be waiting for.
		var namespace *string
		if err := tx.QueryRow(ctx, `SELECT w.owner_namespace_id FROM repos r JOIN repositories w ON w.id=r.repository_id WHERE r.id=$1`, string(repo)).Scan(&namespace); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if namespace != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO storage_accounts(namespace_id) VALUES($1) ON CONFLICT DO NOTHING`, *namespace); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT 1 FROM storage_accounts WHERE namespace_id=$1 FOR UPDATE`, *namespace); err != nil {
				return err
			}
		}

		// A writer holds the advisory lock of every chunk it stores or names
		// until it commits. Taking it exclusively without waiting skips the
		// chunks of writes in flight and makes later writers wait for this
		// transaction; they then store the chunk again.
		keys := make([]int32, 0, len(orphans))
		wanted := make(map[int32]bool, len(orphans))
		for _, orphan := range orphans {
			if key := rawRepresentationLockKey(orphan.hash); !wanted[key] {
				wanted[key] = true
				keys = append(keys, key)
			}
		}
		sort.Slice(keys, func(left, right int) bool { return keys[left] < keys[right] })
		rows, err := tx.Query(ctx, `SELECT wanted.key,pg_try_advisory_xact_lock($1,wanted.key) FROM unnest($2::int4[]) AS wanted(key)`, rawRepresentationLockSpace, keys)
		if err != nil {
			return err
		}
		free := make(map[int32]bool, len(keys))
		for rows.Next() {
			var key int32
			var acquired bool
			if err := rows.Scan(&key, &acquired); err != nil {
				rows.Close()
				return err
			}
			free[key] = acquired
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		expected := make(map[domain.ContentHash]uint32, len(orphans))
		var candidates []string
		for _, orphan := range orphans {
			if free[rawRepresentationLockKey(orphan.hash)] {
				expected[orphan.hash] = orphan.xmin
				candidates = append(candidates, string(orphan.hash))
			}
		}
		if len(candidates) == 0 {
			return nil
		}

		// The body row lock excludes every new reference to the body: a grant
		// or snapshot insert has to key-share lock it. A body someone holds is
		// skipped together with its grant, so no grant is removed while its
		// body cannot be judged in the same transaction.
		stored, err := lockedDocChunkRows(ctx, tx, `SELECT hash,'0' FROM blobs WHERE hash=ANY($1::text[]) ORDER BY hash FOR UPDATE SKIP LOCKED`, candidates)
		if err != nil {
			return err
		}
		candidates = candidates[:0]
		for hash := range stored {
			candidates = append(candidates, string(hash))
		}
		// Root publication and job enqueue share-lock the grants they rely on.
		grants, err := lockedDocChunkRows(ctx, tx, `SELECT hash,xmin::text FROM repo_blobs
 WHERE repo_id=$1 AND kind='chunk' AND hash=ANY($2::text[]) ORDER BY hash FOR UPDATE SKIP LOCKED`, string(repo), candidates)
		if err != nil {
			return err
		}
		// Every committed manifest and job is visible now, and none can be
		// added for these chunks until this transaction ends.
		live, err := s.liveDocChunks(ctx, tx, repo)
		if err != nil {
			return err
		}
		candidates = candidates[:0]
		for hash, xmin := range grants {
			id, err := docChunkLiveID(hash)
			if err != nil {
				continue
			}
			if _, referenced := live[id]; referenced || xmin != expected[hash] {
				continue
			}
			candidates = append(candidates, string(hash))
		}
		if len(candidates) == 0 {
			return nil
		}
		sort.Strings(candidates)
		// Grants first: the metering trigger then sees whether the namespace
		// still owns the body and adjusts its storage account.
		if _, err := tx.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=ANY($2::text[])`, string(repo), candidates); err != nil {
			return err
		}
		// A snapshot keeps a body through its foreign keys even without a
		// grant; such a body is left, not failed on. The two conditions are
		// separate so the unindexed one is one scan for the whole batch.
		freed, err := tx.Query(ctx, `DELETE FROM blobs b WHERE b.hash=ANY($1::text[])
 AND NOT EXISTS (SELECT 1 FROM repo_blobs rb WHERE rb.hash=b.hash)
 AND NOT EXISTS (SELECT 1 FROM snapshots s WHERE s.doc_hash=b.hash)
 AND NOT EXISTS (SELECT 1 FROM snapshots s WHERE s.memory_hash=b.hash)
 RETURNING octet_length(b.bytes)`, candidates)
		if err != nil {
			return err
		}
		for freed.Next() {
			var length int64
			if err := freed.Scan(&length); err != nil {
				freed.Close()
				return err
			}
			bodies++
			size += length
		}
		freed.Close()
		if err := freed.Err(); err != nil {
			return err
		}
		for _, hash := range candidates {
			removed = append(removed, domain.ContentHash(hash))
		}
		return ctx.Err()
	})
	if err != nil {
		removed, bodies, size = nil, 0, 0
		var failure *pgconn.PgError
		if errors.As(err, &failure) && failure.Code == "55P03" {
			// lock_not_available: a writer got there first; retry later.
			return nil, 0, 0, held, nil
		}
	}
	return removed, bodies, size, held, err
}

func lockedDocChunkRows(ctx context.Context, tx pgDatabase, query string, arguments ...any) (map[domain.ContentHash]uint32, error) {
	rows, err := tx.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	locked := make(map[domain.ContentHash]uint32)
	for rows.Next() {
		var hash domain.ContentHash
		var inserted string
		if err := rows.Scan(&hash, &inserted); err != nil {
			return nil, err
		}
		xmin, err := strconv.ParseUint(inserted, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%w: unreadable row version", domain.ErrIntegrity)
		}
		locked[hash] = uint32(xmin)
	}
	return locked, rows.Err()
}
