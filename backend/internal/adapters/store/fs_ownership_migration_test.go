package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func legacyOwnershipFixture(t *testing.T, dir string) (string, []domain.ContentHash) {
	t.Helper()
	boundary := "ws_" + strings.Repeat("a", 32)
	put := func(path string, v any) {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err = writeAtomic(filepath.Join(dir, path), data); err != nil {
			t.Fatal(err)
		}
	}
	put("workspaces/"+boundary+".json", map[string]any{"id": boundary, "name": "Project", "slug": "project", "owner_id": "owner", "owner_username": "alice", "visibility": "private", "secrets_policy": "owner"})
	member := map[string]any{"workspace_id": boundary, "user_id": "reader", "role": "puller"}
	put("members/"+boundary+"/"+opaqueName("reader")+".json", member)
	// A stale pre-hashed membership must not regain its former owner privilege.
	member["role"] = "owner"
	put("members/"+boundary+"/reader.json", member)
	var ids []domain.ContentHash
	for _, name := range []string{"api", "web"} {
		remote := "https://example.test/alice/project/" + name
		id := domain.HashContent([]byte(remote))
		ids = append(ids, id)
		put("repos/"+hexOf(id)+"/repo.json", map[string]any{"id": id, "remote_url": remote, "workspace_id": boundary, "default_branch": "main"})
		put("repos/"+hexOf(id)+"/objects/sentinel", map[string]string{"text": "workspace enterprise original content"})
	}
	put("invites/inv_"+strings.Repeat("b", 32)+".json", map[string]any{"token": "inv_" + strings.Repeat("b", 32), "workspace_id": boundary, "created_by": "owner", "role": "member", "status": "pending"})
	return boundary, ids
}

