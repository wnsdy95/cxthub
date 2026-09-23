package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// IdentityService implements authentication (Firebase) + repository/membership/invitation use-cases.
//
// Dependencies: IdentityVerifier (token→User), RepositoryStore (persistence). Visibility boundary = repository.
// Invitations use the "share link/code" model — tokens are reusable until revoked (member addition is idempotent).
type IdentityService struct {
	verifier     outbound.IdentityVerifier
	repositories outbound.RepositoryStore
	organization outbound.OrganizationStore
	teams        outbound.TeamStore
}

// NewIdentityService creates an IdentityService.
func NewIdentityService(verifier outbound.IdentityVerifier, repositories outbound.RepositoryStore) *IdentityService {
	organization, _ := repositories.(outbound.OrganizationStore)
	teams, _ := repositories.(outbound.TeamStore)
	return &IdentityService{verifier: verifier, repositories: repositories, organization: organization, teams: teams}
}

// Authenticate validates a token, upserts the user, and returns it (login entry point).
// For the first login, a unique username (handle) is automatically assigned from the email local part —
// becomes the first segment of the user's personal namespace URL.
func (s *IdentityService) Authenticate(ctx context.Context, idToken string) (domain.User, error) {
	u, err := s.verifier.Verify(ctx, idToken)
	if err != nil {
		return domain.User{}, err
	}
	if existing, gerr := s.repositories.GetUser(ctx, u.ID); gerr == nil {
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
		u.Username, err = s.uniqueUsername(ctx, u)
		if err != nil {
			return domain.User{}, err
		}
	}
	if err := s.repositories.UpsertUser(ctx, u); err != nil {
		return domain.User{}, err
	}
	if s.organization != nil {
		if _, err := s.ensurePersonalNamespace(ctx, u); err != nil {
			return domain.User{}, err
		}
	}
	return u, nil
}

// reservedUsernames are reserved words that conflict with URL segments (routing disruption prevention — sync with front RESERVED).
var reservedUsernames = map[string]bool{
	"api": true, "invite": true, "w": true, "login": true, "settings": true,
	"assets": true, "public": true, "admin": true, "static": true, "cxt": true,
	"pricing": true, "connect": true, "oauth": true, "mcp": true, "enterprises": true,
}

// uniqueUsername creates a global unique handle from the email local part (or name if none) — collision handling (-2, -3, ...).
func (s *IdentityService) uniqueUsername(ctx context.Context, u domain.User) (string, error) {
	seed := u.Email
	if at := strings.IndexByte(seed, '@'); at > 0 {
		seed = seed[:at]
	}
	if seed == "" {
		seed = u.Name
	}
	base := domain.Slugify(seed, "user")
	if !domain.ValidNamespaceSlug(base) {
		base = "user"
	}
	cand := base
	for n := 2; ; n++ {
		if !reservedUsernames[cand] {
			other, err := s.repositories.GetUserByUsername(ctx, cand)
			userAvailable := errors.Is(err, domain.ErrNotFound) || err == nil && other.ID == u.ID
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return "", err
			}
			namespaceAvailable := true
			if s.organization != nil {
				claimed, namespaceErr := s.organization.GetNamespaceBySlug(ctx, cand)
				namespaceAvailable = errors.Is(namespaceErr, domain.ErrNotFound) ||
					namespaceErr == nil && claimed.Kind == domain.NamespaceUser && claimed.UserID == u.ID
				if namespaceErr != nil && !errors.Is(namespaceErr, domain.ErrNotFound) {
					return "", namespaceErr
				}
			}
			if userAvailable && namespaceAvailable {
				return cand, nil
			}
		}
		suffix := "-" + strconv.Itoa(n)
		trimmed := base
		if len(trimmed)+len(suffix) > 64 {
			trimmed = strings.TrimRight(trimmed[:64-len(suffix)], "-")
		}
		cand = trimmed + suffix
	}
}

// UpdateProfile updates account settings. Nil fields are not changed.
//
//   - nickname: display alias — free change (empty string = remove).
//   - username: personal Namespace first segment. The previous slug is retained
//     as an alias so existing URL-derived RepoIDs and CLI remotes keep working.
func (s *IdentityService) mutateUpdateProfile(ctx context.Context, u domain.User, username, nickname, loadMode, avatar, locale *string) (domain.User, error) {
	original := u
	var personalNamespace domain.Namespace
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
			if other, err := s.repositories.GetUserByUsername(ctx, next); err == nil && other.ID != u.ID {
				return domain.User{}, domain.ErrConflict // handle already in use
			}
			if s.organization != nil {
				var err error
				personalNamespace, err = s.ensurePersonalNamespace(ctx, u)
				if err != nil {
					return domain.User{}, err
				}
				if claimed, claimErr := s.organization.GetNamespaceBySlug(ctx, next); claimErr == nil && claimed.ID != personalNamespace.ID {
					return domain.User{}, domain.ErrConflict
				}
			}
			u.Username = next
			usernameChanged = true
		}
	}
	if err := s.repositories.UpsertUser(ctx, u); err != nil {
		return domain.User{}, err
	}
	if usernameChanged {
		if s.organization != nil {
			if err := s.organization.RenameNamespace(ctx, personalNamespace.ID, u.Username); err != nil {
				_ = s.repositories.UpsertUser(ctx, original)
				return domain.User{}, err
			}
		}
		// Normalize only personal namespace paths. Organization repositories keep the
		// company slug even when their human creator renames a personal handle.
		if list, err := s.repositories.ListRepositoriesForUser(ctx, u.ID); err != nil {
			return domain.User{}, err
		} else {
			for _, w := range list {
				if w.OwnerID != u.ID || w.OwnerUsername == u.Username {
					continue
				}
				if w.OwnerNamespaceID != "" && s.organization != nil {
					namespace, nerr := s.organization.GetNamespace(ctx, w.OwnerNamespaceID)
					if nerr != nil {
						return domain.User{}, nerr
					}
					if namespace.Kind == domain.NamespaceOrganization {
						continue
					}
				}
				w.OwnerUsername = u.Username
				if err := s.repositories.CreateRepository(ctx, w); err != nil {
					return domain.User{}, err
				}
			}
		}
	}
	return u, nil
}

