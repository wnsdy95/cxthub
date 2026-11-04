//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.RepositoryViewSourceStore = (*PostgresStore)(nil)
var _ outbound.RepositoryGraphSourceStore = (*PostgresStore)(nil)

type repositoryViewSourceCursor struct {
	Checkpoint domain.RepositoryViewSourceCheckpoint `json:"checkpoint"`
	Floor      int64                                 `json:"floor,string"`
	Baseline   int64                                 `json:"baseline,string"`
	Mode       string                                `json:"mode"`
	After      int64                                 `json:"after,string"`
	Sequence   int64                                 `json:"sequence,string"`
	Kind       string                                `json:"kind"`
	Key        string                                `json:"key"`
}

func decodeRepositoryViewSourceCursor(encoded string) (repositoryViewSourceCursor, error) {
	var cursor repositoryViewSourceCursor
	if len(encoded) > 16<<10 {
		return cursor, domain.ErrValidation
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || json.Unmarshal(raw, &cursor) != nil {
		return cursor, domain.ErrValidation
	}
	canonical, err := json.Marshal(cursor)
	if err != nil || !bytes.Equal(raw, canonical) {
		return cursor, domain.ErrValidation
	}
	if err := cursor.Checkpoint.Validate(); err != nil {
		return cursor, err
	}
	validKind := domain.RepositoryViewSourceKindValid(cursor.Kind)
	if cursor.Checkpoint.Scope == domain.RepositoryGraphSourceScope {
		validKind = domain.RepositoryGraphSourceKindValid(cursor.Kind)
	}
	if cursor.Floor < 0 || cursor.Baseline < 0 || cursor.Floor > cursor.Checkpoint.Sequence || cursor.Baseline > cursor.Checkpoint.Sequence || cursor.After < 0 || cursor.After > cursor.Checkpoint.Sequence || cursor.Sequence < 0 || cursor.Sequence > cursor.Checkpoint.Sequence || !validKind || cursor.Key == "" {
		return cursor, domain.ErrValidation
	}
	switch cursor.Mode {
	case "baseline":
		if cursor.After != 0 {
			return cursor, domain.ErrValidation
		}
	case "delta":
		if cursor.After < max(cursor.Floor, cursor.Baseline) || cursor.Sequence <= cursor.After {
			return cursor, domain.ErrValidation
		}
	default:
		return cursor, domain.ErrValidation
	}
	return cursor, nil
}

func (s *PostgresStore) RepositoryViewSourceChanges(ctx context.Context, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
	return s.repositorySourceChanges(ctx, repo, request, domain.RepositoryViewSourceScope)
}

func (s *PostgresStore) RepositoryGraphSourceChanges(ctx context.Context, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
	return s.repositorySourceChanges(ctx, repo, request, domain.RepositoryGraphSourceScope)
}

func (s *PostgresStore) repositorySourceChanges(ctx context.Context, repo domain.ContentHash, request domain.RepositoryViewSourceRequest, scope string) (domain.RepositoryViewSourcePage, error) {
	var out domain.RepositoryViewSourcePage
	if err := domain.ValidateContentHash(repo); err != nil {
		return out, err
	}
	if err := request.Validate(); err != nil {
		return out, err
	}
	if request.After != nil && request.After.Scope != scope {
		return out, domain.ErrCatalogResetRequired
	}
	sourceView := "repository_view_source_images"
	validKind := domain.RepositoryViewSourceKindValid
	stateQuery := `SELECT catalog.epoch::text,catalog.head_seq,catalog.floor_seq,source.epoch::text,source.baseline_seq
		FROM repository_catalog_state catalog JOIN repository_view_source_state source USING(repo_id)
		WHERE catalog.repo_id=$1`
	if scope == domain.RepositoryGraphSourceScope {
		sourceView, validKind = "repository_graph_source_images", domain.RepositoryGraphSourceKindValid
		stateQuery = repositoryGraphSourceStateSQL
	}
	err := s.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if !s.InReadOnlyTransaction(bound) {
			return domain.ErrConflict
		}
		if err := s.checkRepositoryDocumentIdentity(bound, s.db(bound), repo, false); err != nil {
			return err
		}
		var epoch, sourceEpoch string
		var head, floor, baseline int64
		if err := s.db(bound).QueryRow(bound, stateQuery, string(repo)).Scan(&epoch, &head, &floor, &sourceEpoch, &baseline); err != nil {
			return mapNoRows(err)
		}
		checkpoint := domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: scope, RepoID: repo, Epoch: epoch, Sequence: head}
		if epoch != sourceEpoch || baseline < 0 || baseline > head || floor < 0 || floor > head || checkpoint.Validate() != nil {
			return domain.ErrIntegrity
		}
		cursor := repositoryViewSourceCursor{Checkpoint: checkpoint, Floor: floor, Baseline: baseline, Mode: "baseline"}
		if request.Cursor != "" {
			var err error
			cursor, err = decodeRepositoryViewSourceCursor(request.Cursor)
			if err != nil {
				return err
			}
			if cursor.Checkpoint.Scope != scope || cursor.Checkpoint.RepoID != repo || cursor.Checkpoint.Epoch != epoch || cursor.Checkpoint.Sequence > head || cursor.Checkpoint.Sequence < max(floor, baseline) || cursor.Floor != floor || cursor.Baseline != baseline {
				return domain.ErrCatalogResetRequired
			}
		} else if request.After != nil {
			after := request.After
			if after.RepoID != repo || after.Epoch != epoch || after.Sequence < max(floor, baseline) || after.Sequence > head {
				return domain.ErrCatalogResetRequired
			}
			cursor.Mode, cursor.After, cursor.Sequence = "delta", after.Sequence, after.Sequence
		}
		limit := request.Limit
		if limit == 0 {
			limit = domain.DefaultRepositoryViewSourceLimit
		}
		out = domain.RepositoryViewSourcePage{Version: checkpoint.Version, Scope: checkpoint.Scope, RepoID: repo, Epoch: epoch, Through: cursor.Checkpoint.Sequence, Mode: cursor.Mode, Entries: []domain.RepositoryViewSourceEntry{}}
		query := `SELECT seq,entity_kind,entity_key,deleted,after_image FROM ` + sourceView + `
			WHERE repo_id=$1 AND epoch=$2 AND seq<=$3 AND seq>$4
			AND (seq,entity_kind,entity_key)>($5,$6 COLLATE "C",$7 COLLATE "C")
			ORDER BY seq,entity_kind,entity_key LIMIT $8`
		args := []any{string(repo), epoch, cursor.Checkpoint.Sequence, cursor.After, cursor.Sequence, cursor.Kind, cursor.Key, limit + 1}
		if cursor.Mode == "baseline" {
			query = `SELECT seq,entity_kind,entity_key,deleted,after_image FROM (
				SELECT DISTINCT ON (entity_kind,entity_key) seq,entity_kind,entity_key,deleted,after_image
				FROM ` + sourceView + ` WHERE repo_id=$1 AND epoch=$2 AND seq<=$3
				AND (entity_kind,entity_key)>($4 COLLATE "C",$5 COLLATE "C")
				ORDER BY entity_kind,entity_key,seq DESC
			) latest WHERE NOT deleted ORDER BY entity_kind,entity_key LIMIT $6`
			args = []any{string(repo), epoch, cursor.Checkpoint.Sequence, cursor.Kind, cursor.Key, limit + 1}
		}
		order := "seq,entity_kind,entity_key"
		if cursor.Mode == "baseline" {
			order = "entity_kind,entity_key"
		}
		query = fmt.Sprintf(`SELECT seq,entity_kind,
			CASE WHEN octet_length(entity_key)<=%d THEN entity_key ELSE '' END,deleted,
			COALESCE(octet_length(after_image::text),0),octet_length(entity_key)
			FROM (%s) selected ORDER BY %s`, domain.MaxRepositoryViewSourceKeyBytes, query, order)
		rows, err := s.db(bound).Query(bound, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		pageBytes := 0
		more := false
		sequences := []int64{}
		kinds, keys := []string{}, []string{}
		sizes := []int{}
		for rows.Next() {
			var entry domain.RepositoryViewSourceEntry
			var size, keySize int
			if err := rows.Scan(&entry.Sequence, &entry.Kind, &entry.Key, &entry.Deleted, &size, &keySize); err != nil {
				return err
			}
			cost := size + keySize + 128
			if len(out.Entries) == limit || (len(out.Entries) != 0 && (pageBytes+cost > domain.MaxRepositoryViewSourcePageBytes || size > domain.MaxRepositoryViewSourceImageBytes || keySize > domain.MaxRepositoryViewSourceKeyBytes)) {
				more = true
				break
			}
			if size > domain.MaxRepositoryViewSourceImageBytes || keySize > domain.MaxRepositoryViewSourceKeyBytes {
				return domain.ErrRepositoryViewSourceLimit
			}
			if !validKind(entry.Kind) || entry.Key == "" || entry.Sequence < 0 || entry.Sequence > cursor.Checkpoint.Sequence || (!entry.Deleted && size == 0) || (entry.Deleted && size != 0) {
				return domain.ErrIntegrity
			}
			pageBytes += cost
			out.Entries = append(out.Entries, entry)
			sequences = append(sequences, entry.Sequence)
			kinds, keys = append(kinds, entry.Kind), append(keys, entry.Key)
			sizes = append(sizes, size)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if more {
			last := out.Entries[len(out.Entries)-1]
			cursor.Sequence, cursor.Kind, cursor.Key = last.Sequence, last.Kind, last.Key
			raw, err := json.Marshal(cursor)
			if err != nil {
				return err
			}
			out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
			if len(out.NextCursor) > 16<<10 {
				return domain.ErrRepositoryViewSourceLimit
			}
		} else {
			out.Checkpoint = &cursor.Checkpoint
		}
		if len(out.Entries) == 0 {
			return nil
		}
		payloads, err := s.db(bound).Query(bound, `SELECT source.after_image
			FROM unnest($3::bigint[],$4::text[],$5::text[]) WITH ORDINALITY AS wanted(seq,kind,key,position)
			JOIN `+sourceView+` source ON source.repo_id=$1 AND source.epoch=$2
			AND source.seq=wanted.seq AND source.entity_kind=wanted.kind AND source.entity_key=wanted.key
			ORDER BY wanted.position`, string(repo), epoch, sequences, kinds, keys)
		if err != nil {
			return err
		}
		defer payloads.Close()
		index := 0
		for payloads.Next() {
			if index >= len(out.Entries) {
				return domain.ErrIntegrity
			}
			if err := payloads.Scan(&out.Entries[index].Value); err != nil {
				return err
			}
			if len(out.Entries[index].Value) != sizes[index] {
				return domain.ErrIntegrity
			}
			index++
		}
		if err := payloads.Err(); err != nil {
			return err
		}
		if index != len(out.Entries) {
			return domain.ErrIntegrity
		}
		return nil
	})
	if err != nil {
		return domain.RepositoryViewSourcePage{}, err
	}
	return out, nil
}
