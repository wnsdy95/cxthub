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

type historyDocumentProofKeyPG struct{}

// Only version metadata survives preparation. This evidence is neither a
// durable certificate nor a replacement for this request's full byte reads.
type historyDocumentProofPG struct {
	owner                       *PostgresStore
	repo                        domain.ContentHash
	active, attempted, captured bool
	snapshots, blobs            map[string]string
	grants                      map[string]map[string]string
}

var _ outbound.HistoryDocumentProofStore = (*PostgresStore)(nil)

func (s *PostgresStore) WithinHistoryDocumentProof(ctx context.Context, repo domain.ContentHash, fn func(context.Context, outbound.HistoryDocumentProof) error) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if prior, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx); ok {
		if prior.owner != s || prior.readOnly || prior.repo != repo {
			return domain.ErrConflict
		}
		return fn(ctx, nil) // Preserve uncommitted caller-owned write state.
	}
	if ctx.Value(historyDocumentProofKeyPG{}) != nil {
		return domain.ErrConflict
	}
	p := &historyDocumentProofPG{owner: s, repo: repo, active: true}
	defer func() { p.active = false }()
	return fn(context.WithValue(ctx, historyDocumentProofKeyPG{}, p), p)
}

func (p *historyDocumentProofPG) transaction(ctx context.Context, readOnly bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ctx.Value(historyDocumentProofKeyPG{}) != p || !p.active {
		return domain.ErrConflict
	}
	tx, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || tx.owner != p.owner || tx.readOnly != readOnly || (!readOnly && tx.repo != p.repo) {
		return domain.ErrConflict
	}
	return nil
}

func (p *historyDocumentProofPG) Capture(ctx context.Context, evidence []domain.Snapshot) error {
	if err := p.transaction(ctx, true); err != nil {
		return err
	}
	if p.attempted {
		return domain.ErrConflict
	}
	p.attempted = true
	snapshots, blobs := map[string]string{}, map[string]string{}
	grants := map[string]map[string]string{"chunk": {}, "doc": {}}
	for _, snap := range evidence {
		if err := snap.DocumentRef().Validate(); err != nil {
			return err
		}
		if snap.RepoID != p.repo || snap.ID != snap.DocHash || validateHash(snap.ID) != nil {
			return domain.ErrIntegrity
		}
		if _, duplicate := snapshots[string(snap.ID)]; duplicate {
			return domain.ErrIntegrity
		}
		current, err := p.owner.GetSnapshot(ctx, p.repo, snap.ID)
		if err != nil {
			return err
		}
		a, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		b, err := json.Marshal(current)
		if err != nil {
			return err
		}
		if string(a) != string(b) {
			return domain.ErrConflict
		}
		snapshots[string(snap.ID)] = ""
		grants["doc"][string(snap.DocHash)] = ""
		blobs[string(snap.DocHash)] = ""
		// The descriptor and its closure are observed in the very same RR
		// snapshot as full verification, never on a pool/second connection.
		raw, err := p.owner.readOwnedDocObject(ctx, p.repo, "doc", snap.DocHash)
		if err != nil {
			return err
		}
		manifest, root, err := storedConversationManifest(ctx, raw)
		if err != nil {
			return err
		}
		if root {
			if snap.DocIdentity != domain.DocumentIdentityRootV1 {
				return domain.ErrIntegrity
			}
			repo, err := p.owner.GetRepo(ctx, p.repo)
			if err != nil {
				return err
			}
			if err := outbound.CheckDocumentIdentityCompatibility(ctx, repo.RequiredDocIdentity); err != nil {
				return err
			}
			if repo.RequiredDocIdentity != domain.DocumentIdentityRootV1 {
				return domain.ErrRootPublicationDisabled
			}
			// Root descriptors are strict and hash-bound. Re-read the complete
			// ordered owned closure in this SAME RR snapshot; no receipt or
			// generic full-CIR/legacy descriptor can authorize root pins.
			proof, err := p.owner.docProofs.verifyConversation(ctx, snap.DocHash, manifest, p.owner.ownedDocChunkReader(p.repo, conversationChunkOrder(manifest)))
			if err != nil {
				return err
			}
			if proof.DocumentRef() != snap.DocumentRef() || !proof.Reference().Matches(snap) {
				return domain.ErrIntegrity
			}
			// Canonical root hashing binds the complete descriptor, including
			// every ordered/repeated occurrence and declared length.
			hash, err := domain.ConversationManifestHash(manifest)
			if err != nil {
				return err
			}
			if hash != snap.DocHash {
				return domain.ErrIntegrity
			}
			for _, chunk := range manifest.Chunks {
				grants["chunk"][string(chunk.Hash)] = ""
				blobs[string(chunk.Hash)] = ""
			}
			continue
		}
		if snap.DocIdentity != domain.DocumentIdentityLegacy {
			return domain.ErrIntegrity
		}
		data, err := docDecompress(raw)
		if err != nil {
			return domain.ErrIntegrity
		}
		if manifest, chunked := domain.ParseDocChunkManifest(data); chunked {
			for _, hash := range manifest.Chunks {
				if validateHash(hash) != nil {
					return domain.ErrIntegrity
				}
				grants["chunk"][string(hash)] = ""
				blobs[string(hash)] = ""
			}
		}
	}
	if err := p.versions(ctx, snapshots, blobs, grants, false); err != nil {
		return err
	}
	// A failed capture never leaves a partial proof available to Pin.
	p.snapshots, p.blobs, p.grants = snapshots, blobs, grants
	p.captured = true
	return nil
}