// RepositoryPatch updates repository settings. Nil fields do not change.
type RepositoryPatch struct {
	Visibility       *domain.Visibility // private|public
	SecretsPolicy    *string            // ""|members(role-based)|owner
	SettingsPolicy   *string            // value meaning same
	GHVisibilitySync *bool              // GitHub public state sync on/off
	Archived         *bool              // Archive (read-only) on/off
	WebhookURL       *string            // Alert webhook (empty string = disabled)
	Slug             *string            // URL segment manual change (heavy — owner unique slug validation)
	PublicRole       *string            // Public role for non-members ("", viewer, puller)
}

// UpdateRepositorySettings updates repository settings (public scope, permission policy) — only owner can do this.
func (s *IdentityService) mutateUpdateRepositorySettings(ctx context.Context, userID, repositoryID string, p RepositoryPatch) (domain.Repository, error) {
	repositoryRecord, err := s.repositories.GetRepository(ctx, repositoryID)
	if err != nil {
		return domain.Repository{}, err
	}
	if !s.IsOwner(ctx, repositoryID, userID) {
		return domain.Repository{}, domain.ErrForbidden // Repository settings are owner exclusive
	}
	if p.GHVisibilitySync != nil {
		repositoryRecord.GHVisibilitySync = *p.GHVisibilitySync
	}
	if p.Visibility != nil {
		if repositoryRecord.GHVisibilitySync {
			return domain.Repository{}, domain.ErrConflict // Manual settings locked during sync (to prevent truth conflict)
		}
		if *p.Visibility != domain.VisibilityPrivate && *p.Visibility != domain.VisibilityPublic {
			return domain.Repository{}, domain.ErrValidation
		}
		if *p.Visibility == domain.VisibilityPublic && repositoryRecord.OwnerNamespaceID != "" && s.organization != nil {
			if namespace, nerr := s.organization.GetNamespace(ctx, repositoryRecord.OwnerNamespaceID); nerr == nil && namespace.Kind == domain.NamespaceOrganization {
				policy, perr := s.effectiveOrganizationPolicy(ctx, namespace.OrganizationID)
				if perr != nil || !policy.AllowPublicRepositories {
					return domain.Repository{}, domain.ErrForbidden
				}
			}
		}
		repositoryRecord.Visibility = *p.Visibility
	}
	validPolicy := func(v string) bool { return v == "" || v == "members" || v == "owner" }
	if p.SecretsPolicy != nil {
		if !validPolicy(*p.SecretsPolicy) {
			return domain.Repository{}, domain.ErrValidation
		}
		repositoryRecord.SecretsPolicy = *p.SecretsPolicy
	}
	if p.SettingsPolicy != nil {
		if !validPolicy(*p.SettingsPolicy) {
			return domain.Repository{}, domain.ErrValidation
		}
		repositoryRecord.SettingsPolicy = *p.SettingsPolicy
	}
	if p.Archived != nil {
		repositoryRecord.Archived = *p.Archived
	}
	if p.PublicRole != nil {
		if !domain.ValidPublicRole(*p.PublicRole) {
			return domain.Repository{}, domain.ErrValidation // Only "" | viewer | puller allowed
		}
		repositoryRecord.PublicRole = *p.PublicRole
	}
	if p.WebhookURL != nil {
		repositoryRecord.WebhookURL = strings.TrimSpace(*p.WebhookURL)
	}
	if p.Slug != nil {
		next := strings.ToLower(strings.TrimSpace(*p.Slug))
		if !domain.ValidRepositorySlug(next) {
			return domain.Repository{}, domain.ErrValidation
		}
		if next != repositoryRecord.Slug {
			// Namespace-unique check (excluding self). Organization repositories may
			// have different human creators, so owner_id alone is insufficient.
			var list []domain.Repository
			var lerr error
			if repositoryRecord.OwnerNamespaceID != "" && s.organization != nil {
				list, lerr = s.organization.ListRepositoriesForNamespace(ctx, repositoryRecord.OwnerNamespaceID)
			} else {
				list, lerr = s.repositories.ListRepositoriesForUser(ctx, repositoryRecord.OwnerID)
			}
			if lerr == nil {
				for _, w := range list {
					if w.ID != repositoryRecord.ID && w.Slug == next {
						return domain.Repository{}, domain.ErrConflict
					}
				}
			}
			repositoryRecord.Slug = next
		}
	}
	if err := s.repositories.CreateRepository(ctx, repositoryRecord); err != nil { // upsert meaning
		return domain.Repository{}, err
	}
	return repositoryRecord, nil
}

