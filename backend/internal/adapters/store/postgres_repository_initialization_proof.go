//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// This request-local proof captures MVCC row identities from the SAME read
// snapshot as the application's full body verification. It is never serialized,
// cached across requests, or accepted from a client. Repacking is safe but makes
// a pending proof stale; a later request may verify the new representation.
type initializationProofPG struct {
	owner     *PostgresStore
	repo      domain.ContentHash
	anchor    domain.RepositoryInitializationAnchor
	snapshots map[domain.ContentHash]domain.ContentHash
	blobs     map[initializationBlobPG]string
	settings  map[domain.ContentHash]string
}
type initializationBlobPG struct {
	kind string
	hash domain.ContentHash
}

func (*initializationProofPG) RepositoryInitializationProof() {}

func (s *PostgresStore) CaptureRepositoryInitialization(ctx context.Context, repo domain.ContentHash, anchor domain.RepositoryInitializationAnchor, evidence outbound.RepositoryInitializationEvidence) (outbound.RepositoryInitializationProof, error) {
	tx, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || tx.owner != s || !tx.readOnly {
		return nil, domain.ErrConflict
	}
	if err := anchor.Validate(repo); err != nil {
		return nil, err
	}
	p := &initializationProofPG{owner: s, repo: repo, anchor: anchor, snapshots: map[domain.ContentHash]domain.ContentHash{}, blobs: map[initializationBlobPG]string{}, settings: map[domain.ContentHash]string{}}
	p.anchor.SnapshotStates = make(map[domain.ContentHash]domain.ContentHash, len(anchor.SnapshotStates))
	for id, state := range anchor.SnapshotStates {
		p.anchor.SnapshotStates[id] = state
	}
	for _, snap := range evidence.Snapshots {
		if snap.RepoID != repo {
			return nil, domain.ErrIntegrity
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			return nil, err
		}
		p.snapshots[snap.ID] = domain.HashContent(raw)
		if err := p.captureBlob(ctx, "doc", snap.DocHash); err != nil {
			return nil, err
		}
		for _, hash := range []domain.ContentHash{snap.ClaudeSettings, snap.AgentsSettings, snap.CodexSettings} {
			if hash == "" {
				continue
			}
			var version string
			if err := s.db(ctx).QueryRow(ctx, `SELECT xmin::text||':'||ctid::text FROM settings_objects WHERE repo_id=$1 AND hash=$2`, repo, hash).Scan(&version); err != nil {
				return nil, mapNoRows(err)
			}
			p.settings[hash] = version
		}
	}
	if len(p.snapshots) != len(anchor.SnapshotStates) {
		return nil, domain.ErrIntegrity
	}
	for id := range anchor.SnapshotStates {
		if _, ok := p.snapshots[id]; !ok {
			return nil, domain.ErrIntegrity
		}
	}
	for _, hash := range evidence.Memories {
		if err := p.captureBlob(ctx, "memory", hash); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *initializationProofPG) captureBlob(ctx context.Context, kind string, hash domain.ContentHash) error {
	key := initializationBlobPG{kind, hash}
	if _, ok := p.blobs[key]; ok {
		return nil
	}
	var version string
	var raw []byte
	if err := p.owner.db(ctx).QueryRow(ctx, `SELECT b.xmin::text||':'||b.ctid::text||':'||rb.xmin::text||':'||rb.ctid::text,b.bytes
 FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash WHERE rb.repo_id=$1 AND rb.kind=$2 AND rb.hash=$3`, p.repo, kind, hash).Scan(&version, &raw); err != nil {
		return mapNoRows(err)
	}
	p.blobs[key] = version
	data, err := docDecompress(raw)
	if err != nil {
		return domain.ErrIntegrity
	}
	var chunks []domain.ContentHash
	chunkKind := "chunk"
	if kind == "doc" {
		if manifest, ok := domain.ParseDocChunkManifest(data); ok {
			chunks = manifest.Chunks
		}
	} else {
		manifest, ok, err := domain.ParseMemoryChunkManifest(data)
		if err != nil {
			return domain.ErrIntegrity
		}
		if ok {
			chunks = append(append([]domain.ContentHash{}, manifest.SummaryChunks...), manifest.FragmentChunks...)
		}
		chunkKind = "memory_chunk"
	}
	for _, id := range chunks {
		key := initializationBlobPG{chunkKind, id}
		if _, ok := p.blobs[key]; ok {
			continue
		}
		if err := p.owner.db(ctx).QueryRow(ctx, `SELECT b.xmin::text||':'||b.ctid::text||':'||rb.xmin::text||':'||rb.ctid::text
 FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash WHERE rb.repo_id=$1 AND rb.kind=$2 AND rb.hash=$3`, p.repo, chunkKind, id).Scan(&version); err != nil {
			return mapNoRows(err)
		}
		p.blobs[key] = version
	}
	return nil
}

func (p *initializationProofPG) recheck(ctx context.Context) error {
	s := p.owner
	if err := s.requireInitializationTransaction(ctx, p.repo); err != nil {
		return err
	}
	// NOWAIT prevents body uploads/repacking from turning this metadata-only
	// commit into a long row-lock wait. Every retained version/owner is checked
	// and held until commit; no object body is loaded or hashed here.
	for _, id := range initializationSortedHashes(p.snapshots) {
		var present int
		if err := s.db(ctx).QueryRow(ctx, `SELECT 1 FROM snapshots WHERE repo_id=$1 AND id=$2 FOR SHARE NOWAIT`, p.repo, id).Scan(&present); err != nil {
			return initializationProofError(err)
		}
		snap, err := s.GetSnapshot(ctx, p.repo, id)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		if domain.HashContent(raw) != p.snapshots[id] {
			return domain.ErrRepositoryInitializationConflict
		}
	}
	keys := make([]initializationBlobPG, 0, len(p.blobs))
	for key := range p.blobs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].kind != keys[j].kind {
			return keys[i].kind < keys[j].kind
		}
		return keys[i].hash < keys[j].hash
	})
	for _, key := range keys {
		var version string
		err := s.db(ctx).QueryRow(ctx, `SELECT b.xmin::text||':'||b.ctid::text||':'||rb.xmin::text||':'||rb.ctid::text
 FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash WHERE rb.repo_id=$1 AND rb.kind=$2 AND rb.hash=$3 FOR SHARE OF b,rb NOWAIT`, p.repo, key.kind, key.hash).Scan(&version)
		if err != nil {
			return initializationProofError(err)
		}
		if version != p.blobs[key] {
			return domain.ErrRepositoryInitializationConflict
		}
	}
	for _, id := range initializationSortedHashes(p.settings) {
		var version string
		err := s.db(ctx).QueryRow(ctx, `SELECT xmin::text||':'||ctid::text FROM settings_objects WHERE repo_id=$1 AND hash=$2 FOR SHARE NOWAIT`, p.repo, id).Scan(&version)
		if err != nil {
			return initializationProofError(err)
		}
		if version != p.settings[id] {
			return domain.ErrRepositoryInitializationConflict
		}
	}
	return nil
}
func initializationSortedHashes[V ~string](m map[domain.ContentHash]V) []domain.ContentHash {
	out := make([]domain.ContentHash, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
func initializationProofError(err error) error {
	var pg *pgconn.PgError
	if errors.Is(mapNoRows(err), domain.ErrNotFound) || (errors.As(err, &pg) && pg.Code == "55P03") {
		return domain.ErrRepositoryInitializationConflict
	}
	return err
}
