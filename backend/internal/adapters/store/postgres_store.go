//go:build postgres

// PostgresStore is an implementation of outbound.MetadataStore + BlobStore for PostgreSQL (pgx).
//
// Compiled with `go build -tags postgres` (default build is FSStore). Schema source:
// schemas/db/migrations/0001_init.sql. CI validates transactions against real PostgreSQL.
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// PostgresStore stores metadata and content in PostgreSQL (repos/blobs/snapshots/refs/memories).
type PostgresStore struct {
	docProofs                    docProofCache
	legacyBases                  legacyBaseHints
	pool                         *pgxpool.Pool
	legacyByteRangePublication   bool
	legacyDerivedBytePublication bool
}

// NewPostgresStore connects to a pgx pool using a dsn.
func NewPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	config.ConnConfig.Tracer = graphQueryTracer{}
	// Never acknowledge writes before durable WAL flush, even if a role/DSN
	// supplies asynchronous commit. Preserve the stronger replay acknowledgement.
	if config.ConnConfig.RuntimeParams["synchronous_commit"] != "remote_apply" {
		config.ConnConfig.RuntimeParams["synchronous_commit"] = "on"
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	var durable bool
	if err := pool.QueryRow(ctx, `SELECT current_setting('fsync')='on' AND current_setting('full_page_writes')='on'`).Scan(&durable); err != nil || !durable {
		pool.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("PostgreSQL requires fsync=on and full_page_writes=on for durable context storage")
	}
	return &PostgresStore{pool: pool}, nil
}

// Close releases this instance's connection pool.
func (s *PostgresStore) Close() { s.pool.Close() }

// Ping checks this process's database connection for deployment readiness.
func (s *PostgresStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

var _ Store = (*PostgresStore)(nil)

func mapNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	return err
}

func mapPGConstraint(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return fmt.Errorf("%w: unique storage constraint", domain.ErrConflict)
		case "23514":
			return fmt.Errorf("%w: storage check constraint", domain.ErrValidation)
		}
	}
	return err
}

func strs(hs []domain.ContentHash) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = string(h)
	}
	return out
}

func hashes(ss []string) []domain.ContentHash {
	out := make([]domain.ContentHash, len(ss))
	for i, s := range ss {
		out[i] = domain.ContentHash(s)
	}
	return out
}

// --- Repo ---

func (s *PostgresStore) GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error) {
	if err := validateHash(id); err != nil {
		return domain.Repo{}, err
	}
	var r domain.Repo
	var topics string
	err := s.db(ctx).QueryRow(ctx, `SELECT id, remote_url, default_branch, COALESCE(repository_id,''), COALESCE(git_remote_url,''), COALESCE(protect_default,false), context_protocol, required_doc_identity,
 COALESCE(description,''), COALESCE(website,''), COALESCE(topics,'null') FROM repos WHERE id=$1`, string(id)).
		Scan(&r.ID, &r.RemoteURL, &r.DefaultBranch, &r.RepositoryID, &r.GitRemoteURL, &r.ProtectDefault, &r.ContextProtocol, &r.RequiredDocIdentity, &r.Description, &r.Website, &topics)
	if err != nil {
		return domain.Repo{}, mapNoRows(err)
	}
	if err := validateHash(r.ID); err != nil || r.ID != id {
		return domain.Repo{}, domain.ErrIntegrity
	}
	if r.DefaultBranch != "" {
		if err := domain.ValidateBranchName(r.DefaultBranch); err != nil {
			return domain.Repo{}, err
		}
	}
	if err := r.RequiredDocIdentity.Validate(); err != nil {
		return domain.Repo{}, err
	}
	if err := json.Unmarshal([]byte(topics), &r.Topics); err != nil {
		return domain.Repo{}, domain.ErrIntegrity
	}
	r.RemoteURL = domain.SanitizeRemoteURL(r.RemoteURL)
	r.GitRemoteURL = domain.SanitizeRemoteURL(r.GitRemoteURL)
	return r, nil
}

