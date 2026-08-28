//go:build postgres

// PostgresStore is an implementation of outbound.MetadataStore + BlobStore for PostgreSQL (pgx).
//
// Compiled with `go build -tags postgres` (default build is FSStore). Schema source:
// schemas/db/migrations/0001_init.sql. Runtime validation requires an actual Postgres instance
// (no DB server in this environment for compile-time validation; runtime validation in deployment).
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// PostgresStore stores metadata and content in PostgreSQL (repos/blobs/snapshots/refs/memories).
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore connects to a pgx pool using a dsn.
func NewPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &PostgresStore{pool: pool}, nil
}

var _ Store = (*PostgresStore)(nil)

func mapNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
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

func emptyIfNil(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
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
	err := s.pool.QueryRow(ctx, `SELECT id, remote_url, default_branch, COALESCE(workspace_id,''), COALESCE(git_remote_url,''), COALESCE(protect_default,false) FROM repos WHERE id=$1`, string(id)).
		Scan(&r.ID, &r.RemoteURL, &r.DefaultBranch, &r.WorkspaceID, &r.GitRemoteURL, &r.ProtectDefault)
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
	r.RemoteURL = domain.SanitizeRemoteURL(r.RemoteURL)
	r.GitRemoteURL = domain.SanitizeRemoteURL(r.GitRemoteURL)
	return r, nil
}

func (s *PostgresStore) PutRepo(ctx context.Context, repo domain.Repo) (domain.Repo, error) {
	if err := validateHash(repo.ID); err != nil {
		return domain.Repo{}, err
	}
	if repo.DefaultBranch != "" {
		if err := domain.ValidateBranchName(repo.DefaultBranch); err != nil {
			return domain.Repo{}, err
		}
	}
	repo.RemoteURL = domain.SanitizeRemoteURL(repo.RemoteURL)
	repo.GitRemoteURL = domain.SanitizeRemoteURL(repo.GitRemoteURL)
	db := repo.DefaultBranch
	if db == "" {
		db = "main"
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO repos (id, remote_url, default_branch, team, workspace_id, git_remote_url) VALUES ($1,$2,$3,$4,NULLIF($5,''),$6)
		 ON CONFLICT (id) DO UPDATE SET workspace_id = COALESCE(repos.workspace_id, EXCLUDED.workspace_id),
		   default_branch = EXCLUDED.default_branch,
		   git_remote_url = COALESCE(NULLIF(EXCLUDED.git_remote_url,''), repos.git_remote_url)`,
		string(repo.ID), repo.RemoteURL, db, "default", repo.WorkspaceID, repo.GitRemoteURL)
	if err != nil {
		return domain.Repo{}, err
	}
	return s.GetRepo(ctx, repo.ID)
}

func (s *PostgresStore) ListRepos(ctx context.Context, team string) ([]domain.Repo, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, remote_url, default_branch, COALESCE(workspace_id,''), COALESCE(git_remote_url,''), COALESCE(protect_default,false) FROM repos WHERE team=$1`, team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Repo
	for rows.Next() {
		var r domain.Repo
		if err := rows.Scan(&r.ID, &r.RemoteURL, &r.DefaultBranch, &r.WorkspaceID, &r.GitRemoteURL, &r.ProtectDefault); err != nil {
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
		&snap.Author.Name, &snap.Author.Email, &snap.Author.Team, &snap.CreatedAt, &snap.Grafted, &snap.SessionID, &models, &snap.CompactionCount, &graftParents, &snap.GraftSeq); err != nil {
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

const snapCols = `id, repo_id, branch, parents, doc_hash, memory_hash, COALESCE(claude_settings,''), COALESCE(agents_settings,''), COALESCE(codex_settings,''), provider, fidelity, message, author_name, author_email, author_team, created_at, grafted, COALESCE(session_id,''), COALESCE(models,'{}'), COALESCE(compaction_count,0), COALESCE(graft_parents,'{}'), COALESCE(graft_seq,0)`

func (s *PostgresStore) GetSnapshot(ctx context.Context, repoID, id domain.ContentHash) (domain.Snapshot, error) {
	if err := validateHashes(repoID, id); err != nil {
		return domain.Snapshot{}, err
	}
	row := s.pool.QueryRow(ctx, `SELECT `+snapCols+` FROM snapshots WHERE repo_id=$1 AND id=$2`, string(repoID), string(id))
	snap, err := scanSnapshot(row)
	if err != nil {
		return domain.Snapshot{}, mapNoRows(err)
	}
	return snap, nil
}

func (s *PostgresStore) PutSnapshot(ctx context.Context, snap domain.Snapshot) error {
	if err := validateSnapshotRefs(snap); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, owned := range []struct {
		kind string
		hash domain.ContentHash
	}{{"doc", snap.DocHash}, {"memory", snap.MemoryHash}} {
		if owned.hash == "" {
			continue
		}
		var one int
		if err := tx.QueryRow(ctx,
			`SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind=$2 AND hash=$3 FOR KEY SHARE`,
			string(snap.RepoID), owned.kind, string(owned.hash)).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: repo does not own %s blob %s", domain.ErrIntegrity, owned.kind, owned.hash)
			}
			return err
		}
	}
	for _, hash := range []domain.ContentHash{snap.ClaudeSettings, snap.AgentsSettings, snap.CodexSettings} {
		if hash == "" {
			continue
		}
		var one int
		if err := tx.QueryRow(ctx,
			`SELECT 1 FROM settings_objects WHERE repo_id=$1 AND hash=$2 FOR KEY SHARE`,
			string(snap.RepoID), string(hash)).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: repo does not own settings object %s", domain.ErrIntegrity, hash)
			}
			return err
		}
	}
	var memHash *string
	if snap.MemoryHash != "" {
		m := string(snap.MemoryHash)
		memHash = &m
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO snapshots (id, repo_id, branch, parents, doc_hash, memory_hash, claude_settings, agents_settings, codex_settings, provider, fidelity, message, author_name, author_email, author_team, grafted, session_id, models, compaction_count, graft_parents, graft_seq)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21) ON CONFLICT (repo_id, id) DO NOTHING`,
		string(snap.ID), string(snap.RepoID), snap.Branch, strs(snap.Parents), string(snap.DocHash), memHash,
		string(snap.ClaudeSettings), string(snap.AgentsSettings), string(snap.CodexSettings),
		string(snap.Provider), string(snap.Fidelity), snap.Message,
		snap.Author.Name, snap.Author.Email, snap.Author.Team, snap.Grafted, snap.SessionID, emptyIfNil(snap.Models), snap.CompactionCount, strs(snap.GraftParents), snap.GraftSeq)
	if err != nil {
		return err
	}
	// stash → Commit promotion (like FS/CLI rules): If an existing row has the "(stash)" label and non-stash storage is
	// added, it updates branch/message to commit (hash-external derived metadata — ID/content immutable).
	if snap.Branch != domain.StashBranchLabel {
		_, err = tx.Exec(ctx,
			`UPDATE snapshots SET branch=$3, message=$4 WHERE repo_id=$1 AND id=$2 AND branch=$5`,
			string(snap.RepoID), string(snap.ID), snap.Branch, snap.Message, domain.StashBranchLabel)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AddGraftParents is an unconditional addition for internal server-side appends.
func (s *PostgresStore) AddGraftParents(ctx context.Context, repoID, id domain.ContentHash, add []domain.ContentHash) error {
	return s.addGraftParents(ctx, repoID, id, add, nil)
}

// AddGraftParentsCAS is a compare-and-add for delayed client events. It confirms the event once
// (current==expected) and retries the previous response only if current==expected+1, ensuring idempotency.
func (s *PostgresStore) AddGraftParentsCAS(ctx context.Context, repoID, id domain.ContentHash, add []domain.ContentHash, expected uint64) error {
	return s.addGraftParents(ctx, repoID, id, add, &expected)
}

func lockRepoGraph(ctx context.Context, tx pgx.Tx, repoID domain.ContentHash) error {
	// Graft requires serializing the entire Add/Join read-check-write for each repo using advisory xact locks.
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), 1735289201)`, string(repoID))
	return err
}

