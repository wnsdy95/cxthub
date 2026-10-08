package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type merkleFixture struct {
	*deltaFixture
	root       domain.CatalogMerkleNode
	nodes      map[domain.ContentHash]domain.CatalogMerkleNode
	checkpoint domain.CatalogCheckpoint
	requests   []domain.CatalogMerkleRequest
	received   int
	mutate     func(*http.Request, *domain.CatalogMerklePage) (*http.Response, error)
}

func newMerkleFixture(t *testing.T, count int) *merkleFixture {
	f := newDeltaFixture(t, count)
	if _, err := f.read(); err != nil {
		t.Fatal(err)
	}
	m := &merkleFixture{deltaFixture: f, checkpoint: domain.CatalogCheckpoint{Version: 1, RepoID: f.repo, Epoch: deltaEpoch, Sequence: 5}}
	f.capRaw = fmt.Sprintf(`{"id":%q,"catalog_version":1,"catalog_merkle_version":1}`, f.repo)
	m.build()
	f.hook = func(r *http.Request, q domain.CatalogRequest, n int) (*http.Response, error) {
		if n >= 0 {
			return deltaReply(r, 409, `{"error":{"code":"reset_required","message":"expired"}}`), nil
		}
		if !strings.HasSuffix(r.URL.Path, "/pull/catalog/merkle") {
			t.Fatalf("unexpected %s", r.URL.Path)
		}
		var request domain.CatalogMerkleRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if err := request.Validate(); err != nil {
			t.Fatal(err)
		}
		m.requests = append(m.requests, request)
		rootHash, _ := domain.CatalogMerkleHash(m.root)
		if request.RootHash != "" && (request.RootHash != rootHash || *request.Checkpoint != m.checkpoint) {
			t.Fatal("lost frozen root boundary")
		}
		node := m.root
		for len(node.Prefix) < len(request.Prefix) {
			node = m.nodes[node.Children[strings.IndexByte("0123456789abcdef", request.Prefix[len(node.Prefix)])].Hash]
		}
		hash, _ := domain.CatalogMerkleHash(node)
		page := domain.CatalogMerklePage{Version: 1, Scope: domain.CatalogMerkleScope, RepoID: f.repo, Checkpoint: m.checkpoint, RootHash: rootHash, Prefix: node.Prefix, NodeHash: hash, Count: node.Count, Children: node.Children, Entries: node.Entries, Offset: request.Offset}
		if len(node.Prefix) == 2 {
			end := min(request.Offset+request.Limit, len(node.Entries))
			page.Entries = node.Entries[request.Offset:end]
			if end < len(node.Entries) {
				page.NextOffset = &end
			}
		}
		if m.mutate != nil {
			if response, err := m.mutate(r, &page); response != nil || err != nil {
				return response, err
			}
		}
		m.received += len(page.Entries)
		return deltaReply(r, 200, page), nil
	}
	return m
}
func (m *merkleFixture) build() {
	m.t.Helper()
	var err error
	m.root, m.nodes, err = domain.BuildCatalogMerkle(m.repo, m.entries)
	if err != nil {
		m.t.Fatal(err)
	}
}
func TestCatalogMerkleResetTransfersOnlyChangedRange(t *testing.T) {
	m := newMerkleFixture(t, 1024)
	oldRevision := m.cacheRevision()
	var changed domain.Snapshot
	if err := json.Unmarshal(m.entries[200].Value, &changed); err != nil {
		t.Fatal(err)
	}
	changed.MemoryHash = domain.HashContent([]byte("new memory"))
	raw, _ := json.Marshal(changed)
	m.entries[200].Value = raw
	m.entries[200].Sequence = 5
	// Include a deletion in another bucket: absent entries must disappear.
	deleted := m.entries[500].Key
	m.entries = append(m.entries[:500], m.entries[501:]...)
	m.build()
	got, err := m.read()
	if err != nil {
		t.Fatal(err)
	}
	_, want, err := domain.CatalogManifest(m.repo, m.entries)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("reconciled metadata differs from complete baseline")
	}
	if m.received >= 40 || m.received == 0 || len(m.requests) > 5 {
		t.Fatalf("not bounded mismatch transfer: %d entries %d requests", m.received, len(m.requests))
	}
	if m.cacheRevision() == oldRevision {
		t.Fatal("checkpoint not installed")
	}
	state, err := m.store.ReadCatalogCache(context.Background(), m.repo, m.client.SyncRemoteIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if *state.Checkpoint != m.checkpoint || len(state.Pending) != 0 {
		t.Fatal("incomplete checkpoint")
	}
	for _, e := range state.Entries {
		if e.Key == deleted {
			t.Fatal("deleted record retained")
		}
	}
	obs, err := m.store.ReadRemoteObservation(context.Background(), m.repo, m.client.SyncRemoteIdentity())
	if err != nil || obs.Revision != "" {
		t.Fatalf("metadata acknowledged objects: %+v %v", obs, err)
	}
}
func TestCatalogMerkleEqualRootAndNewEpoch(t *testing.T) {
	m := newMerkleFixture(t, 300)
	m.checkpoint.Epoch = "d66213bd-003e-47d1-b965-e690036bfb08"
	if _, err := m.read(); err != nil {
		t.Fatal(err)
	}
	if len(m.requests) != 1 || m.received != 0 {
		t.Fatalf("equal root fetched entries: %d/%d", len(m.requests), m.received)
	}
}
func TestCatalogMerkleFailuresPreservePreviousCache(t *testing.T) {
	for _, kind := range []string{"unauthorized", "missing_endpoint", "canceled", "wrong_hash", "wrong_checkpoint", "duplicate_entry", "CAS"} {
		t.Run(kind, func(t *testing.T) {
			m := newMerkleFixture(t, 300)
			m.entries[1].Sequence = 5
			m.build()
			before := m.cacheRevision()
			raced := false
			m.mutate = func(r *http.Request, p *domain.CatalogMerklePage) (*http.Response, error) {
				if len(p.Prefix) != 2 {
					return nil, nil
				}
				switch kind {
				case "unauthorized":
					return deltaReply(r, 403, `{"code":"forbidden"}`), nil
				case "missing_endpoint":
					return deltaReply(r, 404, `{"code":"not_found"}`), nil
				case "canceled":
					return nil, context.Canceled
				case "wrong_hash":
					p.NodeHash = domain.HashContent([]byte("wrong"))
				case "wrong_checkpoint":
					p.Checkpoint.Sequence++
				case "duplicate_entry":
					p.Entries = append(p.Entries, p.Entries[0])
					p.Count++
				case "CAS":
					if !raced {
						var err error
						before, err = m.store.ResetCatalogRun(context.Background(), before, m.repo, m.client.SyncRemoteIdentity())
						if err != nil {
							t.Fatal(err)
						}
						raced = true
					}
				}
				return nil, nil
			}
			if got, err := m.read(); err == nil || got != nil {
				t.Fatalf("invalid result accepted: %d %v", len(got), err)
			}
			if m.cacheRevision() != before {
				t.Fatal("failure overwrote previous complete cache")
			}
		})
	}
}
func TestCatalogMerkleLeafPagination(t *testing.T) {
	m := newMerkleFixture(t, 0)
	// 257 valid records in the same hash bucket require two leaf pages.
	bucket := ""
	for i := 0; len(m.entries) < 258; i++ {
		id := domain.HashContent([]byte(fmt.Sprintf("collision-%d", i)))
		raw, _ := json.Marshal(domain.Snapshot{ID: id, DocHash: id, RepoID: m.repo})
		e := domain.CatalogEntry{Kind: "snapshot", Key: id, Value: raw, Sequence: 5}
		b, _ := domain.CatalogMerkleBucket(e)
		if bucket == "" {
			bucket = b
		}
		if b == bucket {
			m.entries = append(m.entries, e)
		}
	}
	m.build()
	if got, err := m.read(); err != nil || len(got) != 257 {
		t.Fatalf("page result=%d %v", len(got), err)
	}
	offsets := []int{}
	for _, q := range m.requests {
		if q.Prefix == bucket {
			offsets = append(offsets, q.Offset)
		}
	}
	if !reflect.DeepEqual(offsets, []int{0, 256}) {
		t.Fatalf("offsets %v", offsets)
	}
}
func TestCatalogMerkleCapabilityFailsClosed(t *testing.T) {
	for _, wire := range []string{`"catalog_merkle_version":2`, `"catalog_merkle_version":null`, `"Catalog_Merkle_Version":1`, `"catalog_merkle_version":1,"catalog_merkle_version":0`} {
		t.Run(wire, func(t *testing.T) {
			f := newDeltaFixture(t, 1)
			f.capRaw = fmt.Sprintf(`{"id":%q,"catalog_version":1,%s}`, f.repo, wire)
			if _, err := f.read(); err == nil {
				t.Fatal("malformed capability accepted")
			}
			if len(f.calls) != 0 {
				t.Fatal("malformed capability permitted baseline")
			}
		})
	}
}
func TestCatalogMerkleEndpointFence(t *testing.T) {
	m := newMerkleFixture(t, 3)
	m.entries[1].Sequence = 5
	m.build()
	before := m.cacheRevision()
	m.mutate = func(_ *http.Request, p *domain.CatalogMerklePage) (*http.Response, error) {
		if len(p.Prefix) == 2 {
			m.base = "https://changed.invalid/api/v1"
		}
		return nil, nil
	}
	if _, err := m.read(); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("endpoint change=%v", err)
	}
	m.base = "https://delta.invalid/api/v1"
	if m.cacheRevision() != before {
		t.Fatal("changed endpoint accepted cache")
	}
}

