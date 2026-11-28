package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Service implements all server inbound use-cases and HTTP read-through.
type Service struct {
	rootPublication bool // configured before serving; cannot override release readiness
	docVerifier     domain.CanonicalDocVerifier
	meta            outbound.MetadataStore
	blobs           outbound.BlobStore
	// repositories resolves bindings and current write authority. A missing adapter
	// denies user writes; explicit trusted system jobs may operate without one.
	repositories outbound.RepositoryStore
	// documentJobStored wakes this process's document worker when a job it
	// stored is committed; nil where no worker runs. Configured before serving.
	documentJobStored func()
}

// NewService creates a Service with an outbound port injected.
func NewService(meta outbound.MetadataStore, blobs outbound.BlobStore, repositories outbound.RepositoryStore) *Service {
	return &Service{meta: meta, blobs: blobs, repositories: repositories}
}

// --- Helper ---

func difference(all, have []domain.ContentHash) []domain.ContentHash {
	haveSet := make(map[domain.ContentHash]bool, len(have))
	for _, h := range have {
		haveSet[h] = true
	}
	var out []domain.ContentHash
	for _, h := range all {
		if !haveSet[h] {
			out = append(out, h)
		}
	}
	return out
}

func validateHashes(hashes ...domain.ContentHash) error {
	for _, h := range hashes {
		if err := domain.ValidateContentHash(h); err != nil {
			return err
		}
	}
	return nil
}

// --- push use-cases ---

// Negotiate: want (server missing parts) = client haves \ server has (sync protocol step A).
func (s *Service) Negotiate(ctx context.Context, in inbound.PushNegotiateInput) (inbound.PushNegotiateOutput, error) {
	return repositoryReadForRepo(ctx, s, in.RepoID, func(ctx context.Context) (inbound.PushNegotiateOutput, error) {
		if err := domain.ValidateContentHash(in.RepoID); err != nil {
			return inbound.PushNegotiateOutput{}, err
		}
		if err := validateHashes(in.DocHaves...); err != nil {
			return inbound.PushNegotiateOutput{}, err
		}
		haveDocs, err := s.blobs.HasDocs(ctx, in.RepoID, in.DocHaves)
		if err != nil {
			return inbound.PushNegotiateOutput{}, err
		}
		if err := validateHashes(in.ChunkHaves...); err != nil {
			return inbound.PushNegotiateOutput{}, err
		}
		var chunkWants []domain.ContentHash
		if len(in.ChunkHaves) > 0 {
			haveChunks, cerr := s.blobs.HasChunks(ctx, in.RepoID, in.ChunkHaves)
			if cerr != nil {
				return inbound.PushNegotiateOutput{}, cerr
			}
			chunkWants = difference(in.ChunkHaves, haveChunks)
		}
		_, asyncDocs := s.blobs.(outbound.DocJobStore)
		_, storedDocVerification := s.blobs.(outbound.StoredDocVerifier)
		return inbound.PushNegotiateOutput{
			DocIdentitiesSupported:          s.DocumentIdentitiesSupported(),
			RootPublicationEnabled:          s.RootPublicationEnabled(),
			PreparedMemoryArchivesSupported: asyncDocs && storedDocVerification,
			AsyncDocsSupported:              asyncDocs,
			DocWants:                        difference(in.DocHaves, haveDocs),
			ChunksSupported:                 true,
			BoundedChunksSupported:          true,
			ChunkFormatsSupported:           []string{domain.ChunkFormatV1, domain.ChunkFormatV2},
			CIRVersionsSupported:            domain.SupportedCIRVersions(),
			ChunkWants:                      chunkWants,
		}, nil
	})
}

// --- HTTP read pass-through (auxiliary methods for inbound ports) ---

func (s *Service) ensureRepo(ctx context.Context, actorID string, repo domain.Repo) (domain.Repo, error) {
	repo, err := s.resolveRepoRegistration(ctx, actorID, repo)
	if err != nil {
		return domain.Repo{}, err
	}
	existing, err := s.meta.GetRepo(ctx, repo.ID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.Repo{}, err
	}
	read := err == nil && existing.GitRemoteURL != ""
	stored, err := s.meta.PutRepo(ctx, repo)
	if err != nil {
		return domain.Repo{}, err
	}
	// A repository that just got its Git origin was never read. Its first
	// read is asked for here, so that it starts when this registration is
	// committed and not at the reader's next look for due repositories.
	if !read && stored.GitRemoteURL != "" {
		if err := requestRemoteGitSync(ctx, s.meta, stored.ID, time.Now().UTC()); err != nil {
			return domain.Repo{}, err
		}
	}
	return stored, nil
}

