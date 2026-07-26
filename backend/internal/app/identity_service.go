package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// IdentityService implements authentication (Firebase) + workspace/membership/invitation use-cases.
//
// Dependencies: IdentityVerifier (token→User), WorkspaceStore (persistence). Visibility boundary = workspace.
// Invitations use the "share link/code" model — tokens are reusable until revoked (member addition is idempotent).
type IdentityService struct {
	verifier outbound.IdentityVerifier
	ws       outbound.WorkspaceStore
}

// NewIdentityService creates an IdentityService.
func NewIdentityService(verifier outbound.IdentityVerifier, ws outbound.WorkspaceStore) *IdentityService {
	return &IdentityService{verifier: verifier, ws: ws}
}

// Authenticate validates a token, upserts the user, and returns it (login entry point).
// For the first login, a unique username (handle) is automatically assigned from the email local part —
// becomes the first segment of the URL (/<username>/<workspace>).
func (s *IdentityService) Authenticate(ctx context.Context, idToken string) (domain.User, error) {
	u, err := s.verifier.Verify(ctx, idToken)
	if err != nil {
		return domain.User{}, err
	}
	if existing, gerr := s.ws.GetUser(ctx, u.ID); gerr == nil {
		// Existing user: handle, alias, creation time are preserved (no regeneration on re-login).
		u.Username = existing.Username
		u.Nickname = existing.Nickname
		if !existing.CreatedAt.IsZero() {
			u.CreatedAt = existing.CreatedAt
		}
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	if u.Username == "" {
		u.Username = s.uniqueUsername(ctx, u)
	}
	if err := s.ws.UpsertUser(ctx, u); err != nil {
		return domain.User{}, err
	}
	return u, nil
}

// reservedUsernames are reserved words that conflict with URL segments (routing disruption prevention — sync with front RESERVED).
var reservedUsernames = map[string]bool{
	"api": true, "invite": true, "w": true, "login": true, "settings": true,
	"assets": true, "public": true, "admin": true, "static": true, "cxt": true,
}

// uniqueUsername creates a global unique handle from the email local part (or name if none) — collision handling (-2, -3, ...).
func (s *IdentityService) uniqueUsername(ctx context.Context, u domain.User) string {
	seed := u.Email
	if at := strings.IndexByte(seed, '@'); at > 0 {
		seed = seed[:at]
	}
	if seed == "" {
		seed = u.Name
	}
	base := domain.Slugify(seed, "user")
	cand := base
	for n := 2; ; n++ {
		if !reservedUsernames[cand] {
			other, err := s.ws.GetUserByUsername(ctx, cand)
			if err != nil || other.ID == u.ID { // unused or owned by self → confirmed
				return cand
			}
		}
		cand = base + "-" + strconv.Itoa(n)
	}
}

// UpdateProfile updates account settings. Nil fields are not changed.
//
//   - nickname: display alias — free change (empty string = remove).
//   - username: URL (/<username>/…) first segment, heavy change. slug validation,
//     global uniqueness check, and concurrent update of OwnerUsername in all owned workspaces.
//     (existing URL·CLI remote breaks — client must display warning.)
func (s *IdentityService) UpdateProfile(ctx context.Context, u domain.User, username, nickname, loadMode, avatar, locale *string) (domain.User, error) {
	if nickname != nil {
		u.Nickname = strings.TrimSpace(*nickname)
	}
	if avatar != nil {
		a := strings.TrimSpace(*avatar)
		if err := domain.ValidateAvatarDataURL(a); err != nil {
			return domain.User{}, err
		}
		u.Avatar = a
	}
	if loadMode != nil {
		v := strings.TrimSpace(*loadMode)
		if v != "" && v != "full" && v != "reconstructed" && v != "memory" {
			return domain.User{}, domain.ErrValidation
		}
		u.LoadMode = v
	}
	if locale != nil {
		v := strings.TrimSpace(*locale)
		if v != "" && v != "ko" && v != "en" {
			return domain.User{}, domain.ErrValidation
		}
		u.Locale = v
	}
	usernameChanged := false
	if username != nil {
		next := domain.Slugify(strings.TrimSpace(*username), "")
		if next == "" {
			return domain.User{}, domain.ErrValidation
		}
		if next != u.Username {
			if reservedUsernames[next] {
				return domain.User{}, domain.ErrConflict // reserved word (route conflict)
			}
			if other, err := s.ws.GetUserByUsername(ctx, next); err == nil && other.ID != u.ID {
				return domain.User{}, domain.ErrConflict // handle already in use
			}
			u.Username = next
			usernameChanged = true
		}
	}
	if err := s.ws.UpsertUser(ctx, u); err != nil {
		return domain.User{}, err
	}
	if usernameChanged {
		// Normalize owner_username because it is the source of truth for the URL path.
		if list, err := s.ws.ListWorkspacesForUser(ctx, u.ID); err == nil {
			for _, w := range list {
				if w.OwnerID != u.ID || w.OwnerUsername == u.Username {
					continue
				}
				w.OwnerUsername = u.Username
				_ = s.ws.CreateWorkspace(ctx, w) // upsert meaning (FS/PG common)
			}
		}
	}
	return u, nil
}

// WorkspacePatch updates workspace settings. Nil fields do not change.
type WorkspacePatch struct {
	Visibility       *domain.Visibility // private|public
	SecretsPolicy    *string            // ""|members(role-based)|owner
	SettingsPolicy   *string            // value meaning same
	GHVisibilitySync *bool              // GitHub public state sync on/off
	Archived         *bool              // Archive (read-only) on/off
	WebhookURL       *string            // Alert webhook (empty string = disabled)
	Slug             *string            // URL segment manual change (heavy — owner unique slug validation)
	PublicRole       *string            // Public role for non-members ("", viewer, puller)
}

// UpdateWorkspaceSettings updates workspace settings (public scope, permission policy) — only owner can do this.
func (s *IdentityService) UpdateWorkspaceSettings(ctx context.Context, userID, workspaceID string, p WorkspacePatch) (domain.Workspace, error) {
	wsp, err := s.ws.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return domain.Workspace{}, err
	}
	if !s.IsOwner(ctx, workspaceID, userID) {
		return domain.Workspace{}, domain.ErrForbidden // Workspace settings are owner exclusive
	}
	if p.GHVisibilitySync != nil {
		wsp.GHVisibilitySync = *p.GHVisibilitySync
	}
	if p.Visibility != nil {
		if wsp.GHVisibilitySync {
			return domain.Workspace{}, domain.ErrConflict // Manual settings locked during sync (to prevent truth conflict)
		}
		if *p.Visibility != domain.VisibilityPrivate && *p.Visibility != domain.VisibilityPublic {
			return domain.Workspace{}, domain.ErrValidation
		}
		wsp.Visibility = *p.Visibility
	}
	validPolicy := func(v string) bool { return v == "" || v == "members" || v == "owner" }
	if p.SecretsPolicy != nil {
		if !validPolicy(*p.SecretsPolicy) {
			return domain.Workspace{}, domain.ErrValidation
		}
		wsp.SecretsPolicy = *p.SecretsPolicy
	}
	if p.SettingsPolicy != nil {
		if !validPolicy(*p.SettingsPolicy) {
			return domain.Workspace{}, domain.ErrValidation
		}
		wsp.SettingsPolicy = *p.SettingsPolicy
	}
	if p.Archived != nil {
		wsp.Archived = *p.Archived
	}
	if p.PublicRole != nil {
		if !domain.ValidPublicRole(*p.PublicRole) {
			return domain.Workspace{}, domain.ErrValidation // Only "" | viewer | puller allowed
		}
		wsp.PublicRole = *p.PublicRole
	}
	if p.WebhookURL != nil {
		wsp.WebhookURL = strings.TrimSpace(*p.WebhookURL)
	}
	if p.Slug != nil {
		next := strings.ToLower(strings.TrimSpace(*p.Slug))
		if !domain.ValidWorkspaceSlug(next) {
			return domain.Workspace{}, domain.ErrValidation
		}
		if next != wsp.Slug {
			// owner unique check (excluding self).
			if list, lerr := s.ws.ListWorkspacesForUser(ctx, wsp.OwnerID); lerr == nil {
				for _, w := range list {
					if w.OwnerID == wsp.OwnerID && w.ID != wsp.ID && w.Slug == next {
						return domain.Workspace{}, domain.ErrConflict
					}
				}
			}
			wsp.Slug = next
		}
	}
	if err := s.ws.CreateWorkspace(ctx, wsp); err != nil { // upsert meaning
		return domain.Workspace{}, err
	}
	return wsp, nil
}