func (s *PostgresStore) PutRepo(ctx context.Context, repo domain.Repo) (domain.Repo, error) {
	if repo.RequiredDocIdentity != domain.DocumentIdentityLegacy {
		return domain.Repo{}, domain.ErrValidation
	}
	if err := validateHash(repo.ID); err != nil {
		return domain.Repo{}, storageWriteError(mapPGConstraint(err))
	}
	if repo.DefaultBranch != "" {
		if err := domain.ValidateBranchName(repo.DefaultBranch); err != nil {
			return domain.Repo{}, storageWriteError(mapPGConstraint(err))
		}
	}
	repo.RemoteURL = domain.SanitizeRemoteURL(repo.RemoteURL)
	repo.GitRemoteURL = domain.SanitizeRemoteURL(repo.GitRemoteURL)
	db := repo.DefaultBranch
	if db == "" {
		db = "main"
	}
	tag, err := s.db(ctx).Exec(ctx,
		`INSERT INTO repos (id, remote_url, default_branch, team, repository_id, git_remote_url) VALUES ($1,$2,$3,$4,NULLIF($5,''),$6)
		 ON CONFLICT (id) DO UPDATE SET repository_id = COALESCE(repos.repository_id, EXCLUDED.repository_id),
		   default_branch = repos.default_branch,
		   git_remote_url = COALESCE(NULLIF(repos.git_remote_url,''), EXCLUDED.git_remote_url)
           WHERE repos.repository_id IS NULL OR EXCLUDED.repository_id IS NULL OR repos.repository_id=EXCLUDED.repository_id`,
		string(repo.ID), repo.RemoteURL, db, "default", repo.RepositoryID, repo.GitRemoteURL)
	if err != nil {
		return domain.Repo{}, storageWriteError(mapPGConstraint(err))
	}
	if tag.RowsAffected() == 0 {
		return domain.Repo{}, domain.ErrConflict
	}
	return s.GetRepo(ctx, repo.ID)
}

func (s *PostgresStore) ListRepos(ctx context.Context, team string) ([]domain.Repo, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT id, remote_url, default_branch, COALESCE(repository_id,''), COALESCE(git_remote_url,''), COALESCE(protect_default,false), context_protocol, required_doc_identity,
 COALESCE(description,''), COALESCE(website,''), COALESCE(topics,'null') FROM repos WHERE team=$1`, team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Repo
	for rows.Next() {
		var r domain.Repo
		var topics string
		if err := rows.Scan(&r.ID, &r.RemoteURL, &r.DefaultBranch, &r.RepositoryID, &r.GitRemoteURL, &r.ProtectDefault, &r.ContextProtocol, &r.RequiredDocIdentity, &r.Description, &r.Website, &topics); err != nil {
			return nil, err
		}
		if err := validateHash(r.ID); err != nil {
			return nil, err
		}
		if r.DefaultBranch != "" {
			if err := domain.ValidateBranchName(r.DefaultBranch); err != nil {
				return nil, err
			}
		}
		if err := r.RequiredDocIdentity.Validate(); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(topics), &r.Topics); err != nil {
			return nil, domain.ErrIntegrity
		}
		r.RemoteURL = domain.SanitizeRemoteURL(r.RemoteURL)
		r.GitRemoteURL = domain.SanitizeRemoteURL(r.GitRemoteURL)
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- Snapshot Metadata ---

func scanSnapshot(row pgx.Row) (domain.Snapshot, error) {
	var snap domain.Snapshot
	var parents []string
	var graftParents []string
	var models []string
	var memHash *string
	if err := row.Scan(&snap.ID, &snap.RepoID, &snap.Branch, &parents, &snap.DocHash, &memHash,
		&snap.ClaudeSettings, &snap.AgentsSettings, &snap.CodexSettings,
		&snap.Provider, &snap.Fidelity, &snap.Message,
		&snap.Author.Name, &snap.Author.Email, &snap.Author.Team, &snap.CreatedAt, &snap.Grafted, &snap.SessionID, &models, &snap.CompactionCount, &graftParents, &snap.GraftSeq, &snap.DocIdentity); err != nil {
		return domain.Snapshot{}, err
	}
	snap.Parents = hashes(parents)
	if len(graftParents) > 0 {
		snap.GraftParents = hashes(graftParents)
	}
	if len(models) > 0 {
		snap.Models = models
	}
	if memHash != nil {
		snap.MemoryHash = domain.ContentHash(*memHash)
	}
	if err := validateSnapshotRefs(snap); err != nil {
		return domain.Snapshot{}, err
	}
	return snap, nil
}

const snapCols = `id, repo_id, branch, parents, doc_hash, memory_hash, COALESCE(claude_settings,''), COALESCE(agents_settings,''), COALESCE(codex_settings,''), provider, fidelity, message, author_name, author_email, author_team, created_at, grafted, COALESCE(session_id,''), COALESCE(models,'{}'), COALESCE(compaction_count,0), COALESCE(graft_parents,'{}'), COALESCE(graft_seq,0), doc_identity`

func (s *PostgresStore) GetSnapshot(ctx context.Context, repoID, id domain.ContentHash) (domain.Snapshot, error) {
	if err := validateHashes(repoID, id); err != nil {
		return domain.Snapshot{}, err
	}
	row := s.db(ctx).QueryRow(ctx, `SELECT `+snapCols+` FROM snapshots WHERE repo_id=$1 AND id=$2`, string(repoID), string(id))
	snap, err := scanSnapshot(row)
	if err != nil {
		return domain.Snapshot{}, mapNoRows(err)
	}
	return snap, nil
}

func lockRepoGraph(ctx context.Context, tx pgx.Tx, repoID domain.ContentHash) error {
	// Graft requires serializing the entire Add/Join read-check-write for each repo using advisory xact locks.
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), 1735289201)`, string(repoID))
	return err
}

