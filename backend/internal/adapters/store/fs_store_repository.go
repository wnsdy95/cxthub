package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// FSStore implementation of RepositoryStore (user, repository, membership, invite).
//
// Layout:
//
//	dataDir/users/<safe(id)>.json
//	dataDir/repositories/<repositoryID>.json
//	dataDir/members/<repositoryID>/<safe(userID)>.json
//	dataDir/invites/<token>.json
var _ outbound.RepositoryStore = (*FSStore)(nil)

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

func safeName(id string) string { return unsafeChars.ReplaceAllString(id, "_") }

// opaqueName converts external owner identifiers (Firebase UID, etc.) to collision-free fixed file keys.
// safeName is used for legacy read fallbacks where different IDs can have the same name.
func opaqueName(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "id_" + hex.EncodeToString(sum[:])
}

func storedIdentityIntegrity(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: invalid stored identity record", domain.ErrIntegrity)
}

func (s *FSStore) usersDir() string        { return filepath.Join(s.dataDir, "users") }
func (s *FSStore) repositoriesDir() string { return filepath.Join(s.dataDir, "repositories") }
func (s *FSStore) membersDir() string      { return filepath.Join(s.dataDir, "members") }
func (s *FSStore) invitesDir() string      { return filepath.Join(s.dataDir, "invites") }
func (s *FSStore) sessionsDir() string     { return filepath.Join(s.dataDir, "sessions") }

// --- Session ---

func (s *FSStore) CreateSession(_ context.Context, sess domain.Session) error {
	if err := domain.ValidateSessionRecord(sess); err != nil {
		return err
	}
	data, _ := json.Marshal(sess)
	return writeAtomic(filepath.Join(s.sessionsDir(), sess.Token+".json"), data)
}

func (s *FSStore) GetSession(_ context.Context, token string) (domain.Session, error) {
	if err := domain.ValidateStoredSessionToken(token); err != nil {
		return domain.Session{}, err
	}
	var sess domain.Session
	err := readJSON(filepath.Join(s.sessionsDir(), token+".json"), &sess)
	if err == nil {
		if sess.Token != token {
			return domain.Session{}, domain.ErrNotFound
		}
		if verr := domain.ValidateSessionRecord(sess); verr != nil {
			return domain.Session{}, storedIdentityIntegrity(verr)
		}
	}
	return sess, err
}

var fsSessionLocks sync.Map

