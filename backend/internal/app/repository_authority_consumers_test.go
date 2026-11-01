package app

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave59OrganizationReader interface {
	outbound.RepositoryOrganizationAccess
}

func TestWave59PublicCommandFactoriesRequireCompleteAuthorityBeforeExecution(test *testing.T) {
	storage := &wave42WorkStore{get: func(context.Context, domain.ContentHash) (domain.Repo, error) {
		test.Fatal("command construction read repository metadata")
		return domain.Repo{}, domain.ErrConflict
	}}
	withoutTeams := struct {
		outbound.RepositoryStore
		outbound.OrganizationStore
		wave59OrganizationReader
		outbound.RepositoryAccessLocker
	}{storage, storage, storage, storage}
	withoutOrganizations := struct {
		outbound.RepositoryStore
		outbound.OrganizationStore
		outbound.TeamStore
		outbound.RepositoryAccessLocker
	}{storage, storage, storage, storage}
	withoutAudit := struct {
		outbound.RepositoryStore
		outbound.TeamStore
		wave59OrganizationReader
		outbound.RepositoryAccessLocker
	}{storage, storage, storage, storage}
	withoutLocker := struct {
		outbound.RepositoryStore
		outbound.OrganizationStore
		outbound.TeamStore
		wave59OrganizationReader
	}{storage, storage, storage, storage}
	for _, fixture := range []struct {
		name                               string
		repositories                       outbound.RepositoryStore
		teams, organization, audit, locker bool
	}{
		{"without teams", withoutTeams, false, true, true, true},
		{"without organizations", withoutOrganizations, true, false, true, true},
		{"without audit", withoutAudit, true, true, false, true},
		{"without locker", withoutLocker, true, true, true, false},
	} {
		_, teams := fixture.repositories.(outbound.TeamStore)
		_, organization := fixture.repositories.(outbound.RepositoryOrganizationAccess)
		_, audit := fixture.repositories.(outbound.OrganizationStore)
		_, locker := fixture.repositories.(outbound.RepositoryAccessLocker)
		if teams != fixture.teams || organization != fixture.organization || audit != fixture.audit || locker != fixture.locker {
			test.Fatal("authority fixture hides a capability other than its target", fixture.name, teams, organization, audit, locker)
		}
	}
	nontransactional := struct {
		outbound.MetadataStore
		outbound.SecretsCASStore
		RepositoryNotificationQueue
	}{storage, storage, storage}
	for _, scenario := range []struct {
		name         string
		metadata     outbound.MetadataStore
		repositories outbound.RepositoryStore
		want         error
	}{
		{name: "complete transactional", metadata: storage, repositories: storage},
		{name: "complete nontransactional without locker", metadata: nontransactional, repositories: withoutLocker},
		{name: "missing teams", metadata: storage, repositories: withoutTeams, want: domain.ErrCatalogUnsupported},
		{name: "missing organizations", metadata: storage, repositories: withoutOrganizations, want: domain.ErrCatalogUnsupported},
		{name: "missing audit", metadata: storage, repositories: withoutAudit, want: domain.ErrCatalogUnsupported},
		{name: "transactional missing locker", metadata: storage, repositories: withoutLocker, want: domain.ErrCatalogUnsupported},
		{name: "missing repositories", metadata: storage, want: domain.ErrCatalogUnsupported},
		{name: "typed nil repositories", metadata: storage, repositories: (*wave42WorkStore)(nil), want: domain.ErrValidation},
		{name: "typed nil metadata", metadata: (*wave42WorkStore)(nil), repositories: storage, want: domain.ErrValidation},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			service := NewService(scenario.metadata, storage, nil, nil, scenario.repositories)
			assets, err := service.RepositoryAssetCommands()
			if !errors.Is(err, scenario.want) || rootAdapterPresent(assets) != (scenario.want == nil) {
				work := service.repositoryWork()
				_, secrets := scenario.metadata.(outbound.SecretsCASStore)
				_, notifications := scenario.metadata.(RepositoryNotificationQueue)
				test.Fatalf("public asset factory: owner=%T error=%v want=%v authority=%+v authorityError=%v completeAuthority=%v secrets=%v notifications=%v", assets, err, scenario.want, work.authority, work.configurationError, work.validateCompleteAuthority(), secrets, notifications)
			}
			publications, err := service.DocumentPublicationCommands()
			if !errors.Is(err, scenario.want) || rootAdapterPresent(publications) != (scenario.want == nil) {
				test.Fatalf("public publication factory: owner=%T error=%v want=%v", publications, err, scenario.want)
			}
			if errors.Is(scenario.want, domain.ErrValidation) {
				repo := domain.HashContent([]byte(test.Name()))
				if err := service.UpdateAbout(context.Background(), repo, "invalid dependency", "", nil); !errors.Is(err, domain.ErrValidation) {
					test.Fatal("compatibility asset entry weakened a typed-nil authority error", err)
				}
				if output, err := service.Commit(context.Background(), inbound.CommitInput{RepoID: repo}); !errors.Is(err, domain.ErrValidation) || output != (inbound.CommitOutput{}) {
					test.Fatal("compatibility publication entry weakened a typed-nil authority error", output, err)
				}
			}
			if len(storage.calls) != 0 || len(storage.audits) != 0 {
				test.Fatal("factory validation executed repository work", storage.calls, storage.audits)
			}
		})
	}
}