// --- Ref / manifest ---

// --- Memory Meta ---

func (s *PostgresStore) PutMemoryMeta(ctx context.Context, repoID domain.ContentHash, digest domain.MemoryDigest) error {
	if err := validateHashes(repoID, digest.SnapshotID); err != nil {
		return err
	}
	// A nil slice is bound as SQL NULL, violating the NOT NULL constraint (0001) — normalized to an empty array.
	// (PG retest empirical verification: SQLSTATE 23502).
	keyFacts, openTasks := digest.KeyFacts, digest.OpenTasks
	if keyFacts == nil {
		keyFacts = []string{}
	}
	if openTasks == nil {
		openTasks = []string{}
	}
	_, err := s.db(ctx).Exec(ctx,
		`INSERT INTO memories (repo_id, snapshot_id, summary, key_facts, open_tasks, provider) VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (repo_id, snapshot_id) DO UPDATE SET summary=EXCLUDED.summary, key_facts=EXCLUDED.key_facts, open_tasks=EXCLUDED.open_tasks`,
		string(repoID), string(digest.SnapshotID), digest.Summary, keyFacts, openTasks, string(digest.Provider))
	return err
}

// --- BlobStore ---

func (s *PostgresStore) PutDoc(ctx context.Context, repoID domain.ContentHash, doc domain.SessionDoc) (bool, error) {
	if err := validateHash(repoID); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	verified, err := domain.VerifySessionDoc(doc)
	if err != nil {
		return false, err
	}
	return s.PutVerifiedDoc(ctx, repoID, verified)
}

func (s *PostgresStore) PutVerifiedDoc(ctx context.Context, repoID domain.ContentHash, doc domain.VerifiedSessionDoc) (bool, error) {
	if doc.DocumentRef().Identity != domain.DocumentIdentityLegacy {
		return false, domain.ErrUnsupportedDocumentIdentity
	}
	if err := validateHash(repoID); err != nil {
		return false, err
	}
	prepared, err := prepareVerifiedDocPG(ctx, doc)
	if err != nil {
		return false, err
	}
	return s.putPreparedDoc(ctx, repoID, prepared)
}

