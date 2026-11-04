package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type repositoryAssetHTTPCommands struct {
	calls         []string
	repo          domain.ContentHash
	hash          domain.ContentHash
	bundle        domain.SettingsBundle
	secrets       inbound.SaveSecretsInput
	profile       inbound.RepoProfilePatch
	profileResult domain.Repo
	err           error
	observe       func(context.Context, domain.ContentHash)
}

func (commands *repositoryAssetHTTPCommands) record(ctx context.Context, repo domain.ContentHash, command string) error {
	commands.calls = append(commands.calls, command)
	commands.repo = repo
	if commands.observe != nil {
		commands.observe(ctx, repo)
	}
	return commands.err
}

func (commands *repositoryAssetHTTPCommands) UpdateAbout(ctx context.Context, repo domain.ContentHash, description, website string, topics []string) error {
	return commands.record(ctx, repo, "about")
}

func (commands *repositoryAssetHTTPCommands) UpdateRepoConfig(ctx context.Context, repo domain.ContentHash, defaultBranch *string, protectDefault *bool) error {
	return commands.record(ctx, repo, "config")
}

func (commands *repositoryAssetHTTPCommands) PatchRepoProfile(ctx context.Context, repo domain.ContentHash, patch inbound.RepoProfilePatch) (domain.Repo, error) {
	commands.profile = patch
	return commands.profileResult, commands.record(ctx, repo, "profile")
}

func (commands *repositoryAssetHTTPCommands) PutSettings(ctx context.Context, repo domain.ContentHash, bundle domain.SettingsBundle) error {
	commands.bundle = bundle
	return commands.record(ctx, repo, "settings")
}

func (commands *repositoryAssetHTTPCommands) PutSettingsObject(ctx context.Context, repo, hash domain.ContentHash, bundle domain.SettingsBundle) error {
	commands.hash, commands.bundle = hash, bundle
	return commands.record(ctx, repo, "settings object")
}

func (commands *repositoryAssetHTTPCommands) SaveSecrets(ctx context.Context, input inbound.SaveSecretsInput) (inbound.SaveSecretsOutput, error) {
	commands.secrets = input
	return inbound.SaveSecretsOutput{Status: "stored", Revision: "explicit-revision"}, commands.record(ctx, input.RepoID, "secrets")
}

type repositoryAssetHTTPBackend struct {
	Backend
	*repositoryAssetHTTPCommands
	repository domain.Repo
}

func (backend *repositoryAssetHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return backend.repository, nil
}

func (backend *repositoryAssetHTTPBackend) UpdateAbout(ctx context.Context, repo domain.ContentHash, description, website string, topics []string) error {
	return backend.repositoryAssetHTTPCommands.UpdateAbout(ctx, repo, description, website, topics)
}

func (backend *repositoryAssetHTTPBackend) UpdateRepoConfig(ctx context.Context, repo domain.ContentHash, defaultBranch *string, protectDefault *bool) error {
	return backend.repositoryAssetHTTPCommands.UpdateRepoConfig(ctx, repo, defaultBranch, protectDefault)
}

func (backend *repositoryAssetHTTPBackend) PatchRepoProfile(ctx context.Context, repo domain.ContentHash, patch inbound.RepoProfilePatch) (domain.Repo, error) {
	return backend.repositoryAssetHTTPCommands.PatchRepoProfile(ctx, repo, patch)
}

func (backend *repositoryAssetHTTPBackend) PutSettings(ctx context.Context, repo domain.ContentHash, bundle domain.SettingsBundle) error {
	return backend.repositoryAssetHTTPCommands.PutSettings(ctx, repo, bundle)
}

func (backend *repositoryAssetHTTPBackend) PutSettingsObject(ctx context.Context, repo, hash domain.ContentHash, bundle domain.SettingsBundle) error {
	return backend.repositoryAssetHTTPCommands.PutSettingsObject(ctx, repo, hash, bundle)
}