// TransferOwnership transfers the workspace creator (OwnerID) to an existing member — only the current creator can do this.
//
//   - The new owner is promoted to the owner role, and the original creator remains an owner member (GitHub style —
//     you can later downgrade or leave).
//   - OwnerUsername changes, so the workspace URL (/<owner>/<slug>) changes (client will show a warning).
//   - slug must be unique within the owner; if the new owner already has the same slug, -2, -3, etc. are appended.
func (s *IdentityService) TransferOwnership(ctx context.Context, actorID, workspaceID, targetID string) (domain.Workspace, error) {
	wsp, err := s.ws.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return domain.Workspace{}, err
	}
	if wsp.OwnerID != actorID {
		return domain.Workspace{}, domain.ErrForbidden // previous was exclusive to the current creator
	}
	if targetID == actorID {
		return domain.Workspace{}, domain.ErrValidation
	}
	target, err := s.ws.GetUser(ctx, targetID)
	if err != nil {
		return domain.Workspace{}, domain.ErrNotFound
	}
	if ok, err := s.ws.IsMember(ctx, workspaceID, targetID); err != nil || !ok {
		return domain.Workspace{}, domain.ErrNotFound // only existing members can transfer
	}
	// slug conflict resolution: if the new owner's other owned workspaces conflict, -2, -3, etc. are appended.
	taken := map[string]bool{}
	if list, err := s.ws.ListWorkspacesForUser(ctx, targetID); err == nil {
		for _, w := range list {
			if w.OwnerID == targetID && w.ID != wsp.ID {
				taken[w.Slug] = true
			}
		}
	}
	slug := wsp.Slug
	for n := 2; taken[slug]; n++ {
		slug = wsp.Slug + "-" + strconv.Itoa(n)
	}
	wsp.OwnerID = targetID
	wsp.OwnerUsername = target.Username
	wsp.Slug = slug
	if err := s.ws.CreateWorkspace(ctx, wsp); err != nil { // upsert
		return domain.Workspace{}, err
	}
	// Promote the new creator to the owner role (the original creator's owner membership remains unchanged).
	if err := s.ws.AddMember(ctx, domain.Membership{WorkspaceID: workspaceID, UserID: targetID, Role: domain.RoleOwner, CreatedAt: time.Now().UTC()}); err != nil {
		return domain.Workspace{}, err
	}
	return wsp, nil
}

