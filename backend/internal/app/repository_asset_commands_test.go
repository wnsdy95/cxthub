package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave56AssetStore struct {
	getRepo      func(context.Context, domain.ContentHash) (domain.Repo, error)
	updateAbout  func(context.Context, domain.ContentHash, string, string, []string) error
	updateConfig func(context.Context, domain.ContentHash, *string, *bool) error
	putSettings  func(context.Context, domain.ContentHash, domain.SettingsBundle) error
	putObject    func(context.Context, domain.ContentHash, domain.ContentHash, domain.SettingsBundle) error
	getSecrets   func(context.Context, domain.ContentHash) ([]byte, error)
}

func (ports *wave56AssetStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return ports.getRepo(ctx, repo)
}

func (ports *wave56AssetStore) UpdateRepoAbout(ctx context.Context, repo domain.ContentHash, description, website string, topics []string) error {
	return ports.updateAbout(ctx, repo, description, website, topics)
}

func (ports *wave56AssetStore) UpdateRepoConfig(ctx context.Context, repo domain.ContentHash, branch *string, protected *bool) error {
	return ports.updateConfig(ctx, repo, branch, protected)
}

func (ports *wave56AssetStore) PutSettingsBundle(ctx context.Context, repo domain.ContentHash, bundle domain.SettingsBundle) error {
	return ports.putSettings(ctx, repo, bundle)
}

func (ports *wave56AssetStore) PutSettingsObject(ctx context.Context, repo, hash domain.ContentHash, bundle domain.SettingsBundle) error {
	return ports.putObject(ctx, repo, hash, bundle)
}

func (ports *wave56AssetStore) GetSecretsEnvelope(ctx context.Context, repo domain.ContentHash) ([]byte, error) {
	return ports.getSecrets(ctx, repo)
}

type wave56AssetScope struct {
	write     func(context.Context, domain.ContentHash, RepositoryAssetOperation, func(context.Context) error) error
	secrets   func(context.Context, domain.ContentHash, func(context.Context) (inbound.SaveSecretsOutput, error)) (inbound.SaveSecretsOutput, error)
	profile   func(context.Context, domain.ContentHash, func(context.Context) (domain.Repo, error)) (domain.Repo, error)
	authorize func(context.Context, inbound.SaveSecretsInput) error
	require   func(context.Context, domain.ContentHash, domain.DocumentIdentity) error
}

func (ports *wave56AssetScope) WithinAssetWrite(ctx context.Context, repo domain.ContentHash, operation RepositoryAssetOperation, write func(context.Context) error) error {
	return ports.write(ctx, repo, operation, write)
}

func (ports *wave56AssetScope) WithinAssetSecretsWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) (inbound.SaveSecretsOutput, error)) (inbound.SaveSecretsOutput, error) {
	return ports.secrets(ctx, repo, write)
}

func (ports *wave56AssetScope) WithinAssetProfileWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) (domain.Repo, error)) (domain.Repo, error) {
	return ports.profile(ctx, repo, write)
}

func (ports *wave56AssetScope) AuthorizeAssetSecrets(ctx context.Context, input inbound.SaveSecretsInput) error {
	return ports.authorize(ctx, input)
}

func (ports *wave56AssetScope) RequireAssetDocumentIdentity(ctx context.Context, repo domain.ContentHash, identity domain.DocumentIdentity) error {
	return ports.require(ctx, repo, identity)
}

type wave56AssetRepositoryReader struct {
	read func(context.Context, string) (domain.Repository, error)
}

func (ports *wave56AssetRepositoryReader) GetRepository(ctx context.Context, id string) (domain.Repository, error) {
	return ports.read(ctx, id)
}

type wave56AssetCAS struct {
	compare func(context.Context, domain.ContentHash, []byte, []byte) error
}

func (ports *wave56AssetCAS) CompareAndSwapSecrets(ctx context.Context, repo domain.ContentHash, previous, next []byte) error {
	return ports.compare(ctx, repo, previous, next)
}

type wave56AssetNotifications struct {
	enqueue func(context.Context, outbound.NotificationDelivery) error
}

func (ports *wave56AssetNotifications) EnqueueNotification(ctx context.Context, delivery outbound.NotificationDelivery) error {
	return ports.enqueue(ctx, delivery)
}

var (
	_ RepositoryAssetStore            = (*wave56AssetStore)(nil)
	_ RepositoryAssetWriteScope       = (*wave56AssetScope)(nil)
	_ RepositoryAssetRepositoryReader = (*wave56AssetRepositoryReader)(nil)
	_ outbound.SecretsCASStore        = (*wave56AssetCAS)(nil)
	_ RepositoryNotificationQueue     = (*wave56AssetNotifications)(nil)
)

type wave56AssetCallerKey struct{}
type wave56AssetBoundKey struct{}

type wave56AssetFixture struct {
	test           *testing.T
	ctx            context.Context
	repo           domain.ContentHash
	metadata       domain.Repo
	record         domain.Repository
	config         RepositoryAssetCommandsConfig
	scope          *wave56AssetScope
	expectedActor  string
	expectedSystem bool
	active         bool
	operation      RepositoryAssetOperation
	operations     []RepositoryAssetOperation
	events         []string
	callbacks      int
	writes         int
	casCalls       int
	queueCalls     int
	authorized     []inbound.SaveSecretsInput
	settings       []domain.SettingsBundle
	objects        []domain.SettingsBundle
	objectHashes   []domain.ContentHash
	deliveries     []outbound.NotificationDelivery
	branch         *string
	protected      *bool
	current        []byte
	currentErr     error
	previous       []byte
	next           []byte
	scopeErr       error
	exitErr        error
	authorizeErr   error
	writeErr       error
	repoReadErr    error
	repositoryErr  error
	casErr         error
	queueErr       error
	requireErr     error
	required       []domain.DocumentIdentity
	beforeCallback func()
}

func wave56AssetAudit(operation RepositoryAssetOperation) string {
	switch operation {
	case AssetUpdateAbout:
		return "repository.about.updated"
	case AssetUpdateConfig:
		return "repository.config.updated"
	case AssetPutSettings:
		return "repository.settings.updated"
	case AssetPutSettingsObject:
		return "repository.settings.object.stored"
	case AssetSaveSecrets:
		return "repository.secrets.updated"
	case AssetPatchProfile:
		return "repository.profile.updated"
	default:
		return "unknown"
	}
}

func (fixture *wave56AssetFixture) checkContext(ctx context.Context, bound bool) {
	fixture.test.Helper()
	actor, system := inbound.RepositoryActor(ctx)
	if actor != fixture.expectedActor || system != fixture.expectedSystem {
		fixture.test.Fatalf("actor changed: actor=%q system=%t", actor, system)
	}
	if ctx.Value(wave56AssetCallerKey{}) != fixture || inbound.Correlation(ctx) != "wave56-request" {
		fixture.test.Fatal("caller context or correlation was discarded")
	}
	deadline, hasDeadline := ctx.Deadline()
	wantDeadline, wantHasDeadline := fixture.ctx.Deadline()
	if hasDeadline != wantHasDeadline || !deadline.Equal(wantDeadline) {
		fixture.test.Fatal("caller deadline changed")
	}
	if bound && (!fixture.active || ctx.Value(wave56AssetBoundKey{}) != fixture || ctx.Value(auditOperationKey{}) != wave56AssetAudit(fixture.operation)) {
		fixture.test.Fatal("port call escaped the bound scope or lost its audit operation")
	}
}

func (fixture *wave56AssetFixture) observe(ctx context.Context, event string) error {
	fixture.test.Helper()
	fixture.checkContext(ctx, true)
	fixture.events = append(fixture.events, event)
	return ctx.Err()
}

