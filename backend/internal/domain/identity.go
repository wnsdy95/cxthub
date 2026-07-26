package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// User · Workspace · Membership · Invite Domain (Auth/Multi-tenancy). schemas/db/migrations/0002.
// Visibility boundary = Workspace. repo belongs to exactly one workspace.

// MemberRole represents a role within a workspace (distinct from Role in cir.go).
//
// 5-tier ladder (GitLab equivalent) — Role is a "layer boundary" (serial AND gate front), and
// policy-specific actions (SecretsPolicy, etc.) narrow it further behind (separation of truth).
//
//	viewer     read-only (web context·graph·member list)
//	puller     + local synchronization of team assets (settings/secrets pull, session object pull)
//	member     + context write (push/ref/memory — "git write follows")
//	maintainer + write to team shared assets (.cxtsecrets·team settings·About·invite creation)
//	owner      + management (workspace settings·member management·transfer ownership to creator)
type MemberRole string

const (
	RoleViewer     MemberRole = "viewer"
	RolePuller     MemberRole = "puller"
	RoleMember     MemberRole = "member"
	RoleMaintainer MemberRole = "maintainer"
	RoleOwner      MemberRole = "owner"
)

// RoleRank is the role rank (higher value indicates greater permissions). Unknown values default to 0 — conservative rejection.
func RoleRank(r MemberRole) int {
	switch r {
	case RoleViewer:
		return 1
	case RolePuller:
		return 2
	case RoleMember:
		return 3
	case RoleMaintainer:
		return 4
	case RoleOwner:
		return 5
	default:
		return 0
	}
}

// AtLeast checks if r is at least the min permission level.
func (r MemberRole) AtLeast(min MemberRole) bool { return RoleRank(r) >= RoleRank(min) }

// ValidRole validates if the role value is within the 5-tier ladder.
func ValidRole(r MemberRole) bool { return RoleRank(r) > 0 }

// InviteStatus represents the invite status.
type InviteStatus string

const (
	InvitePending  InviteStatus = "pending"
	InviteAccepted InviteStatus = "accepted"
	InviteRevoked  InviteStatus = "revoked"
)

// ValidInviteStatus reports whether an invite status may be persisted. Unknown values must be
// rejected before they participate in authorization decisions.
func ValidInviteStatus(status InviteStatus) bool {
	switch status {
	case InvitePending, InviteAccepted, InviteRevoked:
		return true
	default:
		return false
	}
}