// requireSnapshotIDsPG checks if the graft/ref target exists in the same repo and protects the row
// until the transaction is committed. It is a storage invariant that prevents dangling edges/references
// from being created during the deletion or incorrect port calls between service pre-fetch and save mutations.
func requireSnapshotIDsPG(ctx context.Context, tx pgx.Tx, repoID domain.ContentHash, ids ...domain.ContentHash) error {
	wanted := make(map[string]bool, len(ids))
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || wanted[string(id)] {
			continue
		}
		wanted[string(id)] = true
		values = append(values, string(id))
	}
	if len(values) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx,
		`SELECT id FROM snapshots WHERE repo_id=$1 AND id=ANY($2::text[]) FOR KEY SHARE`,
		string(repoID), values)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		delete(wanted, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id := range wanted {
		return fmt.Errorf("%w: snapshot %s", domain.ErrNotFound, id)
	}
	return nil
}

func ensureNoReachabilityCycle(ctx context.Context, tx pgx.Tx, repoID domain.ContentHash) error {
	var cycle bool
	err := tx.QueryRow(ctx, `
		WITH RECURSIVE edges(child,parent) AS (
			SELECT id, unnest(COALESCE(parents,'{}'::text[]) || COALESCE(graft_parents,'{}'::text[]))
			  FROM snapshots WHERE repo_id=$1
		), reach(start,node) AS (
			SELECT child,parent FROM edges
			UNION
			SELECT reach.start, edges.parent
			  FROM reach JOIN edges ON edges.child=reach.node
		)
		SELECT EXISTS(SELECT 1 FROM reach WHERE start=node)`, string(repoID)).Scan(&cycle)
	if err != nil {
		return err
	}
	if cycle {
		return fmt.Errorf("%w: graft would create a reachability cycle", domain.ErrConflict)
	}
	return nil
}

