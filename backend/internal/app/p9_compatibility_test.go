package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func p9Tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[path] = string(raw)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestP9IdentityAdminFencesBeforeEffects(t *testing.T) {
	modes := []string{"settings", "transfer", "member", "remove-member", "invite", "accept-invite", "revoke-invite", "backfill", "team-grant", "team-revoke", "team-member", "team-remove", "team-delete", "organization-member", "organization-remove", "organization-policy", "offboard", "break-glass", "namespace-rename", "namespace-transfer", "notification-list", "notification-retry"}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			st := store.NewFSStore(dir)
			f := makeTeamFixture(t, st)
			s := f.identity
			ctx := systemTestContext()
			if err := st.AddMember(ctx, domain.Membership{RepositoryID: f.repository.ID, UserID: f.member.ID, Role: domain.RolePuller, CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if err := s.SetTeamRepository(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.repository.ID, domain.RolePuller); err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.member.ID, domain.TeamMember); err != nil {
				t.Fatal(err)
			}
			inv, err := s.Invite(ctx, f.owner.ID, f.repository.ID, "", domain.RolePuller, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := s.GetOrganizationPolicy(ctx, f.owner.ID, f.organization.ID)
			if err != nil {
				t.Fatal(err)
			}
			policy.BreakGlassEnabled = true
			policy.BreakGlassMaxMinutes = 30
			policy, err = s.UpdateOrganizationPolicy(ctx, f.owner.ID, policy)
			if err != nil {
				t.Fatal(err)
			}
			dest, err := s.CreateOrganization(ctx, f.owner, "Destination", "dest-"+domain.NewID("")[:10])
			if err != nil {
				t.Fatal(err)
			}
			repo := hh(t.Name())
			if _, err = st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: f.repository.ID}); err != nil {
				t.Fatal(err)
			}
			if err = st.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
				t.Fatal(err)
			}
			if mode == "backfill" {
				r := f.repository
				r.Slug = ""
				if err = st.CreateRepository(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "notification-list" || mode == "notification-retry" {
				r := f.repository
				r.WebhookURL = "https://example.test/synthetic"
				if err := st.CreateRepository(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			before := p9Tree(t, dir)
			ctx = inbound.WithDocumentIdentities(ctx, []domain.DocumentIdentity{domain.DocumentIdentityLegacy}) // explicit old peer, independent of binary readiness
			switch mode {
			case "notification-list":
				_, err = s.ListNotifications(ctx, f.owner.ID, f.repository.ID)
			case "notification-retry":
				err = s.RetryNotification(ctx, f.owner.ID, f.repository.ID, "synthetic")
			case "settings":
				v := true
				_, err = s.UpdateRepositorySettings(ctx, f.owner.ID, f.repository.ID, RepositoryPatch{Archived: &v})
			case "transfer":
				_, err = s.TransferOwnership(ctx, f.owner.ID, f.repository.ID, f.member.ID)
			case "member":
				err = s.UpdateMemberRole(ctx, f.owner.ID, f.repository.ID, f.member.ID, domain.RoleViewer)
			case "remove-member":
				err = s.RemoveMember(ctx, f.owner.ID, f.repository.ID, f.member.ID)
			case "invite":
				_, err = s.Invite(ctx, f.owner.ID, f.repository.ID, "", domain.RolePuller, time.Hour)
			case "accept-invite":
				_, err = s.AcceptInvite(ctx, f.outsider, inv.Token)
			case "revoke-invite":
				err = s.RevokeInvite(ctx, f.owner.ID, f.repository.ID, inv.Token)
			case "backfill":
				_, err = s.backfillRepository(ctx, f.repository)
			case "team-grant":
				err = s.SetTeamRepository(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.repository.ID, domain.RoleMember)
			case "team-revoke":
				err = s.RemoveTeamRepository(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.repository.ID)
			case "team-member":
				err = s.UpdateTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.member.ID, domain.TeamMaintainer)
			case "team-remove":
				err = s.RemoveTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.member.ID)
			case "team-delete":
				err = s.DeleteTeam(ctx, f.owner.ID, f.organization.ID, f.team.ID)
			case "organization-member":
				err = s.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationAdmin)
			case "organization-remove":
				err = s.RemoveOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID)
			case "organization-policy":
				_, err = s.UpdateOrganizationPolicy(ctx, f.owner.ID, policy)
			case "offboard":
				err = s.OffboardOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, "revoke")
			case "break-glass":
				_, err = s.CreateBreakGlassGrant(ctx, f.owner.ID, f.organization.ID, f.repository.ID, "synthetic audit", 5)
			case "namespace-rename":
				_, err = s.RenameCollaborationSpace(ctx, f.owner.ID, "organization", f.organization.ID, f.organization.Slug, "renamed-"+domain.NewID("")[:8])
			case "namespace-transfer":
				_, err = s.TransferRepositoryNamespace(ctx, f.owner.ID, f.repository.ID, f.repository.OwnerNamespaceID, f.repository.Slug, dest.Slug)
			}
			if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
				t.Fatalf("admitted incompatible mutation: %v", err)
			}
			if after := p9Tree(t, dir); !reflect.DeepEqual(before, after) {
				t.Fatal("denied mutation changed persisted state")
			}
			if _, err := s.GetRepository(ctx, f.repository.ID); err != nil {
				t.Fatal("safe discovery", err)
			}
			if _, err := s.ListRepositories(ctx, f.owner.ID); err != nil {
				t.Fatal("safe listing", err)
			}
			if after := p9Tree(t, dir); !reflect.DeepEqual(before, after) {
				t.Fatal("discovery backfill wrote")
			}
			v := true
			if _, err := s.UpdateRepositorySettings(ctx, f.outsider.ID, f.repository.ID, RepositoryPatch{Archived: &v}); !errors.Is(err, domain.ErrForbidden) {
				t.Fatal("declaration granted auth", err)
			}
		})
	}
}

