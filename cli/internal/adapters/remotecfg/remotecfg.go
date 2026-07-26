// Package remotecfg implements git's remote concept.
//
// Registers a session repo URL (e.g., http://host:8907/acme/my-app) in the local `.cxt/config` under a name (e.g., origin), and push/pull operations are performed from that single URL:
//
//	RepoID  = sha256(normalize(url))      — Automatically assigns the same repo to team members with the same URL
//	API base = scheme://host/api/v1      — Server address (like git's remote URL, which is the destination)
//
// Once origin is registered, the GitContext decorator reanchors the repo identity based on the URL, ensuring that save/list/push/pull operations all use the same RepoID. If not registered, it delegates to the existing behavior (code repo git origin → cwd fallback).
package remotecfg

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Remotes is a mapping of name → repo URL (corresponds to git's remote list).
type Remotes map[string]string

// CheckoutAuto / CheckoutPrepare are context action modes for git branch changes.
//
//	auto    — Restores the branch context to the session immediately upon branch change (default)
//	prepare — Does not restore, only outputs "cxt checkout <branch>"
const (
	CheckoutAuto    = "auto"
	CheckoutPrepare = "prepare"
)

// fileConfig is for the .cxt/config file schema.
type fileConfig struct {
	Remotes Remotes `json:"remotes,omitempty"`
	// CheckoutMode is the context action for git checkout hooks (auto|prepare). Defaults to auto if not set.
	CheckoutMode string `json:"checkout_mode,omitempty"`
	// Staged is a list of providers to include in the next commit (corresponds to git's staging).
	Staged []string `json:"staged,omitempty"`
	// SecretsRedact is a custom masking replacement phrase (defaults to capture phrase if not set).
	SecretsRedact string `json:"secrets_redact,omitempty"`
	// SecretsMinLen is a custom minimum length for masking (0 = default 4).
	SecretsMinLen int `json:"secrets_minlen,omitempty"`
	// SecretsScrub is the pattern scrub tier (off|standard|strict, default is standard).
	SecretsScrub string `json:"secrets_scrub,omitempty"`
	// LoadMode is the default fidelity for load/checkout/fork (full|reconstructed|memory).
	// If none is specified, the full series is the default (--mode flag always takes precedence). Repositories that want to inject only compressed memory can be fixed as memory.
	LoadMode string `json:"load_mode,omitempty"`
	// BoundaryEnforce is the isolation session process termination policy on context switch (kill|none).
	// Default is kill — intentional break, checkpoint precedes so no loss.
	BoundaryEnforce string `json:"boundary_enforce,omitempty"`
	// CaptureDebounceSec is the debounce window (seconds) for hook Stop capture (capture path).
	// 0 (default) = 60 seconds. Negative values not allowed.
	CaptureDebounceSec int `json:"capture_debounce_sec,omitempty"`
}

func configPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".cxt", "config")
}

// loadFile reads the entire .cxt/config. Returns zero value if file does not exist (no error).
func loadFile(repoRoot string) (fileConfig, error) {
	var fc fileConfig
	b, err := providerfs.ReadRepoFile(repoRoot, filepath.Join(".cxt", "config"))
	if os.IsNotExist(err) {
		return fc, nil
	}
	if err != nil {
		return fc, err
	}
	if err := json.Unmarshal(b, &fc); err != nil {
		return fc, fmt.Errorf(".cxt/config parsing failed: %w", err)
	}
	return fc, nil
}

// saveFile writes the entire .cxt/config (.cxt directory is created if it does not exist).
func saveFile(repoRoot string, fc fileConfig) error {
	b, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return err
	}
	return providerfs.WriteRepoFileAtomic(repoRoot, filepath.Join(".cxt", "config"), append(b, '\n'), 0o644)
}

// Load reads the remote list. Returns an empty map if file does not exist (no error).
func Load(repoRoot string) (Remotes, error) {
	fc, err := loadFile(repoRoot)
	if err != nil {
		return nil, err
	}
	if fc.Remotes == nil {
		fc.Remotes = Remotes{}
	}
	return canonicalRemotes(fc.Remotes)
}

// Save records the remote list. Other settings fields (checkout_mode, etc.) are preserved.
func Save(repoRoot string, r Remotes) error {
	fc, err := loadFile(repoRoot)
	if err != nil {
		return err
	}
	fc.Remotes, err = canonicalRemotes(r)
	if err != nil {
		return err
	}
	return saveFile(repoRoot, fc)
}

// CheckoutMode returns the context action mode of the git checkout hook (default is auto).
func CheckoutMode(repoRoot string) string {
	fc, err := loadFile(repoRoot)
	if err != nil || fc.CheckoutMode == "" {
		return CheckoutAuto
	}
	return fc.CheckoutMode
}

// SecretsRedact returns the masking replacement phrase ("" = use default phrase).
func SecretsRedact(repoRoot string) string {
	fc, err := loadFile(repoRoot)
	if err != nil {
		return ""
	}
	return fc.SecretsRedact
}

