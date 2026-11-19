//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.AnchoredPublicationStore = (*PostgresStore)(nil)

func decodeAnchoredPublishedRanges(body []byte) ([]domain.AnchoredPublishedRange, error) {
	ranges := []domain.AnchoredPublishedRange{}
	if err := json.Unmarshal(body, &ranges); err != nil {
		return nil, fmt.Errorf("%w: invalid stored publication: %v", domain.ErrIntegrity, err)
	}
	if err := domain.ValidateAnchoredPublishedRanges(ranges); err != nil || len(ranges) == 0 {
		return nil, fmt.Errorf("%w: invalid stored publication", domain.ErrIntegrity)
	}
	for index := range ranges {
		ranges[index].At = ranges[index].At.UTC()
	}
	return ranges, nil
}

func (s *PostgresStore) GetAnchoredPublication(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) ([]domain.AnchoredPublishedRange, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	var body []byte
	err := s.db(ctx).QueryRow(ctx, `SELECT body FROM anchored_session_publications WHERE repo_id=$1 AND provider=$2 AND session_id=$3`, repo, provider, sessionID).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return []domain.AnchoredPublishedRange{}, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeAnchoredPublishedRanges(body)
}

func (s *PostgresStore) PutAnchoredPublication(ctx context.Context, repo domain.ContentHash, publication domain.AnchoredPublication) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	if err := publication.Validate(); err != nil {
		return err
	}
	if len(publication.Ranges) == 0 {
		return fmt.Errorf("%w: a publication holds at least one range", domain.ErrValidation)
	}
	body, err := json.Marshal(publication.Ranges)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO anchored_session_publications(repo_id,provider,session_id,body) VALUES($1,$2,$3,$4)
 ON CONFLICT (repo_id,provider,session_id) DO UPDATE SET body=EXCLUDED.body`, repo, publication.Provider, publication.SessionID, body)
	return err
}

func (s *PostgresStore) ClearAnchoredPublication(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM anchored_session_publications WHERE repo_id=$1 AND provider=$2 AND session_id=$3`, repo, provider, sessionID)
	return err
}

func (s *PostgresStore) ListAnchoredPublications(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredPublication, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT provider, session_id, body FROM anchored_session_publications WHERE repo_id=$1 ORDER BY provider, session_id`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	publications := []domain.AnchoredPublication{}
	for rows.Next() {
		var publication domain.AnchoredPublication
		var body []byte
		if err := rows.Scan(&publication.Provider, &publication.SessionID, &body); err != nil {
			return nil, err
		}
		if publication.Ranges, err = decodeAnchoredPublishedRanges(body); err != nil {
			return nil, err
		}
		publications = append(publications, publication)
	}
	return publications, rows.Err()
}