func (fixture *wave56AssetFixture) enter(ctx context.Context, repo domain.ContentHash, operation RepositoryAssetOperation) (context.Context, error) {
	fixture.test.Helper()
	fixture.checkContext(ctx, false)
	if repo != fixture.repo {
		fixture.test.Fatalf("scope repository = %q, want %q", repo, fixture.repo)
	}
	fixture.operations = append(fixture.operations, operation)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fixture.scopeErr != nil {
		return nil, fixture.scopeErr
	}
	fixture.active, fixture.operation = true, operation
	bound := auditOperation(context.WithValue(ctx, wave56AssetBoundKey{}, fixture), wave56AssetAudit(operation))
	if fixture.beforeCallback != nil {
		fixture.beforeCallback()
	}
	return bound, nil
}

func newWave56AssetFixture(test *testing.T) *wave56AssetFixture {
	test.Helper()
	fixture := &wave56AssetFixture{test: test, repo: domain.HashContent([]byte("wave56-assets")), expectedActor: "asset-owner", currentErr: domain.ErrNotFound}
	fixture.ctx = inbound.WithCorrelation(inbound.WithRepositoryActor(context.WithValue(context.Background(), wave56AssetCallerKey{}, fixture), fixture.expectedActor), "wave56-request")
	fixture.metadata = domain.Repo{ID: fixture.repo, RepositoryID: "asset-repository", RemoteURL: "https://example.test/team/project", DefaultBranch: "main", Description: "original", Topics: []string{"original"}}
	fixture.record = domain.Repository{ID: fixture.metadata.RepositoryID, OwnerID: fixture.expectedActor, WebhookURL: "https://example.test/webhook/private-credential"}
	fixture.scope = &wave56AssetScope{}
	fixture.scope.write = func(ctx context.Context, repo domain.ContentHash, operation RepositoryAssetOperation, write func(context.Context) error) error {
		bound, err := fixture.enter(ctx, repo, operation)
		if err != nil {
			return err
		}
		defer func() { fixture.active = false }()
		fixture.callbacks++
		if err := write(bound); err != nil {
			return err
		}
		return fixture.exitErr
	}
	fixture.scope.secrets = func(ctx context.Context, repo domain.ContentHash, write func(context.Context) (inbound.SaveSecretsOutput, error)) (inbound.SaveSecretsOutput, error) {
		bound, err := fixture.enter(ctx, repo, AssetSaveSecrets)
		if err != nil {
			return inbound.SaveSecretsOutput{}, err
		}
		defer func() { fixture.active = false }()
		fixture.callbacks++
		result, err := write(bound)
		if err != nil {
			return result, err
		}
		return result, fixture.exitErr
	}
	fixture.scope.profile = func(ctx context.Context, repo domain.ContentHash, write func(context.Context) (domain.Repo, error)) (domain.Repo, error) {
		bound, err := fixture.enter(ctx, repo, AssetPatchProfile)
		if err != nil {
			return domain.Repo{}, err
		}
		defer func() { fixture.active = false }()
		fixture.callbacks++
		result, err := write(bound)
		if err != nil {
			return result, err
		}
		return result, fixture.exitErr
	}
	fixture.scope.authorize = func(ctx context.Context, input inbound.SaveSecretsInput) error {
		if err := fixture.observe(ctx, "authorize-secrets"); err != nil {
			return err
		}
		fixture.authorized = append(fixture.authorized, input)
		return fixture.authorizeErr
	}
	fixture.scope.require = func(ctx context.Context, repo domain.ContentHash, identity domain.DocumentIdentity) error {
		if err := fixture.observe(ctx, "require-identity"); err != nil {
			return err
		}
		if repo != fixture.repo {
			test.Fatal("document identity requirement changed repositories")
		}
		fixture.required = append(fixture.required, identity)
		if fixture.requireErr != nil {
			return fixture.requireErr
		}
		fixture.metadata.RequiredDocIdentity = identity
		return nil
	}
	store := &wave56AssetStore{}
	store.getRepo = func(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
		if err := fixture.observe(ctx, "get-repo"); err != nil {
			return domain.Repo{}, err
		}
		if repo != fixture.repo {
			test.Fatal("metadata lookup changed repositories")
		}
		return fixture.metadata, fixture.repoReadErr
	}
	store.updateAbout = func(ctx context.Context, repo domain.ContentHash, description, website string, topics []string) error {
		if err := fixture.observe(ctx, "update-about"); err != nil {
			return err
		}
		if repo != fixture.repo {
			test.Fatal("about write changed repositories")
		}
		if fixture.writeErr != nil {
			return fixture.writeErr
		}
		fixture.writes++
		fixture.metadata.Description, fixture.metadata.Website = description, website
		fixture.metadata.Topics = append([]string(nil), topics...)
		return nil
	}
	store.updateConfig = func(ctx context.Context, repo domain.ContentHash, branch *string, protected *bool) error {
		if err := fixture.observe(ctx, "update-config"); err != nil {
			return err
		}
		if repo != fixture.repo {
			test.Fatal("config write changed repositories")
		}
		if fixture.writeErr != nil {
			return fixture.writeErr
		}
		fixture.writes++
		fixture.branch, fixture.protected = branch, protected
		if branch != nil {
			fixture.metadata.DefaultBranch = *branch
		}
		if protected != nil {
			fixture.metadata.ProtectDefault = *protected
		}
		return nil
	}
	store.putSettings = func(ctx context.Context, repo domain.ContentHash, bundle domain.SettingsBundle) error {
		if err := fixture.observe(ctx, "put-settings"); err != nil {
			return err
		}
		if repo != fixture.repo {
			test.Fatal("settings write changed repositories")
		}
		if fixture.writeErr != nil {
			return fixture.writeErr
		}
		fixture.writes++
		fixture.settings = append(fixture.settings, bundle)
		return nil
	}
	store.putObject = func(ctx context.Context, repo, hash domain.ContentHash, bundle domain.SettingsBundle) error {
		if err := fixture.observe(ctx, "put-settings-object"); err != nil {
			return err
		}
		if repo != fixture.repo {
			test.Fatal("settings object write changed repositories")
		}
		if fixture.writeErr != nil {
			return fixture.writeErr
		}
		fixture.writes++
		fixture.objects, fixture.objectHashes = append(fixture.objects, bundle), append(fixture.objectHashes, hash)
		return nil
	}
	store.getSecrets = func(ctx context.Context, repo domain.ContentHash) ([]byte, error) {
		if err := fixture.observe(ctx, "get-secrets"); err != nil {
			return nil, err
		}
		if repo != fixture.repo {
			test.Fatal("secrets read changed repositories")
		}
		return fixture.current, fixture.currentErr
	}
	repositories := &wave56AssetRepositoryReader{read: func(ctx context.Context, id string) (domain.Repository, error) {
		if err := fixture.observe(ctx, "get-repository"); err != nil {
			return domain.Repository{}, err
		}
		if id != fixture.metadata.RepositoryID {
			test.Fatal("notification lookup changed repository identity")
		}
		return fixture.record, fixture.repositoryErr
	}}
	cas := &wave56AssetCAS{compare: func(ctx context.Context, repo domain.ContentHash, previous, next []byte) error {
		if err := fixture.observe(ctx, "cas-secrets"); err != nil {
			return err
		}
		if repo != fixture.repo {
			test.Fatal("secrets CAS changed repositories")
		}
		fixture.casCalls++
		fixture.previous, fixture.next = bytes.Clone(previous), bytes.Clone(next)
		if fixture.casErr != nil {
			return fixture.casErr
		}
		fixture.writes++
		fixture.current, fixture.currentErr = bytes.Clone(next), nil
		return nil
	}}
	notifications := &wave56AssetNotifications{enqueue: func(ctx context.Context, delivery outbound.NotificationDelivery) error {
		if err := fixture.observe(ctx, "enqueue-notification"); err != nil {
			return err
		}
		fixture.queueCalls++
		if fixture.queueErr != nil {
			return fixture.queueErr
		}
		fixture.deliveries = append(fixture.deliveries, delivery)
		return nil
	}}
	fixture.config = RepositoryAssetCommandsConfig{Profile: RepositoryAssetsComplete, Store: store, Scope: fixture.scope, Repositories: repositories, SecretsCAS: cas, Notifications: notifications}
	return fixture
}