func (s *FSStore) sessionLock(token string) *sync.Mutex {
	lock, _ := fsSessionLocks.LoadOrStore(s.dataDir+"\x00"+token, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *FSStore) ConsumeSession(ctx context.Context, token, kind, label string) (domain.Session, error) {
	lock := s.sessionLock(token)
	lock.Lock()
	defer lock.Unlock()
	sess, err := s.GetSession(ctx, token)
	if err != nil {
		return domain.Session{}, err
	}
	if sess.Kind != kind || sess.Label != label {
		return domain.Session{}, domain.ErrUnauthorized
	}
	if err := os.Remove(filepath.Join(s.sessionsDir(), token+".json")); err != nil {
		if os.IsNotExist(err) {
			return domain.Session{}, domain.ErrNotFound
		}
		return domain.Session{}, err
	}
	return sess, nil
}

func (s *FSStore) DeleteSession(_ context.Context, token string) error {
	if err := domain.ValidateStoredSessionToken(token); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(s.sessionsDir(), token+".json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ListSessionsForUser returns all sessions for a user (CLI token list, for archival). FS scans linearly.
func (s *FSStore) ListSessionsForUser(_ context.Context, userID string) ([]domain.Session, error) {
	if err := domain.ValidateExternalID(userID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.sessionsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []domain.Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var sess domain.Session
		if readJSON(filepath.Join(s.sessionsDir(), e.Name()), &sess) != nil {
			continue
		}
		if verr := domain.ValidateSessionRecord(sess); verr != nil {
			return nil, storedIdentityIntegrity(verr)
		}
		if sess.UserID == userID {
			out = append(out, sess)
		}
	}
	return out, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return domain.ErrNotFound
		}
		return err
	}
	return json.Unmarshal(data, v)
}

// --- User ---

func (s *FSStore) UpsertUser(_ context.Context, user domain.User) error {
	if err := domain.ValidateUserRecord(user); err != nil {
		return err
	}
	data, _ := json.Marshal(user)
	if err := writeAtomic(filepath.Join(s.usersDir(), opaqueName(user.ID)+".json"), data); err != nil {
		return err
	}
	legacy := filepath.Join(s.usersDir(), safeName(user.ID)+".json")
	var old domain.User
	if readJSON(legacy, &old) == nil && old.ID == user.ID {
		_ = os.Remove(legacy)
	}
	return nil
}

func (s *FSStore) GetUser(_ context.Context, id string) (domain.User, error) {
	if err := domain.ValidateExternalID(id); err != nil {
		return domain.User{}, err
	}
	var u domain.User
	err := readJSON(filepath.Join(s.usersDir(), opaqueName(id)+".json"), &u)
	if err == domain.ErrNotFound {
		err = readJSON(filepath.Join(s.usersDir(), safeName(id)+".json"), &u)
	}
	if err == nil {
		if u.ID != id {
			return domain.User{}, domain.ErrNotFound
		}
		if verr := domain.ValidateUserRecord(u); verr != nil {
			return domain.User{}, storedIdentityIntegrity(verr)
		}
	}
	return u, err
}

// GetUserByUsername scans the users directory to find a user with matching handles. FS store lacks secondary indexes, so it scans linearly (sufficient for scaffold scale).
func (s *FSStore) GetUserByUsername(_ context.Context, username string) (domain.User, error) {
	entries, err := os.ReadDir(s.usersDir())
	if err != nil {
		if os.IsNotExist(err) {
			return domain.User{}, domain.ErrNotFound
		}
		return domain.User{}, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		var u domain.User
		if err := readJSON(filepath.Join(s.usersDir(), e.Name()), &u); err == nil && u.Username == username {
			if verr := domain.ValidateUserRecord(u); verr != nil {
				return domain.User{}, storedIdentityIntegrity(verr)
			}
			return u, nil
		}
	}
	return domain.User{}, domain.ErrNotFound
}

// --- Repository ---

func (s *FSStore) CreateRepository(ctx context.Context, repositoryRecord domain.Repository) error {
	if err := domain.ValidateRepositoryRecord(repositoryRecord); err != nil {
		return err
	}
	// Persist the previous address before changing the display path. The
	// alias is harmless if the subsequent write fails: it still points here.
	if repositoryRecord.OwnerUsername != "" && repositoryRecord.Slug != "" {
		found, err := s.GetRepositoryByPath(ctx, repositoryRecord.OwnerUsername, repositoryRecord.Slug)
		if errors.Is(err, domain.ErrNotFound) && repositoryRecord.OwnerNamespaceID != "" {
			found, err = s.GetRepositoryByNamespacePath(ctx, repositoryRecord.OwnerNamespaceID, repositoryRecord.Slug)
		}
		if err == nil && found.ID != repositoryRecord.ID {
			return domain.ErrConflict
		}
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
	}
	if old, err := s.GetRepository(ctx, repositoryRecord.ID); err == nil && old.Slug != "" && old.OwnerUsername != "" && (old.Slug != repositoryRecord.Slug || old.OwnerUsername != repositoryRecord.OwnerUsername || old.OwnerNamespaceID != repositoryRecord.OwnerNamespaceID) {
		key := old.OwnerNamespaceID
		if key == "" {
			key = "handle:" + old.OwnerUsername
		}
		alias := domain.RepositoryPathAlias{NamespaceID: old.OwnerNamespaceID, Owner: old.OwnerUsername, Path: old.Slug, RepositoryID: old.ID}
		if current, err := s.repositoryAlias(ctx, key, old.Slug); err == nil && current.RepositoryID != old.ID {
			return domain.ErrConflict
		} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		for _, aliasKey := range []string{key, "handle:" + old.OwnerUsername} {
			if current, err := s.repositoryAlias(ctx, aliasKey, old.Slug); err == nil && current.RepositoryID != old.ID {
				return domain.ErrConflict
			} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return err
			}
			data, _ := json.Marshal(alias)
			if err := writeAtomic(filepath.Join(s.dataDir, "repository-aliases", opaqueName(aliasKey+"/"+old.Slug)+".json"), data); err != nil {
				return err
			}
		}
	} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	data, _ := json.Marshal(repositoryRecord)
	return writeAtomic(filepath.Join(s.repositoriesDir(), repositoryRecord.ID+".json"), data)
}

