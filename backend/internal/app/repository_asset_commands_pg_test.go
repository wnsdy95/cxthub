//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type repositoryAssetStorePG interface {
	outbound.MetadataStore
	outbound.BlobStore
	outbound.RepositoryStore
	outbound.SecretsCASStore
	outbound.NotificationStore
}

func repositoryAssetCommandsPG(test *testing.T, storage repositoryAssetStorePG) *RepositoryAssetCommands {
	scope := &repositoryAssetScope{work: newRepositoryWork(storage, storage, ReadDocumentIdentities(storage, storage))}
	test.Helper()
	commands, err := NewRepositoryAssetCommands(RepositoryAssetCommandsConfig{
		Profile: RepositoryAssetsComplete, Store: storage, Scope: scope,
		Repositories: storage, SecretsCAS: storage, Notifications: storage,
	})
	if err != nil {
		test.Fatal(err)
	}
	return commands
}

type repositoryAssetFixturePG struct {
	settingsCodexFixturePG
	attachments []domain.ContentHash
}

func repositoryAssetsPG(test *testing.T) repositoryAssetFixturePG {
	test.Helper()
	fixture := repositoryAssetFixturePG{settingsCodexFixturePG: settingsCodexPG(test)}
	observer := authorityScopeObserver(test, fixture.ctx)
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := observer.Exec(cleanup, `DELETE FROM notification_outbox WHERE repository_id=$1`, fixture.team.repository.ID); err != nil {
			test.Error(err)
		}
	})
	commands := repositoryAssetCommandsPG(test, fixture.storage)
	member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	for _, kind := range []string{"claude", "agents", "codex"} {
		bundle := settingsCodexBundlePG(kind, test.Name()+" baseline "+kind)
		bundle.UpdatedBy = fixture.team.member.ID
		hash, err := domain.SettingsObjectHash(bundle)
		if err != nil {
			test.Fatal(err)
		}
		if err := commands.PutSettingsObject(member, fixture.repo, hash, bundle); err != nil {
			test.Fatal(err)
		}
		if err := commands.PutSettings(member, fixture.repo, bundle); err != nil {
			test.Fatal(err)
		}
		fixture.attachments = append(fixture.attachments, hash)
	}
	doc := makeCommitDoc(test, test.Name()+" attached settings")
	if _, err := fixture.storage.PutDoc(fixture.ctx, fixture.repo, doc); err != nil {
		test.Fatal(err)
	}
	snapshot := domain.Snapshot{
		ID: doc.Hash, RepoID: fixture.repo, Branch: "main", DocHash: doc.Hash,
		ClaudeSettings: fixture.attachments[0], AgentsSettings: fixture.attachments[1], CodexSettings: fixture.attachments[2],
		Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull,
	}
	if err := fixture.storage.PutSnapshot(fixture.ctx, snapshot); err != nil {
		test.Fatal(err)
	}
	if _, err := commands.SaveSecrets(member, inbound.SaveSecretsInput{
		RepoID: fixture.repo, ActorID: fixture.team.member.ID, Envelope: secretsTestEnvelope("aaaaaaaaaaaa"),
		Edit: domain.SecretsEdit{ExpectedRevision: "absent"},
	}); err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.AdvanceRepositoryRevision(fixture.ctx, fixture.repo, true); err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.AdvanceEvidenceRevision(fixture.ctx, fixture.repo); err != nil {
		test.Fatal(err)
	}
	return fixture
}

type repositoryAssetStatePG struct {
	metadata domain.Repo
	settings settingsCodexStatePG
	objects  map[domain.ContentHash]domain.SettingsBundle
	secrets  []byte
}

func repositoryAssetImagePG(test *testing.T, fixture repositoryAssetFixturePG, extra ...domain.ContentHash) repositoryAssetStatePG {
	test.Helper()
	image := repositoryAssetStatePG{
		settings: settingsCodexStatePG{bundles: map[string]domain.SettingsBundle{}},
		objects:  map[domain.ContentHash]domain.SettingsBundle{},
	}
	var err error
	if image.metadata, err = fixture.storage.GetRepo(fixture.ctx, fixture.repo); err != nil {
		test.Fatal(err)
	}
	for _, kind := range []string{"claude", "agents", "codex"} {
		bundle, err := fixture.storage.GetSettingsBundle(fixture.ctx, fixture.repo, kind)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			test.Fatal(err)
		}
		image.settings.bundles[kind] = bundle
	}
	for _, hash := range append(slices.Clone(fixture.attachments), extra...) {
		if hash == "" {
			continue
		}
		bundle, err := fixture.storage.GetSettingsObject(fixture.ctx, fixture.repo, hash)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			test.Fatal(err)
		}
		image.objects[hash] = bundle
	}
	if image.secrets, err = fixture.storage.GetSecretsEnvelope(fixture.ctx, fixture.repo); err != nil {
		test.Fatal(err)
	}
	if image.settings.revision, err = fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo); err != nil {
		test.Fatal(err)
	}
	if image.settings.audits, err = fixture.storage.ListOrganizationAudit(fixture.ctx, fixture.team.organization.ID, 100); err != nil {
		test.Fatal(err)
	}
	if image.settings.notifications, err = fixture.storage.ListNotifications(fixture.ctx, fixture.team.repository.ID); err != nil {
		test.Fatal(err)
	}
	if image.settings.snapshots, err = fixture.storage.ListSnapshots(fixture.ctx, fixture.repo, ""); err != nil {
		test.Fatal(err)
	}
	return image
}