// Resolving identity/authorization does not register or infer a new repository.
func (s *Service) resolveRepoRegistration(ctx context.Context, actorID string, repo domain.Repo) (domain.Repo, error) {
	var err error
	repo, err = s.resolveRepoRegistrationIdentity(ctx, actorID, repo)
	if err != nil {
		return domain.Repo{}, err
	}
	// Git origin verification (onboarding safety measure): If another team member has already connected to the same cxthub URL (same RepoID), and the git origin of the code repo is confirmed, the git origin of the newly connecting local folder must also be the same. If different, it is rejected — "A folder not connected to git in that repository" is prevented from being attached to the same cxthub URL (same origin: URL is the destination, origin is the substance).
	// The first connector confirms the origin (empty → filled), and thereafter, that value holds authority.
	existing, err := s.meta.GetRepo(ctx, repo.ID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.Repo{}, err
	}
	if err == nil && existing.GitRemoteURL != "" {
		if normalizeGitURL(repo.GitRemoteURL) != normalizeGitURL(existing.GitRemoteURL) {
			return domain.Repo{}, fmt.Errorf(
				"%w: The git origin (%s) of this folder is different from the git (%s) connected to this repository — try again in the corresponding code repo",
				domain.ErrGitOriginMismatch, gitOriginLabel(repo.GitRemoteURL), gitOriginLabel(existing.GitRemoteURL))
		}
	}
	return repo, nil
}

func (s *Service) resolveRepoRegistrationIdentity(ctx context.Context, actorID string, repo domain.Repo) (domain.Repo, error) {
	if err := domain.ValidateContentHash(repo.ID); err != nil {
		return domain.Repo{}, err
	}
	if repo.DefaultBranch != "" {
		if err := domain.ValidateBranchName(repo.DefaultBranch); err != nil {
			return domain.Repo{}, err
		}
	}
	// Binding is always interpreted by the server from the remote URL path (/<owner_username>/<repository-slug>/…). The repository_id in the body is untrusted — assuming RepoID from URL hash, allowing client-specified bindings can lead to arbitrary repository takeover (repo squatting) if a matching repository is not found (fail-closed).
	repo.RemoteURL = domain.SanitizeRemoteURL(repo.RemoteURL)
	repo.GitRemoteURL = domain.SanitizeRemoteURL(repo.GitRemoteURL)
	repositoryID, err := s.repositoryForURL(ctx, repo.RemoteURL)
	if err != nil {
		return domain.Repo{}, err
	}
	repo.RepositoryID = repositoryID

	// The authenticity of RepoID is not the Git origin but the cxthub destination URL. Connecting the same Git repo to multiple users' repositories results in different RemoteURLs, thus different RepoIDs.
	expectedID := domain.HashContent([]byte(normalizeGitURL(repo.RemoteURL)))
	if repo.ID != expectedID {
		return domain.Repo{}, fmt.Errorf("%w: repo id does not match cxthub remote URL", domain.ErrIntegrity)
	}
	if _, transactional := s.meta.(outbound.RepositoryTransactions); transactional {
		locker, ok := s.repositories.(outbound.RepositoryAccessLocker)
		if !ok {
			return domain.Repo{}, domain.ErrForbidden
		}
		if err := locker.LockRepositoryAccess(ctx, repositoryID, actorID); err != nil {
			return domain.Repo{}, err
		}
		current, err := s.repositoryForURL(ctx, repo.RemoteURL)
		if err != nil {
			return domain.Repo{}, err
		}
		if current != repositoryID {
			return domain.Repo{}, domain.ErrConflict
		}
	}

	// Registration itself creates the state of the target repository. To prevent URL takeover by others before login, context write permissions (member level or above) are required for both initial registration and re-registration.
	record, err := s.repositories.GetRepository(ctx, repositoryID)
	if err != nil {
		return domain.Repo{}, err
	}
	if record.Archived {
		return domain.Repo{}, domain.ErrForbidden
	}
	role, ok := s.repositoryRole(ctx, repositoryID, actorID)
	if !ok || !role.AtLeast(domain.RoleMember) {
		return domain.Repo{}, domain.ErrForbidden
	}

	return repo, nil
}

func (s *Service) repositoryRole(ctx context.Context, repositoryID, userID string) (domain.MemberRole, bool) {
	return repositoryRole(ctx, s.repositories, repositoryID, userID)
}

