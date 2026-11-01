//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const (
	maxMemoryCaptureBytes    = 64 << 20
	maxMemoryCaptureRecords  = 4096
	maxMemoryCapturePinBytes = 4 << 20
)

var _ outbound.BoundedMemoryReader = (*PostgresStore)(nil)
var _ outbound.MemoryCapturePinReader = (*PostgresStore)(nil)

func (storage *PostgresStore) memoryCaptureTransaction(ctx context.Context) (*repositoryTx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	transaction, present := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if storage == nil || !present || transaction == nil || transaction.owner != storage || !transaction.readOnly || transaction.Tx == nil {
		return nil, domain.ErrConflict
	}
	return transaction, nil
}

func (storage *PostgresStore) GetMemoryForCapture(ctx context.Context, repo, hash domain.ContentHash, maxBytes int) (domain.MemoryDigest, error) {
	return storage.getMemoryForCapture(ctx, repo, hash, maxBytes, nil)
}

func (storage *PostgresStore) getMemoryForCapture(ctx context.Context, repo, hash domain.ContentHash, maxBytes int, witness *domain.MemoryObjectWitness) (domain.MemoryDigest, error) {
	var digest domain.MemoryDigest
	transaction, err := storage.memoryCaptureTransaction(ctx)
	if err != nil {
		return digest, err
	}
	if maxBytes <= 0 || maxBytes > maxMemoryCaptureBytes {
		return digest, domain.ErrValidation
	}
	if err := validateHashes(repo, hash); err != nil {
		return digest, err
	}
	if transaction.repo != "" && transaction.repo != repo {
		return digest, domain.ErrConflict
	}
	storedRemaining := maxBytes
	read := func(kind string, object domain.ContentHash, limit int) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var size int
		var raw []byte
		query := `SELECT octet_length(body.bytes),
 CASE WHEN octet_length(body.bytes)<=$4 THEN body.bytes END
 FROM repo_blobs owned JOIN blobs body ON body.hash=owned.hash
 WHERE owned.repo_id=$1 AND owned.kind=$2 AND owned.hash=$3`
		var incarnation *string
		var version int64
		destinations := []any{&size, &raw}
		if witness != nil {
			query = `SELECT octet_length(body.bytes),
 CASE WHEN octet_length(body.bytes)<=$4 THEN body.bytes END,
 source.incarnation::text,body.doc_range_source_version
 FROM repo_blobs owned JOIN blobs body ON body.hash=owned.hash
 LEFT JOIN blob_source_incarnations source ON source.hash=body.hash
 WHERE owned.repo_id=$1 AND owned.kind=$2 AND owned.hash=$3`
			destinations = append(destinations, &incarnation, &version)
		}
		err := transaction.QueryRow(ctx, query, string(repo), kind, string(object), storedRemaining).Scan(destinations...)
		if errors.Is(err, pgx.ErrNoRows) {
			if kind == "memory" {
				return nil, domain.ErrNotFound
			}
			return nil, domain.ErrIntegrity
		}
		if err != nil {
			return nil, err
		}
		if size > storedRemaining {
			return nil, domain.ErrMemoryProjectionLimit
		}
		if size < 0 || len(raw) != size {
			return nil, domain.ErrIntegrity
		}
		if witness != nil {
			if incarnation == nil {
				return nil, domain.ErrIntegrity
			}
			observed := domain.MemoryObjectVersion{Hash: object, Incarnation: *incarnation, Version: version}
			if err := observed.Validate(); err != nil {
				return nil, err
			}
			if kind == "memory" {
				witness.Root = &observed
			} else {
				if len(witness.Components) == domain.MaxMemoryCaptureObjects-1 {
					return nil, domain.ErrMemoryProjectionLimit
				}
				witness.Components = append(witness.Components, observed)
			}
		}
		storedRemaining -= size
		return boundedMemoryCaptureObject(ctx, raw, limit)
	}
	root, err := read("memory", hash, maxBytes)
	if err != nil {
		return digest, err
	}
	records := 0
	if err := domain.CountMemoryCaptureRecords(ctx, bytes.NewReader(root), &records); err != nil {
		return digest, err
	}
	manifest, chunked, err := domain.ParseMemoryChunkManifest(root)
	if err != nil {
		return digest, domain.ErrIntegrity
	}
	if chunked {
		occurrences := make(map[domain.ContentHash]int)
		order := make([]domain.ContentHash, 0)
		for _, components := range [][]domain.ContentHash{manifest.SummaryChunks, manifest.FragmentChunks} {
			for _, component := range components {
				if domain.ValidateContentHash(component) != nil {
					return digest, domain.ErrIntegrity
				}
				if occurrences[component] == 0 {
					order = append(order, component)
				}
				occurrences[component]++
			}
		}
		remaining := maxBytes - len(root)
		bodies := make(map[domain.ContentHash][]byte, len(order))
		for _, component := range order {
			body, err := read("memory_chunk", component, remaining/occurrences[component])
			if err != nil {
				return digest, err
			}
			if domain.HashContent(body) != component {
				return digest, domain.ErrIntegrity
			}
			remaining -= len(body) * occurrences[component]
			bodies[component] = body
		}
		if len(manifest.FragmentChunks) > 0 {
			fragments := make([]io.Reader, 0, len(manifest.FragmentChunks))
			for _, component := range manifest.FragmentChunks {
				fragments = append(fragments, bytes.NewReader(bodies[component]))
			}
			if err := domain.CountMemoryCaptureRecords(ctx, io.MultiReader(fragments...), &records); err != nil {
				return digest, err
			}
		}
		if err := ctx.Err(); err != nil {
			return digest, err
		}
		digest, err = domain.AssembleMemoryChunks(manifest, bodies)
	} else {
		err = json.Unmarshal(root, &digest)
	}
	if err != nil || validateMemoryDigestRefs(digest) != nil || digest.ValidateMemoryClaims() != nil {
		return domain.MemoryDigest{}, domain.ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return domain.MemoryDigest{}, err
	}
	actual, err := domain.MemoryDigestHashBounded(digest, maxBytes)
	if err != nil {
		return domain.MemoryDigest{}, err
	}
	if actual != hash {
		return domain.MemoryDigest{}, domain.ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return domain.MemoryDigest{}, err
	}
	return digest, nil
}