// TransferOwnership transfers the repository creator (OwnerID) to an existing member, authorized by the current creator or Organization Owner.
//
//   - The new owner is promoted to the owner role, and the original creator remains an owner member (GitHub style —
//     you can later downgrade or leave).
//   - Personal repositories move to the target's personal Namespace and may get a
//     collision suffix. Organization repositories remain in their Organization
//     Namespace; only the human permission anchor changes.
func (s *IdentityService) mutateTransferOwnership(ctx context.Context, actorID, repositoryID, targetID string) (domain.Repository, error) {
	repositoryRecord, err := s.repositories.GetRepository(ctx, repositoryID)
	if err != nil {
		return domain.Repository{}, err
	}
	organizationAccess, err := repositoryOrganizationAccess(ctx, s.repositories, repositoryID, actorID)
	if err != nil {
		return domain.Repository{}, err
	}
	if !domain.CanTransferRepository(repositoryRecord, actorID, organizationAccess) {
		return domain.Repository{}, domain.ErrForbidden
	}
	if targetID == repositoryRecord.OwnerID {
		return domain.Repository{}, domain.ErrValidation
	}
	target, err := s.repositories.GetUser(ctx, targetID)
	if err != nil {
		return domain.Repository{}, domain.ErrNotFound
	}
	if ok, err := s.repositories.IsMember(ctx, repositoryID, targetID); err != nil || !ok {
		return domain.Repository{}, domain.ErrNotFound // only existing members can transfer
	}
	organizationOwned := false
	if repositoryRecord.OwnerNamespaceID != "" && s.organization != nil {
		namespace, nerr := s.organization.GetNamespace(ctx, repositoryRecord.OwnerNamespaceID)
		if nerr != nil {
			return domain.Repository{}, nerr
		}
		organizationOwned = namespace.Kind == domain.NamespaceOrganization
	}
	repositoryRecord.OwnerID = targetID
	if !organizationOwned {
		taken := map[string]bool{}
		if list, lerr := s.repositories.ListRepositoriesForUser(ctx, targetID); lerr == nil {
			for _, repository := range list {
				if repository.OwnerID == targetID && repository.ID != repositoryRecord.ID {
					taken[repository.Slug] = true
				}
			}
		}
		slug := repositoryRecord.Slug
		for n := 2; taken[slug]; n++ {
			slug = repositoryRecord.Slug + "-" + strconv.Itoa(n)
		}
		repositoryRecord.OwnerUsername = target.Username
		repositoryRecord.Slug = slug
		if s.organization != nil {
			namespace, nerr := s.ensurePersonalNamespace(ctx, target)
			if nerr != nil {
				return domain.Repository{}, nerr
			}
			repositoryRecord.OwnerNamespaceID = namespace.ID
			repositoryRecord.Slug, nerr = s.uniqueNamespaceRepositorySlug(ctx, namespace.ID, repositoryRecord.Slug, repositoryRecord.ID)
			if nerr != nil {
				return domain.Repository{}, nerr
			}
		}
	}
	if err := s.repositories.CreateRepository(ctx, repositoryRecord); err != nil { // upsert
		return domain.Repository{}, err
	}
	// Promote the new creator to the owner role (the original creator's owner membership remains unchanged).
	if err := s.repositories.AddMember(ctx, domain.Membership{RepositoryID: repositoryID, UserID: targetID, Role: domain.RoleOwner, CreatedAt: time.Now().UTC()}); err != nil {
		return domain.Repository{}, err
	}
	return repositoryRecord, nil
}

// GetRepository retrieves a repository (used in policy decisions etc. at the delivery boundary).
func (s *IdentityService) GetRepository(ctx context.Context, repositoryID string) (domain.Repository, error) {
	return s.repositories.GetRepository(ctx, repositoryID)
}

// RoleOf combines direct/team grants and current Organization Owner authority.
func (s *IdentityService) RoleOf(ctx context.Context, repositoryID, userID string) (domain.MemberRole, bool) {
	return repositoryRole(ctx, s.repositories, repositoryID, userID)
}

// IsOwner includes inherited Organization Owner permissions.
// Changes to settings, policy-enforced owner restrictions, and member management use this determination.
func (s *IdentityService) IsOwner(ctx context.Context, repositoryID, userID string) bool {
	role, ok := s.RoleOf(ctx, repositoryID, userID)
	return ok && role == domain.RoleOwner
}

// CanTransferOwnership preserves personal creator authority while allowing an
// Organization Owner to change the human anchor of an organization repository.
func (s *IdentityService) CanTransferOwnership(ctx context.Context, repositoryID, actor string) bool {
	repository, err := s.repositories.GetRepository(ctx, repositoryID)
	if err != nil {
		return false
	}
	organization, err := repositoryOrganizationAccess(ctx, s.repositories, repositoryID, actor)
	return err == nil && domain.CanTransferRepository(repository, actor, organization)
}

