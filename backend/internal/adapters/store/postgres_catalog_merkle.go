//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.CatalogMerkleStore = (*PostgresStore)(nil)

type catalogMerkleState struct {
	epoch       string
	head, floor int64
}

type catalogMerkleReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type catalogMerkleBuild struct {
	state      catalogMerkleState
	checkpoint domain.CatalogCheckpoint
	root       domain.ContentHash
	// At most 273 payloads; a one-key delta stages only its leaf and two parents.
	payloads map[domain.ContentHash][]byte
}

// CatalogMerkle owns its coherent reads and the separate derived-cache write.
// It must never escape any caller transaction, including another store's or a
// read-only transaction. No repository/source-write lock is acquired here.
func (s *PostgresStore) CatalogMerkle(ctx context.Context, repo domain.ContentHash, req domain.CatalogMerkleRequest) (domain.CatalogMerklePage, error) {
	var zero domain.CatalogMerklePage
	if ctx.Value(repositoryTxKey{}) != nil {
		return zero, domain.ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := validateHash(repo); err != nil {
		return zero, err
	}
	if err := req.Validate(); err != nil {
		return zero, err
	}
	// This preflight avoids incompatible cache/source work. The page snapshot
	// and cache publication transaction independently recheck before effects.
	if err := s.WithinReadSnapshot(ctx, func(bound context.Context) error {
		return s.checkRepositoryDocumentIdentity(bound, s.db(bound), repo, false)
	}); err != nil {
		return zero, err
	}
	if req.RootHash == "" {
		state, err := s.catalogMerkleState(ctx, s.pool, repo)
		if err != nil {
			return zero, err
		}
		build, err := s.buildCatalogMerkle(ctx, repo, state)
		if err != nil {
			return zero, err
		}
		if len(build.payloads) != 0 {
			if err := s.publishCatalogMerkle(ctx, repo, build); err != nil {
				return zero, err
			}
		}
		req.RootHash, req.Checkpoint = build.root, &build.checkpoint
	}
	return s.readCatalogMerklePage(ctx, repo, req)
}

func (s *PostgresStore) catalogMerkleState(ctx context.Context, db catalogMerkleReader, repo domain.ContentHash) (catalogMerkleState, error) {
	var st catalogMerkleState
	err := db.QueryRow(ctx, `SELECT epoch::text,head_seq,floor_seq FROM repository_catalog_state WHERE repo_id=$1`, string(repo)).Scan(&st.epoch, &st.head, &st.floor)
	return st, mapNoRows(err)
}

func (s *PostgresStore) buildCatalogMerkle(ctx context.Context, repo domain.ContentHash, st catalogMerkleState) (catalogMerkleBuild, error) {
	out := catalogMerkleBuild{state: st}
	// The newest retained usable root is enough. Pruned roots remain frozen-read
	// candidates, but cannot seed a CatalogChanges delta below its current floor.
	var previous domain.ContentHash
	var sequence int64
	err := s.pool.QueryRow(ctx, `SELECT seq,root_hash FROM repository_catalog_merkle_roots
  WHERE repo_id=$1 AND epoch=$2 AND seq BETWEEN $3 AND $4 ORDER BY seq DESC LIMIT 1`,
		string(repo), st.epoch, st.floor, st.head).Scan(&sequence, &previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	var after *domain.CatalogCheckpoint
	var root domain.CatalogMerkleNode
	if err == nil {
		cp := domain.CatalogCheckpoint{Version: domain.CatalogVersion, RepoID: repo, Epoch: st.epoch, Sequence: sequence}
		after = &cp
		root, err = s.loadCatalogMerkleNode(ctx, s.pool, repo, previous, "", -1)
		if err != nil {
			return out, err
		}
		if sequence == st.head {
			// No source catalog enumeration, leaf reads, or derived writes on a hit.
			out.checkpoint, out.root = cp, previous
			return out, nil
		}
	}
	entries, cp, err := s.collectCatalogMerkle(ctx, repo, st, after)
	if err != nil {
		return out, err
	}
	var nodes map[domain.ContentHash]domain.CatalogMerkleNode
	if after == nil {
		root, nodes, err = domain.BuildCatalogMerkle(repo, entries)
	} else {
		root, nodes, err = s.updateCatalogMerkle(ctx, repo, root, entries)
	}
	if err != nil {
		return out, err
	}
	out.checkpoint = cp
	out.root, err = domain.CatalogMerkleHash(root)
	if err != nil {
		return out, err
	}
	out.payloads = make(map[domain.ContentHash][]byte, len(nodes))
	for hash, node := range nodes {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		payload, err := catalogMerklePayload(node)
		if err != nil {
			return out, err
		}
		out.payloads[hash] = payload
	}
	return out, nil
}

func (s *PostgresStore) collectCatalogMerkle(ctx context.Context, repo domain.ContentHash, st catalogMerkleState, after *domain.CatalogCheckpoint) ([]domain.CatalogEntry, domain.CatalogCheckpoint, error) {
	req := domain.CatalogRequest{Version: domain.CatalogVersion, After: after, Limit: domain.MaxCatalogLimit}
	var entries []domain.CatalogEntry
	var through int64 = -1
	cursors := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, domain.CatalogCheckpoint{}, err
		}
		page, err := s.CatalogChanges(ctx, repo, req)
		if err != nil {
			return nil, domain.CatalogCheckpoint{}, err
		}
		if page.Epoch != st.epoch || page.Through < st.head || (through >= 0 && page.Through != through) {
			return nil, domain.CatalogCheckpoint{}, domain.ErrCatalogResetRequired
		}
		through = page.Through
		for _, entry := range page.Entries {
			if entry.Sequence < 0 || entry.Sequence > through || (after != nil && entry.Sequence <= after.Sequence) ||
				(entry.Deleted && (after == nil || len(entry.Value) != 0)) {
				return nil, domain.CatalogCheckpoint{}, domain.ErrIntegrity
			}
			entries = append(entries, entry)
		}
		if page.Checkpoint != nil {
			cp := *page.Checkpoint
			if page.NextCursor != "" || cp.RepoID != repo || cp.Version != domain.CatalogVersion || cp.Epoch != st.epoch || cp.Sequence != through {
				return nil, domain.CatalogCheckpoint{}, domain.ErrIntegrity
			}
			return entries, cp, nil
		}
		if page.NextCursor == "" || cursors[page.NextCursor] || len(page.Entries) == 0 {
			return nil, domain.CatalogCheckpoint{}, domain.ErrIntegrity
		}
		cursors[page.NextCursor] = true
		req = domain.CatalogRequest{Version: domain.CatalogVersion, Cursor: page.NextCursor, Limit: domain.MaxCatalogLimit}
	}
}

