//go:build postgres

package app

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type settingsCodexFixturePG struct {
	service *Service
	storage *store.PostgresStore
	team    teamFixture
	repo    domain.ContentHash
	ctx     context.Context
}

func settingsCodexPG(test *testing.T) settingsCodexFixturePG {
	test.Helper()
	service, storage, repo := collaborationPG(test)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	test.Cleanup(cancel)
	fixture := settingsCodexFixturePG{service: service, storage: storage, team: makeTeamFixture(test, storage), repo: repo, ctx: ctx}
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: fixture.team.repository.ID, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	if err := storage.AddMember(ctx, domain.Membership{RepositoryID: fixture.team.repository.ID, UserID: fixture.team.member.ID, Role: domain.RoleMaintainer}); err != nil {
		test.Fatal(err)
	}
	webhook := "https://example.test/settings-codex"
	if _, err := fixture.team.identity.UpdateRepositorySettings(ctx, fixture.team.owner.ID, fixture.team.repository.ID, RepositoryPatch{WebhookURL: &webhook}); err != nil {
		test.Fatal(err)
	}
	return fixture
}

func settingsCodexBundlePG(kind, content string) domain.SettingsBundle {
	return domain.SettingsBundle{
		Kind:      kind,
		Files:     []domain.SettingsFile{{Path: "config.toml", ContentB64: base64.StdEncoding.EncodeToString([]byte(content))}},
		UpdatedAt: time.Unix(1, 0).UTC(),
	}
}

type settingsCodexStatePG struct {
	bundles       map[string]domain.SettingsBundle
	revision      domain.RepositoryRevision
	audits        []domain.OrganizationAuditEvent
	notifications []domain.NotificationJob
	snapshots     []domain.Snapshot
}

func settingsCodexImagePG(test *testing.T, fixture settingsCodexFixturePG) settingsCodexStatePG {
	test.Helper()
	image := settingsCodexStatePG{bundles: map[string]domain.SettingsBundle{}}
	owner := inbound.WithRepositoryActor(fixture.ctx, fixture.team.owner.ID)
	for _, kind := range []string{"claude", "agents", "codex"} {
		bundle, err := fixture.service.GetSettings(owner, fixture.repo, kind)
		if errors.Is(err, domain.ErrNotFound) && reflect.DeepEqual(bundle, domain.SettingsBundle{}) {
			continue
		}
		if err != nil {
			test.Fatal(kind, err)
		}
		image.bundles[kind] = bundle
	}
	var err error
	if image.revision, err = fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo); err != nil {
		test.Fatal(err)
	}
	if image.audits, err = fixture.storage.ListOrganizationAudit(fixture.ctx, fixture.team.organization.ID, 100); err != nil {
		test.Fatal(err)
	}
	if image.notifications, err = fixture.storage.ListNotifications(fixture.ctx, fixture.team.repository.ID); err != nil {
		test.Fatal(err)
	}
	if image.snapshots, err = fixture.storage.ListSnapshots(fixture.ctx, fixture.repo, ""); err != nil {
		test.Fatal(err)
	}
	return image
}

func settingsCodexEffectsPG(test *testing.T, fixture settingsCodexFixturePG, before, after settingsCodexStatePG, graphDelta uint64, actor, action string) {
	test.Helper()
	expectedRevision := before.revision
	expectedRevision.Graph += graphDelta
	if after.revision != expectedRevision {
		test.Fatalf("revision=%+v, want %+v", after.revision, expectedRevision)
	}
	if !reflect.DeepEqual(after.notifications, before.notifications) {
		test.Fatal("settings operation changed the notification outbox")
	}
	if len(after.audits) != len(before.audits)+1 {
		test.Fatalf("audit count=%d, want %d", len(after.audits), len(before.audits)+1)
	}
	previous := make(map[string]domain.OrganizationAuditEvent, len(before.audits))
	for _, event := range before.audits {
		previous[event.ID] = event
	}
	added := 0
	for _, event := range after.audits {
		if prior, exists := previous[event.ID]; exists {
			if !reflect.DeepEqual(event, prior) {
				test.Fatal("settings operation changed an existing audit event")
			}
			continue
		}
		added++
		if event.ActorID != actor || event.Action != action || event.OrganizationID != fixture.team.organization.ID || event.TargetType != "repository" || event.TargetID != fixture.team.repository.ID {
			test.Fatalf("incorrect settings audit: %+v", event)
		}
	}
	if added != 1 {
		test.Fatalf("new audit events=%d, want 1", added)
	}
}