type repositoryAssetOperationPG struct {
	name       string
	action     string
	graphDelta uint64
	hash       domain.ContentHash
	writes     []string
	run        func(context.Context, *RepositoryAssetCommands) error
}

func repositoryAssetOperationsPG(test *testing.T, fixture repositoryAssetFixturePG, content string) []repositoryAssetOperationPG {
	test.Helper()
	bundle := settingsCodexBundlePG("codex", content)
	hash, err := domain.SettingsObjectHash(bundle)
	if err != nil {
		test.Fatal(err)
	}
	raw, err := fixture.storage.GetSecretsEnvelope(fixture.ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	branch, protected := "develop", true
	description, website := "  "+content+"  ", "example.test/assets"
	return []repositoryAssetOperationPG{
		{name: "about", action: "repository.about.updated", graphDelta: 1, writes: []string{"about", "audit"}, run: func(ctx context.Context, commands *RepositoryAssetCommands) error {
			if err := commands.UpdateAbout(ctx, fixture.repo, description, website, []string{"assets"}); err != nil {
				return err
			}
			metadata, err := fixture.storage.GetRepo(ctx, fixture.repo)
			if err == nil && (metadata.Description != content || metadata.Website != "https://example.test/assets" || !reflect.DeepEqual(metadata.Topics, []string{"assets"})) {
				return fmt.Errorf("about command did not store normalized metadata: %+v", metadata)
			}
			return err
		}},
		{name: "config", action: "repository.config.updated", graphDelta: 1, writes: []string{"config", "audit"}, run: func(ctx context.Context, commands *RepositoryAssetCommands) error {
			if err := commands.UpdateRepoConfig(ctx, fixture.repo, &branch, &protected); err != nil {
				return err
			}
			metadata, err := fixture.storage.GetRepo(ctx, fixture.repo)
			if err == nil && (metadata.DefaultBranch != branch || !metadata.ProtectDefault) {
				return fmt.Errorf("configuration command lost its stored branch or protection: %+v", metadata)
			}
			return err
		}},
		{name: "settings", action: "repository.settings.updated", graphDelta: 1, writes: []string{"settings", "audit"}, run: func(ctx context.Context, commands *RepositoryAssetCommands) error {
			return commands.PutSettings(ctx, fixture.repo, bundle)
		}},
		{name: "object", action: "repository.settings.object.stored", hash: hash, writes: []string{"object", "audit"}, run: func(ctx context.Context, commands *RepositoryAssetCommands) error {
			return commands.PutSettingsObject(ctx, fixture.repo, hash, bundle)
		}},
		{name: "secrets", action: "repository.secrets.updated", graphDelta: 1, writes: []string{"cas", "outbox", "audit"}, run: func(ctx context.Context, commands *RepositoryAssetCommands) error {
			actor, _ := inbound.RepositoryActor(ctx)
			output, err := commands.SaveSecrets(ctx, inbound.SaveSecretsInput{
				RepoID: fixture.repo, ActorID: actor, Envelope: secretsTestEnvelope("aaaaaaaaaaaa"),
				Edit: domain.SecretsEdit{ExpectedRevision: domain.SecretsRevision(raw)},
			})
			if err != nil && output != (inbound.SaveSecretsOutput{}) {
				return fmt.Errorf("failed secrets command returned payload: %+v (%v)", output, err)
			}
			if err == nil && (output.Status != "stored" || output.Revision == "" || output.Revision == domain.SecretsRevision(raw)) {
				return fmt.Errorf("secrets command lost its generation acknowledgement: %+v", output)
			}
			if err == nil {
				stored, readError := fixture.storage.GetSecretsEnvelope(ctx, fixture.repo)
				if readError != nil {
					return readError
				}
				if output.Revision != domain.SecretsRevision(stored) || reflect.DeepEqual(stored, raw) {
					return fmt.Errorf("secrets acknowledgement disagreed with committed ciphertext generation: %+v", output)
				}
			}
			return err
		}},
		{name: "profile", action: "repository.profile.updated", graphDelta: 1, writes: []string{"config", "about", "audit"}, run: func(ctx context.Context, commands *RepositoryAssetCommands) error {
			output, err := commands.PatchRepoProfile(ctx, fixture.repo, inbound.RepoProfilePatch{
				Description: &description, Website: &website, Topics: []string{"assets"}, DefaultBranch: &branch, ProtectDefault: &protected,
			})
			if err != nil && !reflect.DeepEqual(output, domain.Repo{}) {
				return fmt.Errorf("failed compound profile command returned payload: %+v (%v)", output, err)
			}
			if err == nil && (output.ID != fixture.repo || output.Description != content || output.Website != "https://example.test/assets" || output.DefaultBranch != branch || !output.ProtectDefault || !reflect.DeepEqual(output.Topics, []string{"assets"})) {
				return fmt.Errorf("compound profile returned incomplete or unnormalized metadata: %+v", output)
			}
			return err
		}},
	}
}

func repositoryAssetEffectsPG(test *testing.T, fixture repositoryAssetFixturePG, operation repositoryAssetOperationPG, before, after repositoryAssetStatePG, actor string) {
	test.Helper()
	checked := after.settings
	if operation.name == "secrets" {
		previous := map[string]domain.NotificationJob{}
		for _, job := range before.settings.notifications {
			previous[job.ID] = job
		}
		added := 0
		for _, job := range after.settings.notifications {
			if prior, exists := previous[job.ID]; exists {
				if !reflect.DeepEqual(prior, job) {
					test.Fatal("secrets command changed an existing notification")
				}
				continue
			}
			added++
			if job.RepositoryID != fixture.team.repository.ID || job.Kind != "secrets_updated" || job.State != "pending" || job.Text != "cxthub:  — secrets updated (team members run cxt secrets pull)" {
				test.Fatalf("secrets notification changed the established metadata-only helper contract: %+v", job)
			}
		}
		if added != 1 || len(after.settings.notifications) != len(before.settings.notifications)+1 {
			test.Fatal("secrets command did not enqueue exactly one notification")
		}
		checked.notifications = before.settings.notifications
	}
	settingsCodexEffectsPG(test, fixture.settingsCodexFixturePG, before.settings, checked, operation.graphDelta, actor, operation.action)
	if !reflect.DeepEqual(after.settings.snapshots, before.settings.snapshots) {
		test.Fatal("asset command changed immutable snapshot attachments")
	}
	for hash, bundle := range before.objects {
		if !reflect.DeepEqual(after.objects[hash], bundle) {
			test.Fatal("asset command replaced an immutable settings object")
		}
	}
	if operation.name != "secrets" && !reflect.DeepEqual(after.secrets, before.secrets) {
		test.Fatal("non-secrets asset command changed ciphertext")
	}
	if operation.name != "settings" && !reflect.DeepEqual(after.settings.bundles, before.settings.bundles) {
		test.Fatal("asset command changed unrelated latest settings")
	}
}

type repositoryAssetFailurePG struct {
	*store.PostgresStore
	repo            domain.ContentHash
	failAt          string
	failure         error
	calls           []string
	revisionAtAudit domain.RepositoryRevision
	identityAtAudit domain.DocumentIdentity
}

func (storage *repositoryAssetFailurePG) wrote(ctx context.Context, phase string) error {
	if !storage.InTransaction(ctx) {
		return errors.New("asset SQL write escaped its repository transaction")
	}
	storage.calls = append(storage.calls, phase)
	if phase == storage.failAt {
		return storage.failure
	}
	return nil
}

func (storage *repositoryAssetFailurePG) UpdateRepoAbout(ctx context.Context, repo domain.ContentHash, description, website string, topics []string) error {
	if err := storage.PostgresStore.UpdateRepoAbout(ctx, repo, description, website, topics); err != nil {
		return err
	}
	return storage.wrote(ctx, "about")
}

func (storage *repositoryAssetFailurePG) UpdateRepoConfig(ctx context.Context, repo domain.ContentHash, branch *string, protected *bool) error {
	if err := storage.PostgresStore.UpdateRepoConfig(ctx, repo, branch, protected); err != nil {
		return err
	}
	return storage.wrote(ctx, "config")
}

func (storage *repositoryAssetFailurePG) PutSettingsBundle(ctx context.Context, repo domain.ContentHash, bundle domain.SettingsBundle) error {
	if err := storage.PostgresStore.PutSettingsBundle(ctx, repo, bundle); err != nil {
		return err
	}
	return storage.wrote(ctx, "settings")
}

func (storage *repositoryAssetFailurePG) PutSettingsObject(ctx context.Context, repo, hash domain.ContentHash, bundle domain.SettingsBundle) error {
	if err := storage.PostgresStore.PutSettingsObject(ctx, repo, hash, bundle); err != nil {
		return err
	}
	return storage.wrote(ctx, "object")
}

func (storage *repositoryAssetFailurePG) CompareAndSwapSecrets(ctx context.Context, repo domain.ContentHash, expected, next []byte) error {
	if err := storage.PostgresStore.CompareAndSwapSecrets(ctx, repo, expected, next); err != nil {
		return err
	}
	return storage.wrote(ctx, "cas")
}

func (storage *repositoryAssetFailurePG) EnqueueNotification(ctx context.Context, delivery outbound.NotificationDelivery) error {
	if err := storage.PostgresStore.EnqueueNotification(ctx, delivery); err != nil {
		return err
	}
	return storage.wrote(ctx, "outbox")
}

func (storage *repositoryAssetFailurePG) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	if err := storage.PostgresStore.AppendOrganizationAudit(ctx, event); err != nil {
		return err
	}
	var err error
	storage.revisionAtAudit, err = storage.PostgresStore.RepositoryRevision(ctx, storage.repo)
	if err != nil {
		return err
	}
	metadata, err := storage.PostgresStore.GetRepo(ctx, storage.repo)
	if err != nil {
		return err
	}
	storage.identityAtAudit = metadata.RequiredDocIdentity
	return storage.wrote(ctx, "audit")
}