func TestCatalogMerkleRejectsSameEpochRollback(t *testing.T) {
	m := newMerkleFixture(t, 3)
	state, err := m.store.ReadCatalogCache(context.Background(), m.repo, m.client.SyncRemoteIdentity())
	if err != nil {
		t.Fatal(err)
	}
	cp := *state.Checkpoint
	cp.Sequence = 5
	before, err := m.store.InstallCatalogImage(context.Background(), state.Revision, m.repo, m.client.SyncRemoteIdentity(), cp, state.Entries)
	if err != nil {
		t.Fatal(err)
	}
	m.checkpoint.Sequence = 0
	if got, err := m.read(); err == nil || got != nil {
		t.Fatalf("rollback accepted: %v", err)
	}
	if m.cacheRevision() != before {
		t.Fatal("rollback replaced acquired state")
	}
	// Restore/reset is valid only when the server actually changes its epoch.
	m.checkpoint.Epoch = "d66213bd-003e-47d1-b965-e690036bfb08"
	if _, err := m.read(); err != nil {
		t.Fatalf("new epoch rejected: %v", err)
	}
}

func TestCatalogMerkleUnicodeCapabilitiesFailClosed(t *testing.T) {
	for _, name := range []string{"catalog_version", "catalog_merkle_version"} {
		alias := strings.Replace(name, "s", "ſ", 1)
		for _, pair := range []string{fmt.Sprintf(`%q:1,%q:0`, name, alias), fmt.Sprintf(`%q:0,%q:1`, alias, name)} {
			f := newDeltaFixture(t, 1)
			f.capRaw = fmt.Sprintf(`{"id":%q,%s}`, f.repo, pair)
			if _, err := f.read(); err == nil {
				t.Fatalf("alias accepted: %s", pair)
			}
			if len(f.calls) != 0 {
				t.Fatal("alias allowed data request")
			}
		}
	}
}