func wave56AssetOwner(test *testing.T, config RepositoryAssetCommandsConfig) *RepositoryAssetCommands {
	test.Helper()
	commands, err := NewRepositoryAssetCommands(config)
	if err != nil || commands == nil {
		test.Fatalf("declared narrow ports rejected: owner=%v error=%v", commands, err)
	}
	return commands
}

func wave56AssetBundle(kind string) domain.SettingsBundle {
	return domain.SettingsBundle{Kind: kind, Files: []domain.SettingsFile{{Path: "commands/review.md", ContentB64: base64.StdEncoding.EncodeToString([]byte("review carefully"))}}, UpdatedAt: time.Date(2001, 2, 3, 4, 5, 6, 0, time.FixedZone("submitted", 3600)), UpdatedBy: "original-publisher"}
}

func wave56AssetHash(test *testing.T, bundle domain.SettingsBundle) domain.ContentHash {
	test.Helper()
	hash, err := domain.SettingsObjectHash(bundle)
	if err != nil {
		test.Fatal(err)
	}
	return hash
}

func wave56AssetEnvelope(fingerprint string) []byte {
	return []byte(`{"version":1,"kdf":"PBKDF2-SHA256","iterations":600000,"salt_b64":"AAAAAAAAAAAAAAAAAAAAAA==","cipher":"AES-256-GCM","nonce_b64":"AAAAAAAAAAAAAAAA","ciphertext_b64":"b3BhcXVlIGNpcGhlcnRleHQgbXVzdCBub3QgbGVhdmUgc3RvcmFnZQ==","fingerprint":"` + fingerprint + `"}`)
}

func (fixture *wave56AssetFixture) input() inbound.SaveSecretsInput {
	return inbound.SaveSecretsInput{RepoID: fixture.repo, ActorID: "asset-owner", Envelope: wave56AssetEnvelope("aaaaaaaaaaaa"), Edit: domain.SecretsEdit{ExpectedRevision: "absent"}}
}

type wave56AssetAction struct {
	name      string
	operation RepositoryAssetOperation
	run       func(*RepositoryAssetCommands, *wave56AssetFixture) error
}

func wave56AssetActions(test *testing.T) []wave56AssetAction {
	test.Helper()
	bundle := wave56AssetBundle("codex")
	hash := wave56AssetHash(test, bundle)
	branch, description := "release/topic", "updated"
	return []wave56AssetAction{
		{name: "about", operation: AssetUpdateAbout, run: func(commands *RepositoryAssetCommands, fixture *wave56AssetFixture) error {
			return commands.UpdateAbout(fixture.ctx, fixture.repo, "updated", "example.test", []string{"topic"})
		}},
		{name: "config", operation: AssetUpdateConfig, run: func(commands *RepositoryAssetCommands, fixture *wave56AssetFixture) error {
			return commands.UpdateRepoConfig(fixture.ctx, fixture.repo, &branch, nil)
		}},
		{name: "settings", operation: AssetPutSettings, run: func(commands *RepositoryAssetCommands, fixture *wave56AssetFixture) error {
			return commands.PutSettings(fixture.ctx, fixture.repo, bundle)
		}},
		{name: "object", operation: AssetPutSettingsObject, run: func(commands *RepositoryAssetCommands, fixture *wave56AssetFixture) error {
			return commands.PutSettingsObject(fixture.ctx, fixture.repo, hash, bundle)
		}},
		{name: "secrets", operation: AssetSaveSecrets, run: func(commands *RepositoryAssetCommands, fixture *wave56AssetFixture) error {
			_, err := commands.SaveSecrets(fixture.ctx, fixture.input())
			return err
		}},
		{name: "profile", operation: AssetPatchProfile, run: func(commands *RepositoryAssetCommands, fixture *wave56AssetFixture) error {
			_, err := commands.PatchRepoProfile(fixture.ctx, fixture.repo, inbound.RepoProfilePatch{Description: &description})
			return err
		}},
	}
}

func TestWave56RepositoryAssetConstructorProfilesAndNilPorts(test *testing.T) {
	fixture := newWave56AssetFixture(test)
	for _, profile := range []RepositoryAssetProfile{"", "unknown"} {
		config := fixture.config
		config.Profile = profile
		if commands, err := NewRepositoryAssetCommands(config); commands != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatalf("invalid profile %q: owner=%v error=%v", profile, commands, err)
		}
	}
	missing := []struct {
		name     string
		required bool
		remove   func(*RepositoryAssetCommandsConfig)
	}{
		{name: "store", required: true, remove: func(config *RepositoryAssetCommandsConfig) { config.Store = nil }},
		{name: "scope", required: true, remove: func(config *RepositoryAssetCommandsConfig) { config.Scope = nil }},
		{name: "repositories", remove: func(config *RepositoryAssetCommandsConfig) { config.Repositories = nil }},
		{name: "cas", remove: func(config *RepositoryAssetCommandsConfig) { config.SecretsCAS = nil }},
		{name: "notifications", remove: func(config *RepositoryAssetCommandsConfig) { config.Notifications = nil }},
	}
	typedNil := []struct {
		name    string
		replace func(*RepositoryAssetCommandsConfig)
	}{
		{name: "store", replace: func(config *RepositoryAssetCommandsConfig) { config.Store = (*wave56AssetStore)(nil) }},
		{name: "scope", replace: func(config *RepositoryAssetCommandsConfig) { config.Scope = (*wave56AssetScope)(nil) }},
		{name: "repositories", replace: func(config *RepositoryAssetCommandsConfig) { config.Repositories = (*wave56AssetRepositoryReader)(nil) }},
		{name: "cas", replace: func(config *RepositoryAssetCommandsConfig) { config.SecretsCAS = (*wave56AssetCAS)(nil) }},
		{name: "notifications", replace: func(config *RepositoryAssetCommandsConfig) { config.Notifications = (*wave56AssetNotifications)(nil) }},
	}
	for _, profile := range []RepositoryAssetProfile{RepositoryAssetsCompatibility, RepositoryAssetsComplete} {
		config := fixture.config
		config.Profile = profile
		wave56AssetOwner(test, config)
		for _, missingPort := range missing {
			test.Run(string(profile)+"-missing-"+missingPort.name, func(test *testing.T) {
				candidate := config
				missingPort.remove(&candidate)
				commands, err := NewRepositoryAssetCommands(candidate)
				if !missingPort.required && profile == RepositoryAssetsCompatibility {
					if commands == nil || err != nil {
						test.Fatalf("compatibility rejected an omitted optional port: %v", err)
					}
					return
				}
				want := domain.ErrCatalogUnsupported
				if missingPort.required {
					want = domain.ErrValidation
				}
				if commands != nil || !errors.Is(err, want) {
					test.Fatalf("owner=%v error=%v, want %v", commands, err, want)
				}
			})
		}
		for _, nilPort := range typedNil {
			test.Run(string(profile)+"-typed-nil-"+nilPort.name, func(test *testing.T) {
				candidate := config
				nilPort.replace(&candidate)
				if commands, err := NewRepositoryAssetCommands(candidate); commands != nil || !errors.Is(err, domain.ErrValidation) {
					test.Fatalf("typed nil accepted: owner=%v error=%v", commands, err)
				}
			})
		}
	}
	minimal := fixture.config
	minimal.Profile, minimal.Repositories, minimal.SecretsCAS, minimal.Notifications = RepositoryAssetsCompatibility, nil, nil, nil
	wave56AssetOwner(test, minimal)
	if len(fixture.operations) != 0 || len(fixture.events) != 0 {
		test.Fatal("construction performed application work")
	}
}

