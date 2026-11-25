//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.AnchoredSessionStore = (*PostgresStore)(nil)

const anchoredSessionColumns = `repo_id,provider,session_id,uploader,body,body_hash,revision,created_at,updated_at,archived_at,archived_by`

func scanAnchoredSession(row pgx.Row) (domain.AnchoredSession, error) {
	var session domain.AnchoredSession
	var body []byte
	if err := row.Scan(&session.RepoID, &session.Provider, &session.SessionID, &session.Uploader, &body, &session.BodyHash,
		&session.Revision, &session.CreatedAt, &session.UpdatedAt, &session.ArchivedAt, &session.ArchivedBy); err != nil {
		return domain.AnchoredSession{}, err
	}
	session.CreatedAt, session.UpdatedAt = session.CreatedAt.UTC(), session.UpdatedAt.UTC()
	if session.ArchivedAt != nil {
		at := session.ArchivedAt.UTC()
		session.ArchivedAt = &at
	}
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

// SetAnchoredSessionArchive sets or clears the archive mark of a session.
func (s *PostgresStore) SetAnchoredSessionArchive(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string, at *time.Time, by string) error {
	if err := validateAnchoredSessionArchive(repo, provider, sessionID, at, by); err != nil {
		return err
	}
	tag, err := s.db(ctx).Exec(ctx, `UPDATE anchored_sessions SET archived_at=$4, archived_by=$5 WHERE repo_id=$1 AND provider=$2 AND session_id=$3`,
		repo, provider, sessionID, at, by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// PutAnchoredSession replaces a session's report and its commit index entries.
// The caller's repository transaction makes the two one change. The archive
// mark is not among the columns it writes.
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

// DeleteAnchoredSession removes a session's report. Its commit index entries,
// summaries and open ranges are rows that reference it and go with it.
func (s *PostgresStore) DeleteAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	if !domain.ValidAnchoredSessionKey(provider, sessionID) {
		return fmt.Errorf("%w: anchored session key", domain.ErrValidation)
	}
	tag, err := s.db(ctx).Exec(ctx, `DELETE FROM anchored_sessions WHERE repo_id=$1 AND provider=$2 AND session_id=$3`, repo, provider, sessionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// AnchoredContentInUse reports whether a stored report names the document as
// a part of its content: the report is kept as it was sent, and a part is an
// element of the list under content.parts.
func (s *PostgresStore) AnchoredContentInUse(ctx context.Context, repo, doc domain.ContentHash) (bool, error) {
	if err := validateHashes(repo, doc); err != nil {
		return false, err
	}
	var used bool
	err := s.db(ctx).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM anchored_sessions WHERE repo_id=$1 AND body->'content'->'parts' @> jsonb_build_array(jsonb_build_object('doc',$2::text)))`, repo, doc).Scan(&used)
	return used, err
}

var _ outbound.AnchoredRemovalStore = (*PostgresStore)(nil)

func (s *PostgresStore) PutAnchoredSessionRemoval(ctx context.Context, removal domain.AnchoredSessionRemoval) error {
	if err := validateHash(removal.RepoID); err != nil {
		return err
	}
	if err := removal.Validate(); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO anchored_session_removals(repo_id,provider,session_id,uploader,removed_by,removed_at)
 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT (repo_id,provider,session_id) DO NOTHING`,
		removal.RepoID, removal.Provider, removal.SessionID, removal.Uploader, removal.RemovedBy, removal.RemovedAt.UTC())
	return err
}

func scanAnchoredSessionRemoval(row pgx.Row) (domain.AnchoredSessionRemoval, error) {
	var removal domain.AnchoredSessionRemoval
	if err := row.Scan(&removal.RepoID, &removal.Provider, &removal.SessionID, &removal.Uploader, &removal.RemovedBy, &removal.RemovedAt); err != nil {
		return domain.AnchoredSessionRemoval{}, err
	}
	removal.RemovedAt = removal.RemovedAt.UTC()
	if err := removal.Validate(); err != nil {
		return domain.AnchoredSessionRemoval{}, fmt.Errorf("%w: invalid stored removal", domain.ErrIntegrity)
	}
	return removal, nil
}

func (s *PostgresStore) GetAnchoredSessionRemoval(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSessionRemoval, error) {
	if err := validateHash(repo); err != nil {
		return domain.AnchoredSessionRemoval{}, err
	}
	removal, err := scanAnchoredSessionRemoval(s.db(ctx).QueryRow(ctx, `SELECT repo_id,provider,session_id,uploader,removed_by,removed_at
 FROM anchored_session_removals WHERE repo_id=$1 AND provider=$2 AND session_id=$3`, repo, provider, sessionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AnchoredSessionRemoval{}, domain.ErrNotFound
	}
	return removal, err
}

func (s *PostgresStore) DeleteAnchoredSessionRemoval(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	tag, err := s.db(ctx).Exec(ctx, `DELETE FROM anchored_session_removals WHERE repo_id=$1 AND provider=$2 AND session_id=$3`, repo, provider, sessionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *PostgresStore) ListAnchoredSessionRemovals(ctx context.Context, repo domain.ContentHash, uploader string) ([]domain.AnchoredSessionRemoval, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT repo_id,provider,session_id,uploader,removed_by,removed_at
 FROM anchored_session_removals WHERE repo_id=$1 AND uploader=$2 ORDER BY provider,session_id`, repo, uploader)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	removals := []domain.AnchoredSessionRemoval{}
	for rows.Next() {
		removal, err := scanAnchoredSessionRemoval(rows)
		if err != nil {
			return nil, err
		}
		removals = append(removals, removal)
	}
	return removals, rows.Err()
}