func TestPGRepositoryAssetCommandsCurrentAuthority(test *testing.T) {
	for _, mutation := range []string{"role", "remove", "archive", "settings-policy", "secrets-policy", "member"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := repositoryAssetsPG(test)
			commands := repositoryAssetCommandsPG(test, fixture.storage)
			member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
			for _, operation := range repositoryAssetOperationsPG(test, fixture, "authorized") {
				before := repositoryAssetImagePG(test, fixture, operation.hash)
				if err := operation.run(member, commands); err != nil {
					test.Fatal(operation.name, err)
				}
				repositoryAssetEffectsPG(test, fixture, operation, before, repositoryAssetImagePG(test, fixture, operation.hash), fixture.team.member.ID)
			}
			if mutation == "member" {
				if err := fixture.team.identity.UpdateMemberRole(fixture.ctx, fixture.team.owner.ID, fixture.team.repository.ID, fixture.team.member.ID, domain.RoleMember); err != nil {
					test.Fatal(err)
				}
			} else if err := authorityScopeMutation(fixture.ctx, fixture.team.identity, fixture.team, mutation); err != nil {
				test.Fatal(err)
			}
			for _, operation := range repositoryAssetOperationsPG(test, fixture, "after authority change") {
				before := repositoryAssetImagePG(test, fixture, operation.hash)
				denied := mutation == "role" || mutation == "remove" || mutation == "archive" || mutation == "member" && operation.name != "object" || mutation == "settings-policy" && operation.name == "settings" || mutation == "secrets-policy" && operation.name == "secrets"
				err := operation.run(member, commands)
				after := repositoryAssetImagePG(test, fixture, operation.hash)
				if denied {
					if !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(after, before) {
						test.Fatalf("%s retained obsolete authority or left durable effects: %v", operation.name, err)
					}
				} else {
					if err != nil {
						test.Fatal(operation.name, err)
					}
					repositoryAssetEffectsPG(test, fixture, operation, before, after, fixture.team.member.ID)
				}
			}
			if mutation == "settings-policy" || mutation == "secrets-policy" {
				owner := inbound.WithRepositoryActor(fixture.ctx, fixture.team.owner.ID)
				for _, operation := range repositoryAssetOperationsPG(test, fixture, "owner remains authorized") {
					if mutation == "settings-policy" && operation.name != "settings" || mutation == "secrets-policy" && operation.name != "secrets" {
						continue
					}
					before := repositoryAssetImagePG(test, fixture)
					if err := operation.run(owner, commands); err != nil {
						test.Fatal(err)
					}
					repositoryAssetEffectsPG(test, fixture, operation, before, repositoryAssetImagePG(test, fixture), fixture.team.owner.ID)
				}
			}
		})
	}
}