// Semantic keys normalize ref-array whitespace/escaping exactly as the domain
// bucket function does. Values retain their complete original sequence/image.
func catalogMerkleEntryKey(entry domain.CatalogEntry) (string, error) {
	if _, err := domain.CatalogMerkleBucket(entry); err != nil {
		return "", err
	}
	key := entry.Key
	if entry.Kind == "ref" {
		var names [2]string
		if err := json.Unmarshal([]byte(key), &names); err != nil {
			return "", err
		}
		raw, err := json.Marshal(names)
		if err != nil {
			return "", err
		}
		key = string(raw)
	}
	return entry.Kind + "\x00" + key, nil
}

func (s *PostgresStore) updateCatalogMerkle(ctx context.Context, repo domain.ContentHash, root domain.CatalogMerkleNode, changes []domain.CatalogEntry) (domain.CatalogMerkleNode, map[domain.ContentHash]domain.CatalogMerkleNode, error) {
	nodes := map[domain.ContentHash]domain.CatalogMerkleNode{}
	groups := map[string][]domain.CatalogEntry{}
	for _, e := range changes {
		if err := ctx.Err(); err != nil {
			return root, nil, err
		}
		bucket, err := domain.CatalogMerkleBucket(e)
		if err != nil {
			return root, nil, err
		}
		if e.Kind == "protocol" && e.Key != string(repo) {
			return root, nil, domain.ErrIntegrity
		}
		groups[bucket] = append(groups[bucket], e)
	}
	branches := map[string]domain.CatalogMerkleNode{}
	for bucket, changes := range groups {
		if err := ctx.Err(); err != nil {
			return root, nil, err
		}
		prefix := bucket[:1]
		branch, ok := branches[prefix]
		if !ok {
			child := root.Children[strings.IndexByte("0123456789abcdef", prefix[0])]
			var err error
			branch, err = s.loadCatalogMerkleNode(ctx, s.pool, repo, child.Hash, prefix, child.Count)
			if err != nil {
				return root, nil, err
			}
		}
		index := strings.IndexByte("0123456789abcdef", bucket[1])
		child := branch.Children[index]
		leaf, err := s.loadCatalogMerkleNode(ctx, s.pool, repo, child.Hash, bucket, child.Count)
		if err != nil {
			return root, nil, err
		}
		byKey := make(map[string]domain.CatalogEntry, len(leaf.Entries))
		for _, entry := range leaf.Entries {
			key, err := catalogMerkleEntryKey(entry)
			if err != nil {
				return root, nil, err
			}
			byKey[key] = entry
		}
		for _, entry := range changes {
			key, err := catalogMerkleEntryKey(entry)
			if err != nil {
				return root, nil, err
			}
			if entry.Deleted {
				delete(byKey, key)
			} else {
				byKey[key] = entry
			}
		}
		entries := make([]domain.CatalogEntry, 0, len(byKey))
		for _, entry := range byKey {
			entries = append(entries, entry)
		}
		leaf, err = domain.NewCatalogMerkleLeaf(repo, bucket, entries)
		if err != nil {
			return root, nil, err
		}
		hash, err := domain.CatalogMerkleHash(leaf)
		if err != nil {
			return root, nil, err
		}
		nodes[hash] = leaf
		branch.Children[index] = domain.CatalogMerkleChild{Prefix: bucket, Hash: hash, Count: leaf.Count}
		branches[prefix] = branch
	}
	for prefix, changed := range branches {
		branch, err := domain.NewCatalogMerkleBranch(repo, prefix, changed.Children)
		if err != nil {
			return root, nil, err
		}
		hash, err := domain.CatalogMerkleHash(branch)
		if err != nil {
			return root, nil, err
		}
		nodes[hash] = branch
		root.Children[strings.IndexByte("0123456789abcdef", prefix[0])] = domain.CatalogMerkleChild{Prefix: prefix, Hash: hash, Count: branch.Count}
	}
	root, err := domain.NewCatalogMerkleBranch(repo, "", root.Children)
	if err != nil {
		return root, nil, err
	}
	hash, err := domain.CatalogMerkleHash(root)
	if err != nil {
		return root, nil, err
	}
	nodes[hash] = root
	return root, nodes, nil
}