// User is authenticated via Firebase (id = Firebase uid, dev mode is "dev:<email>").
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	// Username is a global unique handle (slug) — the first segment of the URL path (/<username>/<workspace>).
	// Automatically generated on first login from email localpart (with -2, -3, ... on collision).
	// Changing it affects URL, CLI remote (rare change — see UpdateProfile).
	Username string `json:"username"`
	// Nickname is a display alias — unrelated to URL, can be freely changed at any time (light change).
	Nickname string `json:"nickname,omitempty"`
	// LoadMode is the account-wide personal setting for context load default fidelity
	// (full|reconstructed|memory, "" = unset). CLI consumes this at the consumption point
	// and applies it to the priority (flag > local config > this value > full).
	LoadMode string `json:"load_mode,omitempty"`
	// Avatar is the profile picture — a resized data URL (data:image/…;base64,…).
	// Public info (visible to anyone from the profile). "" = unset (fallback to initials).
	Avatar string `json:"avatar,omitempty"`
	// Locale is the UI display language personal setting (ko|en, "" = unset → client detects browser).
	// Stored in the web switcher, applied by the client on login to ensure consistency across devices.
	Locale    string    `json:"locale,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Visibility is the workspace public scope. Default is private (empty value is also interpreted as private).
type Visibility string

const (
	VisibilityPrivate Visibility = "private" // Only members can view (default)
	VisibilityPublic  Visibility = "public"  // Anyone can view (GitHub public repo equivalent)
)

// Workspace is a multi-tenancy/visibility boundary.
type Workspace struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	OwnerID string `json:"owner_id"`
	// Slug is a unique URL segment within the owner (GitHub repo name position):
	// /<owner_username>/<slug>. Automatically generated from the name (conflicts resolved with -2, -3, ...).
	Slug string `json:"slug"`
	// OwnerUsername is the unnormalized owner handle (URL composition — avoids user joins on each query).
	OwnerUsername string `json:"owner_username"`
	// Visibility is the public scope ("" == private). Changes are only possible by the owner.
	Visibility Visibility `json:"visibility,omitempty"`
	// PublicRole is the default role granted to non-members (including anonymous users) in a public workspace:
	// ""(=viewer, default) | "viewer" | "puller". viewer=web view only, puller=+local sync (pull).
	// In a private workspace, it has no meaning (non-members cannot access). Roles above member are not assignable
	// (non-members cannot write — the upper limit is puller).
	PublicRole string `json:"public_role,omitempty"`
	// SecretsPolicy is the upload permission for .cxtsecrets encrypted text:
	// ""|"members"(role-based = maintainer and above, default) | "owner"(only owner).
	// User-level segmentation is handled by a 5-tier role ladder (specified lists are removed upon role introduction).
	SecretsPolicy string `json:"secrets_policy,omitempty"`
	// SettingsPolicy is the upload permission for team default settings (.claude/.agents): the value meaning is the same as SecretsPolicy.
	SettingsPolicy string `json:"settings_policy,omitempty"`
	// GHVisibilitySync, when enabled, derives the visibility from the GitHub repo status (manual toggle lock).
	// Rule: Public if the linked GitHub repo has 1 or more and all are public (conservative —
	// if any are private or inaccessible, it becomes private; if there are no GitHub repos, it remains private).
	GHVisibilitySync bool `json:"gh_visibility_sync,omitempty"`
	// GHSyncedAt is the last synchronization timestamp.
	GHSyncedAt *time.Time `json:"gh_synced_at,omitempty"`
	// Archived makes the workspace read-only. Because P1 never deletes history, archival is the terminal state.
	// Every operation above viewer returns 403; the owner can unarchive the workspace in settings.
	Archived bool `json:"archived,omitempty"`
	// WebhookURL is an alert webhook (Slack incoming webhook compatible — {"text": ...} POST). Asynchronous invocation on ref update (push/branch creation), failures are ignored (best-effort notifications).
	WebhookURL string    `json:"webhook_url,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// PolicyAllows determines if the policy allows the user. "owner" is always allowed. Unknown policy values (legacy "list" included) are conservatively rejected.
func PolicyAllows(policy string, isOwner bool) bool {
	if isOwner {
		return true
	}
	switch policy {
	case "", "members":
		return true
	default: // "owner" legacy value
		return false
	}
}

// IsPublic indicates whether the workspace is public (empty value = private by default).
func (w Workspace) IsPublic() bool { return w.Visibility == VisibilityPublic }

// PublicBaseRole returns the role to assign to non-members in a public workspace (default viewer). Only "puller" is promoted, other/unknown values are conservatively assigned as viewer (fail-closed).
func (w Workspace) PublicBaseRole() MemberRole {
	if w.PublicRole == string(RolePuller) {
		return RolePuller
	}
	return RoleViewer
}

// ValidPublicRole checks if the value is a valid public base role ("" | viewer | puller).
func ValidPublicRole(r string) bool {
	return r == "" || r == string(RoleViewer) || r == string(RolePuller)
}