func TestPGRepositoryAssetCommandsRollbackAfterActualWrites(test *testing.T) {
	fixture := repositoryAssetsPG(test)
	member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	for _, operation := range repositoryAssetOperationsPG(test, fixture, "rollback candidate") {
		stages := []string{"audit", "outer"}
		if operation.name == "secrets" {
			stages = append(stages, "cas", "outbox")
		}
		if operation.name == "profile" {
			stages = append(stages, "about")
		}
		for _, phase := range stages {
			test.Run(operation.name+"/"+phase, func(test *testing.T) {
				before := repositoryAssetImagePG(test, fixture, operation.hash)
				injected := errors.New("injected asset failure after actual " + phase + " writes")
				failure := &repositoryAssetFailurePG{PostgresStore: fixture.storage, repo: fixture.repo, failAt: phase, failure: injected}
				commands := repositoryAssetCommandsPG(test, failure)
				var err error
				if phase == "outer" {
					err = fixture.storage.WithinRepository(member, fixture.repo, func(bound context.Context) error {
						if err := operation.run(bound, commands); err != nil {
							return err
						}
						visible := fixture
						visible.ctx = bound
						if reflect.DeepEqual(repositoryAssetImagePG(test, visible, operation.hash), before) {
							return errors.New("outer rollback never observed the command's SQL writes")
						}
						return injected
					})
				} else {
					err = operation.run(member, commands)
				}
				if !errors.Is(err, injected) {
					test.Fatalf("rollback error=%v, want %v", err, injected)
				}
				wantedCalls := operation.writes
				if phase != "outer" {
					wantedCalls = wantedCalls[:slices.Index(wantedCalls, phase)+1]
				}
				if !reflect.DeepEqual(failure.calls, wantedCalls) {
					test.Fatalf("actual SQL order=%v, want %v", failure.calls, wantedCalls)
				}
				if slices.Contains(wantedCalls, "audit") {
					expected := before.settings.revision
					expected.Graph += operation.graphDelta
					if failure.revisionAtAudit != expected {
						test.Fatalf("revision before audit=%+v, want %+v", failure.revisionAtAudit, expected)
					}
				}
				if after := repositoryAssetImagePG(test, fixture, operation.hash); !reflect.DeepEqual(after, before) {
					test.Fatal("rollback changed metadata, settings, attachments, ciphertext, graph/evidence/pending revisions, audit or outbox")
				}
			})
		}
	}
}

