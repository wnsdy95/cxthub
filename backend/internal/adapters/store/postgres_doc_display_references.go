//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docDisplayObjectBatch struct {
	hashes, parents, children []string
	bodies                    [][]byte
	counts                    []int64
}

func prepareDocDisplayObjectBatch(ctx context.Context, objects []domain.DocRangeObject) (docDisplayObjectBatch, int, error) {
	batch := docDisplayObjectBatch{}
	seen := make(map[domain.ContentHash]int)
	consumed, size := 0, 0
	for _, object := range objects {
		if err := ctx.Err(); err != nil {
			return docDisplayObjectBatch{}, 0, err
		}
		if consumed == domain.MaxDocEventDisplayStepObjects {
			break
		}
		if len(object.Bytes) == 0 || len(object.Bytes) > domain.MaxDocDisplayPageBytes {
			return docDisplayObjectBatch{}, 0, domain.ErrIntegrity
		}
		if size > domain.MaxDocEventDisplayStepBytes-len(object.Bytes) {
			break
		}
		if domain.HashContent(object.Bytes) != object.Hash {
			return docDisplayObjectBatch{}, 0, domain.ErrIntegrity
		}
		consumed++
		size += len(object.Bytes)
		if index, exists := seen[object.Hash]; exists {
			if !bytes.Equal(batch.bodies[index], object.Bytes) {
				return docDisplayObjectBatch{}, 0, domain.ErrIntegrity
			}
			continue
		}
		candidates, err := domain.DocDisplayHashCandidates(ctx, object.Bytes)
		if err != nil {
			return docDisplayObjectBatch{}, 0, err
		}
		seen[object.Hash] = len(batch.hashes)
		batch.hashes = append(batch.hashes, string(object.Hash))
		batch.bodies = append(batch.bodies, object.Bytes)
		batch.counts = append(batch.counts, int64(len(candidates)))
		for _, child := range candidates {
			batch.parents = append(batch.parents, string(object.Hash))
			batch.children = append(batch.children, string(child))
		}
	}
	return batch, consumed, nil
}

