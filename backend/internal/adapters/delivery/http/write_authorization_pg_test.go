//go:build postgres

package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type pausedWriteBody struct {
	reader           *strings.Reader
	entered, release chan struct{}
	once             sync.Once
}

func (b *pausedWriteBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.reader.Read(p)
}
func (b *pausedWriteBody) Close() error { return nil }

func TestPGRepositoryWriteRevocation(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated test DSN required")
	}
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	other, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := st.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	ids := app.NewIdentityService(auth.NewDevVerifier(), st)
	revoker := app.NewIdentityService(auth.NewDevVerifier(), other)
	svc := app.NewService(st, st, nil, gitengine.NewEngine(st), st)
	handler := NewServer(svc, ids).Handler()
	for _, scenario := range []string{"direct/ref", "direct/settings", "organization/ref", "organization/settings", "team/ref", "team/settings", "archive/ref", "archive/settings", "policy/settings"} {
		t.Run(scenario, func(t *testing.T) {
			kind := strings.Split(scenario, "/")[1]
			owner, _, err := ids.Login(ctx, "dev:"+domain.NewID("reviewowner")+"@example.test", "review owner")
			if err != nil {
				t.Fatal(err)
			}
			actor, session, err := ids.Login(ctx, "dev:"+domain.NewID("reviewactor")+"@example.test", "review actor")
			if err != nil {
				t.Fatal(err)
			}
			var record domain.Repository
			organizationID := ""
			teamID := ""
			if strings.HasPrefix(scenario, "organization/") || strings.HasPrefix(scenario, "team/") {
				organization, e := ids.CreateOrganization(ctx, owner, "Review", "review-"+domain.NewID("")[:10])
				if e != nil {
					t.Fatal(e)
				}
				organizationID = organization.ID
				if e = ids.UpdateOrganizationMember(ctx, owner.ID, organizationID, actor.ID, domain.OrganizationOwner); e != nil {
					t.Fatal(e)
				}
				record, err = ids.CreateOrganizationRepository(ctx, owner, organizationID, "review")
			} else {
				record, err = ids.CreateRepository(ctx, owner, "review")
			}
			if err != nil {
				t.Fatal(err)
			}
			repo := domain.Repo{ID: domain.HashContent([]byte(record.ID)), RepositoryID: record.ID, RemoteURL: "https://review.example.test/" + record.ID + "/repo", DefaultBranch: "main"}
			if _, err := st.PutRepo(ctx, repo); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(scenario, "team/") {
				if err = ids.UpdateOrganizationMember(ctx, owner.ID, organizationID, actor.ID, domain.OrganizationMember); err != nil {
					t.Fatal(err)
				}
				team, e := ids.CreateTeam(ctx, owner.ID, organizationID, "Test", "test", "")
				if e != nil {
					t.Fatal(e)
				}
				teamID = team.ID
				if err = ids.UpdateTeamMember(ctx, owner.ID, organizationID, teamID, actor.ID, domain.TeamMember); err != nil {
					t.Fatal(err)
				}
				if err = ids.SetTeamRepository(ctx, owner.ID, organizationID, teamID, record.ID, domain.RoleMaintainer); err != nil {
					t.Fatal(err)
				}
			}
			if organizationID == "" {
				if err := st.AddMember(ctx, domain.Membership{RepositoryID: record.ID, UserID: actor.ID, Role: domain.RoleMaintainer, CreatedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			doc := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{{Seq: 1, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "review target " + record.ID}}}}}}
			raw, err := domain.CanonicalBytes(doc.CIR)
			if err != nil {
				t.Fatal(err)
			}
			doc.Hash = domain.HashContent(raw)
			if _, err := st.PutDoc(ctx, repo.ID, doc); err != nil {
				t.Fatal(err)
			}
			target := doc.Hash
			if err := st.PutSnapshot(ctx, domain.Snapshot{ID: target, DocHash: target, RepoID: repo.ID, CreatedAt: time.Now().UTC(), Provider: domain.ProviderUnknown, Fidelity: domain.FidelityFull}); err != nil {
				t.Fatal(err)
			}
			path := fmt.Sprintf("/api/v1/repos/%s/refs/tag/review-race", repo.ID)
			payload, _ := json.Marshal(map[string]any{"target": target})
			if kind == "settings" {
				path = fmt.Sprintf("/api/v1/repos/%s/settings/agents", repo.ID)
				payload = []byte(`{"files":[{"path":"review.md","content_b64":"cmV2aWV3"}]}`)
			}
			body := &pausedWriteBody{reader: strings.NewReader(string(payload)), entered: make(chan struct{}), release: make(chan struct{})}
			req := httptest.NewRequest("PUT", path, body)
			req.Header.Set("Authorization", "Bearer "+session.Token)
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); handler.ServeHTTP(response, req) }()
			select {
			case <-body.entered:
			case <-done:
				t.Fatalf("request rejected before body: %d %s", response.Code, response.Body.String())
			case <-time.After(10 * time.Second):
				t.Fatal("body read timeout")
			}
			if strings.HasPrefix(scenario, "archive/") {
				archived := true
				_, err = revoker.UpdateRepositorySettings(ctx, owner.ID, record.ID, app.RepositoryPatch{Archived: &archived})
			} else if strings.HasPrefix(scenario, "policy/") {
				policy := "owner"
				_, err = revoker.UpdateRepositorySettings(ctx, owner.ID, record.ID, app.RepositoryPatch{SettingsPolicy: &policy})
			} else if teamID != "" {
				err = revoker.RemoveTeamMember(ctx, owner.ID, organizationID, teamID, actor.ID)
			} else if organizationID == "" {
				err = revoker.RemoveMember(ctx, owner.ID, record.ID, actor.ID)
			} else {
				err = revoker.UpdateOrganizationMember(ctx, owner.ID, organizationID, actor.ID, domain.OrganizationMember)
			}
			if err != nil {
				close(body.release)
				<-done
				t.Fatal(err)
			}
			before, e := st.RepositoryRevision(ctx, repo.ID)
			if e != nil {
				t.Fatal(e)
			}
			beforeJobs, e := st.ListNotifications(ctx, record.ID)
			if e != nil {
				t.Fatal(e)
			}
			control := httptest.NewRequest("PUT", path, strings.NewReader(string(payload)))
			control.Header = req.Header.Clone()
			denied := httptest.NewRecorder()
			handler.ServeHTTP(denied, control)
			if denied.Code != 403 {
				close(body.release)
				<-done
				t.Fatalf("control request after revocation = %d %s", denied.Code, denied.Body.String())
			}
			close(body.release)
			<-done
			persisted := false
			if kind == "ref" {
				ref, e := st.GetRef(ctx, repo.ID, domain.RefTag, "review-race")
				persisted = e == nil && ref.Target == target
			} else {
				b, e := st.GetSettingsBundle(ctx, repo.ID, "agents")
				persisted = e == nil && len(b.Files) == 1
			}

			after, e := st.RepositoryRevision(ctx, repo.ID)
			if e != nil || before != after {
				t.Fatalf("rejected request changed revision: %+v -> %+v (%v)", before, after, e)
			}
			afterJobs, e := st.ListNotifications(ctx, record.ID)
			if e != nil || !reflect.DeepEqual(beforeJobs, afterJobs) {
				t.Fatal("rejected request changed outbox")
			}
			if response.Code != 403 || persisted {
				t.Fatalf("revoked user wrote after completed revocation: status=%d persisted=%v", response.Code, persisted)
			}
		})
	}
}