// UpdateMemberRole changes a member's role — only owner can do this.
// The constructor's role cannot be changed (to prevent orphaned ownership — transfer is a separate feature).
func (s *IdentityService) mutateUpdateMemberRole(ctx context.Context, actorID, repositoryID, targetID string, role domain.MemberRole) error {
	if !domain.ValidRole(role) {
		return domain.ErrValidation
	}
	if !s.IsOwner(ctx, repositoryID, actorID) {
		return domain.ErrForbidden
	}
	repositoryRecord, err := s.repositories.GetRepository(ctx, repositoryID)
	if err != nil {
		return err
	}
	if targetID == repositoryRecord.OwnerID {
		return domain.ErrConflict // Constructor role is fixed
	}
	ok, err := s.repositories.IsMember(ctx, repositoryID, targetID)
	if err != nil {
		return err
	}
	if !ok {
		return domain.ErrNotFound
	}
	return s.repositories.AddMember(ctx, domain.Membership{RepositoryID: repositoryID, UserID: targetID, Role: role, CreatedAt: time.Now().UTC()})
}

// RemoveMember removes a member. Owner can remove anyone, members can remove themselves.
// Repository constructor (OwnerID) cannot be removed.
func (s *IdentityService) mutateRemoveMember(ctx context.Context, actorID, repositoryID, targetID string) error {
	repositoryRecord, err := s.repositories.GetRepository(ctx, repositoryID)
	if err != nil {
		return err
	}
	if targetID == repositoryRecord.OwnerID {
		return domain.ErrConflict // Owner cannot be removed
	}
	if actorID != targetID && !s.IsOwner(ctx, repositoryID, actorID) {
		return domain.ErrForbidden // Removing others is only for owner
	}
	return s.repositories.RemoveMember(ctx, repositoryID, targetID)
}