func (backend *repositoryAssetHTTPBackend) SaveSecrets(ctx context.Context, input inbound.SaveSecretsInput) (inbound.SaveSecretsOutput, error) {
	return backend.repositoryAssetHTTPCommands.SaveSecrets(ctx, input)
}

type repositoryAssetHTTPPoisonedBackend struct{ *repositoryAssetHTTPBackend }

func (*repositoryAssetHTTPPoisonedBackend) UpdateAbout(context.Context, domain.ContentHash, string, string, []string) error {
	panic("about command reached the legacy HTTP facade")
}

func (*repositoryAssetHTTPPoisonedBackend) UpdateRepoConfig(context.Context, domain.ContentHash, *string, *bool) error {
	panic("repository config command reached the legacy HTTP facade")
}

func (*repositoryAssetHTTPPoisonedBackend) PatchRepoProfile(context.Context, domain.ContentHash, inbound.RepoProfilePatch) (domain.Repo, error) {
	panic("profile patch command reached the legacy HTTP facade")
}

func (*repositoryAssetHTTPPoisonedBackend) PutSettings(context.Context, domain.ContentHash, domain.SettingsBundle) error {
	panic("settings command reached the legacy HTTP facade")
}

func (*repositoryAssetHTTPPoisonedBackend) PutSettingsObject(context.Context, domain.ContentHash, domain.ContentHash, domain.SettingsBundle) error {
	panic("settings object command reached the legacy HTTP facade")
}

func (*repositoryAssetHTTPPoisonedBackend) SaveSecrets(context.Context, inbound.SaveSecretsInput) (inbound.SaveSecretsOutput, error) {
	panic("secrets command reached the legacy HTTP facade")
}

type repositoryAssetHTTPIdentity struct {
	IdentityBackend
	repository domain.Repository
	role       domain.MemberRole
}

func (identity *repositoryAssetHTTPIdentity) GetRepository(context.Context, string) (domain.Repository, error) {
	return identity.repository, nil
}

func (*repositoryAssetHTTPIdentity) ResolveUser(context.Context, string) (domain.User, error) {
	return domain.User{ID: "asset-actor", Username: "asset-editor"}, nil
}

func (identity *repositoryAssetHTTPIdentity) RoleOf(context.Context, string, string) (domain.MemberRole, bool) {
	return identity.role, identity.role != ""
}

func (identity *repositoryAssetHTTPIdentity) IsOwner(context.Context, string, string) bool {
	return identity.role == domain.RoleOwner
}

type repositoryAssetHTTPFixture struct {
	server   *Server
	commands *repositoryAssetHTTPCommands
	backend  *repositoryAssetHTTPBackend
	identity *repositoryAssetHTTPIdentity
	hash     domain.ContentHash
}

func newRepositoryAssetHTTPFixture(test *testing.T) repositoryAssetHTTPFixture {
	test.Helper()
	backend := &repositoryAssetHTTPBackend{
		repositoryAssetHTTPCommands: &repositoryAssetHTTPCommands{},
		repository:                  domain.Repo{ID: domain.HashContent([]byte(test.Name())), RepositoryID: "asset-repository"},
	}
	identity := &repositoryAssetHTTPIdentity{repository: domain.Repository{ID: "asset-repository"}, role: domain.RoleMaintainer}
	commands := &repositoryAssetHTTPCommands{profileResult: domain.Repo{ID: backend.repository.ID, DefaultBranch: "owner-branch", Description: "explicit-owner"}}
	server := NewServer(&repositoryAssetHTTPPoisonedBackend{backend}, identity)
	if err := server.SetRepositoryAssetCommands(commands); err != nil {
		test.Fatal(err)
	}
	return repositoryAssetHTTPFixture{server: server, commands: commands, backend: backend, identity: identity, hash: domain.HashContent([]byte("settings object"))}
}

type repositoryAssetHTTPRequest struct {
	name     string
	command  string
	method   string
	path     string
	body     string
	response string
}