// Canonical payloads are independent of jsonb's whitespace, field ordering and
// HTML escapes. Hash validation alone permits equivalent raw JSON; immutable
// storage additionally uses one byte representation for those equivalent nodes.
func catalogMerklePayload(node domain.CatalogMerkleNode) ([]byte, error) {
	if _, err := domain.CatalogMerkleHash(node); err != nil {
		return nil, err
	}
	node.Entries = append([]domain.CatalogEntry{}, node.Entries...)
	for i := range node.Entries {
		if node.Entries[i].Kind == "ref" {
			var key [2]string
			if err := json.Unmarshal([]byte(node.Entries[i].Key), &key); err != nil {
				return nil, err
			}
			raw, err := json.Marshal(key)
			if err != nil {
				return nil, err
			}
			node.Entries[i].Key = string(raw)
		}
	}
	raw, err := json.Marshal(node)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func (s *PostgresStore) loadCatalogMerkleNode(ctx context.Context, db catalogMerkleReader, repo, hash domain.ContentHash, prefix string, count int64) (domain.CatalogMerkleNode, error) {
	var node domain.CatalogMerkleNode
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT payload FROM repository_catalog_merkle_nodes WHERE repo_id=$1 AND hash=$2`, string(repo), string(hash)).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return node, domain.ErrIntegrity
		}
		return node, err
	}
	if err := ctx.Err(); err != nil {
		return node, err
	}
	if err := json.Unmarshal(raw, &node); err != nil {
		return node, domain.ErrIntegrity
	}
	if node.RepoID != repo || node.Prefix != prefix || (count >= 0 && node.Count != count) {
		return node, domain.ErrIntegrity
	}
	actual, err := domain.CatalogMerkleHash(node)
	if err != nil || actual != hash {
		return node, domain.ErrIntegrity
	}
	canonical, err := catalogMerklePayload(node)
	if err != nil || !bytes.Equal(canonical, raw) {
		return node, domain.ErrIntegrity
	}
	return node, nil
}

func (s *PostgresStore) publishCatalogMerkle(ctx context.Context, repo domain.ContentHash, build catalogMerkleBuild) error {
	if ctx.Value(repositoryTxKey{}) != nil {
		return domain.ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Stage every byte and sort before opening the bounded cache transaction.
	hashes := make([]domain.ContentHash, 0, len(build.payloads))
	for hash := range build.payloads {
		hashes = append(hashes, hash)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i] < hashes[j] })
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	if err := s.checkRepositoryDocumentIdentity(ctx, tx, repo, true); err != nil {
		return err
	}
	check := func() error {
		st, err := s.catalogMerkleState(ctx, tx, repo)
		if err != nil {
			return err
		}
		if st.epoch != build.state.epoch || st.floor != build.state.floor || st.head < build.checkpoint.Sequence {
			return domain.ErrCatalogResetRequired
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	for _, hash := range hashes {
		payload := build.payloads[hash]
		inserted, err := tx.Exec(ctx, `INSERT INTO repository_catalog_merkle_nodes(repo_id,hash,payload) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, string(repo), string(hash), payload)
		if err != nil {
			return err
		}
		if inserted.RowsAffected() == 0 {
			var old []byte
			if err := tx.QueryRow(ctx, `SELECT payload FROM repository_catalog_merkle_nodes WHERE repo_id=$1 AND hash=$2`, string(repo), string(hash)).Scan(&old); err != nil {
				return err
			}
			if !bytes.Equal(old, payload) {
				return fmt.Errorf("%w: conflicting immutable catalog node", domain.ErrIntegrity)
			}
		}
	}
	// A reset/prune observed during collection or cache staging discards all
	// staged writes. Later source writes may advance head without invalidating
	// this fixed checkpoint. A reset after our observation makes this old epoch
	// unservable; frozen reads recheck epoch in their own coherent snapshot.
	if err := check(); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO repository_catalog_merkle_roots(repo_id,epoch,seq,root_hash) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
		string(repo), build.checkpoint.Epoch, build.checkpoint.Sequence, string(build.root)); err != nil {
		return err
	}
	var root domain.ContentHash
	if err := tx.QueryRow(ctx, `SELECT root_hash FROM repository_catalog_merkle_roots WHERE repo_id=$1 AND epoch=$2 AND seq=$3`,
		string(repo), build.checkpoint.Epoch, build.checkpoint.Sequence).Scan(&root); err != nil {
		return err
	}
	if root != build.root {
		return fmt.Errorf("%w: conflicting catalog root", domain.ErrIntegrity)
	}
	if err := check(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) readCatalogMerklePage(ctx context.Context, repo domain.ContentHash, req domain.CatalogMerkleRequest) (domain.CatalogMerklePage, error) {
	var page domain.CatalogMerklePage
	err := s.WithinReadSnapshot(ctx, func(ctx context.Context) error {
		if err := s.checkRepositoryDocumentIdentity(ctx, s.db(ctx), repo, false); err != nil {
			return err
		}
		st, err := s.catalogMerkleState(ctx, s.db(ctx), repo)
		if err != nil {
			return err
		}
		cp := *req.Checkpoint
		if cp.RepoID != repo || cp.Epoch != st.epoch || cp.Sequence > st.head {
			return domain.ErrCatalogResetRequired
		}
		var root domain.ContentHash
		if err := s.db(ctx).QueryRow(ctx, `SELECT root_hash FROM repository_catalog_merkle_roots WHERE repo_id=$1 AND epoch=$2 AND seq=$3`,
			string(repo), cp.Epoch, cp.Sequence).Scan(&root); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCatalogResetRequired
			}
			return err
		}
		if root != req.RootHash {
			return domain.ErrCatalogResetRequired
		}
		node, err := s.loadCatalogMerkleNode(ctx, s.db(ctx), repo, root, "", -1)
		if err != nil {
			return err
		}
		nodeHash := root
		for depth := 0; depth < len(req.Prefix); depth++ {
			child := node.Children[strings.IndexByte("0123456789abcdef", req.Prefix[depth])]
			node, err = s.loadCatalogMerkleNode(ctx, s.db(ctx), repo, child.Hash, child.Prefix, child.Count)
			if err != nil {
				return err
			}
			nodeHash = child.Hash
		}
		page = domain.CatalogMerklePage{Version: domain.CatalogMerkleVersion, Scope: domain.CatalogMerkleScope, RepoID: repo,
			Checkpoint: cp, RootHash: root, Prefix: node.Prefix, NodeHash: nodeHash, Count: node.Count,
			Children: node.Children, Entries: []domain.CatalogEntry{}, Offset: req.Offset}
		if len(node.Prefix) == 2 {
			if req.Offset > len(node.Entries) || (req.Offset == len(node.Entries) && req.Offset != 0) {
				return domain.ErrValidation
			}
			limit := req.Limit
			if limit == 0 {
				limit = domain.DefaultCatalogLimit
			}
			end := req.Offset + min(limit, len(node.Entries)-req.Offset)
			page.Entries = append(page.Entries, node.Entries[req.Offset:end]...)
			if end < len(node.Entries) {
				page.NextOffset = &end
			}
		}
		return page.Validate(repo, req)
	})
	if err != nil {
		return domain.CatalogMerklePage{}, err
	}
	return page, nil
}