type settingsCodexFailurePG struct {
	*store.PostgresStore
	bodyError   error
	auditError  error
	bodyWrites  int
	auditWrites int
}

func (storage *settingsCodexFailurePG) PutSettingsBundle(ctx context.Context, repo domain.ContentHash, bundle domain.SettingsBundle) error {
	if !storage.InTransaction(ctx) {
		return errors.New("settings body escaped its PostgreSQL transaction")
	}
	if err := storage.PostgresStore.PutSettingsBundle(ctx, repo, bundle); err != nil {
		return err
	}
	storage.bodyWrites++
	return storage.bodyError
}

func (storage *settingsCodexFailurePG) PutSettingsObject(ctx context.Context, repo, hash domain.ContentHash, bundle domain.SettingsBundle) error {
	if !storage.InTransaction(ctx) {
		return errors.New("settings object escaped its PostgreSQL transaction")
	}
	if err := storage.PostgresStore.PutSettingsObject(ctx, repo, hash, bundle); err != nil {
		return err
	}
	storage.bodyWrites++
	return storage.bodyError
}

func (storage *settingsCodexFailurePG) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	if !storage.InTransaction(ctx) {
		return errors.New("settings audit escaped its PostgreSQL transaction")
	}
	if err := storage.PostgresStore.AppendOrganizationAudit(ctx, event); err != nil {
		return err
	}
	storage.auditWrites++
	return storage.auditError
}

func TestPGSettingsCodexThreeKindsPublicRoundTripAndIsolation(test *testing.T) {
	fixture := settingsCodexPG(test)
	for _, actor := range []string{fixture.team.owner.ID, fixture.team.member.ID} {
		for _, kind := range []string{"claude", "agents", "codex"} {
			before := settingsCodexImagePG(test, fixture)
			bundle := settingsCodexBundlePG(kind, test.Name()+actor+kind)
			ctx := inbound.WithRepositoryActor(fixture.ctx, actor)
			if err := fixture.service.PutSettings(ctx, fixture.repo, bundle); err != nil {
				test.Fatal(kind, actor, err)
			}
			after := settingsCodexImagePG(test, fixture)
			settingsCodexEffectsPG(test, fixture, before, after, 1, actor, "repository.settings.updated")
			got, err := fixture.service.GetSettings(ctx, fixture.repo, kind)
			if err != nil || got.Kind != kind || !reflect.DeepEqual(got.Files, bundle.Files) || !got.UpdatedAt.After(bundle.UpdatedAt) {
				test.Fatalf("%s public roundtrip: %+v %v", kind, got, err)
			}
			if !reflect.DeepEqual(got, after.bundles[kind]) {
				test.Fatal("public reads disagreed about stored settings")
			}
			for otherKind, previous := range before.bundles {
				if otherKind != kind && !reflect.DeepEqual(after.bundles[otherKind], previous) {
					test.Fatalf("writing %s changed %s", kind, otherKind)
				}
			}
			if !reflect.DeepEqual(after.snapshots, before.snapshots) {
				test.Fatal("latest settings write changed snapshots")
			}
			if current := settingsCodexImagePG(test, fixture); !reflect.DeepEqual(current, after) {
				test.Fatal("public settings read produced durable effects")
			}
		}
	}
}