func repositoryAssetHTTPRequests(hash domain.ContentHash) []repositoryAssetHTTPRequest {
	return []repositoryAssetHTTPRequest{
		{name: "about", command: "profile", method: http.MethodPatch, path: "/about", body: `{"description":"retained","website":"https://about.example","topics":["context"]}`},
		{name: "config", command: "profile", method: http.MethodPatch, path: "/about", body: `{"default_branch":"next","protect_default":false}`},
		{name: "settings", command: "settings", method: http.MethodPut, path: "/settings/claude", body: `{"kind":"agents","files":[{"path":"AGENTS.md","content_b64":"cmV0YWluZWQ="}],"updated_by":"forged"}`, response: `{"kind":"claude","files":1}`},
		{name: "settings object", command: "settings object", method: http.MethodPut, path: "/settings-objects/" + string(hash), body: `{"kind":"codex","files":[{"path":"AGENTS.md","content_b64":"cmV0YWluZWQ="}],"updated_by":"object-author"}`, response: `{"status":"stored"}`},
		{name: "secrets", command: "secrets", method: http.MethodPut, path: "/secrets?expected_revision=previous&rotate=true&expect=fingerprint", body: `{"version":1,"ciphertext":"retained"}`, response: `{"status":"stored","revision":"explicit-revision"}`},
	}
}

func (fixture repositoryAssetHTTPFixture) request(input repositoryAssetHTTPRequest) *http.Request {
	request := httptest.NewRequest(input.method, "/api/v1/repos/"+string(fixture.backend.repository.ID)+input.path, strings.NewReader(input.body))
	request.Header.Set("Authorization", "Bearer asset-session")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(documentIdentitiesHeader, string(domain.DocumentIdentityRootV1))
	return request
}

func TestSetRepositoryAssetCommandsRejectsNilWithoutReplacingOwner(test *testing.T) {
	var typedNil *repositoryAssetHTTPCommands
	for _, rejected := range []struct {
		name     string
		commands inbound.RepositoryAssetCommands
	}{
		{name: "nil"},
		{name: "typed nil", commands: typedNil},
	} {
		test.Run(rejected.name, func(test *testing.T) {
			fixture := newRepositoryAssetHTTPFixture(test)
			for _, server := range []*Server{NewServer(nil, nil), NewServer(fixture.backend, nil), fixture.server} {
				previous := server.repositoryAssets
				if err := server.SetRepositoryAssetCommands(rejected.commands); !errors.Is(err, domain.ErrValidation) || server.repositoryAssets != previous {
					test.Fatal("nil commands were accepted or replaced the previous owner", err)
				}
			}
		})
	}
}