func (p *historyDocumentProofPG) Pin(ctx context.Context) error {
	if err := p.transaction(ctx, false); err != nil {
		return err
	}
	if !p.captured {
		return domain.ErrConflict
	}
	return p.versions(ctx, p.snapshots, p.blobs, p.grants, true)
}

// Pin tables in one explicit order: snapshots, global blobs, then grants. All
// keys are sorted and every successful batch stays locked in the outer write
// transaction. NOWAIT aborts on an in-flight mutator; it never skips a row.
func (p *historyDocumentProofPG) versions(ctx context.Context, snapshots, blobs map[string]string, grants map[string]map[string]string, pin bool) error {
	suffix := ""
	if pin {
		suffix = " FOR SHARE NOWAIT"
	}
	if err := p.versionBatches(ctx, snapshots, pin, `SELECT id,xmin::text||':'||ctid::text FROM snapshots
 WHERE repo_id=$1 AND id=ANY($2::text[]) ORDER BY id`+suffix, string(p.repo)); err != nil {
		return err
	}
	if err := p.versionBatches(ctx, blobs, pin, `SELECT hash,xmin::text||':'||ctid::text FROM blobs
 WHERE hash=ANY($1::text[]) ORDER BY hash`+suffix); err != nil {
		return err
	}
	for _, kind := range []string{"chunk", "doc"} {
		if err := p.versionBatches(ctx, grants[kind], pin, `SELECT hash,xmin::text||':'||ctid::text FROM repo_blobs
 WHERE repo_id=$1 AND kind=$2 AND hash=ANY($3::text[]) ORDER BY hash`+suffix, string(p.repo), kind); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (p *historyDocumentProofPG) versionBatches(ctx context.Context, expected map[string]string, pin bool, query string, prefix ...any) error {
	keys := make([]string, 0, len(expected))
	for key := range expected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	const batchSize = 16
	for start := 0; start < len(keys); start += batchSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		wanted := keys[start:min(start+batchSize, len(keys))]
		args := append(append([]any{}, prefix...), wanted)
		rows, err := p.owner.db(ctx).Query(ctx, query, args...)
		if err != nil {
			return historyDocumentProofErrorPG(err)
		}
		seen := make(map[string]bool, len(wanted))
		for rows.Next() {
			var key, version string
			if err := rows.Scan(&key, &version); err != nil {
				rows.Close()
				return err
			}
			index := sort.SearchStrings(wanted, key)
			if index == len(wanted) || wanted[index] != key || seen[key] || version == "" || (pin && expected[key] != version) {
				rows.Close()
				return domain.ErrConflict
			}
			seen[key] = true
			if !pin {
				expected[key] = version
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return historyDocumentProofErrorPG(err)
		}
		if len(seen) != len(wanted) {
			return domain.ErrConflict
		}
	}
	return ctx.Err()
}

func historyDocumentProofErrorPG(err error) error {
	var pg *pgconn.PgError
	if errors.Is(mapNoRows(err), domain.ErrNotFound) || (errors.As(err, &pg) && pg.Code == "55P03") {
		return domain.ErrConflict
	}
	return err
}