func TestPGSettingsCodexWritesRecheckCurrentAuthority(test *testing.T) {
	for _, mutation := range []string{"role", "remove", "archive", "settings-policy"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := settingsCodexPG(test)
			member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
			before := settingsCodexImagePG(test, fixture)
			baseline := settingsCodexBundlePG("codex", test.Name()+" baseline")
			if err := fixture.service.PutSettings(member, fixture.repo, baseline); err != nil {
				test.Fatal("current maintainer membership could not write codex settings", err)
			}
			written := settingsCodexImagePG(test, fixture)
			settingsCodexEffectsPG(test, fixture, before, written, 1, fixture.team.member.ID, "repository.settings.updated")
			if err := authorityScopeMutation(fixture.ctx, fixture.team.identity, fixture.team, mutation); err != nil {
				test.Fatal(err)
			}
			revoked := settingsCodexImagePG(test, fixture)
			if !reflect.DeepEqual(revoked.bundles, written.bundles) {
				test.Fatal("authority mutation changed existing settings")
			}
			if err := fixture.service.PutSettings(member, fixture.repo, settingsCodexBundlePG("codex", "denied replacement")); !errors.Is(err, domain.ErrForbidden) {
				test.Fatalf("codex write retained obsolete authority: %v", err)
			}
			if after := settingsCodexImagePG(test, fixture); !reflect.DeepEqual(after, revoked) {
				test.Fatal("denied codex write changed settings, revisions, audits, outbox or snapshots")
			}
			if mutation == "settings-policy" {
				owner := inbound.WithRepositoryActor(fixture.ctx, fixture.team.owner.ID)
				if err := fixture.service.PutSettings(owner, fixture.repo, settingsCodexBundlePG("codex", "owner replacement")); err != nil {
					test.Fatal("owner-only policy also denied the current owner", err)
				}
				settingsCodexEffectsPG(test, fixture, revoked, settingsCodexImagePG(test, fixture), 1, fixture.team.owner.ID, "repository.settings.updated")
			}
		})
	}
}

func TestPGSettingsCodexRejectedWritesAndRollbackHaveNoEffects(test *testing.T) {
	fixture := settingsCodexPG(test)
	member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	if err := fixture.service.PutSettings(member, fixture.repo, settingsCodexBundlePG("codex", "preserved baseline")); err != nil {
		test.Fatal(err)
	}
	invalidKind := settingsCodexBundlePG("unsupported", "invalid kind")
	invalidBody := settingsCodexBundlePG("codex", "invalid body")
	invalidBody.Files[0].ContentB64 = "not-base64!"
	bodyError := errors.New("injected settings body failure after SQL write")
	auditError := errors.New("injected settings audit failure after SQL write")
	for _, scenario := range []struct {
		name        string
		actor       string
		bundle      domain.SettingsBundle
		bodyError   error
		auditError  error
		wantError   error
		bodyWrites  int
		auditWrites int
	}{
		{name: "missing-actor", bundle: settingsCodexBundlePG("codex", "anonymous"), wantError: domain.ErrUnauthorized},
		{name: "non-member", actor: fixture.team.outsider.ID, bundle: settingsCodexBundlePG("codex", "outsider"), wantError: domain.ErrForbidden},
		{name: "invalid-kind", actor: fixture.team.member.ID, bundle: invalidKind, wantError: domain.ErrIntegrity},
		{name: "invalid-body", actor: fixture.team.member.ID, bundle: invalidBody, wantError: domain.ErrIntegrity},
		{name: "body-error", actor: fixture.team.member.ID, bundle: settingsCodexBundlePG("codex", "rolled back body"), bodyError: bodyError, wantError: bodyError, bodyWrites: 1},
		{name: "audit-error", actor: fixture.team.member.ID, bundle: settingsCodexBundlePG("codex", "rolled back revision and audit"), auditError: auditError, wantError: auditError, bodyWrites: 1, auditWrites: 1},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			before := settingsCodexImagePG(test, fixture)
			failure := &settingsCodexFailurePG{PostgresStore: fixture.storage, bodyError: scenario.bodyError, auditError: scenario.auditError}
			service := NewService(failure, failure, nil, fixture.service.engine, failure)
			ctx := inbound.WithRepositoryActor(fixture.ctx, scenario.actor)
			if err := service.PutSettings(ctx, fixture.repo, scenario.bundle); !errors.Is(err, scenario.wantError) {
				test.Fatalf("write error=%v, want %v", err, scenario.wantError)
			}
			if failure.bodyWrites != scenario.bodyWrites || failure.auditWrites != scenario.auditWrites {
				test.Fatalf("SQL body/audit writes=%d/%d, want %d/%d", failure.bodyWrites, failure.auditWrites, scenario.bodyWrites, scenario.auditWrites)
			}
			if after := settingsCodexImagePG(test, fixture); !reflect.DeepEqual(after, before) {
				test.Fatal("failed public write left durable effects")
			}
			if scenario.name == "invalid-kind" {
				got, err := fixture.service.GetSettings(member, fixture.repo, scenario.bundle.Kind)
				if !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(got, domain.SettingsBundle{}) {
					test.Fatalf("invalid kind read returned payload: %+v %v", got, err)
				}
			}
		})
	}
}