func boundedMemoryCaptureObject(ctx context.Context, raw []byte, limit int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 0 || limit > maxMemoryCaptureBytes {
		return nil, domain.ErrValidation
	}
	reader, closeReader, err := docObjectStream(raw)
	if err != nil {
		return nil, domain.ErrIntegrity
	}
	defer closeReader()
	var body []byte
	var buffer [32 << 10]byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, err := reader.Read(buffer[:min(len(buffer), limit-len(body)+1)])
		if count > limit-len(body) {
			return nil, domain.ErrMemoryProjectionLimit
		}
		body = append(body, buffer[:count]...)
		if errors.Is(err, io.EOF) {
			return body, ctx.Err()
		}
		if err != nil {
			return nil, domain.ErrIntegrity
		}
	}
}

func (storage *PostgresStore) ReadMemoryPinsForCapture(ctx context.Context, repo domain.ContentHash) ([]domain.PRContributionMemory, []domain.GitContributionMemory, error) {
	transaction, err := storage.memoryCaptureTransaction(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return nil, nil, err
	}
	records, remaining := maxMemoryCaptureRecords, maxMemoryCapturePinBytes-4
	prPins, err := readMemoryCapturePins[domain.PRContributionMemory](ctx, transaction, repo, "pr_contribution_memories", &records, &remaining)
	if err != nil {
		return nil, nil, err
	}
	gitPins, err := readMemoryCapturePins[domain.GitContributionMemory](ctx, transaction, repo, "git_contribution_memories", &records, &remaining)
	if err != nil {
		return nil, nil, err
	}
	return prPins, gitPins, nil
}

func readMemoryCapturePins[Pin interface {
	domain.PRContributionMemory | domain.GitContributionMemory
	Validate() error
}](ctx context.Context, transaction *repositoryTx, repo domain.ContentHash, table string, records, remaining *int) ([]Pin, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := transaction.Query(ctx, `WITH bounded AS MATERIALIZED (
 SELECT id,contribution_id,publication_id,memory_hash,memory_source,memory_pinned,payload,octet_length(payload::text) AS size
 FROM `+table+` WHERE repo_id=$1 ORDER BY id LIMIT $2
 ), budgeted AS (
 SELECT *,sum(size+1) OVER (ORDER BY id) AS total,row_number() OVER (ORDER BY id) AS ordinal FROM bounded
 ) SELECT id,contribution_id,publication_id,memory_hash,memory_source,memory_pinned,size,
 CASE WHEN total<=$3 AND ordinal<$2 THEN payload END FROM budgeted ORDER BY id`, string(repo), *records+1, *remaining)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pins := make([]Pin, 0)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var raw []byte
		var size int
		columns := domain.PRContributionMemory{RepoID: repo}
		if err := rows.Scan(&columns.ID, &columns.ContributionID, &columns.PublicationID, &columns.MemoryHash, &columns.MemorySource, &columns.MemoryPinned, &size, &raw); err != nil {
			return nil, err
		}
		if *records == 0 || size >= *remaining || raw == nil {
			return nil, domain.ErrMemoryProjectionLimit
		}
		if size < 0 || len(raw) != size {
			return nil, domain.ErrIntegrity
		}
		var pin Pin
		if json.Unmarshal(raw, &pin) != nil || domain.PRContributionMemory(pin) != columns || pin.Validate() != nil {
			return nil, domain.ErrIntegrity
		}
		cost := max(size, memoryCapturePinWireBytes(columns)) + 1
		if cost > *remaining {
			return nil, domain.ErrMemoryProjectionLimit
		}
		*remaining -= cost
		*records--
		pins = append(pins, pin)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return pins, ctx.Err()
}

func memoryCapturePinWireBytes(pin domain.PRContributionMemory) int {
	return len(`{"id":"","repo_id":"","contribution_id":"","publication_id":"","memory_hash":"","memory_source":"","memory_pinned":true}`) +
		len(pin.ID) + len(pin.RepoID) + len(pin.ContributionID) + len(pin.PublicationID) + len(pin.MemoryHash) + len(pin.MemorySource)
}