// GetWorkspace retrieves a workspace (used in policy decisions etc. at the delivery boundary).
func (s *IdentityService) GetWorkspace(ctx context.Context, workspaceID string) (domain.Workspace, error) {
	return s.ws.GetWorkspace(ctx, workspaceID)
}

// RoleOf returns the user's role within the workspace. The constructor (OwnerID) is always owner.
// Non-members are ("", false).
func (s *IdentityService) RoleOf(ctx context.Context, workspaceID, userID string) (domain.MemberRole, bool) {
	if userID == "" {
		return "", false
	}
	wsp, err := s.ws.GetWorkspace(ctx, workspaceID)
	if err == nil && wsp.OwnerID == userID {
		return domain.RoleOwner, true
	}
	members, err := s.ws.ListMembers(ctx, workspaceID)
	if err != nil {
		return "", false
	}
	for _, m := range members {
		if m.UserID == userID {
			if !domain.ValidRole(m.Role) {
				return "", false // conservatively reject unknown or corrupt role values (fail closed)
			}
			return m.Role, true
		}
	}
	return "", false
}

// IsOwner determines owner permissions: workspace constructor (OwnerID) or owner role member (co-owner).
// Changes to settings, policy-enforced owner restrictions, and member management use this determination.
func (s *IdentityService) IsOwner(ctx context.Context, workspaceID, userID string) bool {
	role, ok := s.RoleOf(ctx, workspaceID, userID)
	return ok && role == domain.RoleOwner
}