func TestRepositoryAssetHTTPUsesInjectedCommands(test *testing.T) {
	for _, input := range repositoryAssetHTTPRequests(domain.HashContent([]byte("settings object"))) {
		test.Run(input.name, func(test *testing.T) {
			fixture := newRepositoryAssetHTTPFixture(test)
			requestKey := struct{ name string }{"repository asset request"}
			parent, cancel := context.WithTimeout(context.WithValue(context.Background(), requestKey, "retained"), 30*time.Second)
			defer cancel()
			fixture.commands.observe = func(ctx context.Context, repo domain.ContentHash) {
				if repo != fixture.backend.repository.ID || ctx.Value(requestKey) != "retained" || ctx.Err() != nil || ctx.Done() != parent.Done() {
					test.Fatal("command lost its repository, values or cancellation scope", repo, ctx.Err())
				}
				deadline, present := ctx.Deadline()
				if original, _ := parent.Deadline(); !present || !deadline.Equal(original) {
					test.Fatal("command changed the request deadline", deadline, original)
				}
				if actor, system := inbound.RepositoryActor(ctx); actor != "asset-actor" || system {
					test.Fatal("command lost the authenticated actor", actor, system)
				}
				if identities := inbound.DocumentIdentities(ctx); len(identities) != 1 || identities[0] != domain.DocumentIdentityRootV1 {
					test.Fatal("command lost the document identity declaration", identities)
				}
			}
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(input).WithContext(parent))
			var actual, expected any
			if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil {
				test.Fatal(err)
			}
			expectedResponse := []byte(input.response)
			if input.command == "profile" {
				var err error
				expectedResponse, err = json.Marshal(fixture.commands.profileResult)
				if err != nil {
					test.Fatal(err)
				}
			}
			if err := json.Unmarshal(expectedResponse, &expected); err != nil {
				test.Fatal(err)
			}
			if response.Code != http.StatusOK || !reflect.DeepEqual(actual, expected) || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
				test.Fatal("command changed the response contract", response.Code, response.Header(), response.Body)
			}
			if !reflect.DeepEqual(fixture.commands.calls, []string{input.command}) || len(fixture.backend.calls) != 0 {
				test.Fatal("request did not execute exactly one injected command", fixture.commands.calls, fixture.backend.calls)
			}
			switch input.name {
			case "about", "config":
				var expectedPatch inbound.RepoProfilePatch
				if err := json.Unmarshal([]byte(input.body), &expectedPatch); err != nil {
					test.Fatal(err)
				}
				if !reflect.DeepEqual(fixture.commands.profile, expectedPatch) {
					test.Fatal("profile command lost the exact patch", fixture.commands.profile)
				}
			case "settings", "settings object":
				kind, author := "claude", "asset-editor"
				if input.name == "settings object" {
					kind, author = "codex", "object-author"
					if fixture.commands.hash != fixture.hash {
						test.Fatal("settings object lost its path hash", fixture.commands.hash)
					}
				}
				expectedBundle := domain.SettingsBundle{Kind: kind, UpdatedBy: author, Files: []domain.SettingsFile{{Path: "AGENTS.md", ContentB64: "cmV0YWluZWQ="}}}
				if !reflect.DeepEqual(fixture.commands.bundle, expectedBundle) {
					test.Fatal("settings command lost the decoded bundle or server overrides", fixture.commands.bundle)
				}
			case "secrets":
				expectedInput := inbound.SaveSecretsInput{RepoID: fixture.backend.repository.ID, ActorID: "asset-actor", Envelope: []byte(input.body), Edit: domain.SecretsEdit{ExpectedRevision: "previous", Rotate: true, ExpectedFingerprint: "fingerprint"}}
				if !reflect.DeepEqual(fixture.commands.secrets, expectedInput) {
					test.Fatal("secrets command lost its envelope, actor or edit preconditions", fixture.commands.secrets)
				}
			}
		})
	}
}

func TestRepositoryAssetHTTPRetainsLegacyConstructorCompatibility(test *testing.T) {
	for _, input := range repositoryAssetHTTPRequests(domain.HashContent([]byte("settings object"))) {
		test.Run(input.name, func(test *testing.T) {
			fixture := newRepositoryAssetHTTPFixture(test)
			server := NewServer(fixture.backend, fixture.identity)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, fixture.request(input))
			if response.Code != http.StatusOK || server.repositoryAssets != fixture.backend || !reflect.DeepEqual(fixture.backend.calls, []string{input.command}) || len(fixture.commands.calls) != 0 {
				test.Fatal("constructor lost its explicit legacy command binding", response.Code, fixture.backend.calls, fixture.commands.calls)
			}
		})
	}
}

func TestRepositoryAssetHTTPPreservesCommandErrors(test *testing.T) {
	for _, input := range repositoryAssetHTTPRequests(domain.HashContent([]byte("settings object"))) {
		for _, failure := range []struct {
			name   string
			err    error
			status int
			code   string
		}{
			{name: "validation", err: domain.ErrValidation, status: http.StatusUnprocessableEntity, code: "validation"},
			{name: "forbidden", err: domain.ErrForbidden, status: http.StatusForbidden, code: "forbidden"},
			{name: "secrets conflict", err: domain.ErrSecretsConflict, status: http.StatusConflict, code: "secrets_conflict"},
			{name: "internal", err: errors.New("private command storage detail"), status: http.StatusInternalServerError, code: "internal"},
		} {
			test.Run(input.name+"/"+failure.name, func(test *testing.T) {
				fixture := newRepositoryAssetHTTPFixture(test)
				fixture.commands.err = failure.err
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, fixture.request(input))
				if response.Code != failure.status || !strings.Contains(response.Body.String(), `"code":"`+failure.code+`"`) || strings.Contains(response.Body.String(), "private command storage detail") || response.Header().Get("Retry-After") != "" || response.Header().Get("Cache-Control") != "no-store" || !reflect.DeepEqual(fixture.commands.calls, []string{input.command}) {
					test.Fatal("command error lost status, sanitization or response headers", response.Code, response.Header(), response.Body, fixture.commands.calls)
				}
			})
		}
	}
}

