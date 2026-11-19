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

var _ outbound.AnchoredSessionStore = (*PostgresStore)(nil)

const anchoredSessionColumns = `repo_id,provider,session_id,uploader,body,body_hash,revision,created_at,updated_at`

func scanAnchoredSession(row pgx.Row) (domain.AnchoredSession, error) {
	var session domain.AnchoredSession
	var body []byte
	if err := row.Scan(&session.RepoID, &session.Provider, &session.SessionID, &session.Uploader, &body, &session.BodyHash,
		&session.Revision, &session.CreatedAt, &session.UpdatedAt); err != nil {
		return domain.AnchoredSession{}, err
	}
	session.CreatedAt, session.UpdatedAt = session.CreatedAt.UTC(), session.UpdatedAt.UTC()
	if err := json.Unmarshal(body, &session.Body); err != nil {
		return domain.AnchoredSession{}, fmt.Errorf("%w: invalid stored anchored session body: %v", domain.ErrIntegrity, err)
	}
	if err := validateAnchoredSession(session); err != nil {
		return domain.AnchoredSession{}, fmt.Errorf("%w: invalid stored anchored session: %v", domain.ErrIntegrity, err)
	}
	return session, nil
}

func (s *PostgresStore) GetAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSession, error) {
	if err := validateHash(repo); err != nil {
		return domain.AnchoredSession{}, err
	}
	if !domain.ValidAnchoredSessionKey(provider, sessionID) {
		return domain.AnchoredSession{}, fmt.Errorf("%w: anchored session key", domain.ErrValidation)
	}
	session, err := scanAnchoredSession(s.db(ctx).QueryRow(ctx, `SELECT `+anchoredSessionColumns+`
 FROM anchored_sessions WHERE repo_id=$1 AND provider=$2 AND session_id=$3`, repo, provider, sessionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AnchoredSession{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.AnchoredSession{}, err
	}
	if session.RepoID != repo || session.Provider != provider || session.SessionID != sessionID {
		return domain.AnchoredSession{}, domain.ErrIntegrity
	}
	return session, nil
}

func (s *PostgresStore) ListRepositoryAnchoredSessions(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSession, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT `+anchoredSessionColumns+`
 FROM anchored_sessions WHERE repo_id=$1 ORDER BY provider, session_id`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := []domain.AnchoredSession{}
	for rows.Next() {
		session, err := scanAnchoredSession(rows)
		if err != nil {
			return nil, err
		}
		if session.RepoID != repo {
			return nil, domain.ErrIntegrity
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sessions, nil
}

func (s *PostgresStore) ListAnchoredSessions(ctx context.Context, repo domain.ContentHash, uploader string) ([]domain.AnchoredSession, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT `+anchoredSessionColumns+`
 FROM anchored_sessions WHERE repo_id=$1 AND uploader=$2 ORDER BY provider, session_id`, repo, uploader)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := []domain.AnchoredSession{}
	for rows.Next() {
		session, err := scanAnchoredSession(rows)
		if err != nil {
			return nil, err
		}
		if session.RepoID != repo || session.Uploader != uploader {
			return nil, domain.ErrIntegrity
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sessions, nil
}

// PutAnchoredSession replaces a session's report and its commit index entries.
// The caller's repository transaction makes the two one change.
func (s *PostgresStore) PutAnchoredSession(ctx context.Context, session domain.AnchoredSession) error {
	if err := validateAnchoredSession(session); err != nil {
		return err
	}
	body, err := json.Marshal(session.Body.Canonical())
	if err != nil {
		return err
	}
	db := s.db(ctx)
	if _, err := db.Exec(ctx, `INSERT INTO anchored_sessions(repo_id,provider,session_id,uploader,parent_session_id,body,body_hash,revision,created_at,updated_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
 ON CONFLICT (repo_id,provider,session_id) DO UPDATE SET
  uploader=EXCLUDED.uploader, parent_session_id=EXCLUDED.parent_session_id, body=EXCLUDED.body,
  body_hash=EXCLUDED.body_hash, revision=EXCLUDED.revision, updated_at=EXCLUDED.updated_at`,
		session.RepoID, session.Provider, session.SessionID, session.Uploader, session.Body.Parent, body, session.BodyHash,
		session.Revision, session.CreatedAt, session.UpdatedAt); err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `DELETE FROM anchored_session_commits WHERE repo_id=$1 AND provider=$2 AND session_id=$3`,
		session.RepoID, session.Provider, session.SessionID); err != nil {
		return err
	}
	for _, link := range session.Body.CommitLinks(session.Provider, session.SessionID) {
		if _, err := db.Exec(ctx, `INSERT INTO anchored_session_commits(repo_id,provider,session_id,commit_sha,segment_no,rule)
 VALUES($1,$2,$3,$4,$5,$6)`, session.RepoID, link.Provider, link.SessionID, link.CommitSHA, link.SegmentNo, link.Rule); err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresStore) ListAnchoredCommitLinks(ctx context.Context, repo domain.ContentHash, commitSHA string) ([]domain.AnchoredCommitLink, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	if !domain.ValidAnchoredCommitSHA(commitSHA) {
		return nil, fmt.Errorf("%w: commit number", domain.ErrValidation)
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT provider,session_id,commit_sha,segment_no,rule
 FROM anchored_session_commits WHERE repo_id=$1 AND commit_sha=$2 ORDER BY provider, session_id`, repo, commitSHA)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []domain.AnchoredCommitLink{}
	for rows.Next() {
		var link domain.AnchoredCommitLink
		if err := rows.Scan(&link.Provider, &link.SessionID, &link.CommitSHA, &link.SegmentNo, &link.Rule); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return links, nil
}