func (s *FSStore) GetRepository(_ context.Context, id string) (domain.Repository, error) {
	if err := domain.ValidateRepositoryID(id); err != nil {
		return domain.Repository{}, err
	}
	var repositoryRecord domain.Repository
	err := readJSON(filepath.Join(s.repositoriesDir(), id+".json"), &repositoryRecord)
	if err == nil {
		if repositoryRecord.ID != id {
			return domain.Repository{}, domain.ErrNotFound
		}
		if verr := domain.ValidateRepositoryRecord(repositoryRecord); verr != nil {
			return domain.Repository{}, storedIdentityIntegrity(verr)
		}
	}
	return repositoryRecord, err
}

// GetRepositoryByPath finds a repository by URL path segments (owner_username, slug). Used for remote URL → repository binding during repo push. FS scans linearly.
func (s *FSStore) GetRepositoryByPath(ctx context.Context, ownerUsername, slug string) (domain.Repository, error) {
	entries, err := os.ReadDir(s.repositoriesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return domain.Repository{}, domain.ErrNotFound
		}
		return domain.Repository{}, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		var repositoryRecord domain.Repository
		if err := readJSON(filepath.Join(s.repositoriesDir(), e.Name()), &repositoryRecord); err == nil &&
			repositoryRecord.OwnerUsername == ownerUsername && repositoryRecord.Slug == slug {
			if verr := domain.ValidateRepositoryRecord(repositoryRecord); verr != nil {
				return domain.Repository{}, storedIdentityIntegrity(verr)
			}
			return repositoryRecord, nil
		}
	}
	if alias, aliasErr := s.repositoryAlias(ctx, "handle:"+ownerUsername, slug); aliasErr == nil {
		return s.GetRepository(ctx, alias.RepositoryID)
	} else if !errors.Is(aliasErr, domain.ErrNotFound) {
		return domain.Repository{}, aliasErr
	}
	return domain.Repository{}, domain.ErrNotFound
}

func (s *FSStore) GetRepositoryByNamespacePath(ctx context.Context, namespaceID, slug string) (domain.Repository, error) {
	if err := domain.ValidateNamespaceID(namespaceID); err != nil {
		return domain.Repository{}, err
	}
	entries, err := os.ReadDir(s.repositoriesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return domain.Repository{}, domain.ErrNotFound
		}
		return domain.Repository{}, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var repository domain.Repository
		if readJSON(filepath.Join(s.repositoriesDir(), entry.Name()), &repository) == nil &&
			repository.OwnerNamespaceID == namespaceID && repository.Slug == slug {
			if err := domain.ValidateRepositoryRecord(repository); err != nil {
				return domain.Repository{}, storedIdentityIntegrity(err)
			}
			return repository, nil
		}
	}
	if alias, aliasErr := s.repositoryAlias(ctx, namespaceID, slug); aliasErr == nil {
		return s.GetRepository(ctx, alias.RepositoryID)
	} else if !errors.Is(aliasErr, domain.ErrNotFound) {
		return domain.Repository{}, aliasErr
	}
	return domain.Repository{}, domain.ErrNotFound
}