// PublicUser returns public information for a user profile (/<username>): user + publicly visible
// repositories (those owned by the user). For others/anonymous, only public; for the user (viewerID==user.ID), full.
// email is filled in only for the user.
func (s *IdentityService) PublicUser(ctx context.Context, username, viewerID string) (domain.User, []domain.Repository, error) {
	u, err := s.repositories.GetUserByUsername(ctx, username)
	if err != nil {
		return domain.User{}, nil, domain.ErrNotFound
	}
	list, err := s.repositories.ListRepositoriesForUser(ctx, u.ID)
	if err != nil {
		return domain.User{}, nil, err
	}
	self := viewerID != "" && viewerID == u.ID
	personalNamespaceID := ""
	if s.organization != nil {
		if ns, nsErr := s.organization.GetNamespaceBySlug(ctx, u.Username); nsErr == nil && ns.Kind == domain.NamespaceUser && ns.UserID == u.ID {
			personalNamespaceID = ns.ID
		}
	}
	out := make([]domain.Repository, 0, len(list))
	for _, w := range list {
		if w.OwnerID != u.ID { // profiles list only owned repositories, not repositories joined as a member
			continue
		}
		// Organization Repository creation records the human creator as OwnerID so
		// Repository authorization has an initial owner. It still belongs on the
		// Organization namespace profile, never on that creator's personal profile.
		if w.OwnerNamespaceID != "" && w.OwnerNamespaceID != personalNamespaceID {
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

func (s *IdentityService) repositoryByNamespacePath(ctx context.Context, namespace, slug string) (domain.Repository, error) {
	if s.organization != nil {
		ns, err := s.organization.GetNamespaceBySlug(ctx, namespace)
		if err == nil {
			repository, lookupErr := s.repositories.GetRepositoryByNamespacePath(ctx, ns.ID, slug)
			if errors.Is(lookupErr, domain.ErrNotFound) {
				return s.repositories.GetRepositoryByPath(ctx, namespace, slug)
			}
			return repository, lookupErr
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return domain.Repository{}, err
		}
		// Exact recorded aliases can predate the namespace registry, including
		// an old owner handle. Resolve their target, then authorize that target.
		// The store never guesses a container child or grants access by URL.
		return s.repositories.GetRepositoryByPath(ctx, namespace, slug)
	}
	return s.repositories.GetRepositoryByPath(ctx, namespace, slug)
}

// PublicRepository interprets a public repository by URL path (username/slug) — for anonymous viewing.
// private repositories return ErrNotFound to avoid leaking existence.
func (s *IdentityService) PublicRepository(ctx context.Context, username, slug string) (domain.Repository, error) {
	repositoryRecord, err := s.repositoryByNamespacePath(ctx, username, slug)
	if err != nil || !repositoryRecord.IsPublic() {
		return domain.Repository{}, domain.ErrNotFound
	}
	return repositoryRecord, nil
}

// ReadableRepository resolves a public path without leaking private Repository
// existence. Break-glass is a narrow viewer exception and never creates a
// durable Repository membership.
func (s *IdentityService) ReadableRepository(ctx context.Context, namespace, slug, viewerID string) (domain.Repository, error) {
	repositoryRecord, err := s.repositoryByNamespacePath(ctx, namespace, slug)
	if err != nil {
		return domain.Repository{}, domain.ErrNotFound
	}
	if repositoryRecord.IsPublic() {
		return repositoryRecord, nil
	}
	if viewerID == "" {
		return domain.Repository{}, domain.ErrNotFound
	}
	if role, ok := s.RoleOf(ctx, repositoryRecord.ID, viewerID); ok && role.AtLeast(domain.RoleViewer) {
		return repositoryRecord, nil
	}
	if allowed, accessErr := s.HasBreakGlassAccess(ctx, repositoryRecord.ID, viewerID); accessErr != nil {
		return domain.Repository{}, accessErr
	} else if allowed {
		return repositoryRecord, nil
	}
	return domain.Repository{}, domain.ErrNotFound
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
	sessions, err := s.repositories.ListSessionsForUser(ctx, userID)
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
	sessions, err := s.repositories.ListSessionsForUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, sess := range sessions {
		if sessionKind(sess) != kind {
			continue
		}
		if sessionHint(sess) == suffix {
			return s.repositories.DeleteSession(ctx, sess.Token)
		}
	}
	return domain.ErrNotFound
}

// cliTokenTTL is the lifespan of a CLI token (time-to-live).
const cliTokenTTL = 365 * 24 * time.Hour

const (
	mcpAccessTokenTTL  = time.Hour
	mcpRefreshTokenTTL = 30 * 24 * time.Hour
)

// CreateCLIToken issues a CLI token session (issued from web → cxt login <token>).
// It follows the "sess_" format, which ResolveUser interprets directly. The original token is exposed only once here, and the server stores only a hash (at-rest encryption).
// The label is the device display name (device flow uses CLI host name, web issuance can be empty).
func (s *IdentityService) CreateCLIToken(ctx context.Context, userID, label string) (domain.Session, error) {
	return s.issueSession(ctx, userID, "sess_cli_", "cli", label, cliTokenTTL)
}

// IssueMCPTokenPair issues a short-lived read-only MCP access token and a
// client-bound refresh token. Both are stored only by hash through the
// existing session store. The refresh token deliberately does not use the
// sess_ prefix, so it cannot be presented to ordinary API/MCP bearer gates.
func (s *IdentityService) IssueMCPTokenPair(ctx context.Context, userID, clientID string) (domain.OAuthTokenPair, error) {
	if _, err := s.repositories.GetUser(ctx, userID); err != nil {
		return domain.OAuthTokenPair{}, domain.ErrUnauthorized
	}
	access, err := s.issueSession(ctx, userID, "sess_", "mcp_access", clientID, mcpAccessTokenTTL)
	if err != nil {
		return domain.OAuthTokenPair{}, err
	}
	refresh, err := s.issueSession(ctx, userID, "refresh_", "mcp_refresh", clientID, mcpRefreshTokenTTL)
	if err != nil {
		_ = s.repositories.DeleteSession(ctx, domain.HashToken(access.Token))
		return domain.OAuthTokenPair{}, err
	}
	return domain.OAuthTokenPair{
		AccessToken: access.Token, RefreshToken: refresh.Token,
		ExpiresIn: int(mcpAccessTokenTTL.Seconds()), Scope: "mcp:read",
	}, nil
}

// RefreshMCPAccessToken validates a client-bound refresh capability and
// returns a new access/refresh pair while atomically consuming the old refresh
// capability. Refresh tokens are never accepted by ordinary bearer gates.
func (s *IdentityService) RefreshMCPAccessToken(ctx context.Context, refreshToken, clientID string) (domain.OAuthTokenPair, error) {
	if !strings.HasPrefix(refreshToken, "refresh_") || clientID == "" {
		return domain.OAuthTokenPair{}, domain.ErrUnauthorized
	}
	storedToken := domain.HashToken(refreshToken)
	sess, err := s.repositories.ConsumeSession(ctx, storedToken, "mcp_refresh", clientID)
	if err != nil || !time.Now().UTC().Before(sess.ExpiresAt) {
		return domain.OAuthTokenPair{}, domain.ErrUnauthorized
	}
	if _, err := s.repositories.GetUser(ctx, sess.UserID); err != nil {
		return domain.OAuthTokenPair{}, domain.ErrUnauthorized
	}
	pair, err := s.IssueMCPTokenPair(ctx, sess.UserID, clientID)
	if err != nil {
		_ = s.repositories.CreateSession(ctx, sess) // Preserve retry capability if issuance storage failed.
		return domain.OAuthTokenPair{}, err
	}
	return pair, nil
}

// RevokeMCPToken invalidates an MCP access or refresh token only when it is
// bound to the requesting public OAuth client. Invalid, foreign, and already
// revoked tokens are intentionally idempotent no-ops.
func (s *IdentityService) RevokeMCPToken(ctx context.Context, token, clientID string) error {
	if clientID == "" {
		return domain.ErrUnauthorized
	}
	kind := ""
	switch {
	case strings.HasPrefix(token, "sess_"):
		kind = "mcp_access"
	case strings.HasPrefix(token, "refresh_"):
		kind = "mcp_refresh"
	default:
		return nil
	}
	_, err := s.repositories.ConsumeSession(ctx, domain.HashToken(token), kind, clientID)
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrUnauthorized) {
		return nil
	}
	return err
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
	if err := s.repositories.CreateSession(ctx, stored); err != nil {
		return domain.Session{}, err
	}
	stored.Token = raw // Only the original token is passed to the caller (cookie/issue response)
	return stored, nil
}

// IsPublicRepository returns the public status (used for bypassing membership checks in query paths).
func (s *IdentityService) IsPublicRepository(ctx context.Context, repositoryID string) bool {
	repositoryRecord, err := s.repositories.GetRepository(ctx, repositoryID)
	return err == nil && repositoryRecord.IsPublic()
}

// CreateRepository creates a repository and registers its creator as an owner member.
// It derives the URL slug from the name, making it unique within the owner via -2, -3, and so on.
func (s *IdentityService) mutateCreateRepository(ctx context.Context, owner domain.User, name string) (domain.Repository, error) {
	name = strings.TrimSpace(name)
	// Naming rules (enforced in English): starts with a letter, ends with a letter or number, middle can only contain [A-Za-z0-9_-].
	if !domain.ValidRepositoryName(name) {
		return domain.Repository{}, domain.ErrValidation
	}
	now := time.Now().UTC()
	var namespaceID string
	if s.organization != nil {
		namespace, err := s.ensurePersonalNamespace(ctx, owner)
		if err != nil {
			return domain.Repository{}, err
		}
		namespaceID = namespace.ID
	}
	slug := s.uniqueSlug(ctx, owner.ID, name, "")
	if namespaceID != "" {
		var err error
		slug, err = s.uniqueNamespaceRepositorySlug(ctx, namespaceID, name, "")
		if err != nil {
			return domain.Repository{}, err
		}
	}
	repositoryRecord := domain.Repository{
		ID:               domain.NewID("ws_"),
		Name:             name,
		OwnerID:          owner.ID,
		Slug:             slug,
		OwnerUsername:    owner.Username,
		OwnerNamespaceID: namespaceID,
		CreatedAt:        now,
	}
	if err := s.repositories.CreateRepository(ctx, repositoryRecord); err != nil {
		return domain.Repository{}, err
	}
	if err := s.repositories.AddMember(ctx, domain.Membership{RepositoryID: repositoryRecord.ID, UserID: owner.ID, Role: domain.RoleOwner, CreatedAt: now}); err != nil {
		return domain.Repository{}, err
	}
	return repositoryRecord, nil
}

// uniqueSlug generates a unique slug among the owner's repositories.
// selfID is a value to exclude self collisions during backfill (new creation is "").
func (s *IdentityService) uniqueSlug(ctx context.Context, ownerID, name, selfID string) string {
	taken := map[string]bool{}
	if list, err := s.repositories.ListRepositoriesForUser(ctx, ownerID); err == nil {
		for _, w := range list {
			if w.OwnerID == ownerID && w.ID != selfID && w.Slug != "" {
				taken[w.Slug] = true
			}
		}
	}
	base := domain.RepositorySlug(name)
	cand := base
	for n := 2; taken[cand]; n++ {
		cand = base + "-" + strconv.Itoa(n)
	}
	return cand
}

// ListRepositories returns the list of repositories the user belongs to.
// Legacy repositories without slugs are lazily backfilled here (self-healing in query paths).
func (s *IdentityService) ListRepositories(ctx context.Context, userID string) ([]domain.Repository, error) {
	list, err := s.repositories.ListRepositoriesForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	for i, w := range list {
		if w.Slug != "" && w.OwnerUsername != "" {
			continue
		}
		if fixed, ferr := s.backfillRepository(ctx, w); ferr == nil {
			list[i] = fixed
		}
	}
	return list, nil
}

// backfillRepository fills in legacy records with slug/owner_username.
func (s *IdentityService) backfillRepository(ctx context.Context, w domain.Repository) (domain.Repository, error) {
	owner, err := s.repositories.GetUser(ctx, w.OwnerID)
	if err != nil {
		return w, err
	}
	if owner.Username == "" { // Heals legacy users too.
		owner.Username, err = s.uniqueUsername(ctx, owner)
		if err != nil {
			return w, err
		}
		if err := s.repositories.UpsertUser(ctx, owner); err != nil {
			return w, err
		}
	}
	if w.Slug == "" {
		w.Slug = s.uniqueSlug(ctx, w.OwnerID, w.Name, w.ID)
	}
	personalNamespace := true
	if w.OwnerNamespaceID != "" && s.organization != nil {
		if namespace, nerr := s.organization.GetNamespace(ctx, w.OwnerNamespaceID); nerr == nil && namespace.Kind == domain.NamespaceOrganization {
			personalNamespace = false
		}
	}
	if personalNamespace {
		w.OwnerUsername = owner.Username
	}
	if w.OwnerNamespaceID == "" && s.organization != nil {
		if namespace, nerr := s.ensurePersonalNamespace(ctx, owner); nerr == nil {
			w.OwnerNamespaceID = namespace.ID
		}
	}
	if err := s.repositories.CreateRepository(ctx, w); err != nil { // Upsert in FS/PG.
		return w, err
	}
	return w, nil
}

// Invite creates a repository invitation (share token) — maintainer level (5-rung ladder).
// If email=="" anyone can join, otherwise only that email can accept.
// If ttl>0 it expires after that time (AcceptInvite checks), 0 is indefinite. Negative values reject.
func (s *IdentityService) mutateInvite(ctx context.Context, userID, repositoryID, email string, role domain.MemberRole, ttl time.Duration) (domain.Invite, error) {
	actor, ok := s.RoleOf(ctx, repositoryID, userID)
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
		Token:        domain.NewID("inv_"),
		RepositoryID: repositoryID,
		Email:        strings.TrimSpace(email),
		Role:         role,
		Status:       domain.InvitePending,
		CreatedBy:    userID,
		CreatedAt:    time.Now().UTC(),
		ExpiresAt:    expires,
	}
	if err := s.repositories.CreateInvite(ctx, inv); err != nil {
		return domain.Invite{}, err
	}
	return inv, nil
}

// AcceptInvite serializes membership changes and their notification in one transaction.
func (s *IdentityService) AcceptInvite(ctx context.Context, user domain.User, token string) (out domain.Repository, err error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.Repository, error) {
		inv, err := s.repositories.GetInvite(ctx, token)
		if err != nil {
			return domain.Repository{}, err
		}
		if tx, ok := s.repositories.(outbound.RepositoryMetadataTransactions); ok {
			var result domain.Repository
			err = tx.WithinRepositoryMetadata(ctx, inv.RepositoryID, func(ctx context.Context) error { result, err = s.acceptInvite(ctx, user, token); return err })
			return result, err
		}
		return s.acceptInvite(ctx, user, token)
	})
}