func TestWave56RepositoryAssetsPreserveActorBoundContextAndScopeOperation(test *testing.T) {
	for _, action := range wave56AssetActions(test) {
		for _, system := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s-system-%t", action.name, system), func(test *testing.T) {
				fixture := newWave56AssetFixture(test)
				if system {
					fixture.ctx = inbound.WithSystemActor(fixture.ctx)
					if action.operation != AssetSaveSecrets {
						fixture.expectedActor, fixture.expectedSystem = "", true
					}
				} else if action.operation == AssetSaveSecrets {
					fixture.ctx = inbound.WithRepositoryActor(fixture.ctx, "different-caller")
				}
				ctx, cancel := context.WithDeadline(fixture.ctx, time.Now().Add(time.Hour))
				defer cancel()
				fixture.ctx = ctx
				if err := action.run(wave56AssetOwner(test, fixture.config), fixture); err != nil {
					test.Fatal(err)
				}
				if !reflect.DeepEqual(fixture.operations, []RepositoryAssetOperation{action.operation}) || fixture.callbacks != 1 || len(fixture.events) == 0 {
					test.Fatalf("operation escaped its single declared scope: operations=%v callbacks=%d events=%v", fixture.operations, fixture.callbacks, fixture.events)
				}
			})
		}
	}
}

func TestWave56RepositoryAssetsNeverRetryOutsideTheScope(test *testing.T) {
	for _, action := range wave56AssetActions(test) {
		for _, stage := range []string{"admission", "callback", "scope-exit"} {
			test.Run(action.name+"-"+stage, func(test *testing.T) {
				fixture := newWave56AssetFixture(test)
				want := domain.ErrRefConflict
				switch stage {
				case "admission":
					fixture.scopeErr = want
				case "callback":
					fixture.writeErr, fixture.casErr = want, want
					if action.operation == AssetSaveSecrets {
						want = domain.ErrSecretsConflict
					}
				case "scope-exit":
					fixture.exitErr = want
				}
				if err := action.run(wave56AssetOwner(test, fixture.config), fixture); !errors.Is(err, want) {
					test.Fatalf("error=%v, want %v", err, want)
				}
				wantCallbacks := 1
				if stage == "admission" {
					wantCallbacks = 0
				}
				if len(fixture.operations) != 1 || fixture.callbacks != wantCallbacks || fixture.casCalls > 1 || fixture.queueCalls > 1 {
					test.Fatalf("owner retried scope work: operations=%v callbacks=%d cas=%d queue=%d", fixture.operations, fixture.callbacks, fixture.casCalls, fixture.queueCalls)
				}
				if stage != "scope-exit" && fixture.writes != 0 {
					test.Fatal("rejected callback stored an asset")
				}
				attempts := 0
				for _, event := range fixture.events {
					switch event {
					case "update-about", "update-config", "put-settings", "put-settings-object", "cas-secrets":
						attempts++
					}
				}
				if attempts != wantCallbacks {
					test.Fatalf("owner retried a mutation inside its callback: attempts=%d callbacks=%d", attempts, wantCallbacks)
				}
			})
		}
	}
}

func TestWave56RepositoryAssetsPropagateCancellation(test *testing.T) {
	for _, action := range wave56AssetActions(test) {
		for _, stage := range []string{"before-scope", "inside-scope"} {
			test.Run(action.name+"-"+stage, func(test *testing.T) {
				fixture := newWave56AssetFixture(test)
				ctx, cancel := context.WithCancel(fixture.ctx)
				defer cancel()
				fixture.ctx = ctx
				if stage == "before-scope" {
					cancel()
				} else {
					fixture.beforeCallback = cancel
				}
				if err := action.run(wave56AssetOwner(test, fixture.config), fixture); !errors.Is(err, context.Canceled) {
					test.Fatalf("cancellation lost: %v", err)
				}
				if len(fixture.operations) != 1 || fixture.callbacks > 1 || fixture.writes != 0 || fixture.casCalls != 0 || fixture.queueCalls != 0 {
					test.Fatal("canceled scope retried or persisted work")
				}
			})
		}
	}
}

func TestWave56RepositoryAssetAboutNormalizationAndValidation(test *testing.T) {
	fixture := newWave56AssetFixture(test)
	commands := wave56AssetOwner(test, fixture.config)
	topics := make([]string, 22)
	for index := range topics {
		topics[index] = fmt.Sprintf("topic-%d", index)
	}
	if err := commands.UpdateAbout(fixture.ctx, fixture.repo, " \t description \n", " HTTP://Example.TEST/ ", topics); err != nil {
		test.Fatal(err)
	}
	if fixture.metadata.Description != "description" || fixture.metadata.Website != "http://example.test" || !reflect.DeepEqual(fixture.metadata.Topics, topics[:20]) || len(topics) != 22 {
		test.Fatalf("normalization changed: %+v", fixture.metadata)
	}
	for _, website := range []string{"javascript:alert(1)", "data:text/html,hello", "https://user:password@example.test", "https://example.test\nhttps://evil.test", "https://example.test\\@evil.test", strings.Repeat("a", 2049)} {
		test.Run(website[:min(len(website), 40)], func(test *testing.T) {
			fixture := newWave56AssetFixture(test)
			if err := wave56AssetOwner(test, fixture.config).UpdateAbout(fixture.ctx, fixture.repo, "description", website, nil); !errors.Is(err, domain.ErrValidation) || fixture.writes != 0 {
				test.Fatalf("invalid website reached storage: %v", err)
			}
		})
	}
	fixture = newWave56AssetFixture(test)
	fixture.repo = "invalid-repository"
	if err := wave56AssetOwner(test, fixture.config).UpdateAbout(fixture.ctx, fixture.repo, "description", "", nil); !errors.Is(err, domain.ErrIntegrity) || fixture.writes != 0 {
		test.Fatalf("invalid repository reached storage: %v", err)
	}
}

func TestWave56RepositoryAssetConfigPreservesOptionalValues(test *testing.T) {
	empty, branch, invalid := "", "release/topic", "../escape"
	protected, unprotected := true, false
	for _, scenario := range []struct {
		name      string
		branch    *string
		protected *bool
		failure   error
	}{
		{name: "unchanged"},
		{name: "explicit-empty-and-false", branch: &empty, protected: &unprotected},
		{name: "branch-and-protected", branch: &branch, protected: &protected},
		{name: "invalid-branch", branch: &invalid, failure: domain.ErrValidation},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newWave56AssetFixture(test)
			err := wave56AssetOwner(test, fixture.config).UpdateRepoConfig(fixture.ctx, fixture.repo, scenario.branch, scenario.protected)
			if !errors.Is(err, scenario.failure) {
				test.Fatalf("error=%v, want %v", err, scenario.failure)
			}
			if scenario.failure != nil {
				if fixture.writes != 0 {
					test.Fatal("invalid branch was stored")
				}
				return
			}
			if fixture.writes != 1 || !reflect.DeepEqual(fixture.branch, scenario.branch) || !reflect.DeepEqual(fixture.protected, scenario.protected) {
				test.Fatal("nil, empty or false configuration values changed")
			}
		})
	}
}

func TestWave56RepositoryAssetCommandsRejectMalformedRepositoryBeforeStoreWrites(test *testing.T) {
	for _, action := range wave56AssetActions(test) {
		if action.operation == AssetSaveSecrets || action.operation == AssetPatchProfile {
			continue
		}
		test.Run(action.name, func(test *testing.T) {
			fixture := newWave56AssetFixture(test)
			fixture.repo = "malformed-repository"
			if err := action.run(wave56AssetOwner(test, fixture.config), fixture); !errors.Is(err, domain.ErrIntegrity) || len(fixture.events) != 0 || fixture.writes != 0 {
				test.Fatalf("malformed repository reached an asset port: %v", err)
			}
		})
	}
}