func TestWave59CompatibilityCommandsPreserveExplicitAbsentAuthorityCapabilities(test *testing.T) {
	storage := store.NewFSStore(test.TempDir())
	repository, secretsInput := seedSecretsRepo(test, storage, storage)
	repositories := struct{ outbound.RepositoryStore }{storage}
	service := NewService(storage, storage, nil, nil, repositories)
	if commands, err := service.RepositoryAssetCommands(); !errors.Is(err, domain.ErrCatalogUnsupported) || rootAdapterPresent(commands) {
		test.Fatal("Complete asset factory silently selected compatibility", err)
	}
	if commands, err := service.DocumentPublicationCommands(); !errors.Is(err, domain.ErrCatalogUnsupported) || rootAdapterPresent(commands) {
		test.Fatal("Complete publication factory silently selected compatibility", err)
	}
	ctx := inbound.WithRepositoryActor(context.Background(), repository.OwnerID)
	if err := service.UpdateAbout(ctx, secretsInput.RepoID, " explicit compatibility ", "example.test", nil); err != nil {
		test.Fatal("legacy asset entry rejected genuinely absent optional authority ports", err)
	}
	document := wave58PublicationDocument(test, test.Name())
	snapshot := wave58PublicationSnapshot(secretsInput.RepoID, document)
	output, err := service.Commit(ctx, inbound.CommitInput{RepoID: secretsInput.RepoID, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{snapshot}})
	if err != nil || output != (inbound.CommitOutput{StoredDocs: 1, StoredSnapshots: 1}) {
		test.Fatal("legacy publication entry lost its original scope", output, err)
	}
	if output, err := service.SaveSecrets(ctx, secretsInput); err != nil || output.Status != "stored" || output.Revision == "" {
		test.Fatal("legacy secrets entry required absent transactional authority", output, err)
	}
	current, err := storage.GetRepo(ctx, secretsInput.RepoID)
	if err != nil || current.Description != "explicit compatibility" || current.Website != "https://example.test" {
		test.Fatal("compatibility asset entry lost its original store", current, err)
	}
	stored, err := storage.GetDoc(ctx, secretsInput.RepoID, document.Hash)
	if err != nil || !reflect.DeepEqual(stored, document) {
		test.Fatal("compatibility publication entry lost its original store", stored, err)
	}
	if err := service.UpdateAbout(context.Background(), secretsInput.RepoID, "anonymous", "", nil); !errors.Is(err, domain.ErrUnauthorized) {
		test.Fatal("compatibility became an anonymous authority bypass", err)
	}
	if err := service.UpdateAbout(systemTestContext(), secretsInput.RepoID, "trusted system", "", nil); err != nil {
		test.Fatal("compatibility removed the original explicit system scope", err)
	}
}