func normalizeGitURL(raw string) string {
	return domain.NormalizeGitRemoteURL(raw)
}

// gitOriginLabel is a label for validation failure messages (empty means "(none)").
func gitOriginLabel(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "(none)"
	}
	return raw
}

func settingsKindOK(kind string) bool { return domain.ValidSettingsKind(kind) }

// GetSecrets returns secret ciphertext envelopes (ciphertext as-is — decryption is on the client).
func (s *Service) GetSecrets(ctx context.Context, repoID domain.ContentHash) ([]byte, error) {
	return repositoryReadForRepo(ctx, s, repoID, func(ctx context.Context) ([]byte, error) {
		if err := domain.ValidateContentHash(repoID); err != nil {
			return nil, err
		}
		return s.meta.GetSecretsEnvelope(ctx, repoID)
	})
}

// GetSettings retrieves setting bundles.
func (s *Service) GetSettings(ctx context.Context, repoID domain.ContentHash, kind string) (domain.SettingsBundle, error) {
	return repositoryReadForRepo(ctx, s, repoID, func(ctx context.Context) (domain.SettingsBundle, error) {
		if err := domain.ValidateContentHash(repoID); err != nil {
			return domain.SettingsBundle{}, err
		}
		if !settingsKindOK(kind) {
			return domain.SettingsBundle{}, domain.ErrNotFound
		}
		return s.meta.GetSettingsBundle(ctx, repoID, kind)
	})
}

// pathSegs splits a URL path into a slice of non-empty segment strings.
func pathSegs(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// repositoryForURL resolves /<owner>/<repository> or an exact historical
// three-segment alias. Aliases retain the original content identity and always
// authorize against their current repository boundary.
func (s *Service) repositoryForURL(ctx context.Context, remoteURL string) (string, error) {
	if s.repositories == nil {
		return "", fmt.Errorf("%w: repository binding store unavailable", domain.ErrForbidden)
	}
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return "", fmt.Errorf("%w: repo remote_url is required", domain.ErrValidation)
	}
	u, err := url.Parse(remoteURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%w: repo remote_url must be an absolute URL: %q", domain.ErrValidation, remoteURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w: repo remote_url scheme must be http or https: %q", domain.ErrValidation, remoteURL)
	}
	seg := pathSegs(u.Path)
	if len(seg) != 2 && len(seg) != 3 {
		return "", fmt.Errorf("%w: repo URL must be <host>/<owner>/<repository> (or an existing legacy connection alias): %q", domain.ErrValidation, remoteURL)
	}
	for _, part := range seg {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: repo URL contains an invalid path segment: %q", domain.ErrValidation, remoteURL)
		}
	}
	var repositoryRecord domain.Repository
	if namespaces, ok := s.repositories.(outbound.OrganizationStore); ok {
		if namespace, nerr := namespaces.GetNamespaceBySlug(ctx, seg[0]); nerr == nil {
			repositoryRecord, err = s.repositories.GetRepositoryByNamespacePath(ctx, namespace.ID, strings.Join(seg[1:], "/"))
			if errors.Is(err, domain.ErrNotFound) {
				repositoryRecord, err = s.repositories.GetRepositoryByPath(ctx, seg[0], strings.Join(seg[1:], "/"))
			}
		} else {
			if !errors.Is(nerr, domain.ErrNotFound) {
				return "", nerr
			}
			// The exact stored alias may use a handle predating the registry.
			repositoryRecord, err = s.repositories.GetRepositoryByPath(ctx, seg[0], strings.Join(seg[1:], "/"))
		}
	} else {
		repositoryRecord, err = s.repositories.GetRepositoryByPath(ctx, seg[0], strings.Join(seg[1:], "/"))
	}
	if err != nil {
		return "", fmt.Errorf("%w: repo remote_url does not match an existing repository: %q", domain.ErrForbidden, remoteURL)
	}
	return repositoryRecord.ID, nil
}

func (s *Service) ListRepos(ctx context.Context, team string) ([]domain.Repo, error) {
	return repositoryRead(ctx, s, func(ctx context.Context) ([]domain.Repo, error) {
		repos, err := s.meta.ListRepos(ctx, team)
		if err != nil {
			return nil, err
		}
		for _, r := range repos {
			if err := r.RequiredDocIdentity.Validate(); err != nil {
				return nil, err
			}
		}
		return repos, nil
	})
}

func (s *Service) GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error) {
	if err := domain.ValidateContentHash(id); err != nil {
		return domain.Repo{}, err
	}
	return s.meta.GetRepo(ctx, id)
}
