package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave44BaseMetadata struct{ outbound.MetadataStore }
type wave44BaseBlobs struct{ outbound.BlobStore }
type wave44BaseRepositories struct{ outbound.RepositoryStore }

func wave44PreparationResources(test *testing.T, metadata outbound.MetadataStore, blobs outbound.BlobStore) *MemoryPreparationResources {
	test.Helper()
	resources, err := NewMemoryPreparationResources(MemoryPreparationResourceConfig{Metadata: metadata, Blobs: blobs})
	if err != nil || resources == nil {
		test.Fatal("direct resource construction failed", err)
	}
	return resources
}

func TestWave44MemoryPreparationResourcesValidateBaseAndTypedNil(test *testing.T) {
	metadata, blobs := &wave44BaseMetadata{}, &wave44BaseBlobs{}
	var absentMetadata *wave44BaseMetadata
	var absentBlobs *wave44BaseBlobs
	var absentRepositories *wave44BaseRepositories
	for _, invalid := range []MemoryPreparationResourceConfig{
		{}, {Metadata: metadata}, {Blobs: blobs},
		{Metadata: absentMetadata, Blobs: blobs},
		{Metadata: metadata, Blobs: absentBlobs},
		{Metadata: metadata, Blobs: blobs, Repositories: absentRepositories},
	} {
		resources, err := NewMemoryPreparationResources(invalid)
		if !errors.Is(err, domain.ErrValidation) || resources != nil {
			test.Fatal("public resources accepted missing or typed-nil declared dependencies", err)
		}
	}
	for _, repositories := range []outbound.RepositoryStore{nil, &wave44BaseRepositories{}} {
		resources, err := NewMemoryPreparationResources(MemoryPreparationResourceConfig{Metadata: metadata, Blobs: blobs, Repositories: repositories})
		if err != nil || resources == nil || resources.meta != metadata || resources.blobs != blobs || resources.work == nil ||
			resources.work.meta != metadata || resources.work.repositories != repositories || resources.scope == nil ||
			resources.scope.resources == nil || resources.scope.resources.ownerKey == nil || resources.delivery == nil {
			test.Fatal("base-only resource construction required active worker ports or lost actual dependencies", err)
		}
		wave44PreparationWorkers(test, MemoryPreparationWorkerConfig{Profile: MemoryPreparationComplete, Resources: resources})
	}
}

func TestWave44MemoryPreparationResourcesOwnIndependentLifetimes(test *testing.T) {
	metadata, blobs := &wave44BaseMetadata{}, &wave44BaseBlobs{}
	first := wave44PreparationResources(test, metadata, blobs)
	second := wave44PreparationResources(test, metadata, blobs)
	if first.work == second.work || first.scope.resources.ownerKey == second.scope.resources.ownerKey || first.delivery == second.delivery {
		test.Fatal("independent resources shared repository work, read authority or delivery state")
	}
	workers := wave44PreparationWorkers(test, MemoryPreparationWorkerConfig{Profile: MemoryPreparationComplete, Resources: first})
	identities := workers.DocumentIdentitiesSupported()
	identities[0] = domain.DocumentIdentityRootV1
	ctx := workers.DocumentIdentityWorkerContext(inbound.WithRepositoryActor(context.Background(), "reader"))
	actor, system := inbound.RepositoryActor(ctx)
	if actor != "reader" || system || !reflect.DeepEqual(inbound.DocumentIdentities(ctx), []domain.DocumentIdentity{domain.DocumentIdentityLegacy}) ||
		!reflect.DeepEqual(workers.DocumentIdentitiesSupported(), first.work.supported) {
		test.Fatal("identity declaration mutated resource support or elevated the current actor")
	}
}