func TestWave56RepositoryAssetProfileUsesOneOrderedCompoundScope(test *testing.T) {
	fixture := newWave56AssetFixture(test)
	commands := wave56AssetOwner(test, fixture.config)
	identity, description, website, branch, protected := domain.DocumentIdentityRootV1, "  updated description  ", "HTTPS://Example.TEST/", "release/topic", true
	patch := inbound.RepoProfilePatch{RequiredDocIdentity: &identity, Description: &description, Website: &website, Topics: []string{"updated"}, DefaultBranch: &branch, ProtectDefault: &protected}
	result, err := commands.PatchRepoProfile(fixture.ctx, fixture.repo, patch)
	if err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.operations, []RepositoryAssetOperation{AssetPatchProfile}) || fixture.callbacks != 1 || !reflect.DeepEqual(fixture.events, []string{"get-repo", "require-identity", "update-config", "update-about", "get-repo"}) {
		test.Fatalf("compound profile work was split, reordered or assigned a different audit scope: operations=%v events=%v", fixture.operations, fixture.events)
	}
	if !reflect.DeepEqual(result, fixture.metadata) || result.ID != fixture.repo || result.RepositoryID != fixture.record.ID || result.RemoteURL != "https://example.test/team/project" || result.DefaultBranch != branch || !result.ProtectDefault || result.RequiredDocIdentity != identity || result.Description != "updated description" || result.Website != "https://example.test" || !reflect.DeepEqual(result.Topics, patch.Topics) {
		test.Fatalf("compound result did not reread the stored profile: %+v", result)
	}
	if !reflect.DeepEqual(fixture.required, []domain.DocumentIdentity{identity}) || description != "  updated description  " || website != "HTTPS://Example.TEST/" {
		test.Fatal("identity requirement or caller patch values changed")
	}
}

func TestWave56RepositoryAssetProfilePreservesAbsentFieldsAndClearsExplicitTopics(test *testing.T) {
	for _, clearTopics := range []bool{false, true} {
		test.Run(fmt.Sprintf("clear-topics-%t", clearTopics), func(test *testing.T) {
			fixture := newWave56AssetFixture(test)
			original := fixture.metadata
			patch := inbound.RepoProfilePatch{}
			wantEvents := []string{"get-repo", "get-repo"}
			if clearTopics {
				patch.Topics = []string{}
				wantEvents = []string{"get-repo", "update-about", "get-repo"}
			}
			result, err := wave56AssetOwner(test, fixture.config).PatchRepoProfile(fixture.ctx, fixture.repo, patch)
			if err != nil || !reflect.DeepEqual(fixture.events, wantEvents) || result.Description != original.Description || result.DefaultBranch != original.DefaultBranch || result.Website != original.Website {
				test.Fatalf("absent profile fields changed: result=%+v error=%v events=%v", result, err, fixture.events)
			}
			if clearTopics && len(result.Topics) != 0 || !clearTopics && !reflect.DeepEqual(result.Topics, original.Topics) {
				test.Fatal("absent and explicitly empty topics were conflated")
			}
		})
	}
}

func TestWave56RepositoryAssetProfileValidatesBeforeConfigAndStopsOnFailure(test *testing.T) {
	failure := errors.New("profile prerequisite unavailable")
	for _, stage := range []string{"read", "identity", "branch", "website", "config"} {
		test.Run(stage, func(test *testing.T) {
			fixture := newWave56AssetFixture(test)
			identity, description, website, branch := domain.DocumentIdentityRootV1, "updated", "example.test", "release/topic"
			patch := inbound.RepoProfilePatch{Description: &description, Website: &website, DefaultBranch: &branch}
			want, wantEvents := failure, []string{"get-repo"}
			switch stage {
			case "read":
				fixture.repoReadErr = failure
			case "identity":
				patch.RequiredDocIdentity, fixture.requireErr = &identity, failure
				wantEvents = append(wantEvents, "require-identity")
			case "branch":
				branch, want = "../escape", domain.ErrValidation
			case "website":
				website, want = "https://user:password@example.test", domain.ErrValidation
			case "config":
				fixture.writeErr = failure
				wantEvents = append(wantEvents, "update-config")
			}
			result, err := wave56AssetOwner(test, fixture.config).PatchRepoProfile(fixture.ctx, fixture.repo, patch)
			if !errors.Is(err, want) || !reflect.DeepEqual(result, domain.Repo{}) || fixture.writes != 0 || !reflect.DeepEqual(fixture.events, wantEvents) || fixture.callbacks != 1 || !reflect.DeepEqual(fixture.operations, []RepositoryAssetOperation{AssetPatchProfile}) {
				test.Fatalf("profile failure escaped its single scope: result=%+v error=%v events=%v", result, err, fixture.events)
			}
		})
	}
}

func TestWave56RepositoryAssetProfileReturnsCompoundFailureToScope(test *testing.T) {
	fixture := newWave56AssetFixture(test)
	failure := errors.New("about write failed after configuration")
	store := fixture.config.Store.(*wave56AssetStore)
	updateConfig := store.updateConfig
	store.updateConfig = func(ctx context.Context, repo domain.ContentHash, branch *string, protected *bool) error {
		if err := updateConfig(ctx, repo, branch, protected); err != nil {
			return err
		}
		fixture.writeErr = failure
		return nil
	}
	branch, description := "release/topic", "updated"
	result, err := wave56AssetOwner(test, fixture.config).PatchRepoProfile(fixture.ctx, fixture.repo, inbound.RepoProfilePatch{DefaultBranch: &branch, Description: &description})
	if !errors.Is(err, failure) || !reflect.DeepEqual(result, domain.Repo{}) || fixture.writes != 1 || fixture.callbacks != 1 || !reflect.DeepEqual(fixture.operations, []RepositoryAssetOperation{AssetPatchProfile}) || !reflect.DeepEqual(fixture.events, []string{"get-repo", "update-config", "update-about"}) {
		test.Fatalf("compound failure was hidden, retried or reread as success: result=%+v error=%v events=%v", result, err, fixture.events)
	}
}

func TestWave56RepositoryAssetSettingsKindsMetadataTimeAndObjectHash(test *testing.T) {
	for _, kind := range []string{"claude", "agents", "codex"} {
		test.Run(kind, func(test *testing.T) {
			fixture := newWave56AssetFixture(test)
			commands := wave56AssetOwner(test, fixture.config)
			bundle := wave56AssetBundle(kind)
			original := bundle
			original.Files = append([]domain.SettingsFile(nil), bundle.Files...)
			before := time.Now().UTC()
			if err := commands.PutSettings(fixture.ctx, fixture.repo, bundle); err != nil {
				test.Fatal(err)
			}
			after := time.Now().UTC()
			if len(fixture.settings) != 1 {
				test.Fatal("latest settings were not stored exactly once")
			}
			stored := fixture.settings[0]
			if stored.UpdatedAt.Before(before) || stored.UpdatedAt.After(after) || stored.UpdatedAt.Location() != time.UTC {
				test.Fatalf("latest timestamp is not server UTC time: %v", stored.UpdatedAt)
			}
			stored.UpdatedAt = bundle.UpdatedAt
			if !reflect.DeepEqual(stored, original) || !reflect.DeepEqual(bundle, original) {
				test.Fatal("latest settings mutated files, attribution or caller metadata")
			}
			hash := wave56AssetHash(test, bundle)
			if err := commands.PutSettingsObject(fixture.ctx, fixture.repo, hash, bundle); err != nil {
				test.Fatal(err)
			}
			changedMetadata := bundle
			changedMetadata.UpdatedAt, changedMetadata.UpdatedBy = time.Time{}, "different-publisher"
			if wave56AssetHash(test, changedMetadata) != hash {
				test.Fatal("object hash unexpectedly includes publication metadata")
			}
			if err := commands.PutSettingsObject(fixture.ctx, fixture.repo, hash, changedMetadata); err != nil {
				test.Fatal(err)
			}
			if !reflect.DeepEqual(fixture.objects, []domain.SettingsBundle{bundle, changedMetadata}) || !reflect.DeepEqual(fixture.objectHashes, []domain.ContentHash{hash, hash}) {
				test.Fatal("immutable settings object metadata or supplied hash changed")
			}
			if err := commands.PutSettingsObject(fixture.ctx, fixture.repo, domain.HashContent([]byte("wrong-object")), bundle); !errors.Is(err, domain.ErrIntegrity) || len(fixture.objects) != 2 {
				test.Fatalf("wrong content hash reached storage: %v", err)
			}
		})
	}
}

