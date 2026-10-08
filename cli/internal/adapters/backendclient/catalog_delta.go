package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

const catalogPageLimit = 256
const catalogResponseBytes = 32 << 20

// acquireCatalog returns only metadata. A completed acquisition checkpoint is
// not a verified remote observation or a receipt for any document/attachment.
// Keeping this optional avoids changing viewer-readable RemoteManifest calls.
func (c *BackendClient) acquireCatalog(ctx context.Context, repo string) (domain.Manifest, []domain.Snapshot, bool, error) {
	var zero domain.Manifest
	if c.catalogCache == nil {
		return zero, nil, false, nil
	}
	if domain.ValidateContentHash(repo) != nil {
		return zero, nil, false, domain.ErrHashMismatch
	}
	base := c.baseURL()
	// Pin all requests to the captured endpoint; token lookup remains fresh.
	peer := NewBackendClient(func() string { return base }, c.token, c.identity)
	peer.httpc = c.httpc
	remote := peer.SyncRemoteIdentity()
	fence := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if remote == "" || remote != c.SyncRemoteIdentity() {
			return domain.ErrSyncConflict
		}
		return nil
	}
	if err := fence(); err != nil {
		return zero, nil, false, err
	}
	capability, err := peer.catalogCapability(ctx, repo)
	if err != nil {
		return zero, nil, false, err
	}
	if err = fence(); err != nil {
		return zero, nil, false, err
	}
	if capability.CatalogMerkleVersion != 0 && capability.CatalogMerkleVersion != domain.CatalogMerkleVersion {
		return zero, nil, false, fmt.Errorf("unsupported catalog Merkle version %d", capability.CatalogMerkleVersion)
	}
	if capability.CatalogMerkleVersion != 0 && capability.CatalogVersion == 0 {
		return zero, nil, false, domain.ErrHashMismatch
	}
	if capability.CatalogVersion == 0 {
		return zero, nil, false, nil
	}
	if capability.CatalogVersion != domain.CatalogVersion {
		return zero, nil, false, fmt.Errorf("unsupported catalog version %d", capability.CatalogVersion)
	}
	state, err := c.catalogCache.ReadCatalogCache(ctx, repo, remote)
	if err != nil {
		return zero, nil, true, fmt.Errorf("read catalog cache: %w", err)
	}
	if err = validateCatalogCache(repo, remote, state); err != nil {
		return zero, nil, true, err
	}
	resumed := len(state.Pending) != 0
	baseline := state.Checkpoint == nil
	if resumed {
		baseline = state.Pending[0].Mode == "baseline"
	}
	resets := 0
	pageLimit := catalogPageLimit
	cursors := map[string]bool{}
	for _, page := range state.Pending {
		cursors[page.NextCursor] = true
	}
	for {
		if err = fence(); err != nil {
			return zero, nil, true, err
		}
		request := domain.CatalogRequest{Version: domain.CatalogVersion, Limit: pageLimit}
		after := state.Checkpoint
		if baseline {
			after = nil
		}
		var previous *domain.CatalogPage
		if n := len(state.Pending); n > 0 {
			previous = &state.Pending[n-1]
			request.Cursor = previous.NextCursor
		} else {
			request.After = after
		}
		var raw json.RawMessage
		err = peer.doLimited(ctx, http.MethodPost, peer.reposPath(repo)+"/pull/catalog", request, &raw, catalogResponseBytes)
		if check := fence(); check != nil {
			return zero, nil, true, check
		}
		if errors.Is(err, errBoundedResponse) {
			if pageLimit <= 1 {
				return zero, nil, true, fmt.Errorf("catalog entry exceeds %d-byte response limit: %w", catalogResponseBytes, err)
			}
			pageLimit = max(1, pageLimit/2)
			continue // Retry the identical checkpoint/cursor, without acknowledging a page.
		}
		if err != nil {
			var he *HTTPError
			if errors.As(err, &he) && he.Status == http.StatusConflict && he.Code == "reset_required" && resets == 0 {
				if installer, ok := c.catalogCache.(outbound.CatalogImageInstaller); ok && capability.CatalogMerkleVersion == domain.CatalogMerkleVersion && state.Checkpoint != nil {
					manifest, snapshots, err := peer.reconcileCatalogMerkle(ctx, repo, remote, state, installer, fence)
					return manifest, snapshots, true, err
				}
				revision, resetErr := c.catalogCache.ResetCatalogRun(ctx, state.Revision, repo, remote)
				if resetErr != nil {
					return zero, nil, true, resetErr
				}
				state.Revision, state.Pending = revision, nil
				baseline, resumed, resets = true, false, resets+1
				cursors = map[string]bool{}
				continue
			}
			// A modern endpoint failure is never interpreted as legacy capability.
			return zero, nil, true, err
		}
		page, err := domain.DecodeCatalogPage(raw)
		if err != nil {
			return zero, nil, true, err
		}
		if page.NextCursor != "" {
			if cursors[page.NextCursor] {
				return zero, nil, true, domain.ErrHashMismatch
			}
			cursors[page.NextCursor] = true
		}
		if len(page.Entries) > pageLimit {
			return zero, nil, true, domain.ErrHashMismatch
		}
		if err = domain.ValidateCatalogPage(repo, after, previous, page); err != nil {
			return zero, nil, true, err
		}
		var image []domain.CatalogEntry
		var manifest domain.Manifest
		var snapshots []domain.Snapshot
		if page.Checkpoint != nil {
			image = completeCatalogImage(state.Entries, state.Pending, page)
			manifest, snapshots, err = domain.CatalogManifest(repo, image)
			if err != nil {
				return zero, nil, true, err
			}
		}
		if err = fence(); err != nil {
			return zero, nil, true, err
		}
		revision, err := c.catalogCache.AppendCatalogPage(ctx, state.Revision, repo, remote, page)
		if err != nil {
			return zero, nil, true, fmt.Errorf("stage catalog page: %w", err)
		}
		state.Revision = revision
		if err = fence(); err != nil {
			return zero, nil, true, err
		}
		if page.Checkpoint == nil {
			state.Pending = append(state.Pending, page)
			continue
		}
		state.Checkpoint, state.Entries, state.Pending = page.Checkpoint, image, nil
		if resumed {
			// A resumed fixed-bound run may be old. Catch up once under a newly fixed
			// committed head before returning it to the current acquisition caller.
			resumed, baseline = false, false
			cursors = map[string]bool{}
			continue
		}
		return manifest, snapshots, true, nil
	}
}