func TestRepositoryAssetHTTPPropagatesCancellation(test *testing.T) {
	for _, input := range repositoryAssetHTTPRequests(domain.HashContent([]byte("settings object"))) {
		test.Run(input.name, func(test *testing.T) {
			fixture := newRepositoryAssetHTTPFixture(test)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			fixture.commands.err = context.Canceled
			fixture.commands.observe = func(ctx context.Context, repo domain.ContentHash) {
				cancel()
				if repo != fixture.backend.repository.ID || ctx.Done() != parent.Done() || !errors.Is(ctx.Err(), context.Canceled) {
					test.Fatal("command did not inherit live request cancellation", repo, ctx.Err())
				}
			}
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(input).WithContext(parent))
			if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"internal"`) || !reflect.DeepEqual(fixture.commands.calls, []string{input.command}) {
				test.Fatal("canceled command was retried, hidden or remapped", response.Code, response.Body, fixture.commands.calls)
			}
		})
	}
}

func TestRepositoryAssetHTTPRetainsTransportGates(test *testing.T) {
	for _, input := range repositoryAssetHTTPRequests(domain.HashContent([]byte("settings object"))) {
		for _, gate := range []struct {
			name   string
			status int
		}{
			{name: "authentication", status: http.StatusUnauthorized},
			{name: "csrf", status: http.StatusForbidden},
			{name: "content type", status: http.StatusUnsupportedMediaType},
		} {
			test.Run(input.name+"/"+gate.name, func(test *testing.T) {
				fixture := newRepositoryAssetHTTPFixture(test)
				request := fixture.request(input)
				switch gate.name {
				case "authentication":
					request.Header.Del("Authorization")
				case "csrf":
					request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "asset-session"})
				case "content type":
					request.Header.Set("Content-Type", "text/plain")
				}
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, request)
				if response.Code != gate.status || len(fixture.commands.calls) != 0 || len(fixture.backend.calls) != 0 {
					test.Fatal("transport gate reached an asset command", response.Code, response.Body, fixture.commands.calls)
				}
			})
		}
	}
}

func TestRepositoryAssetHTTPPreservesAtomicProfilePatch(test *testing.T) {
	fixture := newRepositoryAssetHTTPFixture(test)
	input := repositoryAssetHTTPRequest{method: http.MethodPatch, path: "/about", body: `{"description":"retained","default_branch":"next","protect_default":false,"required_doc_identity":"` + string(domain.DocumentIdentityRootV1) + `"}`}
	response := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(response, fixture.request(input))
	patch := fixture.commands.profile
	if response.Code != http.StatusOK || !reflect.DeepEqual(fixture.commands.calls, []string{"profile"}) || len(fixture.backend.calls) != 0 {
		test.Fatal("atomic profile patch was split into asset commands or used the facade", response.Code, response.Body, fixture.commands.calls)
	}
	if patch.Description == nil || *patch.Description != "retained" || patch.DefaultBranch == nil || *patch.DefaultBranch != "next" || patch.ProtectDefault == nil || *patch.ProtectDefault || patch.RequiredDocIdentity == nil || *patch.RequiredDocIdentity != domain.DocumentIdentityRootV1 || patch.Website != nil || patch.Topics != nil {
		test.Fatal("profile patch lost absent fields, false values or the document identity requirement", patch)
	}
}