// AcceptInvite joins a repository by token (member addition is idempotent — link reuse possible).
func (s *IdentityService) acceptInvite(ctx context.Context, user domain.User, token string) (domain.Repository, error) {
	inv, err := s.repositories.GetInvite(ctx, token)
	if err != nil {
		return domain.Repository{}, err // ErrNotFound
	}
	if inv.Status != domain.InvitePending {
		return domain.Repository{}, domain.ErrConflict // Revoked/expired.
	}
	if inv.ExpiresAt != nil && time.Now().After(*inv.ExpiresAt) {
		return domain.Repository{}, domain.ErrConflict
	}
	if inv.Email != "" && !strings.EqualFold(inv.Email, user.Email) {
		return domain.Repository{}, domain.ErrForbidden // Invite to specific email target
	}
	targets := []string{inv.RepositoryID}
	if bindings, ok := s.repositories.(outbound.RepositoryBindings); ok {
		targets, err = bindings.InviteTargets(ctx, token)
		if err != nil {
			return domain.Repository{}, err
		}
	}
	if len(targets) == 0 {
		return domain.Repository{}, domain.ErrIntegrity
	}
	// Validate every target before the first write (development adapters have no rollback).
	for _, id := range targets {
		if _, err := s.repositories.GetRepository(ctx, id); err != nil {
			return domain.Repository{}, err
		}
	}
	var result domain.Repository
	for _, id := range targets {
		joined, err := s.acceptRepositoryInvite(ctx, user, inv, id)
		if err != nil {
			return domain.Repository{}, err
		}
		if id == inv.RepositoryID {
			result = joined
		}
	}
	if result.ID == "" {
		return domain.Repository{}, domain.ErrIntegrity
	}
	return result, nil
}