func TestPGRepositoryAssetCommandsImmutableSettingsAllKinds(test *testing.T) {
	fixture := repositoryAssetsPG(test)
	commands := repositoryAssetCommandsPG(test, fixture.storage)
	member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	for position, kind := range []string{"claude", "agents", "codex"} {
		before := repositoryAssetImagePG(test, fixture)
		bundle := settingsCodexBundlePG(kind, "replacement "+kind)
		if err := commands.PutSettings(member, fixture.repo, bundle); err != nil {
			test.Fatal(err)
		}
		after := repositoryAssetImagePG(test, fixture)
		operation := repositoryAssetOperationPG{name: "settings", graphDelta: 1, action: "repository.settings.updated"}
		repositoryAssetEffectsPG(test, fixture, operation, before, after, fixture.team.member.ID)
		stored := after.settings.bundles[kind]
		if stored.Kind != kind || !reflect.DeepEqual(stored.Files, bundle.Files) || !stored.UpdatedAt.After(bundle.UpdatedAt) {
			test.Fatal("latest settings did not retain their independent kind or server timestamp", stored)
		}
		for otherKind, previous := range before.settings.bundles {
			if otherKind != kind && !reflect.DeepEqual(after.settings.bundles[otherKind], previous) {
				test.Fatal("latest settings update crossed kind boundaries")
			}
		}
		hash := fixture.attachments[position]
		duplicate := before.objects[hash]
		duplicate.UpdatedAt = duplicate.UpdatedAt.Add(time.Hour)
		duplicate.UpdatedBy = fixture.team.owner.ID
		if err := commands.PutSettingsObject(member, fixture.repo, hash, duplicate); err != nil {
			test.Fatal(err)
		}
		operation = repositoryAssetOperationPG{name: "object", action: "repository.settings.object.stored"}
		repositoryAssetEffectsPG(test, fixture, operation, after, repositoryAssetImagePG(test, fixture), fixture.team.member.ID)
	}
}

