package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Reconciliation stages no partial acknowledgements. Equal ranges come from
// the already byte-verified complete cache, never from pending delta pages.
// Only the fully validated image may atomically replace that cache by CAS.
func (c *BackendClient) reconcileCatalogMerkle(ctx context.Context, repo, remote string, state outbound.CatalogCache, installer outbound.CatalogImageInstaller, fence func() error) (domain.Manifest, []domain.Snapshot, error) {
	var zero domain.Manifest
	_, local, err := domain.BuildCatalogMerkle(repo, state.Entries)
	if err != nil {
		return zero, nil, err
	}
	rootRequest := domain.CatalogMerkleRequest{Version: domain.CatalogMerkleVersion}
	root, err := c.readCatalogMerklePage(ctx, repo, &rootRequest, fence)
	if err != nil {
		return zero, nil, err
	}
	if state.Checkpoint != nil && state.Checkpoint.Epoch == root.Checkpoint.Epoch && root.Checkpoint.Sequence < state.Checkpoint.Sequence {
		return zero, nil, domain.ErrHashMismatch
	}
	var collect func(domain.CatalogMerkleChild, *domain.CatalogMerklePage) ([]domain.CatalogEntry, error)
	collect = func(child domain.CatalogMerkleChild, first *domain.CatalogMerklePage) ([]domain.CatalogEntry, error) {
		if err := fence(); err != nil {
			return nil, err
		}
		if node, ok := local[child.Hash]; ok {
			if node.Prefix != child.Prefix || node.Count != child.Count {
				return nil, domain.ErrHashMismatch
			}
			if len(node.Prefix) == 2 {
				return node.Entries, nil
			}
			entries := []domain.CatalogEntry{}
			for _, sub := range node.Children {
				part, err := collect(sub, nil)
				if err != nil {
					return nil, err
				}
				entries = append(entries, part...)
			}
			return entries, nil
		}
		request := domain.CatalogMerkleRequest{Version: domain.CatalogMerkleVersion, RootHash: root.RootHash, Checkpoint: &root.Checkpoint, Prefix: child.Prefix, Limit: catalogPageLimit}
		var page domain.CatalogMerklePage
		if first != nil {
			page = *first
		} else {
			var err error
			page, err = c.readCatalogMerklePage(ctx, repo, &request, fence)
			if err != nil {
				return nil, err
			}
		}
		if page.NodeHash != child.Hash || page.Count != child.Count {
			return nil, domain.ErrHashMismatch
		}
		if len(child.Prefix) < 2 {
			entries := []domain.CatalogEntry{}
			for _, sub := range page.Children {
				part, err := collect(sub, nil)
				if err != nil {
					return nil, err
				}
				entries = append(entries, part...)
			}
			return entries, nil
		}
		entries := append([]domain.CatalogEntry{}, page.Entries...)
		for page.NextOffset != nil {
			request.Offset = *page.NextOffset
			page, err = c.readCatalogMerklePage(ctx, repo, &request, fence)
			if err != nil {
				return nil, err
			}
			if page.NodeHash != child.Hash || page.Count != child.Count {
				return nil, domain.ErrHashMismatch
			}
			entries = append(entries, page.Entries...)
		}
		// Do not use the sorting constructor: page boundary order/duplicates must
		// be checked as received before accepting the whole leaf's hash.
		node := domain.CatalogMerkleNode{Version: domain.CatalogMerkleVersion, Scope: domain.CatalogMerkleScope, RepoID: repo, Prefix: child.Prefix, Count: child.Count, Children: []domain.CatalogMerkleChild{}, Entries: entries}
		hash, err := domain.CatalogMerkleHash(node)
		if err != nil {
			return nil, err
		}
		if hash != child.Hash {
			return nil, domain.ErrHashMismatch
		}
		return entries, nil
	}
	entries, err := collect(domain.CatalogMerkleChild{Prefix: "", Hash: root.RootHash, Count: root.Count}, &root)
	if err != nil {
		return zero, nil, err
	}
	if int64(len(entries)) != root.Count {
		return zero, nil, domain.ErrHashMismatch
	}
	for _, entry := range entries {
		if entry.Sequence > root.Checkpoint.Sequence {
			return zero, nil, domain.ErrHashMismatch
		}
	}
	// CatalogManifest checks all cross-record identities, independent of hashes.
	manifest, snapshots, err := domain.CatalogManifest(repo, entries)
	if err != nil {
		return zero, nil, err
	}
	assembled, _, err := domain.BuildCatalogMerkle(repo, entries)
	if err != nil {
		return zero, nil, err
	}
	hash, err := domain.CatalogMerkleHash(assembled)
	if err != nil {
		return zero, nil, err
	}
	if hash != root.RootHash {
		return zero, nil, domain.ErrHashMismatch
	}
	if err := fence(); err != nil {
		return zero, nil, err
	}
	if _, err := installer.InstallCatalogImage(ctx, state.Revision, repo, remote, root.Checkpoint, entries); err != nil {
		return zero, nil, fmt.Errorf("install catalog Merkle image: %w", err)
	}
	if err := fence(); err != nil {
		return zero, nil, err
	}
	return manifest, snapshots, nil
}

func (c *BackendClient) readCatalogMerklePage(ctx context.Context, repo string, request *domain.CatalogMerkleRequest, fence func() error) (domain.CatalogMerklePage, error) {
	if request.Limit == 0 {
		request.Limit = catalogPageLimit
	}
	for {
		if err := fence(); err != nil {
			return domain.CatalogMerklePage{}, err
		}
		var raw json.RawMessage
		err := c.doLimited(ctx, http.MethodPost, c.reposPath(repo)+"/pull/catalog/merkle", request, &raw, catalogResponseBytes)
		if check := fence(); check != nil {
			return domain.CatalogMerklePage{}, check
		}
		if errors.Is(err, errBoundedResponse) && request.Limit > 1 {
			request.Limit = max(1, request.Limit/2)
			continue
		}
		if err != nil {
			return domain.CatalogMerklePage{}, err
		}
		page, err := domain.DecodeCatalogMerklePage(raw)
		if err != nil {
			return domain.CatalogMerklePage{}, err
		}
		if err := page.Validate(repo, *request); err != nil {
			return domain.CatalogMerklePage{}, err
		}
		return page, nil
	}
}
