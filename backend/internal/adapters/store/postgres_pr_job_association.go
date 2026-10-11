//go:build postgres

package store

import (
	"context"
	"strconv"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.PRJobAssociationStore = (*PostgresStore)(nil)

func readPRJobAssociationRows[Record interface{ Validate() error }](ctx context.Context, database pgDatabase, query string, repo domain.ContentHash, numbers any, selected func(Record) bool) ([]Record, bool, error) {
	rows, err := database.Query(ctx, query, repo, outbound.PRJobAssociationReadLimit+1, numbers)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var records []Record
	count := 0
	for rows.Next() {
		count++
		if count > outbound.PRJobAssociationReadLimit {
			return nil, false, nil
		}
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, false, err
		}
		if len(raw) > 32<<10 {
			return nil, false, domain.ErrIntegrity
		}
		record, err := decodePRHistory[Record](raw)
		if err != nil {
			return nil, false, err
		}
		if selected(record) {
			records = append(records, record)
		}
	}
	return records, true, rows.Err()
}

func (s *PostgresStore) ReadPRJobAssociationEvidence(ctx context.Context, repo domain.ContentHash, numbers []int) (outbound.PRJobAssociationEvidence, error) {
	return readPRJobAssociationEvidencePG(ctx, s.db(ctx), repo, numbers)
}

func readPRJobAssociationEvidencePG(ctx context.Context, database pgDatabase, repo domain.ContentHash, numbers []int) (outbound.PRJobAssociationEvidence, error) {
	var result outbound.PRJobAssociationEvidence
	selected, err := prJobAssociationNumbers(repo, numbers)
	if err != nil {
		return result, err
	}
	numericNumbers, textNumbers := make([]int64, 0, len(selected)), make([]string, 0, len(selected))
	for number := range selected {
		numericNumbers = append(numericNumbers, int64(number))
		textNumbers = append(textNumbers, strconv.Itoa(number))
	}
	result.Checkpoints, result.CheckpointsComplete, err = readPRJobAssociationRows(ctx, database, `SELECT payload FROM pr_history_checkpoints WHERE repo_id=$1 AND number=ANY($3::bigint[]) ORDER BY number,origin,provider_repository_id LIMIT $2`, repo, numericNumbers,
		func(record domain.PRHistoryCheckpoint) bool {
			return record.RepoID == repo && selected[record.PR.Number]
		})
	if err != nil {
		return result, err
	}
	result.Participations, result.ParticipationsComplete, err = readPRJobAssociationRows(ctx, database, `SELECT payload FROM pr_participations WHERE repo_id=$1 AND number=ANY($3::bigint[]) ORDER BY number,id LIMIT $2`, repo, numericNumbers,
		func(record domain.PRParticipation) bool { return record.RepoID == repo && selected[record.Number] })
	if err != nil {
		return result, err
	}
	result.Contributions, result.ContributionsComplete, err = readPRJobAssociationRows(ctx, database, `SELECT payload FROM pr_contributions WHERE repo_id=$1 AND payload->'pr'->>'number'=ANY($3::text[]) ORDER BY payload->'pr'->>'number',id LIMIT $2`, repo, textNumbers,
		func(record domain.PRContribution) bool { return record.RepoID == repo && selected[record.PR.Number] })
	return result, err
}