func (s *PostgresStore) putPreparedDoc(ctx context.Context, repoID domain.ContentHash, prepared preparedDocPG) (bool, error) {
	if prepared.doc.DocumentRef().Identity != domain.DocumentIdentityLegacy {
		return false, domain.ErrUnsupportedDocumentIdentity
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	doc := prepared.doc
	s.docProofs.put(docProofKey{repo: repoID, expected: doc.Hash(), representation: doc.Hash()}, doc.Reference())
	return rawRepresentationTransaction(ctx, s, func(tx pgx.Tx, attempt *rawRepresentationAttempt) (bool, error) {
		return s.putPreparedDocInRawTransaction(ctx, tx, repoID, prepared, attempt)
	})
}

func (s *PostgresStore) putPreparedDocInRawTransaction(ctx context.Context, tx pgx.Tx, repoID domain.ContentHash, prepared preparedDocPG, attempt *rawRepresentationAttempt) (bool, error) {
	doc := prepared.doc
	// Lock order is doc → chunks, matching GetDocManifest. A new manifest row is
	// invisible until this transaction also stores its chunks and commits.
	plan, chunked := prepared.chunks, prepared.chunked
	if err := attempt.lock(ctx, tx, doc.Hash(), plan.Order); err != nil {
		return false, err
	}
	ct, err := tx.Exec(ctx, `INSERT INTO blobs (hash, bytes) VALUES ($1,$2) ON CONFLICT (hash) DO NOTHING`,
		string(doc.Hash()), prepared.payload)
	if err != nil {
		return false, err
	}
	created := ct.RowsAffected() > 0
	// Blob validation: For existing rows that are legacy blobs or manifests, the original/reshuffled content must match the canonical bytes — blocking content injection under the hash (maintaining existing contract).
	var stored []byte
	if err := tx.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1 FOR UPDATE`, string(doc.Hash())).Scan(&stored); err != nil {
		return false, err
	}
	stored, err = docDecompress(stored)
	if err != nil {
		return false, fmt.Errorf("%w: stored blob undecodable for doc %s", domain.ErrIntegrity, doc.Hash())
	}
	storedMan, isMan := domain.ParseDocChunkManifest(stored)
	// Matching manifests describe the exact verified stream. The chunk loop
	// below still compares each current stored body under its retention lock.
	// Only legacy/differently partitioned representations need full assembly.
	matchingManifest := !created && chunked && isMan && sameDocManifest(storedMan, plan.Manifest)
	if !created && !matchingManifest {
		canonical := doc.Bytes()
		if cb, _, cerr := s.reassembleManifestTx(ctx, tx, stored, doc.Hash()); isMan {
			if cerr != nil || !bytes.Equal(cb, canonical) {
				return false, fmt.Errorf("%w: stored manifest disagrees with doc hash %s", domain.ErrIntegrity, doc.Hash())
			}
		} else if !bytes.Equal(stored, canonical) {
			return false, fmt.Errorf("%w: stored blob disagrees with doc hash %s", domain.ErrIntegrity, doc.Hash())
		}
	}
	if chunked {
		for _, ch := range attempt.order(plan.Order) {
			if err := s.retainRawBlobPG(ctx, tx, ch, plan.Bodies[ch], !matchingManifest, attempt.exclusive[ch]); err != nil {
				return false, err
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO repo_blobs (repo_id, kind, hash) VALUES ($1,'chunk',$2) ON CONFLICT DO NOTHING`,
				string(repoID), string(ch)); err != nil {
				return false, err
			}
		}
	}
	if !created && chunked && (!isMan || storedMan.Format != plan.Manifest.Format) {
		// A global blob can be shared by several repos. Migrate a legacy/v1 body
		// only after every existing doc owner can read the v2 chunks. The current
		// writer already owns them from the planning loop above.
		for _, ch := range plan.Order {
			if _, err := tx.Exec(ctx,
				`INSERT INTO repo_blobs (repo_id, kind, hash)
				 SELECT repo_id, 'chunk', $2 FROM repo_blobs WHERE kind='doc' AND hash=$1
				 ON CONFLICT DO NOTHING`, string(doc.Hash()), string(ch)); err != nil {
				return false, err
			}
		}
		if err := preserveRawBlobPG(ctx, tx, doc.Hash(), prepared.payload); err != nil {
			return false, err
		}
	} else if isMan {
		// If the stored representation is newer/different from this writer's plan,
		// ownership must follow the manifest that readers will actually assemble.
		for _, ch := range storedMan.Chunks {
			if _, err := tx.Exec(ctx,
				`INSERT INTO repo_blobs (repo_id, kind, hash) VALUES ($1,'chunk',$2) ON CONFLICT DO NOTHING`,
				string(repoID), string(ch)); err != nil {
				return false, err
			}
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO repo_blobs (repo_id, kind, hash) VALUES ($1,'doc',$2) ON CONFLICT DO NOTHING`,
		string(repoID), string(doc.Hash())); err != nil {
		return false, err
	}
	if err := putPreparedReadIndexPG(ctx, tx, prepared.read); err != nil {
		return false, err
	}
	return created, nil
}

// reassembleManifestTx reassembles canonical bytes from chunks in blobs if the stored blob is a manifest (for PutDoc blob validation — transaction integrity check, unnecessary owner join).
func (s *PostgresStore) reassembleManifestTx(ctx context.Context, tx pgx.Tx, data []byte, want domain.ContentHash) ([]byte, bool, error) {
	man, isMan := domain.ParseDocChunkManifest(data)
	if !isMan {
		return nil, false, nil
	}
	chunks := make([][]byte, 0, len(man.Chunks))
	for _, ch := range man.Chunks {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(ch)).Scan(&raw); err != nil {
			return nil, true, err
		}
		c, err := docDecompress(raw)
		if err != nil {
			return nil, true, err
		}
		chunks = append(chunks, c)
	}
	cb, err := domain.AssembleDocChunks(man, chunks, want)
	return cb, true, err
}

func (s *PostgresStore) GetDoc(ctx context.Context, repoID, hash domain.ContentHash) (domain.SessionDoc, error) {
	data, chunked, err := s.readDocBytes(ctx, repoID, hash)
	if err != nil {
		return domain.SessionDoc{}, err
	}
	var cir domain.CIRDocument
	if err := json.Unmarshal(data, &cir); err != nil {
		return domain.SessionDoc{}, err
	}
	doc := domain.SessionDoc{Hash: hash, CIR: cir}
	if !chunked {
		if err := domain.ValidateSessionDocHash(doc); err != nil {
			return domain.SessionDoc{}, err
		}
	}
	return doc, nil
}

// PutChunks stores content and its repository ownership grant in one transaction.
func (s *PostgresStore) PutChunks(ctx context.Context, repoID domain.ContentHash, chunks map[domain.ContentHash][]byte) (int, int, error) {
	if err := validateHash(repoID); err != nil {
		return 0, 0, err
	}
	counts, err := rawRepresentationTransaction(ctx, s, func(tx pgx.Tx, attempt *rawRepresentationAttempt) ([2]int, error) {
		return s.putChunksInRawTransaction(ctx, tx, repoID, chunks, attempt)
	})
	return counts[0], counts[1], err
}

func (s *PostgresStore) putChunksInRawTransaction(ctx context.Context, tx pgx.Tx, repoID domain.ContentHash, chunks map[domain.ContentHash][]byte, attempt *rawRepresentationAttempt) ([2]int, error) {
	stored, deduped := 0, 0
	ordered := attempt.mapOrder(chunks)
	if err := attempt.lock(ctx, tx, "", ordered); err != nil {
		return [2]int{}, err
	}
	for _, hash := range ordered {
		body := chunks[hash]
		if err := validateHash(hash); err != nil {
			return [2]int{}, err
		}
		if domain.HashContent(body) != hash {
			return [2]int{}, domain.ErrIntegrity
		}
		if err := s.retainRawBlobPG(ctx, tx, hash, body, true, attempt.exclusive[hash]); err != nil {
			return [2]int{}, err
		}
		ct, err := tx.Exec(ctx,
			`INSERT INTO repo_blobs (repo_id, kind, hash) VALUES ($1,'chunk',$2) ON CONFLICT DO NOTHING`,
			string(repoID), string(hash))
		if err != nil {
			return [2]int{}, err
		}
		if ct.RowsAffected() == 1 {
			stored++
		} else {
			deduped++
		}
	}
	return [2]int{stored, deduped}, nil
}

// HasChunks returns the list of owned chunks in the repo (negotiate delta negotiation).
func (s *PostgresStore) HasChunks(ctx context.Context, repoID domain.ContentHash, hs []domain.ContentHash) ([]domain.ContentHash, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	if err := validateHashes(hs...); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx,
		`SELECT hash FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash = ANY($2)`,
		string(repoID), strs(hs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var have []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		have = append(have, h)
	}
	return hashes(have), rows.Err()
}

// GetChunk returns the body of the owned chunk (uncompressed) from the repo (pull chunk wire).
func (s *PostgresStore) GetChunk(ctx context.Context, repoID, hash domain.ContentHash) ([]byte, error) {
	if err := validateHashes(repoID, hash); err != nil {
		return nil, err
	}
	var raw []byte
	if err := s.db(ctx).QueryRow(ctx,
		`SELECT b.bytes FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
		 WHERE rb.repo_id=$1 AND rb.kind='chunk' AND rb.hash=$2`,
		string(repoID), string(hash)).Scan(&raw); err != nil {
		return nil, mapNoRows(err)
	}
	return docDecompress(raw)
}

// GetDocManifest returns the chunk manifest of the doc (pull delta). If the storage is monolithic (legacy),
// it calculates the plan from canonical and stores it along with chunk ownership (lazy repack — symmetrical to FS).
func (s *PostgresStore) GetDocManifest(ctx context.Context, repoID, hash domain.ContentHash) (domain.DocChunkManifest, error) {
	if err := validateHashes(repoID, hash); err != nil {
		return domain.DocChunkManifest{}, err
	}
	// Normal pulls are read-only snapshots. Existing manifests need neither a
	// write lock nor a savepoint, and must remain readable during publication.
	var current []byte
	if err := s.db(ctx).QueryRow(ctx, `SELECT b.bytes FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
	 WHERE rb.repo_id=$1 AND rb.kind='doc' AND rb.hash=$2`, string(repoID), string(hash)).Scan(&current); err != nil {
		return domain.DocChunkManifest{}, mapNoRows(err)
	}
	if err := rejectStoredRoot(ctx, current); err != nil {
		return domain.DocChunkManifest{}, err
	}
	current, err := docDecompress(current)
	if err != nil {
		return domain.DocChunkManifest{}, domain.ErrIntegrity
	}
	if man, ok := domain.ParseDocChunkManifest(current); ok {
		return man, nil
	}
	if outbound.DocReadOnly(ctx) {
		return domain.DocChunkManifest{}, domain.ErrAgentHistoryUnavailable
	}
	if tx, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx); ok && tx.readOnly {
		// The caller falls back to the legacy body in the same consistent read.
		// Lazy repacking belongs to a writable maintenance operation.
		return domain.DocChunkManifest{}, domain.ErrNotFound
	}
	current, err = canonicalRawDocumentPG(current, hash)
	if err != nil {
		return domain.DocChunkManifest{}, err
	}
	plan, ok := domain.PlanDocChunks(current)
	if !ok {
		return domain.DocChunkManifest{}, domain.ErrNotFound
	}
	manifest, err := rawRepresentationTransaction(ctx, s, func(tx pgx.Tx, attempt *rawRepresentationAttempt) (domain.DocChunkManifest, error) {
		return s.getDocManifestInRawTransaction(ctx, tx, repoID, hash, current, plan, attempt)
	})
	if err == domain.ErrStorageLimit && ctx.Err() == nil && !s.InTransaction(ctx) {
		return domain.DocChunkManifest{}, domain.ErrNotFound
	}
	return manifest, err
}

func (s *PostgresStore) getDocManifestInRawTransaction(ctx context.Context, tx pgx.Tx, repoID, hash domain.ContentHash, expected []byte, plan domain.DocChunkPlan, attempt *rawRepresentationAttempt) (domain.DocChunkManifest, error) {
	if err := attempt.lock(ctx, tx, hash, plan.Order); err != nil {
		return domain.DocChunkManifest{}, err
	}
	var raw []byte
	if err := tx.QueryRow(ctx,
		`SELECT b.bytes FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
		 WHERE rb.repo_id=$1 AND rb.kind='doc' AND rb.hash=$2 FOR UPDATE OF b`,
		string(repoID), string(hash)).Scan(&raw); err != nil {
		return domain.DocChunkManifest{}, mapNoRows(err)
	}
	if err := rejectStoredRoot(ctx, raw); err != nil {
		return domain.DocChunkManifest{}, err
	}
	data, err := docDecompress(raw)
	if err != nil {
		return domain.DocChunkManifest{}, domain.ErrIntegrity
	}
	if man, isMan := domain.ParseDocChunkManifest(data); isMan {
		return man, nil
	}
	data, err = canonicalRawDocumentPG(data, hash)
	if err != nil || !bytes.Equal(data, expected) {
		return domain.DocChunkManifest{}, domain.ErrIntegrity
	}
	for _, ch := range attempt.order(plan.Order) {
		if err := s.retainRawBlobPG(ctx, tx, ch, plan.Bodies[ch], true, attempt.exclusive[ch]); err != nil {
			return domain.DocChunkManifest{}, err
		}
		// blobs are globally deduplicated while ownership is repo-scoped. Before
		// replacing the shared doc body with a manifest, grant every current doc
		// owner all referenced chunks so another repo cannot lose read access.
		if _, err := tx.Exec(ctx,
			`INSERT INTO repo_blobs (repo_id, kind, hash)
			 SELECT repo_id, 'chunk', $2 FROM repo_blobs WHERE kind='doc' AND hash=$1
			 ON CONFLICT DO NOTHING`, string(hash), string(ch)); err != nil {
			return domain.DocChunkManifest{}, err
		}
	}
	manifest, err := json.Marshal(plan.Manifest)
	if err != nil {
		return domain.DocChunkManifest{}, err
	}
	if err := preserveRawBlobPG(ctx, tx, hash, docCompress(manifest)); err != nil {
		return domain.DocChunkManifest{}, err
	}
	return plan.Manifest, nil
}

// getDocChunkedPG reassembles the manifest blob from the repo owned chunks. If isManifest=false, it's legacy.
func (s *PostgresStore) getDocChunkedPG(ctx context.Context, repoID, hash domain.ContentHash, data []byte) ([]byte, bool, error) {
	limit := len(data)
	if limit > 64 {
		limit = 64
	}
	man, isMan := domain.ParseDocChunkManifest(data)
	if !isMan {
		return nil, false, nil
	}
	chunks := make([][]byte, 0, len(man.Chunks))
	for _, ch := range man.Chunks {
		if err := validateHash(ch); err != nil {
			return nil, true, domain.ErrIntegrity
		}
		var raw []byte
		if err := s.db(ctx).QueryRow(ctx,
			`SELECT b.bytes FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
			 WHERE rb.repo_id=$1 AND rb.kind='chunk' AND rb.hash=$2`,
			string(repoID), string(ch)).Scan(&raw); err != nil {
			return nil, true, fmt.Errorf("%w: doc %s missing chunk %s", domain.ErrNotFound, hash, ch)
		}
		c, err := docDecompress(raw)
		if err != nil {
			return nil, true, domain.ErrIntegrity
		}
		chunks = append(chunks, c)
	}
	cb, err := domain.AssembleDocChunks(man, chunks, hash)
	if err != nil {
		return nil, true, fmt.Errorf("%w: chunked doc %s reassembly hash mismatch", domain.ErrIntegrity, hash)
	}
	return cb, true, nil
}

func (s *PostgresStore) HasDocs(ctx context.Context, repoID domain.ContentHash, hs []domain.ContentHash) ([]domain.ContentHash, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	if err := validateHashes(hs...); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx,
		`SELECT hash FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash = ANY($2)`,
		string(repoID), strs(hs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var have []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		have = append(have, h)
	}
	return hashes(have), rows.Err()
}

func (s *PostgresStore) reassembleMemoryManifestTx(ctx context.Context, tx pgx.Tx, data []byte) (domain.MemoryDigest, bool, error) {
	manifest, isManifest, err := domain.ParseMemoryChunkManifest(data)
	if !isManifest || err != nil {
		return domain.MemoryDigest{}, isManifest, err
	}
	bodies := make(map[domain.ContentHash][]byte, len(manifest.SummaryChunks)+len(manifest.FragmentChunks))
	all := append(append([]domain.ContentHash{}, manifest.SummaryChunks...), manifest.FragmentChunks...)
	for _, chunkHash := range all {
		if _, loaded := bodies[chunkHash]; loaded {
			continue
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(chunkHash)).Scan(&raw); err != nil {
			return domain.MemoryDigest{}, true, err
		}
		body, err := docDecompress(raw)
		if err != nil {
			return domain.MemoryDigest{}, true, err
		}
		bodies[chunkHash] = body
	}
	digest, err := domain.AssembleMemoryChunks(manifest, bodies)
	return digest, true, err
}

// UpdateRepoAbout updates the About field (topics are stored as JSON text).
func (s *PostgresStore) UpdateRepoAbout(ctx context.Context, id domain.ContentHash, description, website string, topics []string) error {
	if err := validateHash(id); err != nil {
		return err
	}
	tj, _ := json.Marshal(topics)
	_, err := s.db(ctx).Exec(ctx,
		`UPDATE repos SET description=$2, website=$3, topics=$4 WHERE id=$1`,
		string(id), description, website, string(tj))
	return err
}

// UpdateRepoConfig updates the default branch and protected branch settings (nil = unchanged).
func (s *PostgresStore) UpdateRepoConfig(ctx context.Context, id domain.ContentHash, defaultBranch *string, protectDefault *bool) error {
	if err := validateHash(id); err != nil {
		return err
	}
	if defaultBranch != nil && *defaultBranch != "" {
		if err := domain.ValidateBranchName(*defaultBranch); err != nil {
			return err
		}
	}
	if defaultBranch != nil && *defaultBranch != "" {
		if _, err := s.db(ctx).Exec(ctx, `UPDATE repos SET default_branch=$2 WHERE id=$1`, string(id), *defaultBranch); err != nil {
			return err
		}
	}
	if protectDefault != nil {
		if _, err := s.db(ctx).Exec(ctx, `UPDATE repos SET protect_default=$2 WHERE id=$1`, string(id), *protectDefault); err != nil {
			return err
		}
	}
	return nil
}

// PutSettingsBundle upserts the settings bundle.
func (s *PostgresStore) PutSettingsBundle(ctx context.Context, repoID domain.ContentHash, bundle domain.SettingsBundle) error {
	if err := validateHash(repoID); err != nil {
		return storageWriteError(err)
	}
	if err := domain.ValidateSettingsBundle(bundle.Kind, "", bundle); err != nil {
		return storageWriteError(err)
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		return storageWriteError(err)
	}
	_, err = s.db(ctx).Exec(ctx,
		`INSERT INTO repo_settings (repo_id, kind, data) VALUES ($1,$2,$3)
		 ON CONFLICT (repo_id, kind) DO UPDATE SET data = EXCLUDED.data`,
		string(repoID), bundle.Kind, string(data))
	return storageWriteError(err)
}

// GetSettingsBundle retrieves the settings bundle.
func (s *PostgresStore) GetSettingsBundle(ctx context.Context, repoID domain.ContentHash, kind string) (domain.SettingsBundle, error) {
	if err := validateHash(repoID); err != nil {
		return domain.SettingsBundle{}, err
	}
	if err := validateSettingsKind(kind); err != nil {
		return domain.SettingsBundle{}, err
	}
	var raw string
	err := s.db(ctx).QueryRow(ctx, `SELECT data FROM repo_settings WHERE repo_id=$1 AND kind=$2`, string(repoID), kind).Scan(&raw)
	if err != nil {
		return domain.SettingsBundle{}, mapNoRows(err)
	}
	var b domain.SettingsBundle
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return domain.SettingsBundle{}, err
	}
	if err := domain.ValidateSettingsBundle(kind, "", b); err != nil {
		return domain.SettingsBundle{}, err
	}
	return b, nil
}

func (s *PostgresStore) DeleteDoc(ctx context.Context, repoID domain.ContentHash, hash domain.ContentHash) error {
	if err := validateHashes(repoID, hash); err != nil {
		return err
	}
	tx, err := s.db(ctx).BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	attempt := &rawRepresentationAttempt{}
	if err := attempt.lock(ctx, tx, hash, nil); err != nil {
		return err
	}
	var retained domain.ContentHash
	if err := tx.QueryRow(ctx, `SELECT hash FROM blobs WHERE hash=$1 FOR UPDATE`, hash).Scan(&retained); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`,
		string(repoID), string(hash)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM blobs b WHERE b.hash=$1
		   AND NOT EXISTS (SELECT 1 FROM repo_blobs rb WHERE rb.hash=b.hash)
		   AND NOT EXISTS (SELECT 1 FROM snapshots s WHERE s.doc_hash=b.hash OR s.memory_hash=b.hash)`,
		string(hash)); err != nil {
		return err
	}
	return storageWriteError(tx.Commit(ctx))
}

// PutSecretsEnvelope / GetSecretsEnvelope store opaque end-to-end encrypted secret payloads.
func (s *PostgresStore) PutSecretsEnvelope(ctx context.Context, repoID domain.ContentHash, raw []byte) error {
	if err := validateHash(repoID); err != nil {
		return storageWriteError(err)
	}
	return s.WithinRepository(ctx, repoID, func(ctx context.Context) error {
		_, err := s.db(ctx).Exec(ctx,
			`INSERT INTO repo_secrets (repo_id, data) VALUES ($1,$2)
		 ON CONFLICT (repo_id) DO UPDATE SET data = EXCLUDED.data`,
			string(repoID), string(raw))
		return storageWriteError(err)
	})
}

func (s *PostgresStore) GetSecretsEnvelope(ctx context.Context, repoID domain.ContentHash) ([]byte, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	var raw string
	err := s.db(ctx).QueryRow(ctx, `SELECT data FROM repo_secrets WHERE repo_id=$1`, string(repoID)).Scan(&raw)
	if err != nil {
		return nil, mapNoRows(err)
	}
	return []byte(raw), nil
}