// Workspace name rules (enforced in English): Only allowed characters are letters, digits, '-', '_'.
// The first character must be a letter, and the last character must be a letter or digit. Maximum 64 characters.
var workspaceNameRe = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_-]*[A-Za-z0-9])?$`)

// ValidWorkspaceName validates workspace names according to the rules.
func ValidWorkspaceName(name string) bool {
	return len(name) <= 64 && workspaceNameRe.MatchString(name)
}

// WorkspaceSlug creates a slug from a valid workspace name (lowercased — '_' preserved).
// Legacy (pre-rule) names are fallback to Slugify.
func WorkspaceSlug(name string) string {
	if ValidWorkspaceName(name) {
		return strings.ToLower(name)
	}
	return Slugify(name, "workspace")
}

// ValidWorkspaceSlug validates slugs according to the name rules (lowercased).
func ValidWorkspaceSlug(slug string) bool {
	return ValidWorkspaceName(slug) && slug == strings.ToLower(slug)
}

// Slugify creates a URL-safe slug: lowercased, non-unicode characters/numbers replaced with '-', consecutive/leading/trailing '-' cleaned up.
// Non-ASCII characters like Korean are preserved (URL paths are safe with percent-encoding).
// Returns fallback if result is empty.
func Slugify(s, fallback string) string {
	var b []rune
	prevDash := true // Prevent leading '-'
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			b = append(b, unicode.ToLower(r))
			prevDash = false
		default:
			if !prevDash {
				b = append(b, '-')
				prevDash = true
			}
		}
	}
	for len(b) > 0 && b[len(b)-1] == '-' {
		b = b[:len(b)-1]
	}
	if len(b) == 0 {
		return fallback
	}
	return string(b)
}

// Membership represents the user ↔ workspace relationship.
type Membership struct {
	WorkspaceID string     `json:"workspace_id"`
	UserID      string     `json:"user_id"`
	Role        MemberRole `json:"role"`
	CreatedAt   time.Time  `json:"created_at"`
	// User is a denormalized field for convenience (included in list responses, not stored).
	User *User `json:"user,omitempty"`
}

// Invite is a share invitation token (join using token).
type Invite struct {
	Token       string       `json:"token"`
	WorkspaceID string       `json:"workspace_id"`
	Email       string       `json:"email"`
	Role        MemberRole   `json:"role"`
	Status      InviteStatus `json:"status"`
	CreatedBy   string       `json:"created_by"`
	CreatedAt   time.Time    `json:"created_at"`
	ExpiresAt   *time.Time   `json:"expires_at,omitempty"`
}

// Session is a server login session (issued via IDP token exchange, stored in DB).
//
// at-rest hashing: The Token field contains a HashToken (original) rather than the original —
// if the DB is leaked, no login materials will be exposed. The original exists only in the issuance response/cookie.
type Session struct {
	Token  string `json:"token"`
	UserID string `json:"user_id"`
	// Hint is the last 8 characters of the original token (for list display, discard identification — the original is not stored).
	Hint string `json:"hint,omitempty"`
	// Kind is the session type: "web" | "cli" (hash storage makes prefix differentiation impossible, so it is explicitly specified).
	Kind string `json:"kind,omitempty"`
	// Label is the device display name (which device session it is — CLI is hostname, web is User-Agent summary). It is for display and identification only and is not used for access control.
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// HashToken is the at-rest hash of the session token (sha256, "tkh_" prefix — to distinguish from plain text records).
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return "tkh_" + hex.EncodeToString(sum[:])
}

// TokenHint is the display hint (last 8 characters) of the original token.
func TokenHint(raw string) string {
	if len(raw) < 8 {
		return raw
	}
	return raw[len(raw)-8:]
}

// NewID generates a prefix + random hex (16 bytes=128 bits) identifier (e.g., "ws_<hex>", "inv_<hex>", "sess_<hex>"). It panics on crypto/rand failure — allowing predictable fallbacks would make session, CLI, and invite tokens guessable and collide (security review). rand.Read does not fail on normal OSes, so this panic is actually unreachable.
func NewID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand failure — cannot generate safe token: " + err.Error())
	}
	return prefix + hex.EncodeToString(b[:])
}

func validatePrefixedHexID(value, prefix string, hexLen int) error {
	if len(value) != len(prefix)+hexLen || !strings.HasPrefix(value, prefix) {
		return fmt.Errorf("%w: invalid %s identifier", ErrValidation, strings.TrimSuffix(prefix, "_"))
	}
	for _, r := range value[len(prefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("%w: invalid %s identifier", ErrValidation, strings.TrimSuffix(prefix, "_"))
		}
	}
	return nil
}

// ValidateWorkspaceID validates the workspace identifier issued by the server.
func ValidateWorkspaceID(id string) error { return validatePrefixedHexID(id, "ws_", 32) }

// ValidateInviteToken validates the invite capability token issued by the server.
func ValidateInviteToken(token string) error { return validatePrefixedHexID(token, "inv_", 32) }

// ValidateStoredSessionToken validates the session token used with the store key. New records are
// in the format tkh_<sha256>, and pre-migration plain text sess_/sess_cli_ records are read-only.
func ValidateStoredSessionToken(token string) error {
	switch {
	case strings.HasPrefix(token, "tkh_"):
		return validatePrefixedHexID(token, "tkh_", 64)
	case strings.HasPrefix(token, "sess_cli_"):
		return validatePrefixedHexID(token, "sess_cli_", 32)
	default:
		return validatePrefixedHexID(token, "sess_", 32)
	}
}

// ValidateExternalID enforces the minimal contract for external identifiers that do not own the format.
// Paths use hashes instead of the original text, but empty or overly long values containing control characters are rejected.
func ValidateExternalID(id string) error {
	if id == "" || len(id) > 512 {
		return fmt.Errorf("%w: invalid external identifier", ErrValidation)
	}
	for _, r := range id {
		if r <= 0x1f || r == 0x7f {
			return fmt.Errorf("%w: invalid external identifier", ErrValidation)
		}
	}
	return nil
}

// ValidateUserRecord validates the user record's permission identifier at the storage boundary.
func ValidateUserRecord(user User) error {
	if err := ValidateExternalID(user.ID); err != nil {
		return err
	}
	return ValidateAvatarDataURL(user.Avatar)
}

// ValidateAvatarDataURL allows only raster image data URLs. SVG and arbitrary
// data payloads are excluded so a poisoned profile record cannot become an
// active-content or remote-tracking URL in an image element.
func ValidateAvatarDataURL(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 700_000 {
		return fmt.Errorf("%w: avatar is too large", ErrValidation)
	}
	comma := strings.IndexByte(value, ',')
	if comma < 0 {
		return fmt.Errorf("%w: invalid avatar data URL", ErrValidation)
	}
	switch value[:comma+1] {
	case "data:image/jpeg;base64,", "data:image/jpg;base64,", "data:image/png;base64,", "data:image/webp;base64,", "data:image/gif;base64,":
	default:
		return fmt.Errorf("%w: unsupported avatar format", ErrValidation)
	}
	payload, err := base64.StdEncoding.DecodeString(value[comma+1:])
	if err != nil || len(payload) == 0 || len(payload) > 512*1024 {
		return fmt.Errorf("%w: invalid avatar payload", ErrValidation)
	}
	return nil
}

// ValidateWorkspaceRecord validates the tenant identifier and fail-closed policy value of the workspace at the storage boundary. Legacy display formats for name/slug are allowed.
func ValidateWorkspaceRecord(ws Workspace) error {
	if err := ValidateWorkspaceID(ws.ID); err != nil {
		return err
	}
	if err := ValidateExternalID(ws.OwnerID); err != nil {
		return err
	}
	if ws.Visibility != "" && ws.Visibility != VisibilityPrivate && ws.Visibility != VisibilityPublic {
		return fmt.Errorf("%w: invalid workspace visibility", ErrValidation)
	}
	if !ValidPublicRole(ws.PublicRole) {
		return fmt.Errorf("%w: invalid workspace public role", ErrValidation)
	}
	validPolicy := func(policy string) bool {
		return policy == "" || policy == "members" || policy == "owner"
	}
	if !validPolicy(ws.SecretsPolicy) || !validPolicy(ws.SettingsPolicy) {
		return fmt.Errorf("%w: invalid workspace policy", ErrValidation)
	}
	return nil
}

// ValidateMembershipRecord validates the identifiers and role of the membership at the storage boundary.
func ValidateMembershipRecord(m Membership) error {
	if err := ValidateWorkspaceID(m.WorkspaceID); err != nil {
		return err
	}
	if err := ValidateExternalID(m.UserID); err != nil {
		return err
	}
	if !ValidRole(m.Role) {
		return fmt.Errorf("%w: invalid membership role", ErrValidation)
	}
	return nil
}

// ValidateInviteRecord checks a capability token, tenant, issuer, role, and status in one go.
// Email is an optional display/matching value and is not included as an identifier for this contract.
func ValidateInviteRecord(inv Invite) error {
	if err := ValidateInviteToken(inv.Token); err != nil {
		return err
	}
	if err := ValidateWorkspaceID(inv.WorkspaceID); err != nil {
		return err
	}
	if err := ValidateExternalID(inv.CreatedBy); err != nil {
		return err
	}
	if !ValidRole(inv.Role) {
		return fmt.Errorf("%w: invalid invite role", ErrValidation)
	}
	if !ValidInviteStatus(inv.Status) {
		return fmt.Errorf("%w: invalid invite status", ErrValidation)
	}
	return nil
}

// ValidateSessionRecord checks a stored session capability and user identifier.
func ValidateSessionRecord(sess Session) error {
	if err := ValidateStoredSessionToken(sess.Token); err != nil {
		return err
	}
	return ValidateExternalID(sess.UserID)
}