func TestWave44MemoryPreparationResourceScopeSharesOnlyCurrentAuthority(test *testing.T) {
	for _, scenario := range []string{"shared", "foreign-owner", "actor", "binding", "repository", "completed"} {
		test.Run(scenario, func(test *testing.T) {
			resources := wave44PreparationResources(test, &wave44BaseMetadata{}, &wave44BaseBlobs{})
			first := wave44PreparationWorkers(test, MemoryPreparationWorkerConfig{Profile: MemoryPreparationComplete, Resources: resources})
			if scenario == "foreign-owner" {
				resources = wave44PreparationResources(test, resources.meta, resources.blobs)
			}
			second := wave44PreparationWorkers(test, MemoryPreparationWorkerConfig{Profile: MemoryPreparationCompatibility, Resources: resources})
			repo := hh(test.Name())
			checks := 0
			ctx, gate := admissionTestContext(time.Second)
			ctx = inbound.WithRepositoryActor(ctx, "reader")
			ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
				checks++
				actor, system := inbound.RepositoryActor(bound)
				if requested != repo || actor != "reader" || system || checks > 1 {
					return bound, domain.ErrForbidden
				}
				return bound, nil
			})
			bound, release, err := AcquireResources(ctx, ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer release()
			var captured context.Context
			var original *preparedMemoryScope
			err = first.WithMemoryReadResources(bound, repo, func(current context.Context) error {
				captured = current
				original, _ = current.Value(preparedMemoryScopeKey{}).(*preparedMemoryScope)
				if original == nil || original.ownerKey != first.scope.resources.ownerKey || !original.active.Load() || original.permit == nil || !original.permit.live.Load() {
					test.Fatal("direct owner lost active resource identity or permit")
				}
				if scenario == "completed" {
					return nil
				}
				requested := repo
				switch scenario {
				case "actor":
					current = inbound.WithRepositoryActor(current, "replacement")
				case "binding":
					current = inbound.WithDocumentReadAuthorizer(current, func(changed context.Context, _ domain.ContentHash) (context.Context, error) {
						checks++
						return changed, domain.ErrForbidden
					})
				case "repository":
					requested = hh("foreign repository")
				}
				return second.WithMemoryReadResources(current, requested, func(nested context.Context) error {
					if scenario != "shared" || nested.Value(preparedMemoryScopeKey{}) != original || nested.Value(resourcePermitKey{}) != original.permit {
						test.Fatal("nested owner reused foreign authority or allocated duplicate scope/admission")
					}
					return nil
				})
			})
			if original == nil || original.active.Load() || !original.permit.live.Load() {
				test.Fatal("scope lifetime did not end before its borrowed permit")
			}
			if scenario == "completed" && err == nil {
				err = second.WithMemoryReadResources(captured, repo, func(context.Context) error {
					test.Fatal("completed scope retained authority under a live borrowed permit")
					return nil
				})
			}
			if scenario == "shared" {
				if err != nil || checks != 1 {
					test.Fatal("shared resource owner repeated authority checks", err, checks)
				}
			} else if !errors.Is(err, domain.ErrForbidden) || checks != 2 {
				test.Fatal("changed resource authority reused its old proof", err, checks)
			}
			release()
			if original.permit.live.Load() {
				test.Fatal("released permit remained live")
			}
			assertResourcesReleased(test, gate)
		})
	}
}

type wave44PreparationSnapshotKey struct{}

type wave44PreparationSnapshotStore struct {
	*wave44MemoryPreparationStore
	reads         int
	snapshotError error
}

func (*wave44PreparationSnapshotStore) InTransaction(ctx context.Context) bool {
	return ctx.Value(wave44PreparationSnapshotKey{}) != nil
}

func (storage *wave44PreparationSnapshotStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	storage.reads++
	if err := read(context.WithValue(ctx, wave44PreparationSnapshotKey{}, true)); err != nil {
		return err
	}
	return storage.snapshotError
}

func (storage *wave44PreparationSnapshotStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	if !storage.InTransaction(ctx) {
		storage.test.Fatal("repository identity was read outside the owned snapshot")
	}
	return domain.Repo{ID: repo}, nil
}