func TestWave56RepositoryAssetSettingsRejectUnsafeBundlesAndHashes(test *testing.T) {
	for _, kind := range []string{"claude", "agents", "codex"} {
		for _, scenario := range []struct {
			name   string
			change func(*domain.SettingsBundle)
		}{
			{name: "unknown-kind", change: func(bundle *domain.SettingsBundle) { bundle.Kind = "unsupported" }},
			{name: "parent-path", change: func(bundle *domain.SettingsBundle) { bundle.Files[0].Path = "../secret" }},
			{name: "drive-path", change: func(bundle *domain.SettingsBundle) { bundle.Files[0].Path = "C:/secret" }},
			{name: "device-path", change: func(bundle *domain.SettingsBundle) { bundle.Files[0].Path = "commands/CON.md" }},
			{name: "duplicate-path", change: func(bundle *domain.SettingsBundle) { bundle.Files = append(bundle.Files, bundle.Files[0]) }},
			{name: "invalid-base64", change: func(bundle *domain.SettingsBundle) { bundle.Files[0].ContentB64 = "!" }},
		} {
			for _, object := range []bool{false, true} {
				test.Run(fmt.Sprintf("%s-%s-object-%t", kind, scenario.name, object), func(test *testing.T) {
					fixture := newWave56AssetFixture(test)
					commands := wave56AssetOwner(test, fixture.config)
					bundle := wave56AssetBundle(kind)
					scenario.change(&bundle)
					var err error
					if object {
						err = commands.PutSettingsObject(fixture.ctx, fixture.repo, wave56AssetHash(test, bundle), bundle)
					} else {
						err = commands.PutSettings(fixture.ctx, fixture.repo, bundle)
					}
					if !errors.Is(err, domain.ErrIntegrity) || fixture.writes != 0 {
						test.Fatalf("invalid bundle reached storage: %v", err)
					}
				})
			}
		}
	}
	for _, invalidKey := range []string{"repository", "object"} {
		test.Run("malformed-"+invalidKey, func(test *testing.T) {
			fixture := newWave56AssetFixture(test)
			bundle := wave56AssetBundle("codex")
			hash := wave56AssetHash(test, bundle)
			if invalidKey == "repository" {
				fixture.repo = "invalid"
			} else {
				hash = "invalid"
			}
			if err := wave56AssetOwner(test, fixture.config).PutSettingsObject(fixture.ctx, fixture.repo, hash, bundle); !errors.Is(err, domain.ErrIntegrity) || fixture.writes != 0 {
				test.Fatalf("malformed key reached storage: %v", err)
			}
		})
	}
}

func TestWave56RepositoryAssetSettingsDecodedSizeBoundary(test *testing.T) {
	for _, object := range []bool{false, true} {
		for _, overflow := range []bool{false, true} {
			test.Run(fmt.Sprintf("object-%t-overflow-%t", object, overflow), func(test *testing.T) {
				fixture := newWave56AssetFixture(test)
				commands := wave56AssetOwner(test, fixture.config)
				bundle := domain.SettingsBundle{Kind: "codex", Files: []domain.SettingsFile{{Path: "first", ContentB64: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), domain.MaxSettingsBundleBytes/2))}, {Path: "second", ContentB64: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("b"), domain.MaxSettingsBundleBytes/2))}}}
				if overflow {
					bundle.Files = append(bundle.Files, domain.SettingsFile{Path: "extra", ContentB64: "Yw=="})
				}
				var err error
				if object {
					err = commands.PutSettingsObject(fixture.ctx, fixture.repo, wave56AssetHash(test, bundle), bundle)
				} else {
					err = commands.PutSettings(fixture.ctx, fixture.repo, bundle)
				}
				if overflow {
					if !errors.Is(err, domain.ErrIntegrity) || fixture.writes != 0 {
						test.Fatalf("decoded aggregate size limit bypassed: %v", err)
					}
				} else if err != nil || fixture.writes != 1 {
					test.Fatalf("exact size boundary rejected: %v", err)
				}
			})
		}
	}
}

func TestWave56RepositoryAssetSecretsOriginalBaselineAndFreshGeneration(test *testing.T) {
	fixture := newWave56AssetFixture(test)
	commands := wave56AssetOwner(test, fixture.config)
	fixture.current = append([]byte(" \n"), wave56AssetEnvelope("aaaaaaaaaaaa")...)
	fixture.currentErr = nil
	original := bytes.Clone(fixture.current)
	input := fixture.input()
	input.Edit.ExpectedRevision = domain.SecretsRevision(original)
	input.Envelope = bytes.Replace(input.Envelope, []byte(`"version":1`), []byte(`"revision":"client-generation","extension":{"opaque":true},"version":1`), 1)
	candidate := bytes.Clone(input.Envelope)
	result, err := commands.SaveSecrets(fixture.ctx, input)
	if err != nil || result.Status != "stored" || result.Revision == "" || result.Revision == "client-generation" || result.Revision == input.Edit.ExpectedRevision || result.Revision != domain.SecretsRevision(fixture.next) {
		test.Fatalf("generation contract failed: result=%+v error=%v", result, err)
	}
	if !bytes.Equal(fixture.previous, original) || !bytes.Equal(input.Envelope, candidate) || len(fixture.authorized) != 1 || !reflect.DeepEqual(fixture.authorized[0], input) {
		test.Fatal("original edit, candidate bytes or exact CAS baseline changed")
	}
	var submitted, stored map[string]json.RawMessage
	if err := json.Unmarshal(candidate, &submitted); err != nil {
		test.Fatal(err)
	}
	if err := json.Unmarshal(fixture.next, &stored); err != nil {
		test.Fatal(err)
	}
	delete(submitted, "revision")
	delete(stored, "revision")
	if !reflect.DeepEqual(submitted, stored) {
		test.Fatal("server changed opaque envelope fields")
	}
	firstRevision := result.Revision
	input.Edit.ExpectedRevision = firstRevision
	result, err = commands.SaveSecrets(fixture.ctx, input)
	if err != nil || result.Revision == firstRevision || result.Revision != domain.SecretsRevision(fixture.current) {
		test.Fatalf("identical save reused generation: result=%+v error=%v", result, err)
	}
	before, casCalls, queueCalls := bytes.Clone(fixture.current), fixture.casCalls, fixture.queueCalls
	result, err = commands.SaveSecrets(fixture.ctx, input)
	if !errors.Is(err, domain.ErrSecretsConflict) || result != (inbound.SaveSecretsOutput{}) || fixture.casCalls != casCalls || fixture.queueCalls != queueCalls || !bytes.Equal(before, fixture.current) {
		test.Fatalf("stale original baseline was refreshed or stored: result=%+v error=%v", result, err)
	}
}