func (s *FSStore) ListRepositoriesForUser(ctx context.Context, userID string) ([]domain.Repository, error) {
	if err := domain.ValidateExternalID(userID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.repositoriesDir())
	if os.IsNotExist(err) {
		return []domain.Repository{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []domain.Repository{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		repository, err := s.GetRepository(ctx, id)
		if err != nil {
			return nil, err
		}
		members, err := s.ListMembers(ctx, id)
		if err != nil {
			return nil, err
		}
		grants, err := s.RepositoryTeamAccess(ctx, id, userID)
		if err != nil {
			return nil, err
		}
		if _, ok := domain.EffectiveRepositoryRole(repository, members, userID, grants); ok {
			out = append(out, repository)
		}
	}
	return out, nil
}

// --- Membership ---

func (s *FSStore) AddMember(_ context.Context, m domain.Membership) error {
	if err := domain.ValidateMembershipRecord(m); err != nil {
		return err
	}
	m.User = nil // Do not store denormalized fields
	data, _ := json.Marshal(m)
	if err := writeAtomic(filepath.Join(s.membersDir(), m.RepositoryID, opaqueName(m.UserID)+".json"), data); err != nil {
		return err
	}
	legacy := filepath.Join(s.membersDir(), m.RepositoryID, safeName(m.UserID)+".json")
	var old domain.Membership
	if readJSON(legacy, &old) == nil && old.RepositoryID == m.RepositoryID && old.UserID == m.UserID {
		_ = os.Remove(legacy)
	}
	return nil
}

func (s *FSStore) RemoveMember(_ context.Context, repositoryID, userID string) error {
	if err := domain.ValidateRepositoryID(repositoryID); err != nil {
		return err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return err
	}
	for _, name := range []string{opaqueName(userID), safeName(userID)} {
		p := filepath.Join(s.membersDir(), repositoryID, name+".json")
		var m domain.Membership
		if err := readJSON(p, &m); err == nil && m.RepositoryID == repositoryID && m.UserID == userID {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func (s *FSStore) IsMember(_ context.Context, repositoryID, userID string) (bool, error) {
	if err := domain.ValidateRepositoryID(repositoryID); err != nil {
		return false, err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return false, err
	}
	return s.membershipExists(repositoryID, userID), nil
}

func (s *FSStore) membershipExists(repositoryID, userID string) bool {
	for _, name := range []string{opaqueName(userID), safeName(userID)} {
		var m domain.Membership
		if readJSON(filepath.Join(s.membersDir(), repositoryID, name+".json"), &m) == nil &&
			m.RepositoryID == repositoryID && m.UserID == userID && domain.ValidateMembershipRecord(m) == nil {
			return true
		}
	}
	return false
}

func (s *FSStore) ListMembers(ctx context.Context, repositoryID string) ([]domain.Membership, error) {
	if err := domain.ValidateRepositoryID(repositoryID); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.membersDir(), repositoryID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	byUser := map[string]domain.Membership{}
	fromOpaque := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var m domain.Membership
		if readJSON(filepath.Join(dir, e.Name()), &m) != nil {
			continue
		}
		if m.RepositoryID != repositoryID || domain.ValidateExternalID(m.UserID) != nil || !domain.ValidRole(m.Role) {
			continue
		}
		isOpaque := e.Name() == opaqueName(m.UserID)+".json"
		if fromOpaque[m.UserID] && !isOpaque {
			continue
		}
		byUser[m.UserID] = m
		fromOpaque[m.UserID] = isOpaque
	}
	userIDs := make([]string, 0, len(byUser))
	for userID := range byUser {
		userIDs = append(userIDs, userID)
	}
	sort.Strings(userIDs)
	out := make([]domain.Membership, 0, len(userIDs))
	for _, userID := range userIDs {
		m := byUser[userID]
		if u, err := s.GetUser(ctx, m.UserID); err == nil {
			m.User = &u // For query convenience, denormalize
		}
		out = append(out, m)
	}
	return out, nil
}

// --- Invite ---

func (s *FSStore) CreateInvite(_ context.Context, inv domain.Invite) error {
	if err := domain.ValidateInviteRecord(inv); err != nil {
		return err
	}
	data, _ := json.Marshal(inv)
	return writeAtomic(filepath.Join(s.invitesDir(), inv.Token+".json"), data)
}

func (s *FSStore) GetInvite(_ context.Context, token string) (domain.Invite, error) {
	if err := domain.ValidateInviteToken(token); err != nil {
		return domain.Invite{}, err
	}
	var inv domain.Invite
	err := readJSON(filepath.Join(s.invitesDir(), token+".json"), &inv)
	if err == nil {
		if inv.Token != token {
			return domain.Invite{}, domain.ErrNotFound
		}
		if verr := domain.ValidateInviteRecord(inv); verr != nil {
			return domain.Invite{}, storedIdentityIntegrity(verr)
		}
	}
	return inv, err
}

func (s *FSStore) UpdateInviteStatus(ctx context.Context, token string, status domain.InviteStatus) error {
	if !domain.ValidInviteStatus(status) {
		return domain.ErrValidation
	}
	inv, err := s.GetInvite(ctx, token)
	if err != nil {
		return err
	}
	inv.Status = status
	data, _ := json.Marshal(inv)
	return writeAtomic(filepath.Join(s.invitesDir(), token+".json"), data)
}

func (s *FSStore) ListInvites(_ context.Context, repositoryID string) ([]domain.Invite, error) {
	if err := domain.ValidateRepositoryID(repositoryID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.invitesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []domain.Invite
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var inv domain.Invite
		if readJSON(filepath.Join(s.invitesDir(), e.Name()), &inv) != nil {
			continue
		}
		if inv.RepositoryID == repositoryID {
			if verr := domain.ValidateInviteRecord(inv); verr != nil {
				return nil, storedIdentityIntegrity(verr)
			}
			out = append(out, inv)
		}
	}
	return out, nil
}