// UpdateMemberRole changes a member's role — only owner can do this.
// The constructor's role cannot be changed (to prevent orphaned ownership — transfer is a separate feature).
func (s *IdentityService) UpdateMemberRole(ctx context.Context, actorID, workspaceID, targetID string, role domain.MemberRole) error {
	if !domain.ValidRole(role) {
		return domain.ErrValidation
	}
	if !s.IsOwner(ctx, workspaceID, actorID) {
		return domain.ErrForbidden
	}
	wsp, err := s.ws.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	if targetID == wsp.OwnerID {
		return domain.ErrConflict // Constructor role is fixed
	}
	ok, err := s.ws.IsMember(ctx, workspaceID, targetID)
	if err != nil {
		return err
	}
	if !ok {
		return domain.ErrNotFound
	}
	return s.ws.AddMember(ctx, domain.Membership{WorkspaceID: workspaceID, UserID: targetID, Role: role, CreatedAt: time.Now().UTC()})
}

// RemoveMember removes a member. Owner can remove anyone, members can remove themselves.
// Workspace constructor (OwnerID) cannot be removed.
func (s *IdentityService) RemoveMember(ctx context.Context, actorID, workspaceID, targetID string) error {
	wsp, err := s.ws.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	if targetID == wsp.OwnerID {
		return domain.ErrConflict // Owner cannot be removed
	}
	if actorID != targetID && !s.IsOwner(ctx, workspaceID, actorID) {
		return domain.ErrForbidden // Removing others is only for owner
	}
	return s.ws.RemoveMember(ctx, workspaceID, targetID)
}

// PublicUser returns public information for a user profile (/<username>): user + publicly visible
// workspaces (those owned by the user). For others/anonymous, only public; for the user (viewerID==user.ID), full.
// email is filled in only for the user.
func (s *IdentityService) PublicUser(ctx context.Context, username, viewerID string) (domain.User, []domain.Workspace, error) {
	u, err := s.ws.GetUserByUsername(ctx, username)
	if err != nil {
		return domain.User{}, nil, domain.ErrNotFound
	}
	list, err := s.ws.ListWorkspacesForUser(ctx, u.ID)
	if err != nil {
		return domain.User{}, nil, err
	}
	self := viewerID != "" && viewerID == u.ID
	out := make([]domain.Workspace, 0, len(list))
	for _, w := range list {
		if w.OwnerID != u.ID { // profiles list only owned workspaces, not workspaces joined as a member
			continue
		}
		if w.IsPublic() || self {
			out = append(out, w)
		}
	}
	if !self {
		u.Email = "" // Privacy — email is only for the user
	}
	return u, out, nil
}

// PublicWorkspace interprets a public workspace by URL path (username/slug) — for anonymous viewing.
// private workspaces return ErrNotFound to avoid leaking existence.
func (s *IdentityService) PublicWorkspace(ctx context.Context, username, slug string) (domain.Workspace, error) {
	wsp, err := s.ws.GetWorkspaceByPath(ctx, username, slug)
	if err != nil || !wsp.IsPublic() {
		return domain.Workspace{}, domain.ErrNotFound
	}
	return wsp, nil
}

// cliTokenPrefix is the prefix for CLI token sessions (distinguishes from web sessions "sess_" for listing).
const cliTokenPrefix = "sess_cli_"

