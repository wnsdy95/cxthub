package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// A metadata checkpoint can eliminate object downloads, but a viewer-readable
// manifest cannot grant permission to perform a CLI pull. An empty object
// request is the existing transport's fresh, side-effect-free permission gate.
func TestEmptyPullChecksStrongerPermissionThanManifest(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	repo := domain.Repo{ID: domain.HashContent([]byte("metadata checkpoint permission")), RepositoryID: "ws_" + strings.Repeat("e", 32)}
	if _, err := st.PutRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	svc := app.NewService(st, st, nil, nil, nil)
	path := "/api/v1/repos/" + string(repo.ID)
	for _, role := range []domain.MemberRole{domain.RolePuller, domain.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			identity := branchPullHTTPIdentity{repo: domain.Repository{ID: repo.RepositoryID}, role: role}
			handler := NewServer(svc, identity).Handler()
			manifest := bpHTTP(t, handler, http.MethodGet, path+"/manifest", "", true)
			if manifest.Code != http.StatusOK {
				t.Fatalf("manifest denied: %d %s", manifest.Code, manifest.Body.String())
			}
			response := bpHTTP(t, handler, http.MethodPost, path+"/pull/objects", `{}`, true)
			if role == domain.RoleViewer {
				if response.Code != http.StatusForbidden {
					t.Fatalf("viewer used manifest access to pull: %d %s", response.Code, response.Body.String())
				}
				return
			}
			var out inbound.PullSendOutput
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &out) != nil {
				t.Fatalf("empty authorized pull failed: %d %s", response.Code, response.Body.String())
			}
			if len(out.Snapshots)+len(out.Docs)+len(out.DocManifests)+len(out.ChunkObjects) != 0 {
				t.Fatalf("permission check returned unsolicited objects: %+v", out)
			}
		})
	}
}