// SecretsMinLen returns the masking minimum length (0 = default 4).
func SecretsMinLen(repoRoot string) int {
	fc, err := loadFile(repoRoot)
	if err != nil {
		return 0
	}
	return fc.SecretsMinLen
}

// SetSecretsRedact sets the masking replacement phrase (empty string = revert to default).
func SetSecretsRedact(repoRoot, phrase string) error {
	fc, err := loadFile(repoRoot)
	if err != nil {
		return err
	}
	fc.SecretsRedact = phrase
	return saveFile(repoRoot, fc)
}

// SetSecretsMinLen sets the masking minimum length (1~64, 0 = revert to default 4).
func SetSecretsMinLen(repoRoot string, n int) error {
	if n < 0 || n > 64 {
		return fmt.Errorf("secrets.minlen must be 0(default)~64: %d", n)
	}
	fc, err := loadFile(repoRoot)
	if err != nil {
		return err
	}
	fc.SecretsMinLen = n
	return saveFile(repoRoot, fc)
}

// SecretsScrub returns the pattern scrub tier (empty if none).
func SecretsScrub(repoRoot string) string {
	fc, err := loadFile(repoRoot)
	if err != nil {
		return ""
	}
	return fc.SecretsScrub
}

// SetSecretsScrub sets the pattern scrub tier (off|standard|strict, "" = default standard).
func SetSecretsScrub(repoRoot, tier string) error {
	switch tier {
	case "", "off", "standard", "strict":
	default:
		return fmt.Errorf("secrets.scrub must be off|standard|strict: %q", tier)
	}
	fc, err := loadFile(repoRoot)
	if err != nil {
		return err
	}
	fc.SecretsScrub = tier
	return saveFile(repoRoot, fc)
}

// SetCheckoutMode sets the checkout mode (auto|prepare).
func SetCheckoutMode(repoRoot, mode string) error {
	if mode != CheckoutAuto && mode != CheckoutPrepare {
		return fmt.Errorf("checkout.mode must be %q or %q: %q", CheckoutAuto, CheckoutPrepare, mode)
	}
	fc, err := loadFile(repoRoot)
	if err != nil {
		return err
	}
	fc.CheckoutMode = mode
	return saveFile(repoRoot, fc)
}

// LoadMode returns the default fidelity for load/checkout/fork ("" = full series default).
func LoadMode(repoRoot string) string {
	fc, err := loadFile(repoRoot)
	if err != nil {
		return ""
	}
	return fc.LoadMode
}

// SetLoadMode sets the default fidelity (full|reconstructed|memory, "default" → unset).
func SetLoadMode(repoRoot, mode string) error {
	if mode != "" && mode != "full" && mode != "reconstructed" && mode != "memory" {
		return fmt.Errorf("load.mode must be full|reconstructed|memory: %q", mode)
	}
	fc, err := loadFile(repoRoot)
	if err != nil {
		return err
	}
	fc.LoadMode = mode
	return saveFile(repoRoot, fc)
}

// CaptureDebounce returns the debounce window for hook Stop capture (default 60s, capture path).
func CaptureDebounce(repoRoot string) time.Duration {
	fc, err := loadFile(repoRoot)
	if err != nil || fc.CaptureDebounceSec <= 0 {
		return 60 * time.Second
	}
	return time.Duration(fc.CaptureDebounceSec) * time.Second
}

// SetCaptureDebounce sets the debounce window (seconds) (0 → default 60 seconds off).
func SetCaptureDebounce(repoRoot string, sec int) error {
	if sec < 0 {
		return fmt.Errorf("capture.debounce must be 0 or more seconds: %d", sec)
	}
	fc, err := loadFile(repoRoot)
	if err != nil {
		return err
	}
	fc.CaptureDebounceSec = sec
	return saveFile(repoRoot, fc)
}

// BoundaryEnforce returns the transition execution policy (default kill).
func BoundaryEnforce(repoRoot string) string {
	fc, err := loadFile(repoRoot)
	if err != nil || fc.BoundaryEnforce == "" {
		return "kill"
	}
	return fc.BoundaryEnforce
}

// SetBoundaryEnforce sets the transition execution policy (kill|none, "default" → off).
func SetBoundaryEnforce(repoRoot, v string) error {
	if v != "" && v != "kill" && v != "none" {
		return fmt.Errorf("boundary.enforce must be kill|none: %q", v)
	}
	fc, err := loadFile(repoRoot)
	if err != nil {
		return err
	}
	fc.BoundaryEnforce = v
	return saveFile(repoRoot, fc)
}

// StagedProviders returns the list of staged providers added via cxt add.
func StagedProviders(repoRoot string) []string {
	fc, err := loadFile(repoRoot)
	if err != nil {
		return nil
	}
	return fc.Staged
}