func TestFSOwnershipMigrationPreservesContentAndRestartsWithoutReapplying(t *testing.T) {
	dir := t.TempDir()
	original, ids := legacyOwnershipFixture(t, dir)
	contentPath := filepath.Join(dir, "repos", hexOf(ids[0]), "objects/sentinel")
	content, err := os.ReadFile(contentPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenFSStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	seen := map[string]bool{}
	for _, id := range ids {
		r, e := st.GetRepo(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		seen[r.RepositoryID] = true
		boundary, e := st.GetRepository(ctx, r.RepositoryID)
		if e != nil {
			t.Fatal(e)
		}
		if boundary.Visibility != domain.VisibilityPrivate || boundary.SecretsPolicy != "owner" {
			t.Fatal("migration weakened policy")
		}
		members, e := st.ListMembers(ctx, r.RepositoryID)
		if e != nil {
			t.Fatal(e)
		}
		if len(members) != 1 || members[0].Role != domain.RolePuller {
			t.Fatalf("incorrect member migration: %+v", members)
		}
	}
	if len(seen) != 2 || !seen[original] {
		t.Fatal("did not flatten repository boundaries")
	}
	after, err := os.ReadFile(contentPath)
	if err != nil || string(after) != string(content) {
		t.Fatal("content object was changed")
	}
	var targets []string
	if err = readJSON(filepath.Join(dir, "repository-invite-targets", "inv_"+strings.Repeat("b", 32)+".json"), &targets); err != nil || len(targets) != 2 {
		t.Fatalf("invite scope lost: %v %v", targets, err)
	}
	if err = st.AddMember(ctx, domain.Membership{RepositoryID: original, UserID: "reader", Role: domain.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	st, err = OpenFSStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	members, err := st.ListMembers(ctx, original)
	if err != nil || len(members) != 1 || members[0].Role != domain.RoleViewer {
		t.Fatal("restart overwrote a post-migration permission change")
	}
}

func TestFSOwnershipMigrationRecoversAfterInterruptedMetadataWrite(t *testing.T) {
	dir := t.TempDir()
	_, ids := legacyOwnershipFixture(t, dir)
	if _, err := OpenFSStore(dir); err != nil {
		t.Fatal(err)
	}
	completed := filepath.Join(dir, "ownership-migration-v1.complete.json")
	var journal ownershipFileJournal
	if err := readJSON(completed, &journal); err != nil {
		t.Fatal(err)
	}
	// Reconstruct a crash between metadata renames: some writes reached disk,
	// others still have their old value or have not been created yet.
	for i, change := range journal.Changes {
		if i%2 != 0 {
			continue
		}
		path := filepath.Join(dir, change.Path)
		if len(change.Before) > 0 {
			if err := writeAtomic(path, change.Before); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(completed, filepath.Join(dir, "ownership-migration-v1.json")); err != nil {
		t.Fatal(err)
	}
	st, err := OpenFSStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		r, e := st.GetRepo(context.Background(), id)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = st.GetRepository(context.Background(), r.RepositoryID); e != nil {
			t.Fatal(e)
		}
	}
	if _, err = os.Stat(filepath.Join(dir, "ownership-migration-v1.complete.json")); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyOwnershipDecoderDoesNotRewriteText(t *testing.T) {
	data, err := decodeLegacyOwnership([]byte(`{"workspace_id":"old","enterprise_id":"company","kind":"enterprise","reason":"enterprise workspace remains original","nested":{"workspace_id":"old"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err = json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["repository_id"] != "old" || got["organization_id"] != "company" || got["kind"] != "organization" || got["reason"] != "enterprise workspace remains original" {
		t.Fatalf("incorrect legacy decoding: %s", data)
	}
}

func TestFSOwnershipMigrationPreservesCompanyAdministration(t *testing.T) {
	dir := t.TempDir()
	legacyOwnershipFixture(t, dir)
	organizationID, namespaceID := domain.NewID("ent_"), domain.NewID("ns_")
	put := func(path string, value any) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = writeAtomic(filepath.Join(dir, path), raw); err != nil {
			t.Fatal(err)
		}
	}
	put("enterprises/"+organizationID+".json", map[string]any{"id": organizationID, "namespace_id": namespaceID, "name": "Legacy Company", "slug": "legacy-company", "created_by": "owner"})
	namespace := map[string]any{"id": namespaceID, "slug": "legacy-company", "kind": "enterprise", "enterprise_id": organizationID}
	put("namespaces/by-id/"+namespaceID+".json", namespace)
	put("namespaces/by-slug/legacy-company.json", namespace)
	put("enterprise-members/"+organizationID+"/"+opaqueName("owner")+".json", map[string]any{"enterprise_id": organizationID, "user_id": "owner", "role": "owner"})
	put("enterprise-policies/"+organizationID+".json", map[string]any{"enterprise_id": organizationID, "workspace_creation": "members", "default_workspace_visibility": "private", "allow_public_workspaces": false, "break_glass_max_minutes": 60})
	st, err := OpenFSStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	organization, err := st.GetOrganization(ctx, organizationID)
	if err != nil || organization.NamespaceID != namespaceID || organization.Name != "Legacy Company" {
		t.Fatalf("company: %+v %v", organization, err)
	}
	policy, err := st.GetOrganizationPolicy(ctx, organizationID)
	if err != nil || policy.RepositoryCreation != domain.OrganizationRepositoryMembers || policy.AllowPublicRepositories {
		t.Fatalf("policy: %+v %v", policy, err)
	}
	members, err := st.ListOrganizationMembers(ctx, organizationID)
	if err != nil || len(members) != 1 || members[0].Role != domain.OrganizationOwner {
		t.Fatalf("members: %+v %v", members, err)
	}
	ns, err := st.GetNamespace(ctx, namespaceID)
	if err != nil || ns.Kind != domain.NamespaceOrganization || ns.OrganizationID != organizationID {
		t.Fatalf("namespace: %+v %v", ns, err)
	}
}

func TestLegacyOwnershipDecodePreservesLargeNumbers(t *testing.T) {
	data, err := decodeLegacyOwnership([]byte(`{"workspace_id":"old","revision":9007199254740993}`))
	if err != nil || !strings.Contains(string(data), `"revision":9007199254740993`) {
		t.Fatalf("metadata number changed: %s %v", data, err)
	}
}
