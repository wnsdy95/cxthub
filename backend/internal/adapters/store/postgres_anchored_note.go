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

var (
	_ outbound.AnchoredNoteStore     = (*PostgresStore)(nil)
	_ outbound.ReportedWorktreeStore = (*PostgresStore)(nil)
)

func scanAnchoredNote(row pgx.Row, repo domain.ContentHash) (domain.AnchoredNote, error) {
	var body []byte
	if err := row.Scan(&body); err != nil {
		return domain.AnchoredNote{}, err
	}
	var note domain.AnchoredNote
	if err := json.Unmarshal(body, &note); err != nil {
		return domain.AnchoredNote{}, fmt.Errorf("%w: invalid stored note: %v", domain.ErrIntegrity, err)
	}
	if err := note.Validate(); err != nil || note.RepoID != repo {
		return domain.AnchoredNote{}, fmt.Errorf("%w: invalid stored note", domain.ErrIntegrity)
	}
	note.CreatedAt, note.UpdatedAt = note.CreatedAt.UTC(), note.UpdatedAt.UTC()
	return note, nil
}

func (s *PostgresStore) PutAnchoredNote(ctx context.Context, note domain.AnchoredNote) error {
	if err := note.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(note)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO anchored_notes(repo_id,id,author,target_kind,created_at,body) VALUES($1,$2,$3,$4,$5,$6)
 ON CONFLICT (repo_id,id) DO UPDATE SET author=EXCLUDED.author, target_kind=EXCLUDED.target_kind, created_at=EXCLUDED.created_at, body=EXCLUDED.body`,
		note.RepoID, note.ID, note.Author, note.Target.Kind, note.CreatedAt, body)
	return err
}

func (s *PostgresStore) GetAnchoredNote(ctx context.Context, repo domain.ContentHash, id string) (domain.AnchoredNote, error) {
	if err := validateHash(repo); err != nil {
		return domain.AnchoredNote{}, err
	}
	note, err := scanAnchoredNote(s.db(ctx).QueryRow(ctx, `SELECT body FROM anchored_notes WHERE repo_id=$1 AND id=$2`, repo, id), repo)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AnchoredNote{}, domain.ErrNotFound
	}
	return note, err
}

func (s *PostgresStore) ListAnchoredNotes(ctx context.Context, repo domain.ContentHash, author string) ([]domain.AnchoredNote, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT body FROM anchored_notes WHERE repo_id=$1 AND author=$2 ORDER BY created_at, id`, repo, author)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	notes := []domain.AnchoredNote{}
	for rows.Next() {
		note, err := scanAnchoredNote(rows, repo)
		if err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}

func (s *PostgresStore) DeleteAnchoredNote(ctx context.Context, repo domain.ContentHash, id string) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	tag, err := s.db(ctx).Exec(ctx, `DELETE FROM anchored_notes WHERE repo_id=$1 AND id=$2`, repo, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *PostgresStore) ListReportedWorktrees(ctx context.Context, repo domain.ContentHash, account string) ([]domain.ReportedWorktree, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	var body []byte
	err := s.db(ctx).QueryRow(ctx, `SELECT body FROM reported_worktrees WHERE repo_id=$1 AND account=$2`, repo, account).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return []domain.ReportedWorktree{}, nil
	}
	if err != nil {
		return nil, err
	}
	worktrees := []domain.ReportedWorktree{}
	if err := json.Unmarshal(body, &worktrees); err != nil {
		return nil, fmt.Errorf("%w: invalid stored worktree list: %v", domain.ErrIntegrity, err)
	}
	if err := validateReportedWorktrees(account, worktrees); err != nil {
		return nil, fmt.Errorf("%w: invalid stored worktree list", domain.ErrIntegrity)
	}
	for index := range worktrees {
		worktrees[index].FirstSeenAt, worktrees[index].LastSeenAt = worktrees[index].FirstSeenAt.UTC(), worktrees[index].LastSeenAt.UTC()
	}
	domain.SortReportedWorktrees(worktrees)
	return worktrees, nil
}

func (s *PostgresStore) PutReportedWorktrees(ctx context.Context, repo domain.ContentHash, account string, worktrees []domain.ReportedWorktree) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	if err := validateReportedWorktrees(account, worktrees); err != nil {
		return err
	}
	next := append([]domain.ReportedWorktree{}, worktrees...)
	domain.SortReportedWorktrees(next)
	body, err := json.Marshal(next)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO reported_worktrees(repo_id,account,body) VALUES($1,$2,$3)
 ON CONFLICT (repo_id,account) DO UPDATE SET body=EXCLUDED.body`, repo, account, body)
	return err
}