type p9ReadBoundaryKey struct{}
type p9AggregateStore struct {
	outbound.MetadataStore
	repos []domain.Repo
	calls int
}

func (s *p9AggregateStore) WithinRepository(context.Context, domain.ContentHash, func(context.Context) error) error {
	panic("unexpected write")
}
func (s *p9AggregateStore) WithinReadSnapshot(ctx context.Context, fn func(context.Context) error) error {
	return fn(context.WithValue(ctx, p9ReadBoundaryKey{}, true))
}
func (s *p9AggregateStore) ListRepos(ctx context.Context, _ string) ([]domain.Repo, error) {
	if ctx.Value(p9ReadBoundaryKey{}) != true {
		panic("unpinned inventory")
	}
	return s.repos, nil
}
func (s *p9AggregateStore) ListSnapshots(ctx context.Context, id domain.ContentHash, _ string) ([]domain.Snapshot, error) {
	if ctx.Value(p9ReadBoundaryKey{}) != true || id != s.repos[0].ID {
		panic("unsupported or unpinned payload")
	}
	s.calls++
	return []domain.Snapshot{{CreatedAt: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)}}, nil
}
func TestP9CoherentDiscoveryAggregates(t *testing.T) {
	st := &p9AggregateStore{repos: []domain.Repo{{ID: hh("legacy"), RepositoryID: "legacy"}, {ID: hh("root"), RepositoryID: "root", RequiredDocIdentity: domain.DocumentIdentityRootV1}}}
	svc := NewService(st, nil, nil, nil, nil)
	ctx := context.Background()
	if repos, err := svc.ListRepos(ctx, "default"); err != nil || len(repos) != 2 {
		t.Fatal(repos, err)
	}
	if got, err := svc.Contributions(ctx, []string{"legacy", "root"}); err != nil || got["2026-10-08"] != 1 {
		t.Fatal(got, err)
	}
	got, err := svc.Activity(ctx, []domain.Repository{{ID: "legacy"}, {ID: "root", CreatedAt: time.Now()}})
	if err != nil || len(got) != 1 || got[0].CommitTotal != 1 || len(got[0].Created) != 1 {
		t.Fatal(got, err)
	}
	st.repos[1].RequiredDocIdentity = "unknown"
	if _, err := svc.Contributions(ctx, []string{"root"}); err == nil {
		t.Fatal("malformed requirement ignored")
	}
}
func TestP9WorkerDeclarationIsActualAndNonprivileged(t *testing.T) {
	svc := &Service{}
	root := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
	for _, ctx := range []context.Context{context.Background(), inbound.WithRepositoryActor(context.Background(), "actor"), inbound.WithSystemActor(context.Background())} {
		user, system := inbound.RepositoryActor(ctx)
		got := svc.DocumentIdentityWorkerContext(inbound.WithDocumentIdentities(ctx, root))
		u, s := inbound.RepositoryActor(got)
		if u != user || s != system || hasDocumentIdentity(inbound.DocumentIdentities(got), domain.DocumentIdentityRootV1) {
			t.Fatal("worker declaration escalated")
		}
	}
	if err := svc.ConfigureConversationRootPublication(true); !errors.Is(err, domain.ErrRootPublicationDisabled) {
		t.Fatal(err)
	}
}

func TestP9IdentityAdminSupportedPeerAndAuthorization(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	f := makeTeamFixture(t, st)
	repo := hh(t.Name())
	ctx := inbound.WithDocumentIdentities(context.Background(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: f.repository.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	archived := true
	svc := NewService(st, st, nil, nil, st)
	_, err := f.identity.UpdateRepositorySettings(ctx, f.owner.ID, f.repository.ID, RepositoryPatch{Archived: &archived})
	if hasDocumentIdentity(svc.DocumentIdentitiesSupported(), domain.DocumentIdentityRootV1) {
		if err != nil {
			t.Fatal("supported authorized peer rejected", err)
		}
	} else if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("unsupported binary admitted root administration", err)
	}
	current, readErr := st.GetRepository(ctx, f.repository.ID)
	if readErr != nil || current.Archived != hasDocumentIdentity(svc.DocumentIdentitiesSupported(), domain.DocumentIdentityRootV1) {
		t.Fatal("wrong persisted administration result", readErr)
	}
	archived = false
	if _, err := f.identity.UpdateRepositorySettings(ctx, f.outsider.ID, f.repository.ID, RepositoryPatch{Archived: &archived}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("capability declaration authorized outsider", err)
	}
	after, err := st.GetRepository(ctx, f.repository.ID)
	if err != nil || !reflect.DeepEqual(current, after) {
		t.Fatal("unauthorized attempt changed repository", err)
	}
}
