//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Catalog cursors select immutable images, never current mutable rows. They
// confer no access: every transport request must authorize the repository.
type catalogCursor struct {
	Version int                `json:"version"`
	Scope   string             `json:"scope"`
	RepoID  domain.ContentHash `json:"repo_id"`
	Epoch   string             `json:"epoch"`
	Through int64              `json:"through"`
	Floor   int64              `json:"floor"`
	Mode    string             `json:"mode"`
	After   int64              `json:"after"`
	Seq     int64              `json:"seq"`
	Kind    string             `json:"kind"`
	Key     string             `json:"key"`
}

func decodeCatalogCursor(encoded string) (catalogCursor, error) {
	var cur catalogCursor
	if len(encoded) > 16<<10 {
		return cur, domain.ErrValidation
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || json.Unmarshal(raw, &cur) != nil {
		return cur, domain.ErrValidation
	}
	canonical, err := json.Marshal(cur)
	if err != nil || !bytes.Equal(raw, canonical) {
		// Internally generated cursors have one canonical encoding. Reject
		// duplicate/unknown fields and alternate spellings rather than guess.
		return cur, domain.ErrValidation
	}
	if cur.Version != domain.CatalogVersion || cur.Scope != domain.CatalogScope {
		return cur, domain.ErrCatalogResetRequired
	}
	if cur.Through < 0 || cur.Floor < 0 || cur.Floor > cur.Through || cur.After < 0 || cur.After > cur.Through || cur.Seq < 0 || cur.Seq > cur.Through ||
		(cur.Mode != "baseline" && cur.Mode != "delta") || (cur.Mode == "delta" && cur.Seq <= cur.After) ||
		(cur.Kind != "snapshot" && cur.Kind != "ref" && cur.Kind != "history" && cur.Kind != "protocol") || cur.Key == "" {
		return cur, domain.ErrValidation
	}
	return cur, nil
}

// CatalogChanges returns a fixed-generation baseline or delta. The baseline
// reads each key's latest journal image, so it can be paginated without holding
// a transaction open between HTTP requests. Only the final page publishes a
// checkpoint; consumers stage intermediate pages before atomically applying it.
func (s *PostgresStore) CatalogChanges(ctx context.Context, repo domain.ContentHash, request domain.CatalogRequest) (domain.CatalogPage, error) {
	var out domain.CatalogPage
	if err := validateHash(repo); err != nil {
		return out, err
	}
	if err := request.Validate(); err != nil {
		return out, err
	}
	err := s.WithinReadSnapshot(ctx, func(ctx context.Context) error {
		if !s.InReadOnlyTransaction(ctx) {
			// Do not advertise uncommitted writes from a caller-owned write
			// transaction as a committed synchronization checkpoint.
			return domain.ErrConflict
		}
		var epoch string
		var head, floor int64
		err := s.db(ctx).QueryRow(ctx, `SELECT epoch::text,head_seq,floor_seq FROM repository_catalog_state WHERE repo_id=$1`, string(repo)).Scan(&epoch, &head, &floor)
		if err != nil {
			return mapNoRows(err)
		}
		cur := catalogCursor{Version: domain.CatalogVersion, Scope: domain.CatalogScope, RepoID: repo, Epoch: epoch, Through: head, Floor: floor, Mode: "baseline"}
		if request.Cursor != "" {
			cur, err = decodeCatalogCursor(request.Cursor)
			if err != nil {
				return err
			}
			if cur.RepoID != repo || cur.Epoch != epoch || cur.Through > head || cur.Floor != floor {
				return domain.ErrCatalogResetRequired
			}
		} else if request.After != nil {
			after := request.After
			if after.Version != domain.CatalogVersion || after.RepoID != repo || after.Epoch != epoch || after.Sequence < floor || after.Sequence > head {
				return domain.ErrCatalogResetRequired
			}
			cur.Mode, cur.After, cur.Seq = "delta", after.Sequence, after.Sequence
		}
		if cur.Mode == "delta" && cur.After < floor {
			return domain.ErrCatalogResetRequired
		}
		limit := request.Limit
		if limit == 0 {
			limit = domain.DefaultCatalogLimit
		}
		out = domain.CatalogPage{Version: domain.CatalogVersion, RepoID: repo, Epoch: epoch, Through: cur.Through, Mode: cur.Mode, Entries: []domain.CatalogEntry{}}
		query := `SELECT seq,entity_kind,entity_key,deleted,after_image
			FROM repository_catalog_changes
			WHERE repo_id=$1 AND epoch=$2 AND seq<=$3 AND seq>$4
			  AND (seq,entity_kind,entity_key)>($5,$6 COLLATE "C",$7 COLLATE "C")
			ORDER BY seq,entity_kind,entity_key LIMIT $8`
		args := []any{string(repo), epoch, cur.Through, cur.After, cur.Seq, cur.Kind, cur.Key, limit + 1}
		if cur.Mode == "baseline" {
			query = `SELECT seq,entity_kind,entity_key,deleted,after_image FROM (
				SELECT DISTINCT ON (entity_kind,entity_key) seq,entity_kind,entity_key,deleted,after_image
				FROM repository_catalog_changes
				WHERE repo_id=$1 AND epoch=$2 AND seq<=$3
				  AND (entity_kind,entity_key)>($4 COLLATE "C",$5 COLLATE "C")
				ORDER BY entity_kind,entity_key,seq DESC
			) latest WHERE NOT deleted ORDER BY entity_kind,entity_key LIMIT $6`
			args = []any{string(repo), epoch, cur.Through, cur.Kind, cur.Key, limit + 1}
		}
		rows, err := s.db(ctx).Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item domain.CatalogEntry
			if err := rows.Scan(&item.Sequence, &item.Kind, &item.Key, &item.Deleted, &item.Value); err != nil {
				return err
			}
			out.Entries = append(out.Entries, item)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out.Entries) > limit {
			out.Entries = out.Entries[:limit]
			last := out.Entries[len(out.Entries)-1]
			cur.Seq, cur.Kind, cur.Key = last.Sequence, last.Kind, last.Key
			raw, err := json.Marshal(cur)
			if err != nil {
				return err
			}
			out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
		} else {
			out.Checkpoint = &domain.CatalogCheckpoint{Version: domain.CatalogVersion, RepoID: repo, Epoch: epoch, Sequence: cur.Through}
		}
		return nil
	})
	if err != nil {
		return domain.CatalogPage{}, err
	}
	return out, nil
}