func TestPGSettingsCodexImmutableObjectsAndScopedAttachments(test *testing.T) {
	fixture := settingsCodexPG(test)
	member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	if err := fixture.service.PutSettings(member, fixture.repo, settingsCodexBundlePG("codex", "current defaults")); err != nil {
		test.Fatal(err)
	}
	bundle := settingsCodexBundlePG("codex", test.Name()+" immutable attachment")
	bundle.UpdatedBy = fixture.team.member.ID
	hash, err := domain.SettingsObjectHash(bundle)
	if err != nil {
		test.Fatal(err)
	}
	before := settingsCodexImagePG(test, fixture)
	got, err := fixture.service.GetSettingsObjectByHash(member, fixture.repo, hash)
	if !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(got, domain.SettingsBundle{}) {
		test.Fatalf("absent settings object returned payload: %+v %v", got, err)
	}
	if after := settingsCodexImagePG(test, fixture); !reflect.DeepEqual(after, before) {
		test.Fatal("absent settings object read produced durable effects")
	}
	duplicate := bundle
	duplicate.UpdatedAt = bundle.UpdatedAt.Add(time.Hour)
	duplicate.UpdatedBy = fixture.team.owner.ID
	for _, submitted := range []domain.SettingsBundle{bundle, duplicate} {
		before = settingsCodexImagePG(test, fixture)
		if err := fixture.service.PutSettingsObject(member, fixture.repo, hash, submitted); err != nil {
			test.Fatal(err)
		}
		after := settingsCodexImagePG(test, fixture)
		settingsCodexEffectsPG(test, fixture, before, after, 0, fixture.team.member.ID, "repository.settings.object.stored")
		if !reflect.DeepEqual(after.bundles, before.bundles) || !reflect.DeepEqual(after.snapshots, before.snapshots) {
			test.Fatal("immutable object upload changed defaults or snapshots")
		}
		got, err := fixture.service.GetSettingsObjectByHash(member, fixture.repo, hash)
		if err != nil || !reflect.DeepEqual(got, bundle) {
			test.Fatalf("immutable object was replaced: %+v %v", got, err)
		}
		if current := settingsCodexImagePG(test, fixture); !reflect.DeepEqual(current, after) {
			test.Fatal("object read produced durable effects")
		}
	}
	for _, rejection := range []string{"hash-mismatch", "invalid-kind", "body-error", "audit-error"} {
		test.Run(rejection, func(test *testing.T) {
			candidate := settingsCodexBundlePG("codex", test.Name())
			if rejection == "invalid-kind" {
				candidate.Kind = "unsupported"
			}
			candidateHash, err := domain.SettingsObjectHash(candidate)
			if err != nil {
				test.Fatal(err)
			}
			failure := &settingsCodexFailurePG{PostgresStore: fixture.storage}
			wantError := domain.ErrIntegrity
			if rejection == "hash-mismatch" {
				candidateHash = hash
			}
			if rejection == "body-error" || rejection == "audit-error" {
				wantError = errors.New("injected object " + rejection)
				if rejection == "body-error" {
					failure.bodyError = wantError
				} else {
					failure.auditError = wantError
				}
			}
			before := settingsCodexImagePG(test, fixture)
			service := NewService(failure, failure, nil, fixture.service.engine, failure)
			if err := service.PutSettingsObject(member, fixture.repo, candidateHash, candidate); !errors.Is(err, wantError) {
				test.Fatalf("object write error=%v, want %v", err, wantError)
			}
			expectedBodyWrites, expectedAuditWrites := 0, 0
			if rejection == "body-error" || rejection == "audit-error" {
				expectedBodyWrites = 1
			}
			if rejection == "audit-error" {
				expectedAuditWrites = 1
			}
			if failure.bodyWrites != expectedBodyWrites || failure.auditWrites != expectedAuditWrites {
				test.Fatalf("object SQL body/audit writes=%d/%d, want %d/%d", failure.bodyWrites, failure.auditWrites, expectedBodyWrites, expectedAuditWrites)
			}
			if after := settingsCodexImagePG(test, fixture); !reflect.DeepEqual(after, before) {
				test.Fatal("rejected object upload left durable effects")
			}
			got, err := fixture.service.GetSettingsObjectByHash(member, fixture.repo, candidateHash)
			if rejection == "hash-mismatch" {
				if err != nil || !reflect.DeepEqual(got, bundle) {
					test.Fatalf("forged object replaced the original: %+v %v", got, err)
				}
			} else if !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(got, domain.SettingsBundle{}) {
				test.Fatalf("failed object upload remained readable: %+v %v", got, err)
			}
		})
	}
	otherRepo := hh(test.Name() + " other context repository")
	otherRepository, err := fixture.team.identity.CreateRepository(fixture.ctx, fixture.team.owner, "foreign-codex-settings")
	if err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.AddMember(fixture.ctx, domain.Membership{RepositoryID: otherRepository.ID, UserID: fixture.team.member.ID, Role: domain.RoleMaintainer}); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.PutRepo(fixture.ctx, domain.Repo{ID: otherRepo, RepositoryID: otherRepository.ID, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	foreign := settingsCodexBundlePG("codex", test.Name()+" foreign attachment")
	foreignHash, err := domain.SettingsObjectHash(foreign)
	if err != nil {
		test.Fatal(err)
	}
	if err := fixture.service.PutSettingsObject(member, otherRepo, foreignHash, foreign); err != nil {
		test.Fatal(err)
	}
	before = settingsCodexImagePG(test, fixture)
	got, err = fixture.service.GetSettingsObjectByHash(member, fixture.repo, foreignHash)
	if !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(got, domain.SettingsBundle{}) {
		test.Fatalf("foreign repository object leaked: %+v %v", got, err)
	}
	if after := settingsCodexImagePG(test, fixture); !reflect.DeepEqual(after, before) {
		test.Fatal("foreign object read produced durable effects")
	}
	for _, attachment := range []struct {
		name    string
		hash    domain.ContentHash
		allowed bool
	}{
		{name: "missing", hash: hh(test.Name() + " missing attachment")},
		{name: "foreign", hash: foreignHash},
		{name: "owned", hash: hash, allowed: true},
	} {
		test.Run(attachment.name, func(test *testing.T) {
			before := settingsCodexImagePG(test, fixture)
			doc := makeCommitDoc(test, test.Name())
			snapshot := domain.Snapshot{ID: doc.Hash, RepoID: fixture.repo, Branch: "main", DocHash: doc.Hash, CodexSettings: attachment.hash, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull}
			output, err := fixture.service.Commit(member, inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{doc}, Snapshots: []domain.Snapshot{snapshot}})
			if !attachment.allowed {
				if !errors.Is(err, domain.ErrIntegrity) || output != (inbound.CommitOutput{}) {
					test.Fatalf("invalid attachment returned success or partial output: %+v %v", output, err)
				}
				if after := settingsCodexImagePG(test, fixture); !reflect.DeepEqual(after, before) {
					test.Fatal("invalid attachment changed durable repository state")
				}
				if _, err := fixture.storage.GetDoc(fixture.ctx, fixture.repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
					test.Fatalf("invalid attachment partially stored its document: %v", err)
				}
				got, err := fixture.service.GetSnapshot(member, fixture.repo, snapshot.ID)
				if !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(got, domain.Snapshot{}) {
					test.Fatalf("invalid attachment partially stored its snapshot: %+v %v", got, err)
				}
				return
			}
			if err != nil || output != (inbound.CommitOutput{StoredDocs: 1, StoredSnapshots: 1}) {
				test.Fatalf("owned codex attachment output: %+v %v", output, err)
			}
			after := settingsCodexImagePG(test, fixture)
			settingsCodexEffectsPG(test, fixture, before, after, 0, fixture.team.member.ID, "context.objects.stored")
			if !reflect.DeepEqual(after.bundles, before.bundles) || len(after.snapshots) != len(before.snapshots)+1 {
				test.Fatal("attachment commit changed defaults or stored the wrong snapshot count")
			}
			stored, err := fixture.service.GetSnapshot(member, fixture.repo, snapshot.ID)
			if err != nil || stored.CodexSettings != hash || stored.ClaudeSettings != "" || stored.AgentsSettings != "" {
				test.Fatalf("public snapshot lost its codex attachment: %+v %v", stored, err)
			}
			got, err := fixture.service.GetSettingsObjectByHash(member, fixture.repo, stored.CodexSettings)
			if err != nil || !reflect.DeepEqual(got, bundle) {
				test.Fatalf("public snapshot attachment read: %+v %v", got, err)
			}
		})
	}
}
