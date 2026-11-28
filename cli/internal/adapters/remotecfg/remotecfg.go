// Package remotecfg implements git's remote concept.
//
// Registers a session repo URL (e.g., http://host:8907/acme/my-app) in the local `.cxt/config` under a name (e.g., origin); sessions are uploaded to and read from that single URL:
//
//	RepoID  = sha256(normalize(url))      — Automatically assigns the same repo to team members with the same URL
//	API base = scheme://host/api/v1      — Server address (like git's remote URL, which is the destination)
//
// Once origin is registered, the GitContext decorator reanchors the repo identity based on the URL, so every upload and read uses the same RepoID. If not registered, it delegates to the existing behavior (code repo git origin → cwd fallback).
package remotecfg

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// Remotes is a mapping of name → repo URL (corresponds to git's remote list).
type Remotes map[string]string

// fileConfig is for the .cxt/config file schema. Keys it does not name, such
// as those older versions wrote, are kept as they are by every mutation.
type fileConfig struct {
	// TransportEndpoints and CaptureDocument are read from the raw field map;
	// naming them here makes every read of the config check their JSON types.
	TransportEndpoints map[string]EndpointBinding `json:"transport_endpoints,omitempty"`
	Remotes            Remotes                    `json:"remotes,omitempty"`
	CaptureDocument    *captureDocumentBinding    `json:"capture_document,omitempty"`
	// SecretsRedact is a custom masking replacement phrase (defaults to capture phrase if not set).
	SecretsRedact string `json:"secrets_redact,omitempty"`
	// SecretsMinLen is a custom minimum length for masking (0 = default 4).
	SecretsMinLen int `json:"secrets_minlen,omitempty"`
	// SecretsScrub is the pattern scrub tier (off|standard|strict, default is standard).
	SecretsScrub string `json:"secrets_scrub,omitempty"`
	// SegmentsAuto is "off" when session reports are uploaded only on request.
	// Absent (default) = hooks wake the collector.
	SegmentsAuto string `json:"segments_auto,omitempty"`
}

// sharedConfigRoot makes remotes and capture policy repository-wide even when
// a command still carries the cwd of a linked desktop-app worktree.
func sharedConfigRoot(repoRoot string) string {
	if roots, err := gitctx.ResolveRepositoryRoots(context.Background(), repoRoot); err == nil {
		return roots.SharedRoot
	}
	return repoRoot
}

// loadFile reads the entire .cxt/config. Returns zero value if file does not exist (no error).
func loadFile(repoRoot string) (fileConfig, error) {
	return loadFileAtRoot(sharedConfigRoot(repoRoot))
}

func loadFileAtRoot(repoRoot string) (fileConfig, error) {
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
func SetSecretsRedact(ctx context.Context, repoRoot, phrase string) error {
	return setField(ctx, repoRoot, "secrets_redact", phrase)
}

// SetSecretsMinLen sets the masking minimum length (1~64, 0 = revert to default 4).
func SetSecretsMinLen(ctx context.Context, repoRoot string, n int) error {
	if n < 0 || n > 64 {
		return fmt.Errorf("secrets.minlen must be 0(default)~64: %d", n)
	}
	return setField(ctx, repoRoot, "secrets_minlen", n)
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
func SetSecretsScrub(ctx context.Context, repoRoot, tier string) error {
	switch tier {
	case "", "off", "standard", "strict":
	default:
		return fmt.Errorf("secrets.scrub must be off|standard|strict: %q", tier)
	}
	return setField(ctx, repoRoot, "secrets_scrub", tier)
}

// SegmentsAuto reports whether hooks wake the session report collector
// (default true). A repository whose settings cannot be read keeps the default.
func SegmentsAuto(repoRoot string) bool {
	fc, err := loadFile(repoRoot)
	return err != nil || fc.SegmentsAuto != "off"
}

// SetSegmentsAuto turns automatic session report upload on or off ("" = default on).
func SetSegmentsAuto(ctx context.Context, repoRoot, value string) error {
	switch value {
	case "", "on":
		value = ""
	case "off":
	default:
		return fmt.Errorf("segments.auto must be on|off: %q", value)
	}
	return setField(ctx, repoRoot, "segments_auto", value)
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

// CanonicalURL validates and canonicalizes a repository remote. The repository
// URL is the repo identity: <host>/<namespace>/<repository>/<repository>.
// Existing two-segment URLs remain valid because RepoID=hash(URL), and
// rewriting one would change the repository its sessions belong to.
// Credentials, query parameters and fragments are never part of repo identity
// and must not be persisted in the repository config.
func CanonicalURL(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Opaque != "" {
		return "", fmt.Errorf("Cannot parse repository URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("Repository URL must be an absolute http(s) URL")
	}
	if u.User != nil {
		return "", fmt.Errorf("Repository URL cannot contain username or password")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("Repository URL cannot contain query or fragment")
	}
	pathValue := strings.TrimSuffix(u.Path, "/")
	if !strings.HasPrefix(pathValue, "/") {
		return "", fmt.Errorf("Repository URL must be /<namespace>/<repository>/<repository> (or a legacy /<namespace>/<repository> URL)")
	}
	segments := strings.Split(strings.TrimPrefix(pathValue, "/"), "/")
	if (len(segments) != 2 && len(segments) != 3) || !validRepositorySegments(segments) {
		return "", fmt.Errorf("Repository URL must be /<namespace>/<repository>/<repository> (or a legacy /<namespace>/<repository> URL)")
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = "/" + strings.Join(segments, "/")
	u.RawPath = ""
	return u.String(), nil
}

func validRepositorySegments(segments []string) bool {
	for _, segment := range segments {
		if !validRepositorySegment(segment) {
			return false
		}
	}
	return true
}

func validRepositorySegment(value string) bool {
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

// Ensure interface implementation.
var _ outbound.GitContext = (*GitContextWithRemote)(nil)