func ensureJoinGraphScopePG(ctx context.Context, tx pgx.Tx, m outbound.JoinMutation) error {
	if len(m.Grafts) == 0 {
		return nil
	}
	segmentIDs := make([]string, 0, len(m.Segment))
	for _, id := range m.Segment {
		segmentIDs = append(segmentIDs, string(id))
	}
	attached := make(map[domain.ContentHash]bool)
	sessionPrefix := domain.SessionRefPrefix(m.Branch)
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE edges(child,parent) AS (
			SELECT id, unnest(COALESCE(parents,'{}'::text[]) || COALESCE(graft_parents,'{}'::text[]))
			  FROM snapshots WHERE repo_id=$1
		), reach(node) AS (
			SELECT target FROM refs
			 WHERE repo_id=$1 AND target IS NOT NULL
			   AND ((kind='branch' AND name=$2)
				        OR (kind='session' AND left(name,length($3))=$3))
			UNION
			SELECT edges.parent FROM reach JOIN edges ON edges.child=reach.node
		)
		SELECT node FROM reach`, string(m.RepoID), m.Branch, sessionPrefix)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		attached[domain.ContentHash(id)] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	byID := make(map[domain.ContentHash]domain.Snapshot)
	rows, err = tx.Query(ctx, `SELECT id, COALESCE(parents,'{}'::text[]) FROM snapshots WHERE repo_id=$1`, string(m.RepoID))
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var parents []string
		if err := rows.Scan(&id, &parents); err != nil {
			rows.Close()
			return err
		}
		hashID := domain.ContentHash(id)
		byID[hashID] = domain.Snapshot{ID: hashID, Parents: hashes(parents)}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if err := validateJoinSegmentTopology(m, byID, attached); err != nil {
		return err
	}
	ids := append([]string{}, segmentIDs...)
	for _, patch := range m.Grafts {
		if err := validateHash(patch.SnapshotID); err != nil {
			return err
		}
		ids = append(ids, string(patch.SnapshotID))
	}
	var blocked bool
	err = tx.QueryRow(ctx, `
		WITH RECURSIVE edges(child,parent) AS (
			SELECT id, unnest(COALESCE(parents,'{}'::text[]) || COALESCE(graft_parents,'{}'::text[]))
			  FROM snapshots WHERE repo_id=$1
		), reach(branch,node) AS (
			SELECT name,target FROM refs
			 WHERE repo_id=$1 AND target IS NOT NULL
			   AND ((kind='branch' AND name<>$2)
			        OR (kind='session' AND left(name,length($4))<>$4))
			UNION
			SELECT reach.branch, edges.parent
			  FROM reach JOIN edges ON edges.child=reach.node
		)
		SELECT EXISTS(SELECT 1 FROM reach WHERE node=ANY($3::text[]))`,
		string(m.RepoID), m.Branch, ids, sessionPrefix).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked {
		return fmt.Errorf("%w: join mutation is reachable from another git branch", domain.ErrConflict)
	}
	return nil
}

// addGraftParents groups read/seq-CAS/cycle-check/write into a single transaction.
func (s *PostgresStore) addGraftParents(ctx context.Context, repoID, id domain.ContentHash, add []domain.ContentHash, expected *uint64) error {
	if err := validateHashes(repoID, id); err != nil {
		return err
	}
	if err := validateHashes(add...); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockRepoGraph(ctx, tx, repoID); err != nil {
		return err
	}
	if err := requireSnapshotIDsPG(ctx, tx, repoID, add...); err != nil {
		return err
	}
	var parents, graft []string
	var seq uint64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(parents,'{}'), COALESCE(graft_parents,'{}'), COALESCE(graft_seq,0) FROM snapshots WHERE repo_id=$1 AND id=$2 FOR UPDATE`,
		string(repoID), string(id)).Scan(&parents, &graft, &seq); err != nil {
		return mapNoRows(err)
	}
	seen := map[string]bool{string(id): true}
	for _, p := range parents {
		seen[p] = true
	}
	for _, g := range graft {
		seen[g] = true
	}
	changed := false
	for _, a := range add {
		if a == "" || seen[string(a)] {
			continue
		}
		seen[string(a)] = true
		graft = append(graft, string(a))
		changed = true
	}
	if !changed {
		if expected == nil {
			return tx.Commit(ctx) // Server internal redundant calls are seq non-advancing.
		}
		switch {
		case seq == *expected:
			if seq == domain.MaxGraftSeq {
				return fmt.Errorf("%w: graft sequence exhausted", domain.ErrConflict)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE snapshots SET graft_seq=graft_seq+1 WHERE repo_id=$1 AND id=$2 AND graft_seq=$3`,
				string(repoID), string(id), seq); err != nil {
				return err
			}
			return tx.Commit(ctx)
		case *expected < domain.MaxGraftSeq && seq == *expected+1:
			return tx.Commit(ctx) // Loss of previous application response or queue retry.
		default:
			return domain.ErrConflict
		}
	}
	if expected != nil && seq != *expected {
		return domain.ErrConflict
	}
	if seq == domain.MaxGraftSeq {
		return fmt.Errorf("%w: graft sequence exhausted", domain.ErrConflict)
	}
	tag, err := tx.Exec(ctx, `UPDATE snapshots SET graft_parents=$3, grafted=true, graft_seq=COALESCE(graft_seq,0)+1 WHERE repo_id=$1 AND id=$2`,
		string(repoID), string(id), graft)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	if err := ensureNoReachabilityCycle(ctx, tx, repoID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CompareAndSwapSnapshotMemory advances the causal memory ref under a row lock.
func (s *PostgresStore) CompareAndSwapSnapshotMemory(ctx context.Context, repoID, id, expected, next domain.ContentHash) error {
	if err := validateHashes(repoID, id, next); err != nil {
		return err
	}
	if err := domain.ValidateOptionalContentHash(expected); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var current string
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(memory_hash,'') FROM snapshots WHERE repo_id=$1 AND id=$2 FOR UPDATE`,
		string(repoID), string(id)).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}
	if domain.ContentHash(current) == next {
		return tx.Commit(ctx)
	}
	if domain.ContentHash(current) != expected {
		return domain.ErrConflict
	}
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='memory' AND hash=$2)`,
		string(repoID), string(next)).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return domain.ErrNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE snapshots SET memory_hash=$3 WHERE repo_id=$1 AND id=$2`,
		string(repoID), string(id), string(next)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) ListSnapshots(ctx context.Context, repoID domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	q := `SELECT ` + snapCols + ` FROM snapshots WHERE repo_id=$1`
	args := []any{string(repoID)}
	if branch != "" {
		q += ` AND branch=$2`
		args = append(args, branch)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Snapshot
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

func (s *PostgresStore) HasSnapshots(ctx context.Context, repoID domain.ContentHash, ids []domain.ContentHash) ([]domain.ContentHash, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	if err := validateHashes(ids...); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM snapshots WHERE repo_id=$1 AND id = ANY($2)`, string(repoID), strs(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var have []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		have = append(have, id)
	}
	return hashes(have), rows.Err()
}

// --- Ref / manifest ---

func (s *PostgresStore) GetRef(ctx context.Context, repoID domain.ContentHash, kind domain.RefKind, name string) (domain.Ref, error) {
	if err := validateHash(repoID); err != nil {
		return domain.Ref{}, err
	}
	if err := domain.ValidateRefName(kind, name); err != nil {
		return domain.Ref{}, err
	}
	var ref domain.Ref
	err := s.pool.QueryRow(ctx, `SELECT kind, name, repo_id, COALESCE(target,''), symbolic FROM refs WHERE repo_id=$1 AND kind=$2 AND name=$3`,
		string(repoID), string(kind), name).Scan(&ref.Kind, &ref.Name, &ref.RepoID, &ref.Target, &ref.Symbolic)
	if err != nil {
		return domain.Ref{}, mapNoRows(err)
	}
	if err := domain.ValidateRef(ref); err != nil {
		return domain.Ref{}, err
	}
	return ref, nil
}

func (s *PostgresStore) ListRefs(ctx context.Context, repoID domain.ContentHash) ([]domain.Ref, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT kind, name, repo_id, COALESCE(target,''), symbolic FROM refs WHERE repo_id=$1`, string(repoID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Ref
	for rows.Next() {
		var ref domain.Ref
		if err := rows.Scan(&ref.Kind, &ref.Name, &ref.RepoID, &ref.Target, &ref.Symbolic); err != nil {
			return nil, err
		}
		if err := domain.ValidateRef(ref); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// CompareAndSwapRef: moves to next only if expected matches current target (optimistic locking, version++).
func (s *PostgresStore) CompareAndSwapRef(ctx context.Context, repoID domain.ContentHash, next domain.Ref, expected domain.ContentHash) error {
	next.RepoID = repoID
	if err := domain.ValidateRef(next); err != nil {
		return err
	}
	if err := domain.ValidateOptionalContentHash(expected); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// branch and session ref participate in Join's source attachment/cross-branch determination.
	// Must use the same repo advisory lock to prevent ref movement after service pre-fetch from colliding
	// with ApplyJoin storage re-validation.
	if next.Kind == domain.RefBranch || next.Kind == domain.RefSession {
		if err := lockRepoGraph(ctx, tx, repoID); err != nil {
			return err
		}
	}
	if expected == "" {
		// New creation (only if it does not exist).
		ct, err := tx.Exec(ctx,
			`INSERT INTO refs (repo_id, kind, name, target, symbolic) VALUES ($1,$2,$3,NULLIF($4,''),$5)
			 ON CONFLICT (repo_id, kind, name) DO NOTHING`,
			string(repoID), string(next.Kind), next.Name, string(next.Target), next.Symbolic)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return domain.ErrRefConflict
		}
	} else {
		ct, err := tx.Exec(ctx,
			`UPDATE refs SET target=NULLIF($1,''), symbolic=$2, version=version+1, updated_at=now()
			 WHERE repo_id=$3 AND kind=$4 AND name=$5 AND target IS NOT DISTINCT FROM NULLIF($6,'')`,
			string(next.Target), next.Symbolic, string(repoID), string(next.Kind), next.Name, string(expected))
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return domain.ErrRefConflict
		}
	}
	if next.Kind != domain.RefHead {
		if _, err := tx.Exec(ctx,
			`INSERT INTO reflog (repo_id,kind,name,old,new) VALUES ($1,$2,$3,$4,$5)`,
			string(repoID), string(next.Kind), next.Name, string(expected), string(next.Target)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ReadReflog: Returns the latest ref movement logs of the repo (read-only).
func (s *PostgresStore) ReadReflog(ctx context.Context, repoID domain.ContentHash) ([]domain.RefLogEntry, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT kind, name, old, new, created_at FROM reflog WHERE repo_id=$1 ORDER BY id DESC`,
		string(repoID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.RefLogEntry
	for rows.Next() {
		var e domain.RefLogEntry
		if err := rows.Scan(&e.Kind, &e.Name, &e.Old, &e.New, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PostgresStore) GetManifest(ctx context.Context, repoID domain.ContentHash) (domain.Manifest, error) {
	if err := validateHash(repoID); err != nil {
		return domain.Manifest{}, err
	}
	refs, err := s.ListRefs(ctx, repoID)
	if err != nil {
		return domain.Manifest{}, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, branch, COALESCE(memory_hash,''), message, grafted,
		       COALESCE(graft_parents,'{}'), COALESCE(graft_seq,0)
		  FROM snapshots WHERE repo_id=$1`, string(repoID))
	if err != nil {
		return domain.Manifest{}, err
	}
	defer rows.Close()
	var index []domain.ContentHash
	memoryAttachments := map[domain.ContentHash]domain.ContentHash{}
	snapshotStates := map[domain.ContentHash]domain.ContentHash{}
	for rows.Next() {
		var id, branch, memoryHash, message string
		var grafted bool
		var graftParents []string
		var graftSeq uint64
		if err := rows.Scan(&id, &branch, &memoryHash, &message, &grafted, &graftParents, &graftSeq); err != nil {
			return domain.Manifest{}, err
		}
		snap := domain.Snapshot{
			ID: domain.ContentHash(id), Branch: branch, MemoryHash: domain.ContentHash(memoryHash), Message: message,
			Grafted: grafted, GraftParents: hashes(graftParents), GraftSeq: graftSeq,
		}
		if err := domain.ValidateContentHash(snap.ID); err != nil {
			return domain.Manifest{}, err
		}
		if err := domain.ValidateOptionalContentHash(snap.MemoryHash); err != nil {
			return domain.Manifest{}, err
		}
		if err := validateHashes(snap.GraftParents...); err != nil {
			return domain.Manifest{}, err
		}
		if snap.GraftSeq > domain.MaxGraftSeq {
			return domain.Manifest{}, domain.ErrIntegrity
		}
		index = append(index, snap.ID)
		if snap.MemoryHash != "" {
			memoryAttachments[snap.ID] = snap.MemoryHash
		}
		state, err := domain.SnapshotStateHash(snap)
		if err != nil {
			return domain.Manifest{}, err
		}
		snapshotStates[snap.ID] = state
	}
	return domain.Manifest{RepoID: repoID, Refs: refs, SnapshotIndex: index, MemoryAttachments: memoryAttachments, SnapshotStates: snapshotStates, Version: len(index)}, rows.Err()
}

// --- Memory Meta ---

func (s *PostgresStore) GetMemoryMeta(ctx context.Context, repoID, snapshotID domain.ContentHash) (domain.MemoryDigest, error) {
	if err := validateHashes(repoID, snapshotID); err != nil {
		return domain.MemoryDigest{}, err
	}
	var d domain.MemoryDigest
	err := s.pool.QueryRow(ctx, `SELECT snapshot_id, summary, key_facts, open_tasks, provider FROM memories WHERE repo_id=$1 AND snapshot_id=$2`, string(repoID), string(snapshotID)).
		Scan(&d.SnapshotID, &d.Summary, &d.KeyFacts, &d.OpenTasks, &d.Provider)
	if err != nil {
		return domain.MemoryDigest{}, mapNoRows(err)
	}
	return d, nil
}

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
	_, err := s.pool.Exec(ctx,
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
	if err := domain.ValidateSessionDocHash(doc); err != nil {
		return false, err
	}
	canonical, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	// Lock order is doc → chunks, matching GetDocManifest. A new manifest row is
	// invisible until this transaction also stores its chunks and commits.
	plan, chunked := domain.PlanDocChunks(canonical)
	payload := docCompress(canonical)
	if chunked {
		mb, merr := json.Marshal(plan.Manifest)
		if merr != nil {
			return false, merr
		}
		payload = docCompress(mb)
	}
	ct, err := tx.Exec(ctx, `INSERT INTO blobs (hash, bytes) VALUES ($1,$2) ON CONFLICT (hash) DO NOTHING`,
		string(doc.Hash), payload)
	if err != nil {
		return false, err
	}
	created := ct.RowsAffected() > 0
	// Blob validation: For existing rows that are legacy blobs or manifests, the original/reshuffled content must match the canonical bytes — blocking content injection under the hash (maintaining existing contract).
	var stored []byte
	if err := tx.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1 FOR UPDATE`, string(doc.Hash)).Scan(&stored); err != nil {
		return false, err
	}
	stored, err = docDecompress(stored)
	if err != nil {
		return false, fmt.Errorf("%w: stored blob undecodable for doc %s", domain.ErrIntegrity, doc.Hash)
	}
	storedMan, isMan := domain.ParseDocChunkManifest(stored)
	if !created {
		if cb, _, cerr := s.reassembleManifestTx(ctx, tx, stored, doc.Hash); isMan {
			if cerr != nil || !bytes.Equal(cb, canonical) {
				return false, fmt.Errorf("%w: stored manifest disagrees with doc hash %s", domain.ErrIntegrity, doc.Hash)
			}
		} else if !bytes.Equal(stored, canonical) {
			return false, fmt.Errorf("%w: stored blob disagrees with doc hash %s", domain.ErrIntegrity, doc.Hash)
		}
	}
	if chunked {
		for _, ch := range plan.Order {
			if _, err := tx.Exec(ctx, `INSERT INTO blobs (hash, bytes) VALUES ($1,$2) ON CONFLICT (hash) DO NOTHING`,
				string(ch), docCompress(plan.Bodies[ch])); err != nil {
				return false, err
			}
			var existing []byte
			if err := tx.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(ch)).Scan(&existing); err != nil {
				return false, err
			}
			existing, err = docDecompress(existing)
			if err != nil || !bytes.Equal(existing, plan.Bodies[ch]) {
				return false, domain.ErrIntegrity
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
				 ON CONFLICT DO NOTHING`, string(doc.Hash), string(ch)); err != nil {
				return false, err
			}
		}
		mb, merr := json.Marshal(plan.Manifest)
		if merr != nil {
			return false, merr
		}
		if _, err := tx.Exec(ctx, `UPDATE blobs SET bytes=$1 WHERE hash=$2`, docCompress(mb), string(doc.Hash)); err != nil {
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
		string(repoID), string(doc.Hash)); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
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
	if err := validateHashes(repoID, hash); err != nil {
		return domain.SessionDoc{}, err
	}
	var b []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT b.bytes FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
		 WHERE rb.repo_id=$1 AND rb.kind='doc' AND rb.hash=$2`,
		string(repoID), string(hash)).Scan(&b); err != nil {
		return domain.SessionDoc{}, mapNoRows(err)
	}
	b, err := docDecompress(b)
	if err != nil {
		return domain.SessionDoc{}, err
	}
	// If chunk-type (manifest), assemble chunks using owner join(kind='chunk') and compare with integrity hash — block chunk access across repos + detect contamination.
	if cb, isMan, cerr := s.getDocChunkedPG(ctx, repoID, hash, b); isMan {
		if cerr != nil {
			return domain.SessionDoc{}, cerr
		}
		var cir domain.CIRDocument
		if err := json.Unmarshal(cb, &cir); err != nil {
			return domain.SessionDoc{}, err
		}
		return domain.SessionDoc{Hash: hash, CIR: cir}, nil
	}
	var cir domain.CIRDocument
	if err := json.Unmarshal(b, &cir); err != nil {
		return domain.SessionDoc{}, err
	}
	doc := domain.SessionDoc{Hash: hash, CIR: cir}
	if err := domain.ValidateSessionDocHash(doc); err != nil {
		return domain.SessionDoc{}, err
	}
	return doc, nil
}

// PutChunks stores the first arrived chunk in the global CAS and grants ownership to the repo. Blob collision verification and repo grant are bundled in a single transaction.
func (s *PostgresStore) PutChunks(ctx context.Context, repoID domain.ContentHash, chunks map[domain.ContentHash][]byte) (int, int, error) {
	if err := validateHash(repoID); err != nil {
		return 0, 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	stored, deduped := 0, 0
	for hash, body := range chunks {
		if err := validateHash(hash); err != nil {
			return 0, 0, err
		}
		if domain.HashContent(body) != hash {
			return 0, 0, domain.ErrIntegrity
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO blobs (hash, bytes) VALUES ($1,$2) ON CONFLICT (hash) DO NOTHING`,
			string(hash), docCompress(body)); err != nil {
			return 0, 0, err
		}
		var existing []byte
		if err := tx.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(hash)).Scan(&existing); err != nil {
			return 0, 0, err
		}
		existing, err = docDecompress(existing)
		if err != nil || !bytes.Equal(existing, body) {
			return 0, 0, domain.ErrIntegrity
		}
		ct, err := tx.Exec(ctx,
			`INSERT INTO repo_blobs (repo_id, kind, hash) VALUES ($1,'chunk',$2) ON CONFLICT DO NOTHING`,
			string(repoID), string(hash))
		if err != nil {
			return 0, 0, err
		}
		if ct.RowsAffected() == 1 {
			stored++
		} else {
			deduped++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return stored, deduped, nil
}

// HasChunks returns the list of owned chunks in the repo (negotiate delta negotiation).
func (s *PostgresStore) HasChunks(ctx context.Context, repoID domain.ContentHash, hs []domain.ContentHash) ([]domain.ContentHash, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	if err := validateHashes(hs...); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
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
	if err := s.pool.QueryRow(ctx,
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.DocChunkManifest{}, err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	if err := tx.QueryRow(ctx,
		`SELECT b.bytes FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
		 WHERE rb.repo_id=$1 AND rb.kind='doc' AND rb.hash=$2 FOR UPDATE OF b`,
		string(repoID), string(hash)).Scan(&raw); err != nil {
		return domain.DocChunkManifest{}, mapNoRows(err)
	}
	data, err := docDecompress(raw)
	if err != nil {
		return domain.DocChunkManifest{}, domain.ErrIntegrity
	}
	if man, isMan := domain.ParseDocChunkManifest(data); isMan {
		return man, nil
	}
	if domain.HashContent(data) != hash {
		var cir domain.CIRDocument
		if json.Unmarshal(data, &cir) != nil {
			return domain.DocChunkManifest{}, domain.ErrIntegrity
		}
		cb, cerr := domain.CanonicalBytes(cir)
		if cerr != nil || domain.HashContent(cb) != hash {
			return domain.DocChunkManifest{}, domain.ErrIntegrity
		}
		data = cb
	}
	plan, ok := domain.PlanDocChunks(data)
	if !ok {
		return domain.DocChunkManifest{}, domain.ErrNotFound // Plan not possible — legacy response fallback
	}
	for _, ch := range plan.Order {
		if _, err := tx.Exec(ctx, `INSERT INTO blobs (hash, bytes) VALUES ($1,$2) ON CONFLICT (hash) DO NOTHING`,
			string(ch), docCompress(plan.Bodies[ch])); err != nil {
			return domain.DocChunkManifest{}, err
		}
		var existing []byte
		if err := tx.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(ch)).Scan(&existing); err != nil {
			return domain.DocChunkManifest{}, err
		}
		existing, err = docDecompress(existing)
		if err != nil || !bytes.Equal(existing, plan.Bodies[ch]) {
			return domain.DocChunkManifest{}, domain.ErrIntegrity
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
	if _, err := tx.Exec(ctx, `UPDATE blobs SET bytes=$1 WHERE hash=$2`, docCompress(manifest), string(hash)); err != nil {
		return domain.DocChunkManifest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
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
		if err := s.pool.QueryRow(ctx,
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
	rows, err := s.pool.Query(ctx,
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

func (s *PostgresStore) PutMemory(ctx context.Context, repoID domain.ContentHash, digest domain.MemoryDigest) (domain.ContentHash, error) {
	if err := validateHash(repoID); err != nil {
		return "", err
	}
	if err := validateMemoryDigestRefs(digest); err != nil {
		return "", err
	}
	data, err := json.Marshal(digest)
	if err != nil {
		return "", err
	}
	h, err := domain.MemoryDigestHash(digest)
	if err != nil {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	// Lock order is memory object → memory components. A newly inserted manifest
	// is invisible until every component and ownership grant commits with it.
	plan, chunked, err := domain.PlanMemoryChunks(digest)
	if err != nil {
		return "", err
	}
	payload := data
	if chunked {
		payload, err = json.Marshal(plan.Manifest)
		if err != nil {
			return "", err
		}
	}
	createdTag, err := tx.Exec(ctx, `INSERT INTO blobs (hash, bytes) VALUES ($1,$2) ON CONFLICT (hash) DO NOTHING`, string(h), payload)
	if err != nil {
		return "", err
	}
	created := createdTag.RowsAffected() > 0
	var stored []byte
	if err := tx.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1 FOR UPDATE`, string(h)).Scan(&stored); err != nil {
		return "", err
	}
	stored, err = docDecompress(stored)
	if err != nil {
		return "", fmt.Errorf("%w: stored blob undecodable for memory %s", domain.ErrIntegrity, h)
	}
	storedManifest, isManifest, parseErr := domain.ParseMemoryChunkManifest(stored)
	if parseErr != nil {
		return "", fmt.Errorf("%w: invalid memory manifest %s", domain.ErrIntegrity, h)
	}
	if !created {
		var existing domain.MemoryDigest
		if isManifest {
			existing, _, err = s.reassembleMemoryManifestTx(ctx, tx, stored)
		} else {
			err = json.Unmarshal(stored, &existing)
		}
		if err != nil {
			return "", fmt.Errorf("%w: stored blob disagrees with memory hash %s", domain.ErrIntegrity, h)
		}
		got, hashErr := domain.MemoryDigestHash(existing)
		if hashErr != nil || got != h {
			return "", fmt.Errorf("%w: stored blob disagrees with memory hash %s", domain.ErrIntegrity, h)
		}
	}
	if chunked {
		for _, chunkHash := range plan.Order {
			body := plan.Bodies[chunkHash]
			if _, err := tx.Exec(ctx, `INSERT INTO blobs (hash, bytes) VALUES ($1,$2) ON CONFLICT (hash) DO NOTHING`,
				string(chunkHash), docCompress(body)); err != nil {
				return "", err
			}
			var existing []byte
			if err := tx.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(chunkHash)).Scan(&existing); err != nil {
				return "", err
			}
			existing, err = docDecompress(existing)
			if err != nil || !bytes.Equal(existing, body) {
				return "", domain.ErrIntegrity
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO repo_blobs (repo_id, kind, hash) VALUES ($1,'memory_chunk',$2) ON CONFLICT DO NOTHING`,
				string(repoID), string(chunkHash)); err != nil {
				return "", err
			}
		}
	}
	if !created && chunked && !isManifest {
		// The blob is global but readability is repo-scoped. Before replacing a
		// shared legacy memory with its manifest, grant every current memory owner
		// every component so rolling multi-repo dedup cannot revoke access.
		for _, chunkHash := range plan.Order {
			if _, err := tx.Exec(ctx,
				`INSERT INTO repo_blobs (repo_id, kind, hash)
				 SELECT repo_id, 'memory_chunk', $2 FROM repo_blobs WHERE kind='memory' AND hash=$1
				 ON CONFLICT DO NOTHING`, string(h), string(chunkHash)); err != nil {
				return "", err
			}
		}
		manifest, err := json.Marshal(plan.Manifest)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE blobs SET bytes=$1 WHERE hash=$2`, manifest, string(h)); err != nil {
			return "", err
		}
	} else if isManifest {
		// A repo deduplicating against an already-manifested global memory must
		// own the components referenced by the stored representation. Re-grant
		// every current owner too, healing any interrupted/older migration.
		all := append(append([]domain.ContentHash{}, storedManifest.SummaryChunks...), storedManifest.FragmentChunks...)
		for _, chunkHash := range all {
			if _, err := tx.Exec(ctx,
				`INSERT INTO repo_blobs (repo_id, kind, hash) VALUES ($1,'memory_chunk',$2) ON CONFLICT DO NOTHING`,
				string(repoID), string(chunkHash)); err != nil {
				return "", err
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO repo_blobs (repo_id, kind, hash)
				 SELECT repo_id, 'memory_chunk', $2 FROM repo_blobs WHERE kind='memory' AND hash=$1
				 ON CONFLICT DO NOTHING`, string(h), string(chunkHash)); err != nil {
				return "", err
			}
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO repo_blobs (repo_id, kind, hash) VALUES ($1,'memory',$2) ON CONFLICT DO NOTHING`,
		string(repoID), string(h)); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return h, nil
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

func (s *PostgresStore) GetMemory(ctx context.Context, repoID, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if err := validateHashes(repoID, hash); err != nil {
		return domain.MemoryDigest{}, err
	}
	var b []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT b.bytes FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
		 WHERE rb.repo_id=$1 AND rb.kind='memory' AND rb.hash=$2`,
		string(repoID), string(hash)).Scan(&b); err != nil {
		return domain.MemoryDigest{}, mapNoRows(err)
	}
	b, err := docDecompress(b)
	if err != nil {
		return domain.MemoryDigest{}, domain.ErrIntegrity
	}
	if manifest, isManifest, parseErr := domain.ParseMemoryChunkManifest(b); isManifest {
		if parseErr != nil {
			return domain.MemoryDigest{}, domain.ErrIntegrity
		}
		bodies := make(map[domain.ContentHash][]byte, len(manifest.SummaryChunks)+len(manifest.FragmentChunks))
		all := append(append([]domain.ContentHash{}, manifest.SummaryChunks...), manifest.FragmentChunks...)
		for _, chunkHash := range all {
			if _, loaded := bodies[chunkHash]; loaded {
				continue
			}
			var raw []byte
			if err := s.pool.QueryRow(ctx,
				`SELECT b.bytes FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
				 WHERE rb.repo_id=$1 AND rb.kind='memory_chunk' AND rb.hash=$2`,
				string(repoID), string(chunkHash)).Scan(&raw); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.MemoryDigest{}, fmt.Errorf("%w: memory %s missing component %s", domain.ErrIntegrity, hash, chunkHash)
				}
				return domain.MemoryDigest{}, err
			}
			body, err := docDecompress(raw)
			if err != nil {
				return domain.MemoryDigest{}, domain.ErrIntegrity
			}
			bodies[chunkHash] = body
		}
		digest, err := domain.AssembleMemoryChunks(manifest, bodies)
		if err != nil {
			return domain.MemoryDigest{}, err
		}
		if err := validateMemoryDigestRefs(digest); err != nil {
			return domain.MemoryDigest{}, err
		}
		got, err := domain.MemoryDigestHash(digest)
		if err != nil || got != hash {
			return domain.MemoryDigest{}, fmt.Errorf("%w: memory hash mismatch: got %s want %s", domain.ErrIntegrity, got, hash)
		}
		return digest, nil
	}
	var d domain.MemoryDigest
	if err := json.Unmarshal(b, &d); err != nil {
		return domain.MemoryDigest{}, err
	}
	got, err := domain.MemoryDigestHash(d)
	if err != nil {
		return domain.MemoryDigest{}, err
	}
	if got != hash {
		return domain.MemoryDigest{}, fmt.Errorf("%w: memory hash mismatch: got %s want %s", domain.ErrIntegrity, got, hash)
	}
	if err := validateMemoryDigestRefs(d); err != nil {
		return domain.MemoryDigest{}, err
	}
	return d, nil
}

// UpdateRepoAbout updates the About field (topics are stored as JSON text).
func (s *PostgresStore) UpdateRepoAbout(ctx context.Context, id domain.ContentHash, description, website string, topics []string) error {
	if err := validateHash(id); err != nil {
		return err
	}
	tj, _ := json.Marshal(topics)
	_, err := s.pool.Exec(ctx,
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
		if _, err := s.pool.Exec(ctx, `UPDATE repos SET default_branch=$2 WHERE id=$1`, string(id), *defaultBranch); err != nil {
			return err
		}
	}
	if protectDefault != nil {
		if _, err := s.pool.Exec(ctx, `UPDATE repos SET protect_default=$2 WHERE id=$1`, string(id), *protectDefault); err != nil {
			return err
		}
	}
	return nil
}

// PutSettingsBundle upserts the settings bundle.
func (s *PostgresStore) PutSettingsBundle(ctx context.Context, repoID domain.ContentHash, bundle domain.SettingsBundle) error {
	if err := validateHash(repoID); err != nil {
		return err
	}
	if err := domain.ValidateSettingsBundle(bundle.Kind, "", bundle); err != nil {
		return err
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO repo_settings (repo_id, kind, data) VALUES ($1,$2,$3)
		 ON CONFLICT (repo_id, kind) DO UPDATE SET data = EXCLUDED.data`,
		string(repoID), bundle.Kind, string(data))
	return err
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
	err := s.pool.QueryRow(ctx, `SELECT data FROM repo_settings WHERE repo_id=$1 AND kind=$2`, string(repoID), kind).Scan(&raw)
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

// PutPending upserts the session's latest uncommitted capture pointer.
func (s *PostgresStore) PutPending(ctx context.Context, repoID domain.ContentHash, p domain.Pending) error {
	_, err := s.ReplacePending(ctx, repoID, p)
	return err
}

// ReplacePending linearizes capture replacement on the pointer row and returns
// its exact predecessor. INSERT ... ON CONFLICT DO NOTHING establishes a row
// when absent; otherwise SELECT FOR UPDATE serializes replacements, dismiss
// mutation, and target-CAS deletion without a check-then-act window.
func (s *PostgresStore) ReplacePending(ctx context.Context, repoID domain.ContentHash, p domain.Pending) (domain.ContentHash, error) {
	if err := validateHashes(repoID, p.Target); err != nil {
		return "", err
	}
	if p.RepoID != repoID || p.SessionID == "" || len(p.SessionID) > 128 {
		return "", domain.ErrIntegrity
	}
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for {
		var raw string
		err = tx.QueryRow(ctx,
			`SELECT data FROM pending_contexts
			 WHERE repo_id=$1 AND session_id=$2 FOR UPDATE`,
			string(repoID), p.SessionID).Scan(&raw)
		if err == nil {
			var current domain.Pending
			if err := json.Unmarshal([]byte(raw), &current); err != nil {
				return "", err
			}
			if current.RepoID != repoID || current.SessionID != p.SessionID {
				return "", domain.ErrIntegrity
			}
			if err := validateHash(current.Target); err != nil {
				return "", err
			}
			if current.Dismissed {
				p.Dismissed = true
				data, err = json.Marshal(p)
				if err != nil {
					return "", err
				}
			}
			if _, err := tx.Exec(ctx,
				`UPDATE pending_contexts SET data=$3 WHERE repo_id=$1 AND session_id=$2`,
				string(repoID), p.SessionID, string(data)); err != nil {
				return "", err
			}
			if err := tx.Commit(ctx); err != nil {
				return "", err
			}
			return current.Target, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO pending_contexts (repo_id, session_id, data)
			 VALUES ($1,$2,$3) ON CONFLICT (repo_id, session_id) DO NOTHING`,
			string(repoID), p.SessionID, string(data))
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return "", nil
	}
}

// ListPendings returns all durable uncommitted capture pointers in the repo.
func (s *PostgresStore) ListPendings(ctx context.Context, repoID domain.ContentHash) ([]domain.Pending, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT data FROM pending_contexts WHERE repo_id=$1`, string(repoID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Pending
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var p domain.Pending
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, err
		}
		if err := validateHashes(p.RepoID, p.Target); err != nil || p.RepoID != repoID || p.SessionID == "" || len(p.SessionID) > 128 {
			return nil, domain.ErrIntegrity
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePending removes the session's ongoing pointer (no error if not present — idempotent).
func (s *PostgresStore) DeletePending(ctx context.Context, repoID domain.ContentHash, sessionID string) error {
	if err := validateHash(repoID); err != nil {
		return err
	}
	if sessionID == "" || len(sessionID) > 128 {
		return domain.ErrValidation
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM pending_contexts WHERE repo_id=$1 AND session_id=$2`, string(repoID), sessionID)
	return err
}

func (s *PostgresStore) CompareAndDeletePending(ctx context.Context, repoID domain.ContentHash, sessionID string, expected domain.ContentHash) (domain.PendingDeleteResult, error) {
	if err := validateHashes(repoID, expected); err != nil {
		return domain.PendingDeleteKept, err
	}
	if sessionID == "" || len(sessionID) > 128 {
		return domain.PendingDeleteKept, domain.ErrValidation
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM pending_contexts
		 WHERE repo_id=$1 AND session_id=$2 AND data->>'target'=$3`,
		string(repoID), sessionID, string(expected))
	if err != nil {
		return domain.PendingDeleteKept, err
	}
	if tag.RowsAffected() > 0 {
		return domain.PendingDeleteDeleted, nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pending_contexts WHERE repo_id=$1 AND session_id=$2)`,
		string(repoID), sessionID).Scan(&exists); err != nil {
		return domain.PendingDeleteKept, err
	}
	if exists {
		return domain.PendingDeleteKept, nil
	}
	return domain.PendingDeleteAbsent, nil
}

func (s *PostgresStore) SetPendingDismissed(ctx context.Context, repoID domain.ContentHash, sessionID string, dismissed bool) (bool, error) {
	if err := validateHash(repoID); err != nil {
		return false, err
	}
	if sessionID == "" || len(sessionID) > 128 {
		return false, domain.ErrValidation
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE pending_contexts
		 SET data=jsonb_set(data, '{dismissed}', to_jsonb($3::boolean), true)
		 WHERE repo_id=$1 AND session_id=$2`,
		string(repoID), sessionID, dismissed)
	return tag.RowsAffected() > 0, err
}

// PutUnsync upserts the push pending pointer.
func (s *PostgresStore) PutUnsync(ctx context.Context, repoID domain.ContentHash, u domain.Unsync) error {
	if err := validateHashes(repoID, u.Target); err != nil {
		return err
	}
	if err := domain.ValidateBranchName(u.Branch); err != nil {
		return err
	}
	if u.RepoID != repoID || u.User == "" || len(u.User) > 128 {
		return domain.ErrIntegrity
	}
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO unsync_contexts (repo_id, username, branch, data) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (repo_id, username, branch) DO UPDATE SET data = EXCLUDED.data`,
		string(repoID), u.User, u.Branch, string(data))
	return err
}

// ListUnsyncs returns the entire list of push pending pointers in the repo.
func (s *PostgresStore) ListUnsyncs(ctx context.Context, repoID domain.ContentHash) ([]domain.Unsync, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT data FROM unsync_contexts WHERE repo_id=$1`, string(repoID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Unsync
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var u domain.Unsync
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			return nil, err
		}
		if err := validateHashes(u.RepoID, u.Target); err != nil || u.RepoID != repoID || u.User == "" || len(u.User) > 128 {
			return nil, domain.ErrIntegrity
		}
		if err := domain.ValidateBranchName(u.Branch); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteUnsync removes the push pending pointer (no error if not present — idempotent).
func (s *PostgresStore) DeleteUnsync(ctx context.Context, repoID domain.ContentHash, user, branch string) error {
	if err := validateHash(repoID); err != nil {
		return err
	}
	if err := domain.ValidateBranchName(branch); err != nil {
		return err
	}
	if user == "" || len(user) > 128 {
		return domain.ErrValidation
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM unsync_contexts WHERE repo_id=$1 AND username=$2 AND branch=$3`,
		string(repoID), user, branch)
	return err
}

