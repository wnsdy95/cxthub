package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.AnchoredSessionStore = (*FSStore)(nil)

var fsAnchoredSessionLocks sync.Map

// validateAnchoredSession checks a stored report: its key, its attribution and
// that its recorded hash is the hash of its body.
func validateAnchoredSession(session domain.AnchoredSession) error {
	if err := validateHashes(session.RepoID, session.BodyHash); err != nil {
		return err
	}
	if !domain.ValidAnchoredSessionKey(session.Provider, session.SessionID) {
		return fmt.Errorf("%w: anchored session key", domain.ErrValidation)
	}
	if strings.TrimSpace(session.Uploader) == "" || session.Revision < 1 || session.CreatedAt.IsZero() || session.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: anchored session attribution required", domain.ErrValidation)
	}
	if err := session.Body.Validate(); err != nil {
		return err
	}
	hash, err := session.Body.Hash()
	if err != nil {
		return err
	}
	if hash != session.BodyHash {
		return fmt.Errorf("%w: anchored session body hash mismatch", domain.ErrIntegrity)
	}
	return nil
}

func (s *FSStore) anchoredSessionLock(repo domain.ContentHash) *sync.Mutex {
	lock, _ := fsAnchoredSessionLocks.LoadOrStore(s.dataDir+"\x00"+string(repo), &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *FSStore) anchoredSessionDir(repo domain.ContentHash, provider domain.ProviderKind) string {
	return filepath.Join(s.repoDir(repo), "anchored-sessions", string(provider))
}

// The file name is a hash of the session identifier, so no identifier a tool
// chose ever becomes a path.
func (s *FSStore) anchoredSessionPath(repo domain.ContentHash, provider domain.ProviderKind, sessionID string) string {
	return filepath.Join(s.anchoredSessionDir(repo, provider), hexOf(domain.HashContent([]byte(sessionID)))+".json")
}

func (s *FSStore) readAnchoredSessionFile(repo domain.ContentHash, path string) (domain.AnchoredSession, error) {
	var session domain.AnchoredSession
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return session, domain.ErrNotFound
	}
	if err != nil {
		return session, err
	}
	if !info.Mode().IsRegular() {
		return session, fmt.Errorf("%w: anchored session is not a regular file", domain.ErrIntegrity)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return session, err
	}
	if err = json.Unmarshal(raw, &session); err != nil {
		return session, fmt.Errorf("%w: invalid anchored session JSON: %v", domain.ErrIntegrity, err)
	}
	if err = validateAnchoredSession(session); err != nil {
		return session, fmt.Errorf("%w: invalid stored anchored session: %v", domain.ErrIntegrity, err)
	}
	if session.RepoID != repo || path != s.anchoredSessionPath(repo, session.Provider, session.SessionID) {
		return session, fmt.Errorf("%w: anchored session path mismatch", domain.ErrIntegrity)
	}
	return session, nil
}

func (s *FSStore) GetAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSession, error) {
	if err := validateHash(repo); err != nil {
		return domain.AnchoredSession{}, err
	}
	if !domain.ValidAnchoredSessionKey(provider, sessionID) {
		return domain.AnchoredSession{}, fmt.Errorf("%w: anchored session key", domain.ErrValidation)
	}
	if err := ctx.Err(); err != nil {
		return domain.AnchoredSession{}, err
	}
	lock := s.anchoredSessionLock(repo)
	lock.Lock()
	defer lock.Unlock()
	return s.readAnchoredSessionFile(repo, s.anchoredSessionPath(repo, provider, sessionID))
}

func (s *FSStore) listAnchoredSessions(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSession, error) {
	sessions := []domain.AnchoredSession{}
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		dir := s.anchoredSessionDir(repo, provider)
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") && strings.Contains(entry.Name(), ".json.tmp-") {
				continue
			}
			if !strings.HasSuffix(entry.Name(), ".json") {
				return nil, fmt.Errorf("%w: invalid anchored session filename", domain.ErrIntegrity)
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			session, err := s.readAnchoredSessionFile(repo, filepath.Join(dir, entry.Name()))
			if err != nil {
				return nil, err
			}
			sessions = append(sessions, session)
		}
	}
	sort.Slice(sessions, func(left, right int) bool {
		if sessions[left].Provider != sessions[right].Provider {
			return sessions[left].Provider < sessions[right].Provider
		}
		return sessions[left].SessionID < sessions[right].SessionID
	})
	return sessions, nil
}

func (s *FSStore) ListAnchoredSessions(ctx context.Context, repo domain.ContentHash, uploader string) ([]domain.AnchoredSession, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	lock := s.anchoredSessionLock(repo)
	lock.Lock()
	defer lock.Unlock()
	all, err := s.listAnchoredSessions(ctx, repo)
	if err != nil {
		return nil, err
	}
	sessions := []domain.AnchoredSession{}
	for _, session := range all {
		if session.Uploader == uploader {
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}

func (s *FSStore) ListRepositoryAnchoredSessions(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSession, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	lock := s.anchoredSessionLock(repo)
	lock.Lock()
	defer lock.Unlock()
	return s.listAnchoredSessions(ctx, repo)
}

func (s *FSStore) PutAnchoredSession(ctx context.Context, session domain.AnchoredSession) error {
	if err := validateAnchoredSession(session); err != nil {
		return err
	}
	lock := s.anchoredSessionLock(session.RepoID)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(session)
	if err != nil {
		return err
	}
	return writeAtomic(s.anchoredSessionPath(session.RepoID, session.Provider, session.SessionID), raw)
}

// ListAnchoredCommitLinks derives the commit index from the stored reports.
// The development store keeps no separate index to fall out of step.
func (s *FSStore) ListAnchoredCommitLinks(ctx context.Context, repo domain.ContentHash, commitSHA string) ([]domain.AnchoredCommitLink, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	if !domain.ValidAnchoredCommitSHA(commitSHA) {
		return nil, fmt.Errorf("%w: commit number", domain.ErrValidation)
	}
	lock := s.anchoredSessionLock(repo)
	lock.Lock()
	defer lock.Unlock()
	all, err := s.listAnchoredSessions(ctx, repo)
	if err != nil {
		return nil, err
	}
	links := []domain.AnchoredCommitLink{}
	for _, session := range all {
		for _, link := range session.Body.CommitLinks(session.Provider, session.SessionID) {
			if link.CommitSHA == commitSHA {
				links = append(links, link)
			}
		}
	}
	return links, nil
}
