package domain

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func migrationContainer(key, name string) Repository {
	return Repository{ID: "ws_" + strings.Repeat(key, 32), Name: name, Slug: name, OwnerID: "owner", OwnerUsername: "alice", Visibility: VisibilityPrivate, SecretsPolicy: "owner", SettingsPolicy: "owner", Archived: true}
}
func migrationRepo(w Repository, name string) Repo {
	remote := "https://example.test/alice/" + w.Slug
	if name != "" {
		remote += "/" + name
	}
	return Repo{ID: HashContent([]byte(remote)), RemoteURL: remote, RepositoryID: w.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/example/" + name}
}

func TestOwnershipMigrationPreservesIdentityPermissionsAndInviteScope(t *testing.T) {
	w := migrationContainer("1", "project")
	root, a, b := migrationRepo(w, ""), migrationRepo(w, "api"), migrationRepo(w, "web")
	m := Membership{RepositoryID: w.ID, UserID: "reader", Role: RolePuller}
	inv := Invite{Token: "inv_" + strings.Repeat("a", 32), RepositoryID: w.ID, CreatedBy: w.OwnerID, Role: RoleMember, Status: InvitePending, CreatedAt: time.Unix(1, 0)}
	plan, err := PlanRepositoryOwnership([]Repository{w}, []Repo{b, root, a}, []Membership{m}, []Invite{inv})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Repositories) != 3 || len(plan.Members) != 3 || len(plan.InviteTargets) != 3 {
		t.Fatalf("incomplete split: %+v", plan)
	}
	if plan.Repositories[0].ID != w.ID || plan.Repositories[0].Slug != w.Slug {
		t.Fatal("original two-segment repository identity moved")
	}
	for _, next := range plan.Repositories {
		if next.Visibility != w.Visibility || next.SecretsPolicy != w.SecretsPolicy || next.SettingsPolicy != w.SettingsPolicy || next.Archived != w.Archived || next.OwnerID != w.OwnerID {
			t.Fatal("repository security policy changed")
		}
	}
	for _, next := range plan.Members {
		if next.Role != m.Role || next.UserID != m.UserID {
			t.Fatal("collaborator privilege changed")
		}
	}
	for _, before := range []Repo{root, a, b} {
		found := false
		for _, after := range plan.Repos {
			if before.ID == after.ID {
				found = true
				after.RepositoryID = before.RepositoryID
				if !reflect.DeepEqual(after, before) {
					t.Fatal("content repository identity changed")
				}
			}
		}
		if !found {
			t.Fatal("lost content repository")
		}
	}
	for _, alias := range plan.Aliases {
		if alias.ContextRepoID == root.ID && alias.Path == w.Slug {
			return
		}
	}
	t.Fatal("original root path not retained")
}

func TestOwnershipMigrationIsOrderIndependentAndKeepsEstablishedNames(t *testing.T) {
	w, existing := migrationContainer("1", "project"), migrationContainer("2", "api")
	r := migrationRepo(w, "api")
	other := migrationRepo(existing, "")
	a, err := PlanRepositoryOwnership([]Repository{w, existing}, []Repo{r, other}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := PlanRepositoryOwnership([]Repository{existing, w}, []Repo{other, r}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("input order changed migration")
	}
	for _, next := range a.Repositories {
		if next.ID == w.ID && next.Slug != "project-api" {
			t.Fatalf("collision not resolved: %s", next.Slug)
		}
		if next.ID == existing.ID && next.Slug != "api" {
			t.Fatal("existing canonical name moved")
		}
	}
}

func TestOwnershipMigrationKeepsRenamedAndEmptyRepositoryAddresses(t *testing.T) {
	w := migrationContainer("1", "new-name")
	r := migrationRepo(w, "")
	r.RemoteURL = "https://example.test/old-owner/old-name"
	r.ID = HashContent([]byte(r.RemoteURL))
	empty := migrationContainer("2", "empty")
	plan, err := PlanRepositoryOwnership([]Repository{w, empty}, []Repo{r}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Repositories) != 2 || plan.Repos[0].ID != r.ID {
		t.Fatal("identity or empty repository lost")
	}
	paths := map[string]bool{}
	for _, alias := range plan.Aliases {
		paths[alias.Path] = true
	}
	if !paths["old-name"] || !paths["new-name"] || !paths["empty"] {
		t.Fatalf("missing aliases: %v", paths)
	}
	for _, alias := range plan.Aliases {
		if alias.Owner == "old-owner" && alias.Path == "old-name" && alias.ContextRepoID == r.ID && alias.NamespaceID == "" {
			return
		}
	}
	t.Fatal("historical owner handle no longer resolves the original ID")
}

func TestOwnershipMigrationNormalizesInvalidChildSlugWithoutChangingIdentity(t *testing.T) {
	w := migrationContainer("1", "project")
	r := migrationRepo(w, "invalid.child")
	plan, err := PlanRepositoryOwnership([]Repository{w}, []Repo{r}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Repositories) != 1 || !ValidRepositorySlug(plan.Repositories[0].Slug) || plan.Repos[0].ID != r.ID {
		t.Fatalf("invalid conversion: %+v", plan)
	}
	for _, alias := range plan.Aliases {
		if alias.Path == "project/invalid.child" && alias.ContextRepoID == r.ID {
			return
		}
	}
	t.Fatal("legacy child address was discarded")
}

func TestOwnershipMigrationRejectsAmbiguousAndCorruptSources(t *testing.T) {
	w := migrationContainer("1", "project")
	r := migrationRepo(w, "")
	for _, tc := range []struct {
		name         string
		repositories []Repository
		repos        []Repo
		members      []Membership
	}{
		{"duplicate boundary", []Repository{w, w}, []Repo{r}, nil},
		{"duplicate content", []Repository{w}, []Repo{r, r}, nil},
		{"missing boundary", nil, []Repo{r}, nil},
		{"corrupt role", []Repository{w}, []Repo{r}, []Membership{{RepositoryID: w.ID, UserID: "reader", Role: "admin-from-corrupt-input"}}},
		{"malformed URL", []Repository{w}, []Repo{{ID: r.ID, RepositoryID: w.ID, RemoteURL: "/relative"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PlanRepositoryOwnership(tc.repositories, tc.repos, tc.members, nil); err == nil {
				t.Fatal("unsafe migration accepted")
			}
		})
	}
}
