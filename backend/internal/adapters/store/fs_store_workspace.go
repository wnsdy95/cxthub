package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// FSStore implementation of WorkspaceStore (user, workspace, membership, invite).
//
// Layout:
//
//	dataDir/users/<safe(id)>.json
//	dataDir/workspaces/<wsID>.json
//	dataDir/members/<wsID>/<safe(userID)>.json
//	dataDir/invites/<token>.json
var _ outbound.WorkspaceStore = (*FSStore)(nil)

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

func (s *FSStore) usersDir() string      { return filepath.Join(s.dataDir, "users") }
func (s *FSStore) workspacesDir() string { return filepath.Join(s.dataDir, "workspaces") }
func (s *FSStore) membersDir() string    { return filepath.Join(s.dataDir, "members") }
func (s *FSStore) invitesDir() string    { return filepath.Join(s.dataDir, "invites") }
func (s *FSStore) sessionsDir() string   { return filepath.Join(s.dataDir, "sessions") }

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

// --- Workspace ---

func (s *FSStore) CreateWorkspace(_ context.Context, ws domain.Workspace) error {
	if err := domain.ValidateWorkspaceRecord(ws); err != nil {
		return err
	}
	data, _ := json.Marshal(ws)
	return writeAtomic(filepath.Join(s.workspacesDir(), ws.ID+".json"), data)
}

func (s *FSStore) GetWorkspace(_ context.Context, id string) (domain.Workspace, error) {
	if err := domain.ValidateWorkspaceID(id); err != nil {
		return domain.Workspace{}, err
	}
	var ws domain.Workspace
	err := readJSON(filepath.Join(s.workspacesDir(), id+".json"), &ws)
	if err == nil {
		if ws.ID != id {
			return domain.Workspace{}, domain.ErrNotFound
		}
		if verr := domain.ValidateWorkspaceRecord(ws); verr != nil {
			return domain.Workspace{}, storedIdentityIntegrity(verr)
		}
	}
	return ws, err
}

// GetWorkspaceByPath finds a workspace by URL path segments (owner_username, slug). Used for remote URL → workspace binding during repo push. FS scans linearly.
func (s *FSStore) GetWorkspaceByPath(_ context.Context, ownerUsername, slug string) (domain.Workspace, error) {
	entries, err := os.ReadDir(s.workspacesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return domain.Workspace{}, domain.ErrNotFound
		}
		return domain.Workspace{}, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		var ws domain.Workspace
		if err := readJSON(filepath.Join(s.workspacesDir(), e.Name()), &ws); err == nil &&
			ws.OwnerUsername == ownerUsername && ws.Slug == slug {
			if verr := domain.ValidateWorkspaceRecord(ws); verr != nil {
				return domain.Workspace{}, storedIdentityIntegrity(verr)
			}
			return ws, nil
		}
	}
	return domain.Workspace{}, domain.ErrNotFound
}

func (s *FSStore) ListWorkspacesForUser(ctx context.Context, userID string) ([]domain.Workspace, error) {
	if err := domain.ValidateExternalID(userID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.membersDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []domain.Workspace
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		wsID := e.Name()
		if domain.ValidateWorkspaceID(wsID) != nil || !s.membershipExists(wsID, userID) {
			continue
		}
		if ws, err := s.GetWorkspace(ctx, wsID); err == nil {
			out = append(out, ws)
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
	if err := writeAtomic(filepath.Join(s.membersDir(), m.WorkspaceID, opaqueName(m.UserID)+".json"), data); err != nil {
		return err
	}
	legacy := filepath.Join(s.membersDir(), m.WorkspaceID, safeName(m.UserID)+".json")
	var old domain.Membership
	if readJSON(legacy, &old) == nil && old.WorkspaceID == m.WorkspaceID && old.UserID == m.UserID {
		_ = os.Remove(legacy)
	}
	return nil
}

func (s *FSStore) RemoveMember(_ context.Context, workspaceID, userID string) error {
	if err := domain.ValidateWorkspaceID(workspaceID); err != nil {
		return err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return err
	}
	for _, name := range []string{opaqueName(userID), safeName(userID)} {
		p := filepath.Join(s.membersDir(), workspaceID, name+".json")
		var m domain.Membership
		if err := readJSON(p, &m); err == nil && m.WorkspaceID == workspaceID && m.UserID == userID {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func (s *FSStore) IsMember(_ context.Context, workspaceID, userID string) (bool, error) {
	if err := domain.ValidateWorkspaceID(workspaceID); err != nil {
		return false, err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return false, err
	}
	return s.membershipExists(workspaceID, userID), nil
}

func (s *FSStore) membershipExists(workspaceID, userID string) bool {
	for _, name := range []string{opaqueName(userID), safeName(userID)} {
		var m domain.Membership
		if readJSON(filepath.Join(s.membersDir(), workspaceID, name+".json"), &m) == nil &&
			m.WorkspaceID == workspaceID && m.UserID == userID && domain.ValidateMembershipRecord(m) == nil {
			return true
		}
	}
	return false
}

func (s *FSStore) ListMembers(ctx context.Context, workspaceID string) ([]domain.Membership, error) {
	if err := domain.ValidateWorkspaceID(workspaceID); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.membersDir(), workspaceID)
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
		if m.WorkspaceID != workspaceID || domain.ValidateExternalID(m.UserID) != nil || !domain.ValidRole(m.Role) {
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

func (s *FSStore) ListInvites(_ context.Context, workspaceID string) ([]domain.Invite, error) {
	if err := domain.ValidateWorkspaceID(workspaceID); err != nil {
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
		if inv.WorkspaceID == workspaceID {
			if verr := domain.ValidateInviteRecord(inv); verr != nil {
				return nil, storedIdentityIntegrity(verr)
			}
			out = append(out, inv)
		}
	}
	return out, nil
}