func TestPGRepositoryAssetCommandsCompoundProfileWritesOnceInOrder(test *testing.T) {
	fixture := repositoryAssetsPG(test)
	member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	for _, operation := range repositoryAssetOperationsPG(test, fixture, "compound profile success") {
		if operation.name != "profile" {
			continue
		}
		before := repositoryAssetImagePG(test, fixture)
		observed := &repositoryAssetFailurePG{PostgresStore: fixture.storage, repo: fixture.repo}
		if err := operation.run(member, repositoryAssetCommandsPG(test, observed)); err != nil {
			test.Fatal(err)
		}
		after := repositoryAssetImagePG(test, fixture)
		if !reflect.DeepEqual(observed.calls, operation.writes) || observed.revisionAtAudit != after.settings.revision {
			test.Fatal("compound profile split its configuration, about, revision or audit transaction", observed.calls, observed.revisionAtAudit)
		}
		if after.metadata.ID != before.metadata.ID || after.metadata.RepositoryID != before.metadata.RepositoryID || after.metadata.RemoteURL != before.metadata.RemoteURL || after.metadata.RequiredDocIdentity != before.metadata.RequiredDocIdentity {
			test.Fatal("compound profile changed absent identity or immutable repository fields")
		}
		repositoryAssetEffectsPG(test, fixture, operation, before, after, fixture.team.member.ID)
	}
}

func TestPGRepositoryAssetCommandsSecretsReplayIsExactlyOnce(test *testing.T) {
	fixture := repositoryAssetsPG(test)
	commands := repositoryAssetCommandsPG(test, fixture.storage)
	member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	for _, operation := range repositoryAssetOperationsPG(test, fixture, "secrets replay") {
		if operation.name != "secrets" {
			continue
		}
		before := repositoryAssetImagePG(test, fixture)
		if err := operation.run(member, commands); err != nil {
			test.Fatal(err)
		}
		after := repositoryAssetImagePG(test, fixture)
		repositoryAssetEffectsPG(test, fixture, operation, before, after, fixture.team.member.ID)
		if err := operation.run(member, commands); !errors.Is(err, domain.ErrSecretsConflict) {
			test.Fatal("stale secrets baseline was accepted", err)
		}
		if replayed := repositoryAssetImagePG(test, fixture); !reflect.DeepEqual(replayed, after) {
			test.Fatal("stale secrets replay duplicated revision, audit, ciphertext generation or notification")
		}
	}
}

func TestPGRepositoryAssetCommandsAuthorityLockOrdering(test *testing.T) {
	for _, kind := range []string{"settings", "secrets", "profile"} {
		for _, order := range []string{"revoke-first", "write-first"} {
			test.Run(kind+"/"+order, func(test *testing.T) {
				fixture := repositoryAssetsPG(test)
				observer := authorityScopeObserver(test, fixture.ctx)
				writer, writerName := authorityScopePeer(test, fixture.ctx)
				authority, authorityName := authorityScopePeer(test, fixture.ctx)
				member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
				var selected repositoryAssetOperationPG
				for _, operation := range repositoryAssetOperationsPG(test, fixture, "ordered write") {
					if operation.name == kind {
						selected = operation
					}
				}
				before := repositoryAssetImagePG(test, fixture)
				release := make(chan struct{})
				var released sync.Once
				defer released.Do(func() { close(release) })
				first, second := make(chan error, 1), make(chan error, 1)
				if order == "revoke-first" {
					paused := &authorityScopeHoldPG{PostgresStore: authority, ready: make(chan struct{}), release: release}
					go func() {
						first <- authorityScopeMutation(fixture.ctx, NewIdentityService(nil, paused), fixture.team, "remove")
					}()
					authorityScopeAwait(test, fixture.ctx, paused.ready, first)
					commands := repositoryAssetCommandsPG(test, writer)
					go func() { second <- selected.run(member, commands) }()
					authorityScopeBlocked(test, fixture.ctx, observer, writerName, authorityName, "repositories", second)
				} else {
					paused := &authorityScopeWriteHoldPG{PostgresStore: writer, ready: make(chan struct{}), release: release}
					commands := repositoryAssetCommandsPG(test, paused)
					go func() { first <- selected.run(member, commands) }()
					authorityScopeAwait(test, fixture.ctx, paused.ready, first)
					go func() {
						second <- authorityScopeMutation(fixture.ctx, NewIdentityService(nil, authority), fixture.team, "remove")
					}()
					authorityScopeBlocked(test, fixture.ctx, observer, authorityName, writerName, "repositories", second)
				}
				if uncommitted := repositoryAssetImagePG(test, fixture); !reflect.DeepEqual(uncommitted, before) {
					test.Fatal("held command exposed uncommitted assets, revisions, audit or notifications")
				}
				released.Do(func() { close(release) })
				if err := authorityScopeDone(test, fixture.ctx, first); err != nil {
					test.Fatal(err)
				}
				secondError := authorityScopeDone(test, fixture.ctx, second)
				after := repositoryAssetImagePG(test, fixture)
				priorAudits := map[string]domain.OrganizationAuditEvent{}
				for _, event := range before.settings.audits {
					priorAudits[event.ID] = event
				}
				filtered := []domain.OrganizationAuditEvent{}
				authorityAudits := 0
				for _, event := range after.settings.audits {
					if prior, exists := priorAudits[event.ID]; exists {
						if !reflect.DeepEqual(prior, event) {
							test.Fatal("concurrent authority mutation changed an existing audit")
						}
						filtered = append(filtered, event)
						continue
					}
					if event.ActorID == fixture.team.owner.ID {
						authorityAudits++
					} else {
						filtered = append(filtered, event)
					}
				}
				if authorityAudits != 1 {
					test.Fatal("authority mutation did not commit exactly one audit", authorityAudits)
				}
				after.settings.audits = filtered
				if order == "revoke-first" {
					if !errors.Is(secondError, domain.ErrForbidden) || !reflect.DeepEqual(after, before) {
						test.Fatal("queued command retained revoked authority or committed effects", secondError)
					}
				} else {
					if secondError != nil {
						test.Fatal(secondError)
					}
					repositoryAssetEffectsPG(test, fixture, selected, before, after, fixture.team.member.ID)
					committed := repositoryAssetImagePG(test, fixture)
					if err := selected.run(member, repositoryAssetCommandsPG(test, writer)); !errors.Is(err, domain.ErrForbidden) {
						test.Fatal("later command retained revoked authority", err)
					}
					if denied := repositoryAssetImagePG(test, fixture); !reflect.DeepEqual(denied, committed) {
						test.Fatal("post-revocation command changed committed assets")
					}
				}
			})
		}
	}
}