// SetStagedProviders sets the staging list (empty slice = staging off).
func SetStagedProviders(repoRoot string, providers []string) error {
	fc, err := loadFile(repoRoot)
	if err != nil {
		return err
	}
	fc.Staged = providers
	return saveFile(repoRoot, fc)
}

// Origin returns the origin remote URL. Returns ("", false) if not set.
func Origin(repoRoot string) (string, bool) {
	r, err := Load(repoRoot)
	if err != nil {
		return "", false
	}
	u, ok := r["origin"]
	return u, ok && u != ""
}

// Validate checks the repo URL format.
//
// Workspace URL is the repo identity: <host>/<username>/<workspace> (2-segment).
// Simply paste the workspace URL from the browser (e.g., cxthub.com/alice/backend).
// Currently, the policy is one repo per workspace — to extend to multi-repos, allow 3-segment (<…>/<repo>), and keep the RepoID=hash(URL) rule.
func Validate(rawURL string) error {
	_, err := CanonicalURL(rawURL)
	return err
}

// CanonicalURL validates and canonicalizes a workspace remote. Credentials,
// query parameters and fragments are never part of repo identity and must not
// be persisted in the repository config.
func CanonicalURL(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Opaque != "" {
		return "", fmt.Errorf("Cannot parse workspace URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("Workspace URL must be an absolute http(s) URL")
	}
	if u.User != nil {
		return "", fmt.Errorf("Workspace URL cannot contain username or password")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("Workspace URL cannot contain query or fragment")
	}
	pathValue := strings.TrimSuffix(u.Path, "/")
	if !strings.HasPrefix(pathValue, "/") {
		return "", fmt.Errorf("workspace URL must be in the format /<username>/<workspace>")
	}
	segments := strings.Split(strings.TrimPrefix(pathValue, "/"), "/")
	if len(segments) != 2 || !validWorkspaceSegment(segments[0]) || !validWorkspaceSegment(segments[1]) {
		return "", fmt.Errorf("Workspace URL must be in the format /<username>/<workspace>")
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = "/" + strings.Join(segments, "/")
	u.RawPath = ""
	return u.String(), nil
}

func validWorkspaceSegment(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func validRemoteName(value string) bool {
	if value == "" || len(value) > 64 || value[0] == '-' {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func canonicalRemotes(remotes Remotes) (Remotes, error) {
	out := make(Remotes, len(remotes))
	for name, rawURL := range remotes {
		if !validRemoteName(name) {
			return nil, fmt.Errorf("Invalid remote name")
		}
		canonical, err := CanonicalURL(rawURL)
		if err != nil {
			return nil, fmt.Errorf("remote %q: %w", name, err)
		}
		out[name] = canonical
	}
	return out, nil
}

// RepoIDFor returns the normalized hash of the repo URL = RepoID.
// Uses the same normalization (gitctx.NormalizeRemoteURL) as the git origin URL derivation.
func RepoIDFor(rawURL string) domain.ContentHash {
	return domain.HashContent([]byte(gitctx.NormalizeRemoteURL(rawURL)))
}

// APIBase derives the server REST base (scheme://host/api/v1) from the repo URL.
func APIBase(rawURL string) (string, error) {
	canonical, err := CanonicalURL(rawURL)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(canonical)
	if err != nil {
		return "", err
	}
	return u.Scheme + "://" + u.Host + "/api/v1", nil
}

// GitContextWithRemote is a outbound.GitContext decorator.
// If the origin remote is registered, it reanchors the repo integrity to that URL,
// otherwise, it delegates to the internal GitContext (code repo origin/branch fallback).
type GitContextWithRemote struct {
	repoRoot string
	inner    outbound.GitContext
}

// Wrap creates a GitContextWithRemote.
func Wrap(repoRoot string, inner outbound.GitContext) *GitContextWithRemote {
	return &GitContextWithRemote{repoRoot: repoRoot, inner: inner}
}

// CurrentRepo returns the URL-derived integrity (RepoID/RemoteURL) if origin exists.
// Branches and local paths use the raw results from the internal adapter.
func (g *GitContextWithRemote) CurrentRepo(ctx context.Context, cwd string) (domain.Repo, error) {
	repo, err := g.inner.CurrentRepo(ctx, cwd)
	if err != nil {
		return domain.Repo{}, err
	}
	remotes, err := Load(g.repoRoot)
	if err != nil {
		return domain.Repo{}, err
	}
	if origin, ok := remotes["origin"]; ok && origin != "" {
		repo.ID = RepoIDFor(origin)
		repo.RemoteURL = origin
	}
	return repo, nil
}

// CurrentBranch delegates to the internal adapter.
func (g *GitContextWithRemote) CurrentBranch(ctx context.Context, cwd string) (string, error) {
	return g.inner.CurrentBranch(ctx, cwd)
}

// Ensure interface implementation.
var _ outbound.GitContext = (*GitContextWithRemote)(nil)