func TestWave44MemoryPreparationReadOwnsSnapshotAndZeroesFailedResult(test *testing.T) {
	storage := &wave44PreparationSnapshotStore{wave44MemoryPreparationStore: &wave44MemoryPreparationStore{test: test}}
	resources := wave44PreparationResources(test, storage, storage)
	workers := wave44PreparationWorkers(test, MemoryPreparationWorkerConfig{Profile: MemoryPreparationComplete, Resources: resources})
	for _, failure := range []error{nil, domain.ErrConflict} {
		storage.snapshotError = failure
		ctx, gate := admissionTestContext(time.Second)
		result, err := memoryPreparationReadForRepo(inbound.WithSystemActor(ctx), workers, hh(test.Name()), func(bound context.Context) (int, error) {
			if !storage.InTransaction(bound) || bound.Value(resourcePermitKey{}) == nil {
				test.Fatal("read callback lost its admitted snapshot")
			}
			return 7, nil
		})
		if failure == nil && (err != nil || result != 7) || failure != nil && (!errors.Is(err, failure) || result != 0) {
			test.Fatal("read helper leaked a partial result or replaced snapshot failure", result, err)
		}
		assertResourcesReleased(test, gate)
	}
	if storage.reads != 2 {
		test.Fatal("read helper changed its single-snapshot ownership", storage.reads)
	}
}

func TestWave44MemoryPreparationBorrowedServiceResourcesKeepScopeAndDelivery(test *testing.T) {
	storage := &wave44MemoryPreparationStore{test: test}
	service := &Service{meta: storage, blobs: storage}
	resources := service.MemoryPreparationResources()
	legacy := service.memoryPreparation()
	if resources.delivery != &service.deliveryCache || legacy.delivery != resources.delivery ||
		resources.scope.resources.ownerKey != service.memoryResourceScope().ownerKey ||
		legacy.scope.resources.ownerKey != resources.scope.resources.ownerKey {
		test.Fatal("legacy resource facade duplicated delivery state or read identity")
	}
	workers := wave44PreparationWorkers(test, MemoryPreparationWorkerConfig{Profile: MemoryPreparationComplete, Resources: resources, Repositories: []domain.ContentHash{hh(test.Name())}})
	live, err := workers.memoryLiveQueries(true, false)
	if err != nil || reflect.ValueOf(live).Elem().FieldByName("delivery").Pointer() != reflect.ValueOf(&service.deliveryCache).Pointer() {
		test.Fatal("worker live reference replaced the service's actual delivery cache", err)
	}
	ctx, gate := admissionTestContext(time.Second)
	checks := 0
	ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		checks++
		return bound, nil
	})
	err = service.WithMemoryReadResources(ctx, hh(test.Name()), func(bound context.Context) error {
		return workers.WithMemoryReadResources(bound, hh(test.Name()), func(current context.Context) error {
			if current.Value(preparedMemoryScopeKey{}) != bound.Value(preparedMemoryScopeKey{}) || current.Value(resourcePermitKey{}) != bound.Value(resourcePermitKey{}) {
				test.Fatal("borrowed owner did not share the legacy wrapper's actual active scope")
			}
			return nil
		})
	})
	if err != nil || checks != 1 {
		test.Fatal("borrowed scope duplicated authority checks", err, checks)
	}
	service.meta, service.blobs = nil, nil
	if err := workers.ProcessMemoryReadPreparations(ctx, 1); err != nil || len(storage.calls) != 3 {
		test.Fatal("direct owner rediscovered its former service dependencies", err)
	}
	assertResourcesReleased(test, gate)
	generation := &domain.RepositoryMemorySourceGeneration{}
	service.projectionMemory, service.projectionEvidence, service.projectionMemorySource = storage, storage, generation
	frozen := service.MemoryPreparationResources()
	if frozen.projectionMemory != storage || frozen.projectionEvidence != storage || frozen.projectionMemorySource != generation {
		test.Fatal("borrowed legacy resources lost the existing frozen source binding")
	}
}