func TestWave56RepositoryAssetSecretsFingerprintAndEditFailures(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*wave56AssetFixture, *inbound.SaveSecretsInput)
		want   error
	}{
		{name: "missing-baseline", change: func(_ *wave56AssetFixture, input *inbound.SaveSecretsInput) { input.Edit.ExpectedRevision = "" }, want: domain.ErrSecretsRevisionRequired},
		{name: "stale-baseline", change: func(_ *wave56AssetFixture, input *inbound.SaveSecretsInput) { input.Edit.ExpectedRevision = "stale" }, want: domain.ErrSecretsConflict},
		{name: "changed-passphrase", change: func(_ *wave56AssetFixture, input *inbound.SaveSecretsInput) {
			input.Envelope = wave56AssetEnvelope("bbbbbbbbbbbb")
		}, want: domain.ErrSecretsPassphraseMismatch},
		{name: "fingerprint-removed", change: func(_ *wave56AssetFixture, input *inbound.SaveSecretsInput) { input.Envelope = wave56AssetEnvelope("") }, want: domain.ErrSecretsFingerprintRequired},
		{name: "rotation-baseline", change: func(_ *wave56AssetFixture, input *inbound.SaveSecretsInput) {
			input.Edit.Rotate, input.Edit.ExpectedFingerprint = true, "cccccccccccc"
			input.Envelope = wave56AssetEnvelope("bbbbbbbbbbbb")
		}, want: domain.ErrSecretsRotateConflict},
		{name: "rotation-missing-fingerprint", change: func(_ *wave56AssetFixture, input *inbound.SaveSecretsInput) {
			input.Edit.Rotate, input.Edit.ExpectedFingerprint = true, "aaaaaaaaaaaa"
			input.Envelope = wave56AssetEnvelope("")
		}, want: domain.ErrSecretsFingerprintRequired},
		{name: "malformed-candidate", change: func(_ *wave56AssetFixture, input *inbound.SaveSecretsInput) { input.Envelope = []byte("null") }, want: domain.ErrSecretsMalformedEnvelope},
		{name: "unsupported-crypto", change: func(_ *wave56AssetFixture, input *inbound.SaveSecretsInput) {
			input.Envelope = bytes.Replace(input.Envelope, []byte("600000"), []byte("1"), 1)
		}, want: domain.ErrIntegrity},
		{name: "corrupt-current", change: func(fixture *wave56AssetFixture, input *inbound.SaveSecretsInput) {
			fixture.current = []byte("corrupt")
			input.Edit.ExpectedRevision = domain.SecretsRevision(fixture.current)
		}, want: domain.ErrSecretsConsistency},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newWave56AssetFixture(test)
			fixture.current, fixture.currentErr = wave56AssetEnvelope("aaaaaaaaaaaa"), nil
			input := fixture.input()
			input.Edit.ExpectedRevision = domain.SecretsRevision(fixture.current)
			scenario.change(fixture, &input)
			original := bytes.Clone(fixture.current)
			result, err := wave56AssetOwner(test, fixture.config).SaveSecrets(fixture.ctx, input)
			if !errors.Is(err, scenario.want) || result != (inbound.SaveSecretsOutput{}) || fixture.casCalls != 0 || fixture.queueCalls != 0 || !bytes.Equal(original, fixture.current) {
				test.Fatalf("rejected edit mutated storage: result=%+v error=%v, want %v", result, err, scenario.want)
			}
		})
	}
	for _, rotation := range []bool{false, true} {
		test.Run(fmt.Sprintf("accepted-rotation-%t", rotation), func(test *testing.T) {
			fixture := newWave56AssetFixture(test)
			oldFingerprint := ""
			if rotation {
				oldFingerprint = "bbbbbbbbbbbb"
			}
			fixture.current, fixture.currentErr = wave56AssetEnvelope(oldFingerprint), nil
			input := fixture.input()
			input.Edit = domain.SecretsEdit{ExpectedRevision: domain.SecretsRevision(fixture.current), Rotate: rotation, ExpectedFingerprint: oldFingerprint}
			if _, err := wave56AssetOwner(test, fixture.config).SaveSecrets(fixture.ctx, input); err != nil || fixture.casCalls != 1 {
				test.Fatalf("legacy fingerprint upgrade or explicit rotation rejected: %v", err)
			}
		})
	}
}

func TestWave56RepositoryAssetSecretsFailClosedAndDoNotRetryCAS(test *testing.T) {
	readFailure, casFailure := errors.New("baseline unavailable"), errors.New("CAS unavailable")
	for _, scenario := range []struct {
		name   string
		change func(*wave56AssetFixture)
		want   error
		calls  int
	}{
		{name: "authority", change: func(fixture *wave56AssetFixture) { fixture.authorizeErr = domain.ErrForbidden }, want: domain.ErrForbidden},
		{name: "read", change: func(fixture *wave56AssetFixture) { fixture.currentErr = readFailure }, want: readFailure},
		{name: "read-deadline", change: func(fixture *wave56AssetFixture) { fixture.currentErr = context.DeadlineExceeded }, want: context.DeadlineExceeded},
		{name: "CAS-conflict", change: func(fixture *wave56AssetFixture) { fixture.casErr = domain.ErrRefConflict }, want: domain.ErrSecretsConflict, calls: 1},
		{name: "CAS-error", change: func(fixture *wave56AssetFixture) { fixture.casErr = casFailure }, want: casFailure, calls: 1},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newWave56AssetFixture(test)
			scenario.change(fixture)
			result, err := wave56AssetOwner(test, fixture.config).SaveSecrets(fixture.ctx, fixture.input())
			if !errors.Is(err, scenario.want) || result != (inbound.SaveSecretsOutput{}) || fixture.casCalls != scenario.calls || fixture.writes != 0 || fixture.queueCalls != 0 || fixture.callbacks != 1 || len(fixture.operations) != 1 {
				test.Fatalf("failure was hidden or retried: result=%+v error=%v events=%v", result, err, fixture.events)
			}
			if strings.HasPrefix(scenario.name, "read") && !errors.Is(err, domain.ErrSecretsConsistency) {
				test.Fatal("baseline read failure lost its consistency classification")
			}
			if scenario.name == "authority" && !reflect.DeepEqual(fixture.events, []string{"authorize-secrets"}) {
				test.Fatal("ciphertext was read before authority succeeded")
			}
		})
	}
	for _, scenario := range []string{"anonymous", "invalid-repository", "missing-CAS"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newWave56AssetFixture(test)
			input := fixture.input()
			want := domain.ErrUnauthorized
			switch scenario {
			case "anonymous":
				input.ActorID = ""
			case "invalid-repository":
				input.RepoID, want = "invalid", domain.ErrIntegrity
			case "missing-CAS":
				fixture.config.Profile, fixture.config.SecretsCAS = RepositoryAssetsCompatibility, nil
				want = domain.ErrSecretsConsistency
			}
			result, err := wave56AssetOwner(test, fixture.config).SaveSecrets(fixture.ctx, input)
			if !errors.Is(err, want) || result != (inbound.SaveSecretsOutput{}) || len(fixture.operations) != 0 || len(fixture.events) != 0 {
				test.Fatalf("invalid request entered storage: result=%+v error=%v", result, err)
			}
		})
	}
	fixture := newWave56AssetFixture(test)
	fixture.current = []byte("ignored bytes returned with not found")
	if _, err := wave56AssetOwner(test, fixture.config).SaveSecrets(fixture.ctx, fixture.input()); err != nil || fixture.previous != nil {
		test.Fatalf("not-found baseline was not represented by nil CAS bytes: %v", err)
	}
}