func TestWave59PublicCommandOwnersUseCurrentAuthorityAndOriginalStores(test *testing.T) {
	storage := store.NewFSStore(test.TempDir())
	fixture := makeTeamFixture(test, storage)
	repo := domain.HashContent([]byte(test.Name()))
	if _, err := storage.PutRepo(systemTestContext(), domain.Repo{ID: repo, RepositoryID: fixture.repository.ID, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	service := NewService(storage, storage, nil, nil, storage)
	assetPort, err := service.RepositoryAssetCommands()
	if err != nil {
		test.Fatal(err)
	}
	assets, actual := assetPort.(*RepositoryAssetCommands)
	if !actual || assets == nil {
		test.Fatalf("public asset factory returned a facade: %T", assetPort)
	}
	publicationPort, err := service.DocumentPublicationCommands()
	if err != nil {
		test.Fatal(err)
	}
	publications, actual := publicationPort.(*DocumentPublicationCommands)
	if !actual || publications == nil {
		test.Fatalf("public publication factory returned a facade: %T", publicationPort)
	}
	service.meta, service.blobs, service.repositories = nil, nil, nil
	secretsRevision := "absent"
	description := ""
	exercise := func(stage string, want error) {
		test.Helper()
		ctx := inbound.WithCorrelation(inbound.WithRepositoryActor(context.Background(), fixture.member.ID), stage)
		document := wave58PublicationDocument(test, stage)
		snapshot := wave58PublicationSnapshot(repo, document)
		chunkBody := []byte(stage + " staged chunk")
		chunk := inbound.ChunkObject{Hash: domain.HashContent(chunkBody), Data: chunkBody}
		previousSecrets, readErr := storage.GetSecretsEnvelope(systemTestContext(), repo)
		if readErr != nil && !errors.Is(readErr, domain.ErrNotFound) {
			test.Fatal(readErr)
		}
		operations := []struct {
			name string
			run  func() error
		}{
			{name: "about", run: func() error { return assets.UpdateAbout(ctx, repo, stage, "", nil) }},
			{name: "secrets", run: func() error {
				output, err := assets.SaveSecrets(ctx, inbound.SaveSecretsInput{RepoID: repo, ActorID: fixture.member.ID,
					Envelope: secretsTestEnvelope("aaaaaaaaaaaa"), Edit: domain.SecretsEdit{ExpectedRevision: secretsRevision}})
				if err == nil {
					if output.Status != "stored" || output.Revision == "" {
						test.Fatal("public secrets command lost its result", output)
					}
					secretsRevision = output.Revision
				} else if output != (inbound.SaveSecretsOutput{}) {
					test.Fatal("denied secrets command exposed a result", output)
				}
				return err
			}},
			{name: "commit", run: func() error {
				output, err := publications.Commit(ctx, inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{snapshot}})
				if err == nil && output != (inbound.CommitOutput{StoredDocs: 1, StoredSnapshots: 1}) || err != nil && output != (inbound.CommitOutput{}) {
					test.Fatal("public commit command lost result isolation", output, err)
				}
				return err
			}},
			{name: "chunks", run: func() error {
				output, err := publications.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: []inbound.ChunkObject{chunk}})
				if err == nil && output != (inbound.StoreChunksOutput{Stored: 1}) || err != nil && output != (inbound.StoreChunksOutput{}) {
					test.Fatal("public chunk command lost result isolation", output, err)
				}
				return err
			}},
		}
		for _, operation := range operations {
			if err := operation.run(); !errors.Is(err, want) {
				test.Fatalf("%s/%s: error=%v want=%v", stage, operation.name, err, want)
			}
		}
		if want == nil {
			description = stage
		}
		current, err := storage.GetRepo(systemTestContext(), repo)
		if err != nil || current.Description != description {
			test.Fatal("actual owner lost its original metadata or a denied write changed it", current, err)
		}
		storedDocument, documentErr := storage.GetDoc(systemTestContext(), repo, document.Hash)
		storedSnapshot, snapshotErr := storage.GetSnapshot(systemTestContext(), repo, snapshot.ID)
		storedChunk, chunkErr := storage.GetChunk(systemTestContext(), repo, chunk.Hash)
		currentSecrets, secretsErr := storage.GetSecretsEnvelope(systemTestContext(), repo)
		if want == nil {
			if documentErr != nil || !reflect.DeepEqual(storedDocument, document) || snapshotErr != nil || storedSnapshot.DocHash != document.Hash || storedSnapshot.RepoID != repo || chunkErr != nil || !bytes.Equal(storedChunk, chunk.Data) || secretsErr != nil || domain.SecretsRevision(currentSecrets) != secretsRevision {
				test.Fatal("actual public owners did not mutate their original stores", documentErr, snapshotErr, chunkErr, secretsErr)
			}
		} else if !errors.Is(documentErr, domain.ErrNotFound) || !errors.Is(snapshotErr, domain.ErrNotFound) || !errors.Is(chunkErr, domain.ErrNotFound) || secretsErr != nil || !bytes.Equal(previousSecrets, currentSecrets) {
			test.Fatal("revoked authority reached an original-store mutation", documentErr, snapshotErr, chunkErr, secretsErr)
		}
		audits, err := storage.ListOrganizationAudit(systemTestContext(), fixture.organization.ID, 100)
		if err != nil {
			test.Fatal(err)
		}
		actions := make(map[string]int)
		for _, event := range audits {
			if event.CorrelationID != stage {
				continue
			}
			if event.ActorID != fixture.member.ID || event.OrganizationID != fixture.organization.ID || event.TargetType != "repository" || event.TargetID != fixture.repository.ID {
				test.Fatal("public owner changed original authority audit metadata", event)
			}
			actions[event.Action]++
		}
		expected := make(map[string]int)
		if want == nil {
			for _, action := range []string{"repository.about.updated", "repository.secrets.updated", "context.objects.stored", "context.chunks.stored"} {
				expected[action] = 1
			}
		}
		if !reflect.DeepEqual(actions, expected) {
			test.Fatal("public owners lost original audit actions or audited rejected work", stage, actions)
		}
	}
	if err := fixture.identity.UpdateTeamMember(systemTestContext(), fixture.owner.ID, fixture.organization.ID, fixture.team.ID, fixture.member.ID, domain.TeamMember); err != nil {
		test.Fatal(err)
	}
	if err := fixture.identity.SetTeamRepository(systemTestContext(), fixture.owner.ID, fixture.organization.ID, fixture.team.ID, fixture.repository.ID, domain.RoleMaintainer); err != nil {
		test.Fatal(err)
	}
	exercise("team-granted", nil)
	if err := fixture.identity.RemoveTeamMember(systemTestContext(), fixture.owner.ID, fixture.organization.ID, fixture.team.ID, fixture.member.ID); err != nil {
		test.Fatal(err)
	}
	exercise("team-revoked", domain.ErrForbidden)
	if err := fixture.identity.UpdateOrganizationMember(systemTestContext(), fixture.owner.ID, fixture.organization.ID, fixture.member.ID, domain.OrganizationOwner); err != nil {
		test.Fatal(err)
	}
	exercise("organization-promoted", nil)
	if err := fixture.identity.UpdateOrganizationMember(systemTestContext(), fixture.owner.ID, fixture.organization.ID, fixture.member.ID, domain.OrganizationMember); err != nil {
		test.Fatal(err)
	}
	exercise("organization-demoted", domain.ErrForbidden)
}