// CLITokenInfo is a token list item — only the suffix is stored, as the full value is only exposed once at issuance.
type CLITokenInfo struct {
	Suffix    string    `json:"suffix"`          // Last 8 characters (identifier for display and disposal)
	Label     string    `json:"label,omitempty"` // Device display name (host name/browser summary)
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// sessionKind determines the type of storage record (new uses Kind field, legacy uses plain prefix).
func sessionKind(sess domain.Session) string {
	if sess.Kind != "" {
		return sess.Kind
	}
	if strings.HasPrefix(sess.Token, cliTokenPrefix) {
		return "cli"
	}
	if strings.HasPrefix(sess.Token, "sess_") {
		return "web"
	}
	return ""
}

// sessionHint returns the display suffix (new uses Hint, legacy uses plain token suffix).
func sessionHint(sess domain.Session) string {
	if sess.Hint != "" {
		return sess.Hint
	}
	return domain.TokenHint(sess.Token)
}

// ListCLITokens returns the user's CLI token list (token values are masked — only hint).
func (s *IdentityService) ListCLITokens(ctx context.Context, userID string) ([]CLITokenInfo, error) {
	return s.listSessions(ctx, userID, "cli")
}

func (s *IdentityService) listSessions(ctx context.Context, userID, kind string) ([]CLITokenInfo, error) {
	sessions, err := s.ws.ListSessionsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []CLITokenInfo
	for _, sess := range sessions {
		if sessionKind(sess) != kind {
			continue
		}
		out = append(out, CLITokenInfo{Suffix: sessionHint(sess), Label: sess.Label, CreatedAt: sess.CreatedAt, ExpiresAt: sess.ExpiresAt})
	}
	return out, nil
}

// RevokeCLIToken revokes the user's CLI token by suffix (last 8 characters).
func (s *IdentityService) RevokeCLIToken(ctx context.Context, userID, suffix string) error {
	return s.revokeSession(ctx, userID, suffix, true)
}

// ListWebSessions returns the list of web login sessions (device management — only hint exposed).
func (s *IdentityService) ListWebSessions(ctx context.Context, userID string) ([]CLITokenInfo, error) {
	return s.listSessions(ctx, userID, "web")
}

// RevokeWebSession revokes the user's web session by suffix (logs out from other devices).
func (s *IdentityService) RevokeWebSession(ctx context.Context, userID, suffix string) error {
	return s.revokeSession(ctx, userID, suffix, false)
}

// revokeSession deletes the user's session by hint (original suffix 8 characters) based on CLI flag.
func (s *IdentityService) revokeSession(ctx context.Context, userID, suffix string, cli bool) error {
	kind := "web"
	if cli {
		kind = "cli"
	}
	sessions, err := s.ws.ListSessionsForUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, sess := range sessions {
		if sessionKind(sess) != kind {
			continue
		}
		if sessionHint(sess) == suffix {
			return s.ws.DeleteSession(ctx, sess.Token)
		}
	}
	return domain.ErrNotFound
}

// cliTokenTTL is the lifespan of a CLI token (time-to-live).
const cliTokenTTL = 365 * 24 * time.Hour

// CreateCLIToken issues a CLI token session (issued from web → cxt login <token>).
// It follows the "sess_" format, which ResolveUser interprets directly. The original token is exposed only once here, and the server stores only a hash (at-rest encryption).
// The label is the device display name (device flow uses CLI host name, web issuance can be empty).
func (s *IdentityService) CreateCLIToken(ctx context.Context, userID, label string) (domain.Session, error) {
	return s.issueSession(ctx, userID, "sess_cli_", "cli", label, cliTokenTTL)
}

// issueSession issues a session: storage is HashToken(original)+hint+kind+label, the returned Token is the original.
// The label is a client-provided string, truncated to a maximum of 64 characters.
func (s *IdentityService) issueSession(ctx context.Context, userID, prefix, kind, label string, ttl time.Duration) (domain.Session, error) {
	now := time.Now().UTC()
	raw := domain.NewID(prefix)
	if label = strings.TrimSpace(label); len(label) > 64 {
		label = label[:64]
	}
	stored := domain.Session{
		Token:     domain.HashToken(raw),
		UserID:    userID,
		Hint:      domain.TokenHint(raw),
		Kind:      kind,
		Label:     label,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	if err := s.ws.CreateSession(ctx, stored); err != nil {
		return domain.Session{}, err
	}
	stored.Token = raw // Only the original token is passed to the caller (cookie/issue response)
	return stored, nil
}

// IsPublicWorkspace returns the public status (used for bypassing membership checks in query paths).
func (s *IdentityService) IsPublicWorkspace(ctx context.Context, workspaceID string) bool {
	wsp, err := s.ws.GetWorkspace(ctx, workspaceID)
	return err == nil && wsp.IsPublic()
}

// CreateWorkspace creates a workspace and registers its creator as an owner member.
// It derives the URL slug from the name, making it unique within the owner via -2, -3, and so on.
func (s *IdentityService) CreateWorkspace(ctx context.Context, owner domain.User, name string) (domain.Workspace, error) {
	name = strings.TrimSpace(name)
	// Naming rules (enforced in English): starts with a letter, ends with a letter or number, middle can only contain [A-Za-z0-9_-].
	if !domain.ValidWorkspaceName(name) {
		return domain.Workspace{}, domain.ErrValidation
	}
	now := time.Now().UTC()
	wsp := domain.Workspace{
		ID:            domain.NewID("ws_"),
		Name:          name,
		OwnerID:       owner.ID,
		Slug:          s.uniqueSlug(ctx, owner.ID, name, ""),
		OwnerUsername: owner.Username,
		CreatedAt:     now,
	}
	if err := s.ws.CreateWorkspace(ctx, wsp); err != nil {
		return domain.Workspace{}, err
	}
	if err := s.ws.AddMember(ctx, domain.Membership{WorkspaceID: wsp.ID, UserID: owner.ID, Role: domain.RoleOwner, CreatedAt: now}); err != nil {
		return domain.Workspace{}, err
	}
	return wsp, nil
}

// uniqueSlug generates a unique slug among the owner's workspaces.
// selfID is a value to exclude self collisions during backfill (new creation is "").
func (s *IdentityService) uniqueSlug(ctx context.Context, ownerID, name, selfID string) string {
	taken := map[string]bool{}
	if list, err := s.ws.ListWorkspacesForUser(ctx, ownerID); err == nil {
		for _, w := range list {
			if w.OwnerID == ownerID && w.ID != selfID && w.Slug != "" {
				taken[w.Slug] = true
			}
		}
	}
	base := domain.WorkspaceSlug(name)
	cand := base
	for n := 2; taken[cand]; n++ {
		cand = base + "-" + strconv.Itoa(n)
	}
	return cand
}

// ListWorkspaces returns the list of workspaces the user belongs to.
// Legacy workspaces without slugs are lazily backfilled here (self-healing in query paths).
func (s *IdentityService) ListWorkspaces(ctx context.Context, userID string) ([]domain.Workspace, error) {
	list, err := s.ws.ListWorkspacesForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	for i, w := range list {
		if w.Slug != "" && w.OwnerUsername != "" {
			continue
		}
		if fixed, ferr := s.backfillWorkspace(ctx, w); ferr == nil {
			list[i] = fixed
		}
	}
	return list, nil
}

// backfillWorkspace fills in legacy records with slug/owner_username.
func (s *IdentityService) backfillWorkspace(ctx context.Context, w domain.Workspace) (domain.Workspace, error) {
	owner, err := s.ws.GetUser(ctx, w.OwnerID)
	if err != nil {
		return w, err
	}
	if owner.Username == "" { // Heals legacy users too.
		owner.Username = s.uniqueUsername(ctx, owner)
		if err := s.ws.UpsertUser(ctx, owner); err != nil {
			return w, err
		}
	}
	if w.Slug == "" {
		w.Slug = s.uniqueSlug(ctx, w.OwnerID, w.Name, w.ID)
	}
	w.OwnerUsername = owner.Username
	if err := s.ws.CreateWorkspace(ctx, w); err != nil { // Upsert in FS/PG.
		return w, err
	}
	return w, nil
}

// Invite creates a workspace invitation (share token) — maintainer level (5-rung ladder).
// If email=="" anyone can join, otherwise only that email can accept.
// If ttl>0 it expires after that time (AcceptInvite checks), 0 is indefinite. Negative values reject.
func (s *IdentityService) Invite(ctx context.Context, userID, workspaceID, email string, role domain.MemberRole, ttl time.Duration) (domain.Invite, error) {
	actor, ok := s.RoleOf(ctx, workspaceID, userID)
	if !ok || !actor.AtLeast(domain.RoleMaintainer) {
		return domain.Invite{}, domain.ErrForbidden
	}
	if !domain.ValidRole(role) {
		return domain.Invite{}, domain.ErrValidation // Reject invalid/ruined role values (RoleOf fail-closed and consistent).
	}
	// Prevent role upgrades: higher roles than own cannot be granted directly.
	// Especially owner grants are blocked via this gate (maintainer circumvents owner invites).
	// UpdateMemberRole is owner-only — invites cannot be backdoors.
	if !actor.AtLeast(role) {
		return domain.Invite{}, domain.ErrForbidden
	}
	if ttl < 0 {
		return domain.Invite{}, domain.ErrValidation
	}
	var expires *time.Time
	if ttl > 0 {
		t := time.Now().UTC().Add(ttl)
		expires = &t
	}
	inv := domain.Invite{
		Token:       domain.NewID("inv_"),
		WorkspaceID: workspaceID,
		Email:       strings.TrimSpace(email),
		Role:        role,
		Status:      domain.InvitePending,
		CreatedBy:   userID,
		CreatedAt:   time.Now().UTC(),
		ExpiresAt:   expires,
	}
	if err := s.ws.CreateInvite(ctx, inv); err != nil {
		return domain.Invite{}, err
	}
	return inv, nil
}

// AcceptInvite joins a workspace by token (member addition is idempotent — link reuse possible).
func (s *IdentityService) AcceptInvite(ctx context.Context, user domain.User, token string) (domain.Workspace, error) {
	inv, err := s.ws.GetInvite(ctx, token)
	if err != nil {
		return domain.Workspace{}, err // ErrNotFound
	}
	if inv.Status != domain.InvitePending {
		return domain.Workspace{}, domain.ErrConflict // Revoked/expired.
	}
	if inv.ExpiresAt != nil && time.Now().After(*inv.ExpiresAt) {
		return domain.Workspace{}, domain.ErrConflict
	}
	if inv.Email != "" && !strings.EqualFold(inv.Email, user.Email) {
		return domain.Workspace{}, domain.ErrForbidden // Invite to specific email target
	}
	existingRole, wasMember := s.RoleOf(ctx, inv.WorkspaceID, user.ID)
	// Reinviting an old low role invite does not downgrade the current permissions. For existing members,
	// an invite is upserted only if it explicitly promotes, and is a no-op if it is equal or lower.
	if !wasMember || !existingRole.AtLeast(inv.Role) {
		if err := s.ws.AddMember(ctx, domain.Membership{
			WorkspaceID: inv.WorkspaceID,
			UserID:      user.ID,
			Role:        inv.Role,
			CreatedAt:   time.Now().UTC(),
		}); err != nil {
			return domain.Workspace{}, err
		}
	}
	wsp, err := s.ws.GetWorkspace(ctx, inv.WorkspaceID)
	if err != nil {
		return domain.Workspace{}, err
	}
	// Legacy records are immediately healed for redirect URL (/<owner_username>/<slug>) for join redirection.
	if wsp.Slug == "" || wsp.OwnerUsername == "" {
		if fixed, ferr := s.backfillWorkspace(ctx, wsp); ferr == nil {
			wsp = fixed
		}
	}
	// Only new joins are notified — invite links are idempotent (accept), so re-clicks by existing members are silent.
	if !wasMember {
		who := user.Username
		if who == "" {
			who = user.Email
		}
		notifyWorkspace(wsp, fmt.Sprintf("cxthub: %s — %s joined as %s", wsp.Name, who, inv.Role))
	}
	return wsp, nil
}

// ListMembers returns the list of workspace members (caller must be a member).
func (s *IdentityService) ListMembers(ctx context.Context, userID, workspaceID string) ([]domain.Membership, error) {
	ok, err := s.ws.IsMember(ctx, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, domain.ErrForbidden
	}
	return s.ws.ListMembers(ctx, workspaceID)
}

// RevokeInvite invalidates the invite — requires maintainer or higher (same gate as creation). Blocks token reuse.
func (s *IdentityService) RevokeInvite(ctx context.Context, userID, workspaceID, token string) error {
	inv, err := s.ws.GetInvite(ctx, token)
	if err != nil {
		return err
	}
	if inv.WorkspaceID != workspaceID {
		return domain.ErrNotFound
	}
	actor, ok := s.RoleOf(ctx, inv.WorkspaceID, userID)
	if !ok || !actor.AtLeast(domain.RoleMaintainer) {
		return domain.ErrForbidden
	}
	return s.ws.UpdateInviteStatus(ctx, token, domain.InviteRevoked)
}

// ListInvites returns the list of workspace invites — requires maintainer or higher (for invite management screen).
func (s *IdentityService) ListInvites(ctx context.Context, userID, workspaceID string) ([]domain.Invite, error) {
	actor, ok := s.RoleOf(ctx, workspaceID, userID)
	if !ok || !actor.AtLeast(domain.RoleMaintainer) {
		return nil, domain.ErrForbidden
	}
	return s.ws.ListInvites(ctx, workspaceID)
}

// sessionTTL is the server session lifetime.
const sessionTTL = 30 * 24 * time.Hour

// Login validates and upserts an IDP token (Firebase ID token / dev token), issuing a server session (stored in DB).
// label is the device display name (browser/OS summary — derived from UA in the delivery layer).
func (s *IdentityService) Login(ctx context.Context, idpToken, label string) (domain.User, domain.Session, error) {
	u, err := s.Authenticate(ctx, idpToken)
	if err != nil {
		return domain.User{}, domain.Session{}, err
	}
	sess, err := s.issueSession(ctx, u.ID, "sess_", "web", label, sessionTTL)
	if err != nil {
		return domain.User{}, domain.Session{}, err
	}
	return u, sess, nil
}

// Logout deletes the session (idempotent — deletes hash record and legacy plaintext record).
func (s *IdentityService) Logout(ctx context.Context, sessionToken string) error {
	if err := s.ws.DeleteSession(ctx, domain.HashToken(sessionToken)); err != nil {
		return err
	}
	return s.ws.DeleteSession(ctx, sessionToken) // legacy residue cleanup
}

// ResolveSession interprets a session token as a user. Expired/invalid → domain.ErrUnauthorized.
// The storage is queried by hash, and legacy plaintext records are promoted to hash records lazily (migration — existing login/CLI tokens do not break).
func (s *IdentityService) ResolveSession(ctx context.Context, token string) (domain.User, error) {
	sess, err := s.ws.GetSession(ctx, domain.HashToken(token))
	if err != nil {
		legacy, lerr := s.ws.GetSession(ctx, token)
		if lerr != nil {
			return domain.User{}, domain.ErrUnauthorized
		}
		// Promotion: plaintext record → hash record (+hint/kind enhancement), plaintext is deleted.
		upgraded := legacy
		upgraded.Token = domain.HashToken(token)
		upgraded.Hint = domain.TokenHint(token)
		if strings.HasPrefix(token, cliTokenPrefix) {
			upgraded.Kind = "cli"
		} else {
			upgraded.Kind = "web"
		}
		if uerr := s.ws.CreateSession(ctx, upgraded); uerr == nil {
			_ = s.ws.DeleteSession(ctx, token)
		}
		sess = upgraded
	}
	if time.Now().After(sess.ExpiresAt) {
		_ = s.ws.DeleteSession(ctx, sess.Token)
		return domain.User{}, domain.ErrUnauthorized
	}
	u, err := s.ws.GetUser(ctx, sess.UserID)
	if err != nil {
		return domain.User{}, domain.ErrUnauthorized
	}
	return u, nil
}

// ResolveUser interprets a Bearer token as a user: "sess_" prefix indicates a server session, otherwise validates and upserts an IDP token.
// The requireUser middleware is used on all protected routes.
func (s *IdentityService) ResolveUser(ctx context.Context, bearer string) (domain.User, error) {
	if bearer == "" {
		return domain.User{}, domain.ErrUnauthorized
	}
	if strings.HasPrefix(bearer, "sess_") {
		return s.ResolveSession(ctx, bearer)
	}
	return s.Authenticate(ctx, bearer)
}
