package http

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type merkleHTTPBackend struct {
	*catalogHTTPBackend
	request domain.CatalogMerkleRequest
}

func (*merkleHTTPBackend) BranchPullVersion() int    { return 0 }
func (*merkleHTTPBackend) CatalogMerkleVersion() int { return 1 }
func (b *merkleHTTPBackend) CatalogMerkle(_ context.Context, repo domain.ContentHash, q domain.CatalogMerkleRequest) (domain.CatalogMerklePage, error) {
	b.calls++
	b.request = q
	root, _, err := domain.BuildCatalogMerkle(repo, nil)
	if err != nil {
		return domain.CatalogMerklePage{}, err
	}
	hash, err := domain.CatalogMerkleHash(root)
	return domain.CatalogMerklePage{Version: 1, Scope: domain.CatalogMerkleScope, RepoID: repo, Checkpoint: *b.page.Checkpoint, RootHash: hash, NodeHash: hash, Children: root.Children, Entries: []domain.CatalogEntry{}}, err
}
func TestCatalogMerkleHTTPAuthorizationAndWire(t *testing.T) {
	base, id, path := catalogHTTPFixture()
	b := &merkleHTTPBackend{catalogHTTPBackend: base}
	h := NewServer(b, id).Handler()
	for _, tc := range []struct {
		role domain.MemberRole
		code int
	}{{domain.RoleViewer, 403}, {domain.RolePuller, 200}, {domain.RoleViewer, 403}} {
		id.role = tc.role
		w := bpHTTP(t, h, http.MethodPost, path+"/merkle", `{"version":1}`, true)
		if w.Code != tc.code {
			t.Fatalf("role %s: %d %s", tc.role, w.Code, w.Body.String())
		}
		if tc.code == 200 {
			var page domain.CatalogMerklePage
			if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if err := page.Validate(b.repo.ID, domain.CatalogMerkleRequest{Version: 1}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(w.Body.String(), `"entries":[]`) {
				t.Fatal("null entries")
			}
		}
	}
	if b.calls != 1 {
		t.Fatalf("denied requests reached store: %d", b.calls)
	}
	wire := getCatalogCapabilityRepo(t, b, b.repo, "")
	if string(wire["catalog_merkle_version"]) != "1" {
		t.Fatalf("missing capability: %s", wire)
	}
}
func TestCatalogMerkleHTTPRejectsMalformedBeforeStore(t *testing.T) {
	base, id, path := catalogHTTPFixture()
	b := &merkleHTTPBackend{catalogHTTPBackend: base}
	h := NewServer(b, id).Handler()
	for _, body := range []string{`{}`, `null`, `[]`, `{"version":2}`, `{"version":1,"version":1}`, `{"Version":1}`, `{"version":1,"prefix":"ff"}`, `{"version":1,"offset":null}`, `{"version":1,"checkpoint":null}`, `{"version":1,"limit":1001}`, `{"version":1,"offset":-1}`, `{"version":1} {}`, `{"version":1,"unknown":true}`, `{"version":1,"checkpoint":{}}`} {
		w := bpHTTP(t, h, http.MethodPost, path+"/merkle", body, true)
		if w.Code != 400 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if b.calls != 0 {
		t.Fatal("malformed request reached store")
	}
	w := bpHTTP(t, NewServer(&branchPullHTTPBackend{repo: b.repo}, id).Handler(), http.MethodPost, path+"/merkle", `{"version":1}`, true)
	if w.Code != 501 {
		t.Fatalf("unsupported=%d", w.Code)
	}
}
func TestCatalogMerkleOpenAPISchemaFieldDrift(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(domain.CatalogMerkleRequest{}), reflect.TypeOf(domain.CatalogMerklePage{}), reflect.TypeOf(domain.CatalogMerkleNode{}), reflect.TypeOf(domain.CatalogMerkleChild{})} {
		t.Run(typ.Name(), func(t *testing.T) {
			props := schemaProps(t, "../../../../../schemas/openapi.yaml", typ.Name())
			fields := jsonFields(typ)
			if len(props) != len(fields) {
				t.Fatalf("%v != %v", props, fields)
			}
			for _, f := range fields {
				if !props[f] {
					t.Fatalf("missing %s", f)
				}
			}
		})
	}
}