func putDocDisplayObjects(ctx context.Context, transaction pgx.Tx, source domain.ContentHash, objects []domain.DocRangeObject) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if domain.ValidateContentHash(source) != nil {
		return domain.ErrIntegrity
	}
	batch, consumed, err := prepareDocDisplayObjectBatch(ctx, objects)
	if err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `SELECT cxthub_queue_doc_display_maintenance($1)`, source); err != nil {
		return err
	}
	for len(objects) > 0 {
		if consumed <= 0 {
			return domain.ErrIntegrity
		}
		if _, err := transaction.Exec(ctx, `INSERT INTO doc_display_objects_v1(source_hash,hash,bytes)
 SELECT $1,requested.hash,requested.bytes FROM unnest($2::text[],$3::bytea[]) AS requested(hash,bytes)
 LEFT JOIN LATERAL (SELECT 1 AS present FROM doc_display_objects_v1 object
 WHERE object.source_hash=$1 AND object.hash=requested.hash LIMIT 1) existing ON true
 WHERE existing.present IS NULL ON CONFLICT DO NOTHING`, source, batch.hashes, batch.bodies); err != nil {
			return err
		}
		if err := indexDocDisplayReferenceBatch(ctx, transaction, source, batch); err != nil {
			return err
		}
		objects = objects[consumed:]
		if len(objects) > 0 {
			batch, consumed, err = prepareDocDisplayObjectBatch(ctx, objects)
			if err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func indexDocDisplayObjectReferences(ctx context.Context, transaction pgx.Tx, source, hash domain.ContentHash, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if domain.ValidateContentHash(source) != nil {
		return domain.ErrIntegrity
	}
	batch, _, err := prepareDocDisplayObjectBatch(ctx, []domain.DocRangeObject{{Hash: hash, Bytes: raw}})
	if err != nil {
		return err
	}
	return indexDocDisplayReferenceBatch(ctx, transaction, source, batch)
}

func indexDocDisplayReferenceBatch(ctx context.Context, transaction pgx.Tx, source domain.ContentHash, batch docDisplayObjectBatch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var present int
	if err := transaction.QueryRow(ctx, `SELECT 1 FROM doc_display_sources_v1 WHERE source_hash=$1 FOR NO KEY UPDATE`, source).Scan(&present); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrIntegrity
		}
		return err
	}
	rows, err := transaction.Query(ctx, `WITH requested AS (
 SELECT * FROM unnest($2::text[],$3::bytea[],$4::bigint[]) AS requested(hash,bytes,edges)
 ), expected AS (
 SELECT * FROM unnest($5::text[],$6::text[]) AS expected(parent_hash,child_hash)
 ), matched AS (
 SELECT expected.parent_hash,count(*) AS edges FROM expected CROSS JOIN LATERAL (
 SELECT 1 FROM doc_display_object_edges_v1 edge WHERE edge.source_hash=$1
 AND edge.parent_hash=expected.parent_hash AND edge.child_hash=expected.child_hash LIMIT 1
 ) edge GROUP BY expected.parent_hash
 )
 SELECT object.hash,object.agrees,object.references_version,
 coalesce(matched.edges,0)=requested.edges AND
 (SELECT count(*) FROM (SELECT 1 FROM doc_display_object_edges_v1 edge
 WHERE edge.source_hash=$1 AND edge.parent_hash=object.hash LIMIT requested.edges+1) existing_edges)=requested.edges
 FROM requested CROSS JOIN LATERAL (
 SELECT object.hash,object.bytes=requested.bytes AS agrees,object.references_version FROM doc_display_objects_v1 object
 WHERE object.source_hash=$1 AND object.hash=requested.hash LIMIT 1 FOR UPDATE OF object
 ) object LEFT JOIN matched ON matched.parent_hash=object.hash ORDER BY object.hash`,
		source, batch.hashes, batch.bodies, batch.counts, batch.parents, batch.children)
	if err != nil {
		return err
	}
	pending := make(map[string]struct{})
	found := 0
	for rows.Next() {
		var hash string
		var agrees, covered bool
		var version int16
		if err := rows.Scan(&hash, &agrees, &version, &covered); err != nil {
			rows.Close()
			return err
		}
		if !agrees || version != 0 && version != 1 || version == 1 && !covered {
			rows.Close()
			return domain.ErrIntegrity
		}
		found++
		if version == 0 {
			pending[hash] = struct{}{}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if found != len(batch.hashes) {
		return domain.ErrIntegrity
	}
	if len(pending) == 0 {
		return ctx.Err()
	}
	parents, children := make([]string, 0), make([]string, 0)
	for index, parent := range batch.parents {
		if _, exists := pending[parent]; exists {
			parents = append(parents, parent)
			children = append(children, batch.children[index])
		}
	}
	if len(parents) > 0 {
		if _, err := transaction.Exec(ctx, `INSERT INTO doc_display_object_edges_v1(source_hash,parent_hash,child_hash)
 SELECT $1,parent_hash,child_hash FROM unnest($2::text[],$3::text[]) AS expected(parent_hash,child_hash) ON CONFLICT DO NOTHING`, source, parents, children); err != nil {
			return err
		}
	}
	hashes, counts := make([]string, 0, len(pending)), make([]int64, 0, len(pending))
	for index, hash := range batch.hashes {
		if _, exists := pending[hash]; exists {
			hashes = append(hashes, hash)
			counts = append(counts, batch.counts[index])
		}
	}
	tag, err := transaction.Exec(ctx, `UPDATE doc_display_objects_v1 object SET references_version=1
 FROM unnest($2::text[],$3::bigint[]) AS expected(hash,edges)
 WHERE object.source_hash=$1 AND object.hash=expected.hash AND object.references_version=0
 AND (SELECT count(*) FROM (SELECT 1 FROM doc_display_object_edges_v1 edge
 WHERE edge.source_hash=$1 AND edge.parent_hash=object.hash LIMIT expected.edges+1) existing_edges)=expected.edges`, source, hashes, counts)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != int64(len(pending)) {
		return domain.ErrIntegrity
	}
	return ctx.Err()
}
