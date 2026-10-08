package store

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"strings"
	"testing"
	"time"
)

func TestP6FSRequirementMonotonic(t *testing.T) {
	st, err := OpenFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := domain.HashContent([]byte("p6"))
	if _, err = st.PutRepo(ctx, domain.Repo{ID: id, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	cap, ok := any(st).(interface {
		RequireDocumentIdentity(context.Context, domain.ContentHash, domain.DocumentIdentity) error
	})
	if !ok {
		t.Fatal("missing requirement persistence")
	}
	if err = cap.RequireDocumentIdentity(ctx, id, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if cap.RequireDocumentIdentity(ctx, id, domain.DocumentIdentityLegacy) == nil {
		t.Fatal("downgrade")
	}
	if cap.RequireDocumentIdentity(ctx, id, domain.DocumentIdentity("unknown")) == nil {
		t.Fatal("unknown accepted")
	}
	branch := "other"
	if err = st.UpdateRepoConfig(ctx, id, &branch, nil); err != nil {
		t.Fatal(err)
	}
	r, err := st.PutRepo(ctx, domain.Repo{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), `"required_doc_identity":"cxt-manifest-sha256-v1"`) {
		t.Fatal("requirement erased")
	}
}
func TestP6FSRegistrationCannotOptIn(t *testing.T) {
	st, err := OpenFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"id": domain.HashContent([]byte("p6-new")), "required_doc_identity": "cxt-manifest-sha256-v1"})
	var r domain.Repo
	if err = json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if _, err = st.PutRepo(context.Background(), r); err == nil {
		t.Fatal("registration opted in")
	}
}

// The same metadata lock must cover every repo.json read/modify/write.
func TestP6FSAboutSharesRequirementLock(t *testing.T) {
	st, err := OpenFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := domain.HashContent([]byte("p6-lock"))
	if _, err = st.PutRepo(ctx, domain.Repo{ID: id}); err != nil {
		t.Fatal(err)
	}
	if err = st.RequireDocumentIdentity(ctx, id, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	lock := st.refLock(id, domain.RefBranch, "")
	lock.Lock()
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() { close(started); done <- st.UpdateRepoAbout(ctx, id, "new", "", nil) }()
	<-started
	select {
	case err := <-done:
		lock.Unlock()
		t.Fatalf("About bypassed metadata lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	lock.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("About did not finish after unlock")
	}
	r, err := st.GetRepo(ctx, id)
	if err != nil || r.RequiredDocIdentity != domain.DocumentIdentityRootV1 || r.Description != "new" {
		t.Fatalf("lost metadata: %+v %v", r, err)
	}
}