func (s *IdentityService) acceptRepositoryInvite(ctx context.Context, user domain.User, inv domain.Invite, repositoryID string) (domain.Repository, error) {
	repository, err := s.repositories.GetRepository(ctx, repositoryID)
	if err != nil {
		return domain.Repository{}, err
	}
	members, err := s.repositories.ListMembers(ctx, repositoryID)
	if err != nil {
		return domain.Repository{}, err
	}
	var existingRole domain.MemberRole
	wasMember := false
	for _, member := range members {
		if member.UserID == user.ID {
			if !domain.ValidRole(member.Role) {
				return domain.Repository{}, domain.ErrIntegrity
			}
			existingRole, wasMember = member.Role, true
			break
		}
	}
	if repository.OwnerID == user.ID {
		existingRole, wasMember = domain.RoleOwner, true
	}
	// Reinviting an old low role invite does not downgrade the current permissions. For existing members,
	// an invite is upserted only if it explicitly promotes, and is a no-op if it is equal or lower.
	if !wasMember || !existingRole.AtLeast(inv.Role) {
		if err := s.repositories.AddMember(ctx, domain.Membership{
			RepositoryID: repositoryID,
			UserID:       user.ID,
			Role:         inv.Role,
			CreatedAt:    time.Now().UTC(),
		}); err != nil {
			return domain.Repository{}, err
		}
	}
	repositoryRecord, err := s.repositories.GetRepository(ctx, repositoryID)
	if err != nil {
		return domain.Repository{}, err
	}
	// Legacy records are immediately healed for redirect URL (/<owner_username>/<slug>) for join redirection.
	if repositoryRecord.Slug == "" || repositoryRecord.OwnerUsername == "" {
		if fixed, ferr := s.backfillRepository(ctx, repositoryRecord); ferr == nil {
			repositoryRecord = fixed
		}
	}
	// Only new joins are notified — invite links are idempotent (accept), so re-clicks by existing members are silent.
	if !wasMember {
		who := user.Username
		if who == "" {
			who = user.Email
		}
		if err := enqueueRepositoryNotification(ctx, s.repositories, repositoryRecord, "member_joined", fmt.Sprintf("cxthub: %s — %s joined as %s", repositoryRecord.Name, who, inv.Role)); err != nil {
			return domain.Repository{}, err
		}
	}
	return repositoryRecord, nil
}