func TestCatalogMerkleRetainsReducedLeafLimit(t *testing.T) {
	m := newMerkleFixture(t, 0)
	bucket := ""
	for i := 0; len(m.entries) < 9; i++ {
		id := domain.HashContent([]byte(fmt.Sprintf("oversize-%d", i)))
		raw, _ := json.Marshal(domain.Snapshot{ID: id, DocHash: id, RepoID: m.repo})
		e := domain.CatalogEntry{Kind: "snapshot", Key: id, Value: raw, Sequence: 5}
		b, _ := domain.CatalogMerkleBucket(e)
		if bucket == "" {
			bucket = b
		}
		if b == bucket {
			m.entries = append(m.entries, e)
		}
	}
	m.build()
	m.mutate = func(_ *http.Request, p *domain.CatalogMerklePage) (*http.Response, error) {
		if p.Prefix == bucket && m.requests[len(m.requests)-1].Limit > 2 {
			return nil, errBoundedResponse
		}
		return nil, nil
	}
	if got, err := m.read(); err != nil || len(got) != 8 {
		t.Fatalf("reduced leaf: %d %v", len(got), err)
	}
	for _, q := range m.requests {
		if q.Prefix == bucket && q.Offset > 0 && q.Limit > 2 {
			t.Fatalf("forgot measured page limit: %+v", q)
		}
	}
}