func validateCatalogCache(repo, remote string, state outbound.CatalogCache) error {
	if state.Version != 1 || state.RepoID != repo || state.Remote != remote {
		return domain.ErrHashMismatch
	}
	if state.Checkpoint == nil {
		if len(state.Entries) != 0 {
			return domain.ErrHashMismatch
		}
	} else {
		if err := domain.ValidateCatalogCheckpoint(repo, *state.Checkpoint); err != nil {
			return err
		}
		for _, entry := range state.Entries {
			if entry.Sequence > state.Checkpoint.Sequence {
				return domain.ErrHashMismatch
			}
		}
		if _, _, err := domain.CatalogManifest(repo, state.Entries); err != nil {
			return err
		}
	}
	after := state.Checkpoint
	if len(state.Pending) != 0 && state.Pending[0].Mode == "baseline" {
		after = nil
	}
	var previous *domain.CatalogPage
	for i := range state.Pending {
		page := &state.Pending[i]
		if page.Checkpoint != nil {
			return domain.ErrHashMismatch
		}
		if err := domain.ValidateCatalogPage(repo, after, previous, *page); err != nil {
			return err
		}
		previous = page
	}
	return nil
}

func completeCatalogImage(entries []domain.CatalogEntry, pending []domain.CatalogPage, final domain.CatalogPage) []domain.CatalogEntry {
	image := make(map[string]domain.CatalogEntry, len(entries))
	if final.Mode == "delta" {
		for _, e := range entries {
			image[domain.CatalogEntryIdentity(e)] = e
		}
	}
	apply := func(page domain.CatalogPage) {
		for _, e := range page.Entries {
			k := domain.CatalogEntryIdentity(e)
			if e.Deleted {
				delete(image, k)
			} else {
				image[k] = e
			}
		}
	}
	for _, page := range pending {
		apply(page)
	}
	apply(final)
	out := make([]domain.CatalogEntry, 0, len(image))
	for _, e := range image {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Capability is obtained from the small, freshly authorized repository view.
// Duplicate/case-aliased/null version fields cannot turn a protocol error into
// an apparent old server. Unrelated repository fields retain their contract.
func (c *BackendClient) catalogCapability(ctx context.Context, repo string) (repositoryView, error) {
	var view repositoryView
	var raw json.RawMessage
	if err := c.doLimited(ctx, http.MethodGet, c.reposPath(repo), nil, &raw, 1<<20); err != nil {
		return view, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return view, domain.ErrHashMismatch
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return view, err
		}
		name, ok := token.(string)
		if !ok || seen[strings.ToLower(name)] {
			return view, domain.ErrHashMismatch
		}
		seen[strings.ToLower(name)] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return view, err
		}
		for _, field := range []string{"catalog_version", "catalog_merkle_version"} {
			if strings.EqualFold(name, field) && (name != field || bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
				return view, domain.ErrHashMismatch
			}
		}
	}
	if _, err = decoder.Token(); err != nil {
		return view, err
	}
	if _, err = decoder.Token(); err != io.EOF {
		return view, domain.ErrHashMismatch
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		return view, err
	}
	if view.ID != repo || (view.DefaultBranch != "" && domain.ValidateBranchName(view.DefaultBranch) != nil) {
		return view, domain.ErrHashMismatch
	}
	return view, nil
}