func TestWave56RepositoryAssetSecretsEnvelopeAndServerRevisionSizeLimits(test *testing.T) {
	for _, size := range []int{domain.MaxSecretsEnvelopeBytes - 128, domain.MaxSecretsEnvelopeBytes, domain.MaxSecretsEnvelopeBytes + 1} {
		test.Run(fmt.Sprint(size), func(test *testing.T) {
			fixture := newWave56AssetFixture(test)
			input := fixture.input()
			prefix := strings.TrimSuffix(string(input.Envelope), "}") + `,"padding":"`
			input.Envelope = []byte(prefix + strings.Repeat("x", size-len(prefix)-2) + `"}`)
			if len(input.Envelope) != size {
				test.Fatal("incorrect size fixture")
			}
			result, err := wave56AssetOwner(test, fixture.config).SaveSecrets(fixture.ctx, input)
			if size < domain.MaxSecretsEnvelopeBytes {
				if err != nil || fixture.casCalls != 1 || len(fixture.next) > domain.MaxSecretsEnvelopeBytes || result.Revision != domain.SecretsRevision(fixture.next) {
					test.Fatalf("bounded envelope rejected: result=%+v error=%v", result, err)
				}
			} else if !errors.Is(err, domain.ErrIntegrity) || result != (inbound.SaveSecretsOutput{}) || fixture.casCalls != 0 || fixture.queueCalls != 0 {
				test.Fatalf("candidate or server-generated revision exceeded stored size limit: result=%+v error=%v", result, err)
			}
		})
	}
}

func TestWave56RepositoryAssetSecretsNotificationIsBoundMetadataOnly(test *testing.T) {
	fixture := newWave56AssetFixture(test)
	before := time.Now().UTC()
	if _, err := wave56AssetOwner(test, fixture.config).SaveSecrets(fixture.ctx, fixture.input()); err != nil {
		test.Fatal(err)
	}
	after := time.Now().UTC()
	if len(fixture.deliveries) != 1 || !reflect.DeepEqual(fixture.events, []string{"authorize-secrets", "get-secrets", "cas-secrets", "get-repo", "get-repository", "enqueue-notification"}) {
		test.Fatalf("secrets notification sequence changed: %v", fixture.events)
	}
	delivery := fixture.deliveries[0]
	job := delivery.Job
	if delivery.Destination != fixture.record.WebhookURL || job.RepositoryID != fixture.record.ID || job.Kind != "secrets_updated" || job.State != "pending" || !strings.HasPrefix(job.ID, "evt_") || job.Text != "cxthub: project — secrets updated (team members run cxt secrets pull)" {
		test.Fatalf("notification metadata changed: %+v", job)
	}
	if job.CreatedAt.Before(before) || job.CreatedAt.After(after) || job.CreatedAt.Location() != time.UTC || !job.UpdatedAt.Equal(job.CreatedAt) || !job.NextAttempt.Equal(job.CreatedAt) {
		test.Fatal("notification scheduling did not use one server UTC timestamp")
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		test.Fatal(err)
	}
	for _, forbidden := range []string{"ciphertext_b64", "b3BhcXVlIGNpcGhlcnRleHQ", "aaaaaaaaaaaa", string(fixture.next), fixture.record.WebhookURL, "private-credential"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			test.Fatalf("notification job leaked opaque content or destination credentials: %q", forbidden)
		}
	}
	for _, scenario := range []string{"no-webhook", "unowned", "compatibility-no-reader"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newWave56AssetFixture(test)
			switch scenario {
			case "no-webhook":
				fixture.record.WebhookURL = ""
			case "unowned":
				fixture.metadata.RepositoryID = ""
			case "compatibility-no-reader":
				fixture.config.Profile, fixture.config.Repositories, fixture.config.Notifications = RepositoryAssetsCompatibility, nil, nil
			}
			if _, err := wave56AssetOwner(test, fixture.config).SaveSecrets(fixture.ctx, fixture.input()); err != nil || fixture.casCalls != 1 || fixture.queueCalls != 0 {
				test.Fatalf("notification-free save changed: %v", err)
			}
		})
	}
}

func TestWave56RepositoryAssetSecretsNotificationErrorsReturnToScope(test *testing.T) {
	failure := errors.New("notification metadata or queue unavailable")
	for _, stage := range []string{"repo", "repository", "queue"} {
		test.Run(stage, func(test *testing.T) {
			fixture := newWave56AssetFixture(test)
			switch stage {
			case "repo":
				fixture.repoReadErr = failure
			case "repository":
				fixture.repositoryErr = failure
			case "queue":
				fixture.queueErr = failure
			}
			result, err := wave56AssetOwner(test, fixture.config).SaveSecrets(fixture.ctx, fixture.input())
			if !errors.Is(err, failure) || result != (inbound.SaveSecretsOutput{}) || fixture.casCalls != 1 || fixture.callbacks != 1 || len(fixture.operations) != 1 || len(fixture.deliveries) != 0 {
				test.Fatalf("notification error was hidden or retried: result=%+v error=%v", result, err)
			}
		})
	}
}

func TestWave56RepositoryAssetTypedScopesOwnFinalResults(test *testing.T) {
	failure := errors.New("typed scope final error")
	fixture := newWave56AssetFixture(test)
	wantSecrets := inbound.SaveSecretsOutput{Status: "scope-result", Revision: "scope-revision"}
	fixture.scope.secrets = func(ctx context.Context, repo domain.ContentHash, _ func(context.Context) (inbound.SaveSecretsOutput, error)) (inbound.SaveSecretsOutput, error) {
		fixture.checkContext(ctx, false)
		if repo != fixture.repo {
			test.Fatal("typed secrets scope lost repository")
		}
		return wantSecrets, failure
	}
	commands := wave56AssetOwner(test, fixture.config)
	if result, err := commands.SaveSecrets(fixture.ctx, fixture.input()); result != wantSecrets || !errors.Is(err, failure) || len(fixture.events) != 0 {
		test.Fatalf("typed secrets result was captured outside the scope: result=%+v error=%v", result, err)
	}
	wantProfile := domain.Repo{ID: fixture.repo, Description: "scope-result"}
	fixture.scope.profile = func(ctx context.Context, repo domain.ContentHash, _ func(context.Context) (domain.Repo, error)) (domain.Repo, error) {
		fixture.checkContext(ctx, false)
		if repo != fixture.repo {
			test.Fatal("typed profile scope lost repository")
		}
		return wantProfile, failure
	}
	if result, err := commands.PatchRepoProfile(fixture.ctx, fixture.repo, inbound.RepoProfilePatch{}); !reflect.DeepEqual(result, wantProfile) || !errors.Is(err, failure) || len(fixture.events) != 0 {
		test.Fatalf("typed profile result was captured outside the scope: result=%+v error=%v", result, err)
	}
}

func TestWave56RepositoryAssetSecretsScopeRetryCannotRetainEarlierResult(test *testing.T) {
	fixture := newWave56AssetFixture(test)
	fixture.scope.secrets = func(ctx context.Context, repo domain.ContentHash, write func(context.Context) (inbound.SaveSecretsOutput, error)) (inbound.SaveSecretsOutput, error) {
		bound, err := fixture.enter(ctx, repo, AssetSaveSecrets)
		if err != nil {
			return inbound.SaveSecretsOutput{}, err
		}
		defer func() { fixture.active = false }()
		fixture.callbacks++
		first, err := write(bound)
		if err != nil || first.Status != "stored" || first.Revision == "" {
			test.Fatalf("initial retry attempt failed: result=%+v error=%v", first, err)
		}
		fixture.current, fixture.currentErr, fixture.authorizeErr = nil, domain.ErrNotFound, domain.ErrForbidden
		fixture.callbacks++
		return write(bound)
	}
	result, err := wave56AssetOwner(test, fixture.config).SaveSecrets(fixture.ctx, fixture.input())
	if !errors.Is(err, domain.ErrForbidden) || result != (inbound.SaveSecretsOutput{}) || len(fixture.operations) != 1 || fixture.callbacks != 2 || len(fixture.authorized) != 2 || fixture.casCalls != 1 {
		test.Fatalf("earlier callback result survived the scope's rejected retry: result=%+v error=%v", result, err)
	}
}