// ListMembers returns the list of repository members (caller must be a member).
func (s *IdentityService) ListMembers(ctx context.Context, userID, repositoryID string) ([]domain.Membership, error) {
	_, ok := s.RoleOf(ctx, repositoryID, userID)
	if !ok {
		return nil, domain.ErrForbidden
	}
	return s.repositories.ListMembers(ctx, repositoryID)
}

// RevokeInvite invalidates the invite — requires maintainer or higher (same gate as creation). Blocks token reuse.
func (s *IdentityService) mutateRevokeInvite(ctx context.Context, userID, repositoryID, token string) error {
	inv, err := s.repositories.GetInvite(ctx, token)
	if err != nil {
		return err
	}
	if inv.RepositoryID != repositoryID {
		return domain.ErrNotFound
	}
	actor, ok := s.RoleOf(ctx, inv.RepositoryID, userID)
	if !ok || !actor.AtLeast(domain.RoleMaintainer) {
		return domain.ErrForbidden
	}
	return s.repositories.UpdateInviteStatus(ctx, token, domain.InviteRevoked)
}

// ListInvites returns the list of repository invites — requires maintainer or higher (for invite management screen).
func (s *IdentityService) ListInvites(ctx context.Context, userID, repositoryID string) ([]domain.Invite, error) {
	actor, ok := s.RoleOf(ctx, repositoryID, userID)
	if !ok || !actor.AtLeast(domain.RoleMaintainer) {
		return nil, domain.ErrForbidden
	}
	return s.repositories.ListInvites(ctx, repositoryID)
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
	if err := s.repositories.DeleteSession(ctx, domain.HashToken(sessionToken)); err != nil {
		return err
	}
	return s.repositories.DeleteSession(ctx, sessionToken) // legacy residue cleanup
}

// ResolveSession interprets a session token as a user. Expired/invalid → domain.ErrUnauthorized.
// The storage is queried by hash, and legacy plaintext records are promoted to hash records lazily (migration — existing login/CLI tokens do not break).
func (s *IdentityService) resolveSessionRecord(ctx context.Context, token string) (domain.Session, domain.User, error) {
	sess, err := s.repositories.GetSession(ctx, domain.HashToken(token))
	if err != nil {
		legacy, lerr := s.repositories.GetSession(ctx, token)
		if lerr != nil {
			return domain.Session{}, domain.User{}, domain.ErrUnauthorized
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
		if uerr := s.repositories.CreateSession(ctx, upgraded); uerr == nil {
			_ = s.repositories.DeleteSession(ctx, token)
		}
		sess = upgraded
	}
	if time.Now().After(sess.ExpiresAt) {
		_ = s.repositories.DeleteSession(ctx, sess.Token)
		return domain.Session{}, domain.User{}, domain.ErrUnauthorized
	}
	u, err := s.repositories.GetUser(ctx, sess.UserID)
	if err != nil {
		return domain.Session{}, domain.User{}, domain.ErrUnauthorized
	}
	return sess, u, nil
}

func (s *IdentityService) ResolveSession(ctx context.Context, token string) (domain.User, error) {
	sess, user, err := s.resolveSessionRecord(ctx, token)
	if err != nil || sess.Kind == "mcp_access" || sess.Kind == "mcp_refresh" {
		return domain.User{}, domain.ErrUnauthorized
	}
	return user, nil
}

// ResolveUser interprets a Bearer token as a user: "sess_" prefix indicates a server session, otherwise validates and upserts an IDP token.
// The requireUser middleware is used on all protected routes.
func (s *IdentityService) ResolveUser(ctx context.Context, bearer string) (domain.User, error) {
	if bearer == "" {
		return domain.User{}, domain.ErrUnauthorized
	}
	// OAuth refresh capabilities are accepted only by /oauth/token. Sending one
	// to an API or MCP bearer gate must not fall through to the external IDP.
	if strings.HasPrefix(bearer, "refresh_") {
		return domain.User{}, domain.ErrUnauthorized
	}
	if strings.HasPrefix(bearer, "sess_") {
		sess, user, err := s.resolveSessionRecord(ctx, bearer)
		if err != nil || sess.Kind == "mcp_access" || sess.Kind == "mcp_refresh" {
			return domain.User{}, domain.ErrUnauthorized
		}
		return user, nil
	}
	return s.Authenticate(ctx, bearer)
}

// ResolveMCPUser accepts only short-lived MCP access sessions. It deliberately
// excludes web, CLI, refresh, and raw IDP tokens so the remote connector has a
// separate authentication boundary from the mutation-capable REST API.
func (s *IdentityService) ResolveMCPUser(ctx context.Context, bearer string) (domain.User, error) {
	if !strings.HasPrefix(bearer, "sess_") {
		return domain.User{}, domain.ErrUnauthorized
	}
	sess, user, err := s.resolveSessionRecord(ctx, bearer)
	if err != nil || sess.Kind != "mcp_access" {
		return domain.User{}, domain.ErrUnauthorized
	}
	return user, nil
}