// DeleteSnapshot removes the snapshot metadata (hook capture leaf GC exclusive — idempotent).
func (s *PostgresStore) DeleteSnapshot(ctx context.Context, repoID, id domain.ContentHash) error {
	if err := validateHashes(repoID, id); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`, string(repoID), string(id))
	return err
}

// DeleteDoc removes the doc body (hook capture leaf GC exclusive — idempotent).
// blobs are global (hash PK), but session docs differ by session_meta/timestamp,
// making hash collisions across repos practically impossible — target is hook leaf doc (service layer guard).
// SetGraftParents replaces the entire graft (set, seq) register (LWW — join supersede).
func (s *PostgresStore) SetGraftParents(ctx context.Context, repoID, id domain.ContentHash, parents []domain.ContentHash) error {
	if err := validateHashes(repoID, id); err != nil {
		return err
	}
	if err := validateHashes(parents...); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockRepoGraph(ctx, tx, repoID); err != nil {
		return err
	}
	if err := requireSnapshotIDsPG(ctx, tx, repoID, parents...); err != nil {
		return err
	}
	var natural []string
	var seq uint64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(parents,'{}'), COALESCE(graft_seq,0) FROM snapshots WHERE repo_id=$1 AND id=$2 FOR UPDATE`,
		string(repoID), string(id)).Scan(&natural, &seq); err != nil {
		return mapNoRows(err)
	}
	if seq == domain.MaxGraftSeq {
		return fmt.Errorf("%w: graft sequence exhausted", domain.ErrConflict)
	}
	seen := map[string]bool{string(id): true}
	for _, p := range natural {
		seen[p] = true
	}
	out := make([]string, 0, len(parents))
	for _, p := range parents {
		if p == "" || seen[string(p)] {
			continue
		}
		seen[string(p)] = true
		out = append(out, string(p))
	}
	tag, err := tx.Exec(ctx,
		`UPDATE snapshots SET graft_parents=$3, grafted=(cardinality($3::text[])>0), graft_seq=COALESCE(graft_seq,0)+1 WHERE repo_id=$1 AND id=$2`,
		string(repoID), string(id), out)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	if err := ensureNoReachabilityCycle(ctx, tx, repoID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ApplyJoin applies the graft replacement, optional session ref creation, and target branch ref CAS in a single PostgreSQL
// transaction. It prevents overwriting append/join updates from different server instances due to row lock and graft_seq CAS.
func (s *PostgresStore) ApplyJoin(ctx context.Context, m outbound.JoinMutation) error {
	if err := validateHashes(m.RepoID, m.Source, m.ExpectedHead, m.NewHead); err != nil {
		return err
	}
	if len(m.Segment) == 0 || m.Segment[0] != m.Source {
		return fmt.Errorf("%w: join segment must start at source", domain.ErrValidation)
	}
	segmentSeen := map[domain.ContentHash]bool{}
	for _, id := range m.Segment {
		if err := validateHash(id); err != nil {
			return err
		}
		if segmentSeen[id] {
			return fmt.Errorf("%w: duplicate join segment snapshot", domain.ErrIntegrity)
		}
		segmentSeen[id] = true
	}
	if !segmentSeen[m.NewHead] {
		return fmt.Errorf("%w: join head is outside segment", domain.ErrValidation)
	}
	if err := domain.ValidateBranchName(m.Branch); err != nil {
		return err
	}
	if (m.ForkName == "") != (m.ForkTip == "") {
		return fmt.Errorf("%w: join fork name and tip must be provided together", domain.ErrValidation)
	}
	if m.ForkName != "" {
		if err := domain.ValidateBranchName(m.ForkName); err != nil {
			return err
		}
		if err := validateHash(m.ForkTip); err != nil {
			return err
		}
		if !segmentSeen[m.ForkTip] {
			return fmt.Errorf("%w: join session tip is outside segment", domain.ErrValidation)
		}
	}
	if len(m.Grafts) == 0 {
		return fmt.Errorf("%w: join requires at least one graft patch", domain.ErrValidation)
	}
	required := []domain.ContentHash{m.Source, m.ExpectedHead, m.NewHead, m.ForkTip}
	required = append(required, m.Segment...)
	seenPatch := map[domain.ContentHash]bool{}
	for _, patch := range m.Grafts {
		if err := validateHash(patch.SnapshotID); err != nil {
			return err
		}
		if err := validateHashes(patch.Parents...); err != nil {
			return err
		}
		if seenPatch[patch.SnapshotID] {
			return domain.ErrIntegrity
		}
		seenPatch[patch.SnapshotID] = true
		required = append(required, patch.SnapshotID)
		required = append(required, patch.Parents...)
	}
	if err := validateJoinMutationPlan(m); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockRepoGraph(ctx, tx, m.RepoID); err != nil {
		return err
	}
	var current *string
	if err := tx.QueryRow(ctx,
		`SELECT target FROM refs WHERE repo_id=$1 AND kind='branch' AND name=$2 FOR UPDATE`,
		string(m.RepoID), m.Branch).Scan(&current); err != nil {
		return mapNoRows(err)
	}
	if current == nil || *current != string(m.ExpectedHead) {
		return domain.ErrRefConflict
	}
	if err := requireSnapshotIDsPG(ctx, tx, m.RepoID, required...); err != nil {
		return err
	}
	if err := ensureJoinGraphScopePG(ctx, tx, m); err != nil {
		return err
	}
	forkCreated := false
	if m.ForkName != "" {
		ct, err := tx.Exec(ctx,
			`INSERT INTO refs (repo_id,kind,name,target,symbolic) VALUES ($1,'session',$2,$3,'') ON CONFLICT DO NOTHING`,
			string(m.RepoID), m.ForkName, string(m.ForkTip))
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			var forkTarget *string
			if err := tx.QueryRow(ctx,
				`SELECT target FROM refs WHERE repo_id=$1 AND kind='session' AND name=$2 FOR UPDATE`,
				string(m.RepoID), m.ForkName).Scan(&forkTarget); err != nil {
				return mapNoRows(err)
			}
			if forkTarget == nil || *forkTarget != string(m.ForkTip) {
				return domain.ErrRefConflict
			}
		} else {
			forkCreated = true
		}
	}
	for _, patch := range m.Grafts {
		var natural []string
		var seq uint64
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(parents,'{}'), COALESCE(graft_seq,0) FROM snapshots WHERE repo_id=$1 AND id=$2 FOR UPDATE`,
			string(m.RepoID), string(patch.SnapshotID)).Scan(&natural, &seq); err != nil {
			return mapNoRows(err)
		}
		if seq != patch.ExpectedSeq {
			return domain.ErrConflict
		}
		if seq == domain.MaxGraftSeq {
			return fmt.Errorf("%w: graft sequence exhausted", domain.ErrConflict)
		}
		seen := map[string]bool{string(patch.SnapshotID): true}
		for _, p := range natural {
			seen[p] = true
		}
		out := make([]string, 0, len(patch.Parents))
		for _, p := range patch.Parents {
			if p == "" || seen[string(p)] {
				continue
			}
			seen[string(p)] = true
			out = append(out, string(p))
		}
		ct, err := tx.Exec(ctx,
			`UPDATE snapshots SET graft_parents=$3, grafted=(cardinality($3::text[])>0), graft_seq=graft_seq+1 WHERE repo_id=$1 AND id=$2 AND graft_seq=$4`,
			string(m.RepoID), string(patch.SnapshotID), out, patch.ExpectedSeq)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return domain.ErrConflict
		}
	}
	if err := ensureNoReachabilityCycle(ctx, tx, m.RepoID); err != nil {
		return err
	}
	ct, err := tx.Exec(ctx,
		`UPDATE refs SET target=$1, version=version+1, updated_at=now() WHERE repo_id=$2 AND kind='branch' AND name=$3 AND target=$4`,
		string(m.NewHead), string(m.RepoID), m.Branch, string(m.ExpectedHead))
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return domain.ErrRefConflict
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO reflog (repo_id,kind,name,old,new) VALUES ($1,'branch',$2,$3,$4)`,
		string(m.RepoID), m.Branch, string(m.ExpectedHead), string(m.NewHead)); err != nil {
		return err
	}
	if forkCreated {
		if _, err := tx.Exec(ctx,
			`INSERT INTO reflog (repo_id,kind,name,old,new) VALUES ($1,'session',$2,'',$3)`,
			string(m.RepoID), m.ForkName, string(m.ForkTip)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// UpdateSnapshotMessage is a meta update for committing hook labels — exclusive commit message promotion.
// Conditional UPDATE(CAS): updates only if hook prefix or same message (idempotent retry) —
// blocks concurrent promotions in the storage layer (reliability P1).
func (s *PostgresStore) UpdateSnapshotMessage(ctx context.Context, repoID, id domain.ContentHash, message string) error {
	if err := validateHashes(repoID, id); err != nil {
		return err
	}
	ct, err := s.pool.Exec(ctx,
		`UPDATE snapshots SET message=$3 WHERE repo_id=$1 AND id=$2
		   AND (message LIKE $4 || '%' OR message=$3)`,
		string(repoID), string(id), message, domain.HookMessagePrefix)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		var exists bool
		if qerr := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM snapshots WHERE repo_id=$1 AND id=$2)`,
			string(repoID), string(id)).Scan(&exists); qerr != nil {
			return qerr
		}
		if !exists {
			return domain.ErrNotFound
		}
		return fmt.Errorf("%w: snapshot message already promoted", domain.ErrConflict)
	}
	return nil
}

func (s *PostgresStore) DeleteDoc(ctx context.Context, repoID domain.ContentHash, hash domain.ContentHash) error {
	if err := validateHashes(repoID, hash); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
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
	return tx.Commit(ctx)
}

// PutSettingsObject stores the commit attachment settings object (idempotent).
func (s *PostgresStore) PutSettingsObject(ctx context.Context, repoID domain.ContentHash, hash domain.ContentHash, bundle domain.SettingsBundle) error {
	if err := validateHashes(repoID, hash); err != nil {
		return err
	}
	if err := domain.ValidateSettingsBundle(bundle.Kind, hash, bundle); err != nil {
		return err
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO settings_objects (repo_id, hash, data) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
		string(repoID), string(hash), string(data))
	return err
}

// GetSettingsObject retrieves the settings object.
func (s *PostgresStore) GetSettingsObject(ctx context.Context, repoID domain.ContentHash, hash domain.ContentHash) (domain.SettingsBundle, error) {
	if err := validateHashes(repoID, hash); err != nil {
		return domain.SettingsBundle{}, err
	}
	var raw string
	err := s.pool.QueryRow(ctx, `SELECT data FROM settings_objects WHERE repo_id=$1 AND hash=$2`, string(repoID), string(hash)).Scan(&raw)
	if err != nil {
		return domain.SettingsBundle{}, mapNoRows(err)
	}
	var b domain.SettingsBundle
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return domain.SettingsBundle{}, err
	}
	if err := domain.ValidateSettingsBundle(b.Kind, hash, b); err != nil {
		return domain.SettingsBundle{}, err
	}
	return b, nil
}

// PutSecretsEnvelope / GetSecretsEnvelope store opaque end-to-end encrypted secret payloads.
func (s *PostgresStore) PutSecretsEnvelope(ctx context.Context, repoID domain.ContentHash, raw []byte) error {
	if err := validateHash(repoID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO repo_secrets (repo_id, data) VALUES ($1,$2)
		 ON CONFLICT (repo_id) DO UPDATE SET data = EXCLUDED.data`,
		string(repoID), string(raw))
	return err
}

func (s *PostgresStore) GetSecretsEnvelope(ctx context.Context, repoID domain.ContentHash) ([]byte, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	var raw string
	err := s.pool.QueryRow(ctx, `SELECT data FROM repo_secrets WHERE repo_id=$1`, string(repoID)).Scan(&raw)
	if err != nil {
		return nil, mapNoRows(err)
	}
	return []byte(raw), nil
}
