//go:build postgres

package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memoryCaptureStoredHeader struct {
	header     domain.MemoryCaptureHeader
	raw        []byte
	partCount  int64
	bodyBytes  int64
	keyBytes   int64
	expiresAt  time.Time
	anchorID   domain.ContentHash
	checkpoint domain.RepositoryViewSourceCheckpoint
}

func readMemoryCaptureHeader(ctx context.Context, transaction pgx.Tx, repo, identifier domain.ContentHash, lock bool) (memoryCaptureStoredHeader, error) {
	query := `SELECT complete,expires_at>clock_timestamp(),
		isfinite(expires_at) AND isfinite(created_at) AND expires_at>created_at AND expires_at<=created_at+interval '24 hours',
		part_count,body_bytes,key_bytes,octet_length(header),CASE WHEN octet_length(header)<=$3 THEN header END,
		expires_at,capture_version,octet_length(graph_epoch),CASE WHEN octet_length(graph_epoch)=36 THEN graph_epoch END,
		graph_sequence,octet_length(graph_anchor_id),CASE WHEN octet_length(graph_anchor_id)=71 THEN graph_anchor_id END
		FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`
	if lock {
		query += ` FOR KEY SHARE`
	}
	var stored memoryCaptureStoredHeader
	var complete, live, retentionValid bool
	var headerBytes int64
	var version, epochBytes, anchorBytes *int
	var epoch, anchor *string
	var sequence *int64
	if err := transaction.QueryRow(ctx, query, string(repo), string(identifier), memoryCaptureHeaderBytes).
		Scan(&complete, &live, &retentionValid, &stored.partCount, &stored.bodyBytes, &stored.keyBytes, &headerBytes, &stored.raw,
			&stored.expiresAt, &version, &epochBytes, &epoch, &sequence, &anchorBytes, &anchor); err != nil {
		return memoryCaptureStoredHeader{}, mapNoRows(err)
	}
	if !complete || !retentionValid || stored.partCount < 1 || stored.bodyBytes < 0 || stored.keyBytes < 0 || headerBytes < 1 {
		return memoryCaptureStoredHeader{}, domain.ErrIntegrity
	}
	if !live {
		return memoryCaptureStoredHeader{}, domain.ErrMemoryCaptureExpired
	}
	if stored.partCount > domain.MaxMemoryCaptureParts || headerBytes > memoryCaptureHeaderBytes || stored.bodyBytes > domain.MaxMemoryCaptureBytes ||
		stored.keyBytes > domain.MaxMemoryCaptureBytes || stored.bodyBytes+stored.keyBytes+headerBytes+stored.partCount*128 > domain.MaxMemoryCaptureBytes {
		return memoryCaptureStoredHeader{}, domain.ErrMemoryProjectionLimit
	}
	if int64(len(stored.raw)) != headerBytes {
		return memoryCaptureStoredHeader{}, domain.ErrIntegrity
	}
	if err := domain.DecodeRepositorySourceJSON(ctx, stored.raw, &stored.header); err != nil {
		return memoryCaptureStoredHeader{}, err
	}
	capture := stored.header
	if (capture.Version != domain.MemoryCaptureVersion && capture.Version != domain.MemoryCaptureDeltaVersion) ||
		capture.Rules != domain.MemoryCaptureRules || capture.Generation.Validate() != nil || capture.Selection.Validate() != nil ||
		capture.Generation.Graph.RepoID != repo || capture.PinsCaptured != (capture.Selection.Branch != "") ||
		(capture.Content != "" && capture.Content != "claims" && capture.Content != "prompt") {
		return memoryCaptureStoredHeader{}, domain.ErrIntegrity
	}
	if version == nil {
		if capture.Version != domain.MemoryCaptureVersion || epochBytes != nil || sequence != nil || anchorBytes != nil {
			return memoryCaptureStoredHeader{}, domain.ErrIntegrity
		}
	} else {
		if *version != capture.Version || epoch == nil || *epoch != capture.Generation.Graph.Epoch ||
			sequence == nil || *sequence != capture.Generation.Graph.Sequence {
			return memoryCaptureStoredHeader{}, domain.ErrIntegrity
		}
		if capture.Version == domain.MemoryCaptureVersion {
			if anchorBytes != nil {
				return memoryCaptureStoredHeader{}, domain.ErrIntegrity
			}
		} else {
			if anchor == nil || domain.ValidateContentHash(domain.ContentHash(*anchor)) != nil || domain.ContentHash(*anchor) == identifier {
				return memoryCaptureStoredHeader{}, domain.ErrIntegrity
			}
			stored.anchorID = domain.ContentHash(*anchor)
		}
	}
	return stored, ctx.Err()
}

func memoryCaptureCheckAnchor(ctx context.Context, transaction pgx.Tx, stored *memoryCaptureStoredHeader, lock bool) error {
	if stored.anchorID == "" {
		return ctx.Err()
	}
	anchor, err := readMemoryCaptureHeader(ctx, transaction, stored.header.Generation.Graph.RepoID, stored.anchorID, lock)
	if err != nil {
		return err
	}
	checkpoint := anchor.header.Generation.Graph
	current := stored.header.Generation.Graph
	if anchor.header.Version != domain.MemoryCaptureVersion || anchor.anchorID != "" || anchor.header.Rules != stored.header.Rules ||
		checkpoint.RepoID != current.RepoID || checkpoint.Scope != current.Scope || checkpoint.Version != current.Version ||
		checkpoint.Epoch != current.Epoch || checkpoint.Sequence > current.Sequence || stored.expiresAt.After(anchor.expiresAt) {
		return domain.ErrIntegrity
	}
	stored.checkpoint = checkpoint
	return ctx.Err()
}

func memoryCaptureCheckExpiry(ctx context.Context, transaction pgx.Tx, repo, identifier domain.ContentHash) error {
	var live, anchorPresent, anchorLive, expiryValid bool
	if err := transaction.QueryRow(ctx, `SELECT expires_at>clock_timestamp(),
		capture.graph_anchor_id IS NULL OR anchor.capture_id IS NOT NULL,
		capture.graph_anchor_id IS NULL OR COALESCE(anchor.anchor_expires_at>clock_timestamp(),false),
		capture.graph_anchor_id IS NULL OR COALESCE(capture.expires_at<=anchor.anchor_expires_at,false)
		FROM memory_capture_headers capture LEFT JOIN
		(SELECT repo_id,capture_id,expires_at AS anchor_expires_at FROM memory_capture_headers) anchor
		ON anchor.repo_id=capture.repo_id AND anchor.capture_id=capture.graph_anchor_id
		WHERE capture.repo_id=$1 AND capture.capture_id=$2`, string(repo), string(identifier)).
		Scan(&live, &anchorPresent, &anchorLive, &expiryValid); err != nil {
		return mapNoRows(err)
	}
	if !anchorPresent {
		return domain.ErrNotFound
	}
	if !live || !anchorLive {
		return domain.ErrMemoryCaptureExpired
	}
	if !expiryValid {
		return domain.ErrIntegrity
	}
	return ctx.Err()
}