func TestPGRepositoryAssetCommandsSaveSecretsKeepsIdentityNegotiationOrder(test *testing.T) {
	fixture := repositoryAssetsPG(test)
	policy := "owner"
	if _, err := fixture.team.identity.UpdateRepositorySettings(fixture.ctx, fixture.team.owner.ID, fixture.team.repository.ID, RepositoryPatch{SecretsPolicy: &policy}); err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.WithinRepository(fixture.ctx, fixture.repo, func(bound context.Context) error {
		return fixture.storage.RequireDocumentIdentity(bound, fixture.repo, domain.DocumentIdentityRootV1)
	}); err != nil {
		test.Fatal(err)
	}
	commands := repositoryAssetCommandsPG(test, fixture.storage)
	before := repositoryAssetImagePG(test, fixture)
	input := inbound.SaveSecretsInput{
		RepoID: fixture.repo, ActorID: fixture.team.member.ID, Envelope: secretsTestEnvelope("aaaaaaaaaaaa"),
		Edit: domain.SecretsEdit{ExpectedRevision: domain.SecretsRevision(before.secrets)},
	}
	for _, identities := range [][]domain.DocumentIdentity{nil, {domain.DocumentIdentityLegacy}} {
		ctx := inbound.WithDocumentIdentities(fixture.ctx, identities)
		output, err := commands.SaveSecrets(ctx, input)
		if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || output != (inbound.SaveSecretsOutput{}) {
			test.Fatal("secrets policy ran before common document identity negotiation", output, err)
		}
		if after := repositoryAssetImagePG(test, fixture); !reflect.DeepEqual(after, before) {
			test.Fatal("old document identity peer changed secrets or transaction evidence")
		}
	}
	ctx := inbound.WithDocumentIdentities(fixture.ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	expected := domain.ErrDocumentIdentityUpgradeRequired
	if hasDocumentIdentity(ReadDocumentIdentities(fixture.storage, fixture.storage), domain.DocumentIdentityRootV1) {
		expected = domain.ErrForbidden
	}
	if output, err := commands.SaveSecrets(ctx, input); !errors.Is(err, expected) || output != (inbound.SaveSecretsOutput{}) {
		test.Fatal("secrets command bypassed binary support or current owner-only policy", output, err)
	}
	if after := repositoryAssetImagePG(test, fixture); !reflect.DeepEqual(after, before) {
		test.Fatal("rejected negotiated secrets command changed durable state")
	}
}

func TestPGRepositoryAssetCommandsPartialAboutKeepsUnselectedFields(test *testing.T) {
	fixture := repositoryAssetsPG(test)
	commands := repositoryAssetCommandsPG(test, fixture.storage)
	ctx := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	if err := commands.UpdateAbout(ctx, fixture.repo, "original", "https://original.example/path", []string{"first", "first", "\ud55c\uae00"}); err != nil {
		test.Fatal(err)
	}
	description, website := " changed description ", "https://changed.example/path"
	for _, name := range []string{"description", "website", "clear-topics"} {
		test.Run(name, func(test *testing.T) {
			before := repositoryAssetImagePG(test, fixture)
			expected := before.metadata
			patch := inbound.RepoProfilePatch{}
			switch name {
			case "description":
				patch.Description, expected.Description = &description, "changed description"
			case "website":
				patch.Website, expected.Website = &website, website
			case "clear-topics":
				patch.Topics, expected.Topics = []string{}, []string{}
			}
			output, err := commands.PatchRepoProfile(ctx, fixture.repo, patch)
			after := repositoryAssetImagePG(test, fixture)
			if err != nil || !reflect.DeepEqual(output, expected) || !reflect.DeepEqual(after.metadata, expected) {
				test.Fatal("partial profile patch lost an unselected persisted field", output, expected, err)
			}
			repositoryAssetEffectsPG(test, fixture, repositoryAssetOperationPG{name: "profile", action: "repository.profile.updated", graphDelta: 1}, before, after, fixture.team.member.ID)
		})
	}
}

func TestPGRepositoryAssetCommandsCompoundProfileIdentityGates(test *testing.T) {
	fixture := repositoryAssetsPG(test)
	storage := fixture.storage
	member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	root := domain.DocumentIdentityRootV1
	description, branch, protected := "compound identity profile", "identity-main", true
	patch := inbound.RepoProfilePatch{RequiredDocIdentity: &root, Description: &description, DefaultBranch: &branch, ProtectDefault: &protected}
	auditFailure := errors.New("injected compound identity profile audit failure after SQL insert")
	for _, scenario := range []struct {
		name      string
		enabled   bool
		failAudit bool
		supported []domain.DocumentIdentity
		peer      []domain.DocumentIdentity
		wantError error
	}{
		{name: "publication-disabled", supported: []domain.DocumentIdentity{domain.DocumentIdentityLegacy, root}, peer: []domain.DocumentIdentity{root}, wantError: domain.ErrRootPublicationDisabled},
		{name: "binary-unsupported", enabled: true, supported: []domain.DocumentIdentity{domain.DocumentIdentityLegacy}, peer: []domain.DocumentIdentity{root}, wantError: domain.ErrDocumentIdentityUpgradeRequired},
		{name: "peer-unsupported", enabled: true, supported: []domain.DocumentIdentity{domain.DocumentIdentityLegacy, root}, wantError: domain.ErrDocumentIdentityUpgradeRequired},
		{name: "audit-rollback", enabled: true, failAudit: true, supported: []domain.DocumentIdentity{domain.DocumentIdentityLegacy, root}, peer: []domain.DocumentIdentity{root}, wantError: auditFailure},
		{name: "supported", enabled: true, supported: []domain.DocumentIdentity{domain.DocumentIdentityLegacy, root}, peer: []domain.DocumentIdentity{root}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			var commandStorage repositoryAssetStorePG = storage
			var failure *repositoryAssetFailurePG
			if scenario.failAudit {
				failure = &repositoryAssetFailurePG{PostgresStore: storage, repo: fixture.repo, failAt: "audit", failure: auditFailure}
				commandStorage = failure
			}
			scope := &repositoryAssetScope{
				work:            newRepositoryWork(commandStorage, commandStorage, scenario.supported),
				rootPublication: scenario.enabled, identityStore: storage, transactional: true,
			}
			commands, constructorError := NewRepositoryAssetCommands(RepositoryAssetCommandsConfig{
				Profile: RepositoryAssetsComplete, Store: commandStorage, Scope: scope,
				Repositories: commandStorage, SecretsCAS: commandStorage, Notifications: commandStorage,
			})
			if constructorError != nil {
				test.Fatal(constructorError)
			}
			before := repositoryAssetImagePG(test, fixture)
			output, err := commands.PatchRepoProfile(inbound.WithDocumentIdentities(member, scenario.peer), fixture.repo, patch)
			after := repositoryAssetImagePG(test, fixture)
			if scenario.wantError != nil {
				if !errors.Is(err, scenario.wantError) || !reflect.DeepEqual(output, domain.Repo{}) || !reflect.DeepEqual(after, before) {
					test.Fatal("profile identity gate returned payload or durable effects", output, err)
				}
				if scenario.failAudit && (failure.identityAtAudit != root || !reflect.DeepEqual(failure.calls, []string{"config", "about", "audit"})) {
					test.Fatal("compound identity rollback did not reach actual identity, configuration, about and audit writes", failure.identityAtAudit, failure.calls)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(output, after.metadata) || output.RequiredDocIdentity != root || output.Description != description || output.DefaultBranch != branch || !output.ProtectDefault {
				test.Fatal("compound identity profile did not commit a complete coherent result", output, err)
			}
			repositoryAssetEffectsPG(test, fixture, repositoryAssetOperationPG{name: "profile", graphDelta: 1, action: "repository.profile.updated"}, before, after, fixture.team.member.ID)
		})
	}
}
