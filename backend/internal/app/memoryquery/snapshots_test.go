package memoryquery

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type snapshotOwnerResourceKey struct{}
type snapshotOwnerSnapshotKey struct{}

type snapshotOwnerScope struct {
	test                *testing.T
	repo                domain.ContentHash
	resources           int
	snapshots           int
	resourceDepth       int
	snapshotDepth       int
	resourceFailure     error
	resourceExitFailure error
	snapshotFailure     error
	snapshotExitFailure error
}

func (scope *snapshotOwnerScope) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	scope.resources++
	if domain.ValidateContentHash(repo) != nil || repo != scope.repo || read == nil {
		return domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope.resourceFailure != nil {
		return scope.resourceFailure
	}
	scope.resourceDepth++
	defer func() { scope.resourceDepth-- }()
	bound := inbound.WithRepositoryActor(context.WithValue(ctx, snapshotOwnerResourceKey{}, scope), "resource-authority")
	if err := read(bound); err != nil {
		return err
	}
	return scope.resourceExitFailure
}

func (scope *snapshotOwnerScope) WithinMemorySnapshot(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	scope.snapshots++
	actor, system := inbound.RepositoryActor(ctx)
	if repo != scope.repo || scope.resourceDepth == 0 || ctx.Value(snapshotOwnerResourceKey{}) != scope || actor != "resource-authority" || system {
		scope.test.Fatal("snapshot discarded the resource-bound authority")
	}
	if scope.snapshotFailure != nil {
		return scope.snapshotFailure
	}
	scope.snapshotDepth++
	defer func() { scope.snapshotDepth-- }()
	current := inbound.WithRepositoryActor(context.WithValue(ctx, snapshotOwnerSnapshotKey{}, scope), "snapshot-authority")
	if err := read(current); err != nil {
		return err
	}
	return scope.snapshotExitFailure
}

func (scope *snapshotOwnerScope) requireSnapshot(ctx context.Context, repo domain.ContentHash) {
	scope.test.Helper()
	actor, system := inbound.RepositoryActor(ctx)
	if repo != scope.repo || scope.resourceDepth == 0 || scope.snapshotDepth == 0 ||
		ctx.Value(snapshotOwnerResourceKey{}) != scope || ctx.Value(snapshotOwnerSnapshotKey{}) != scope || actor != "snapshot-authority" || system {
		scope.test.Fatal("read escaped the current snapshot-bound authority")
	}
}

type snapshotOwnerMetadata struct {
	scope         *snapshotOwnerScope
	snapshot      domain.Snapshot
	digest        domain.MemoryDigest
	snapshotReads int
	metaReads     int
	snapshotError error
	metaError     error
}

func (metadata *snapshotOwnerMetadata) GetSnapshot(ctx context.Context, repo, id domain.ContentHash) (domain.Snapshot, error) {
	metadata.scope.requireSnapshot(ctx, repo)
	metadata.snapshotReads++
	if id != metadata.snapshot.ID {
		return domain.Snapshot{}, domain.ErrNotFound
	}
	return metadata.snapshot, metadata.snapshotError
}

func (metadata *snapshotOwnerMetadata) GetMemoryMeta(ctx context.Context, repo, id domain.ContentHash) (domain.MemoryDigest, error) {
	metadata.scope.requireSnapshot(ctx, repo)
	metadata.metaReads++
	if id != metadata.snapshot.ID {
		return domain.MemoryDigest{}, domain.ErrNotFound
	}
	return metadata.digest, metadata.metaError
}

type snapshotOwnerObjects struct {
	scope    *snapshotOwnerScope
	memories map[domain.ContentHash]domain.MemoryDigest
	reads    []domain.ContentHash
	failure  error
	onRead   func()
}

func (objects *snapshotOwnerObjects) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	objects.scope.requireSnapshot(ctx, repo)
	objects.reads = append(objects.reads, hash)
	if objects.onRead != nil {
		objects.onRead()
	}
	digest, found := objects.memories[hash]
	if objects.failure != nil {
		return digest, objects.failure
	}
	if !found {
		return domain.MemoryDigest{}, domain.ErrNotFound
	}
	return digest, nil
}

type snapshotOwnerProjection struct {
	*snapshotOwnerObjects
	snapshots []domain.Snapshot
	lists     int
	failure   error
}

func (projection *snapshotOwnerProjection) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	projection.scope.requireSnapshot(ctx, repo)
	if branch != "" {
		projection.scope.test.Fatal("projection narrowed the metadata batch by branch")
	}
	projection.lists++
	return append([]domain.Snapshot(nil), projection.snapshots...), projection.failure
}

var _ SnapshotScope = (*snapshotOwnerScope)(nil)
var _ SnapshotMetadata = (*snapshotOwnerMetadata)(nil)
var _ outbound.MemoryObjectReader = (*snapshotOwnerObjects)(nil)
var _ outbound.MemoryProjectionReader = (*snapshotOwnerProjection)(nil)

type snapshotOwnerFixture struct {
	repo       domain.ContentHash
	id         domain.ContentHash
	hash       domain.ContentHash
	scope      *snapshotOwnerScope
	metadata   *snapshotOwnerMetadata
	objects    *snapshotOwnerObjects
	projection *snapshotOwnerProjection
	queries    *SnapshotQueries
}

func newSnapshotOwnerFixture(test *testing.T) *snapshotOwnerFixture {
	test.Helper()
	repo := domain.HashContent([]byte(test.Name()))
	id := domain.HashContent([]byte("selected snapshot"))
	digest := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: id, Summary: "SELECTED DECISION", KeyFacts: []string{"selected fact"}})
	hash, err := domain.MemoryDigestHash(digest)
	if err != nil {
		test.Fatal(err)
	}
	scope := &snapshotOwnerScope{test: test, repo: repo}
	snapshot := domain.Snapshot{RepoID: repo, ID: id, MemoryHash: hash}
	metadata := &snapshotOwnerMetadata{scope: scope, snapshot: snapshot, digest: domain.MemoryDigest{SnapshotID: id, Summary: "stale legacy metadata"}}
	objects := &snapshotOwnerObjects{scope: scope, memories: map[domain.ContentHash]domain.MemoryDigest{hash: digest}}
	projection := &snapshotOwnerProjection{
		snapshotOwnerObjects: &snapshotOwnerObjects{scope: scope, memories: map[domain.ContentHash]domain.MemoryDigest{hash: digest}},
		snapshots:            []domain.Snapshot{snapshot},
	}
	queries, err := NewSnapshots(SnapshotConfig{Scope: scope, Metadata: metadata, Objects: objects, Projection: projection})
	if err != nil {
		test.Fatal(err)
	}
	return &snapshotOwnerFixture{repo: repo, id: id, hash: hash, scope: scope, metadata: metadata, objects: objects, projection: projection, queries: queries}
}

func readSnapshotOwner(ctx context.Context, queries *SnapshotQueries, operation string, repo, id domain.ContentHash) (any, error) {
	switch operation {
	case "object":
		return queries.GetMemoryObject(ctx, repo, id)
	case "digest":
		return queries.GetMemoryDigest(ctx, repo, id)
	case "projection":
		return queries.GetMemoryProjection(ctx, repo, id)
	default:
		panic("unknown snapshot owner operation")
	}
}

func requireSnapshotOwnerZero(test *testing.T, operation string, result any, err, expected error) {
	test.Helper()
	var zero any = domain.MemoryDigest{}
	if operation == "projection" {
		zero = domain.MemoryProjection{}
	}
	if !errors.Is(err, expected) || !reflect.DeepEqual(result, zero) {
		test.Fatalf("%s returned data or wrong error: %+v, %v; want %v", operation, result, err, expected)
	}
}

func TestSnapshotOwnerConstructorRejectsMissingAndTypedNilDependencies(test *testing.T) {
	fixture := newSnapshotOwnerFixture(test)
	for _, sample := range []struct {
		name   string
		change func(*SnapshotConfig)
	}{
		{"missing-scope", func(config *SnapshotConfig) { config.Scope = nil }},
		{"nil-scope", func(config *SnapshotConfig) { config.Scope = (*snapshotOwnerScope)(nil) }},
		{"missing-metadata", func(config *SnapshotConfig) { config.Metadata = nil }},
		{"nil-metadata", func(config *SnapshotConfig) { config.Metadata = (*snapshotOwnerMetadata)(nil) }},
		{"missing-objects", func(config *SnapshotConfig) { config.Objects = nil }},
		{"nil-objects", func(config *SnapshotConfig) { config.Objects = (*snapshotOwnerObjects)(nil) }},
		{"missing-projection", func(config *SnapshotConfig) { config.Projection = nil }},
		{"nil-projection", func(config *SnapshotConfig) { config.Projection = (*snapshotOwnerProjection)(nil) }},
	} {
		test.Run(sample.name, func(test *testing.T) {
			config := SnapshotConfig{Scope: fixture.scope, Metadata: fixture.metadata, Objects: fixture.objects, Projection: fixture.projection}
			sample.change(&config)
			queries, err := NewSnapshots(config)
			if queries != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
				test.Fatal("invalid dependencies accepted", queries, err)
			}
		})
	}
	if fixture.scope.resources != 0 || fixture.scope.snapshots != 0 || fixture.metadata.snapshotReads != 0 ||
		fixture.metadata.metaReads != 0 || len(fixture.objects.reads) != 0 || fixture.projection.lists != 0 || len(fixture.projection.reads) != 0 {
		test.Fatal("constructor reached operational ports")
	}
	for _, queries := range []*SnapshotQueries{nil, {}} {
		for _, operation := range []string{"object", "digest", "projection"} {
			result, err := readSnapshotOwner(context.Background(), queries, operation, fixture.repo, fixture.id)
			requireSnapshotOwnerZero(test, operation, result, err, domain.ErrCatalogUnsupported)
		}
		if err := queries.WithMemoryReadResources(context.Background(), fixture.repo, func(context.Context) error {
			test.Fatal("nil query entered the callback")
			return nil
		}); !errors.Is(err, domain.ErrCatalogUnsupported) {
			test.Fatal(err)
		}
	}
}

func TestSnapshotOwnerDelegatesResourceScope(test *testing.T) {
	fixture := newSnapshotOwnerFixture(test)
	callbackFailure := errors.New("consumer failed")
	err := fixture.queries.WithMemoryReadResources(context.Background(), fixture.repo, func(bound context.Context) error {
		actor, system := inbound.RepositoryActor(bound)
		if actor != "resource-authority" || system || bound.Value(snapshotOwnerResourceKey{}) != fixture.scope || bound.Value(snapshotOwnerSnapshotKey{}) != nil {
			test.Fatal("resource delegation changed the bound context")
		}
		return callbackFailure
	})
	if err != callbackFailure || fixture.scope.resources != 1 || fixture.scope.snapshots != 0 {
		test.Fatal("resource delegation changed callback semantics", err)
	}
}

func TestSnapshotOwnerUsesBoundSnapshotAuthority(test *testing.T) {
	for _, operation := range []string{"object", "digest", "projection"} {
		test.Run(operation, func(test *testing.T) {
			fixture := newSnapshotOwnerFixture(test)
			selected := fixture.id
			if operation == "object" {
				selected = fixture.hash
			}
			ctx := inbound.WithRepositoryActor(context.Background(), "caller-authority")
			result, err := readSnapshotOwner(ctx, fixture.queries, operation, fixture.repo, selected)
			if err != nil || fixture.scope.resources != 1 || fixture.scope.snapshots != 1 || fixture.scope.resourceDepth != 0 || fixture.scope.snapshotDepth != 0 {
				test.Fatal("read bypassed or retained a scope", result, err)
			}
			actor, _ := inbound.RepositoryActor(ctx)
			if actor != "caller-authority" {
				test.Fatal("read mutated caller authority")
			}
			if operation == "projection" {
				projection := result.(domain.MemoryProjection)
				if !projection.Found || projection.Digest.SnapshotID != fixture.id || domain.ValidateContentHash(projection.StateHash) != nil {
					test.Fatal("projection was not derived", projection)
				}
			} else if !reflect.DeepEqual(result, fixture.objects.memories[fixture.hash]) {
				test.Fatal("object read was replaced by metadata or projection", result)
			}
		})
	}
}

func TestSnapshotOwnerValidationAndAuthorizationPrecedence(test *testing.T) {
	for _, operation := range []string{"object", "digest", "projection"} {
		for _, sample := range []struct {
			name            string
			invalidRepo     bool
			canceled        bool
			resourceFailure error
			snapshotFailure error
			want            error
			snapshots       int
		}{
			{name: "repo-before-authorization", invalidRepo: true, resourceFailure: domain.ErrForbidden, snapshotFailure: domain.ErrUnauthorized, want: domain.ErrValidation},
			{name: "resources-before-id", resourceFailure: domain.ErrForbidden, want: domain.ErrForbidden},
			{name: "snapshot-before-id", snapshotFailure: domain.ErrUnauthorized, want: domain.ErrUnauthorized, snapshots: 1},
			{name: "id-after-snapshot", want: domain.ErrIntegrity, snapshots: 1},
			{name: "cancellation-before-id", canceled: true, want: context.Canceled},
		} {
			test.Run(operation+"/"+sample.name, func(test *testing.T) {
				fixture := newSnapshotOwnerFixture(test)
				fixture.scope.resourceFailure = sample.resourceFailure
				fixture.scope.snapshotFailure = sample.snapshotFailure
				repo := fixture.repo
				if sample.invalidRepo {
					repo = "invalid-repository"
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if sample.canceled {
					cancel()
				}
				result, err := readSnapshotOwner(ctx, fixture.queries, operation, repo, "invalid-id")
				requireSnapshotOwnerZero(test, operation, result, err, sample.want)
				if fixture.scope.resources != 1 || fixture.scope.snapshots != sample.snapshots || fixture.metadata.snapshotReads != 0 ||
					fixture.metadata.metaReads != 0 || len(fixture.objects.reads) != 0 || fixture.projection.lists != 0 || len(fixture.projection.reads) != 0 {
					test.Fatal("validation or authority failure reached a read port")
				}
			})
		}
	}
}

func TestSnapshotOwnerDigestUsesOnlyTheCurrentPointer(test *testing.T) {
	fixture := newSnapshotOwnerFixture(test)
	fixture.metadata.snapshot.MemoryHash = ""
	legacy, err := fixture.queries.GetMemoryDigest(context.Background(), fixture.repo, fixture.id)
	if err != nil || !reflect.DeepEqual(legacy, fixture.metadata.digest) || fixture.metadata.metaReads != 1 || len(fixture.objects.reads) != 0 {
		test.Fatal("pointerless snapshot did not use only legacy metadata", legacy, err)
	}
	for _, summary := range []string{"first pointer", "replacement pointer"} {
		digest := domain.MemoryDigest{SnapshotID: fixture.id, Summary: summary}
		hash, err := domain.MemoryDigestHash(digest)
		if err != nil {
			test.Fatal(err)
		}
		fixture.metadata.snapshot.MemoryHash = hash
		fixture.objects.memories[hash] = digest
		actual, err := fixture.queries.GetMemoryDigest(context.Background(), fixture.repo, fixture.id)
		if err != nil || !reflect.DeepEqual(actual, digest) || fixture.objects.reads[len(fixture.objects.reads)-1] != hash {
			test.Fatal("snapshot pointer was not resolved afresh", actual, err)
		}
	}
	if fixture.metadata.snapshotReads != 3 || fixture.metadata.metaReads != 1 || len(fixture.objects.reads) != 2 || fixture.projection.lists != 0 || len(fixture.projection.reads) != 0 {
		test.Fatal("digest used fallback, projection, or retained pointer state")
	}
}

func TestSnapshotOwnerDigestNeverFallsBackAfterPointerFailure(test *testing.T) {
	for _, sample := range []struct {
		name string
		want error
	}{
		{"missing-snapshot", domain.ErrNotFound},
		{"missing-object", domain.ErrNotFound},
		{"corrupt-object", domain.ErrIntegrity},
		{"different-snapshot", domain.ErrIntegrity},
	} {
		test.Run(sample.name, func(test *testing.T) {
			fixture := newSnapshotOwnerFixture(test)
			switch sample.name {
			case "missing-snapshot":
				fixture.metadata.snapshotError = domain.ErrNotFound
			case "missing-object":
				delete(fixture.objects.memories, fixture.hash)
			case "corrupt-object":
				fixture.objects.failure = domain.ErrIntegrity
			case "different-snapshot":
				fixture.objects.memories[fixture.hash] = domain.MemoryDigest{SnapshotID: domain.HashContent([]byte("other snapshot")), Summary: "foreign digest"}
			}
			result, err := fixture.queries.GetMemoryDigest(context.Background(), fixture.repo, fixture.id)
			requireSnapshotOwnerZero(test, "digest", result, err, sample.want)
			reads := 1
			if sample.name == "missing-snapshot" {
				reads = 0
			}
			if fixture.metadata.snapshotReads != 1 || fixture.metadata.metaReads != 0 || len(fixture.objects.reads) != reads || fixture.projection.lists != 0 {
				test.Fatal("pointer failure fell back or read beyond failed metadata")
			}
		})
	}
}

func TestSnapshotOwnerDiscardsDataAfterAnyReadOrScopeError(test *testing.T) {
	for _, operation := range []string{"object", "digest", "projection"} {
		for _, stage := range []string{"resources", "snapshot", "snapshot-exit", "resource-exit", "reader", "legacy-metadata"} {
			if stage == "legacy-metadata" && operation != "digest" {
				continue
			}
			test.Run(operation+"/"+stage, func(test *testing.T) {
				fixture := newSnapshotOwnerFixture(test)
				failure := errors.New("read or scope failure")
				selected := fixture.id
				if operation == "object" {
					selected = fixture.hash
				}
				switch stage {
				case "resources":
					fixture.scope.resourceFailure = failure
				case "snapshot":
					fixture.scope.snapshotFailure = failure
				case "snapshot-exit":
					fixture.scope.snapshotExitFailure = failure
				case "resource-exit":
					fixture.scope.resourceExitFailure = failure
				case "reader":
					fixture.objects.failure = failure
					fixture.projection.snapshotOwnerObjects.failure = failure
				case "legacy-metadata":
					fixture.metadata.snapshot.MemoryHash = ""
					fixture.metadata.metaError = failure
				}
				result, err := readSnapshotOwner(context.Background(), fixture.queries, operation, fixture.repo, selected)
				requireSnapshotOwnerZero(test, operation, result, err, failure)
				if stage == "snapshot-exit" || stage == "resource-exit" {
					if operation == "projection" && fixture.projection.lists != 2 || operation != "projection" && len(fixture.objects.reads) != 1 {
						test.Fatal("exit failure fixture did not first produce data")
					}
				}
			})
		}
	}
}

func TestSnapshotOwnerActualProjectionCompositionAndReadCosts(test *testing.T) {
	fixture := newSnapshotOwnerFixture(test)
	parent := domain.HashContent([]byte("projection parent"))
	digest := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: parent, Summary: "PARENT DECISION", KeyFacts: []string{"parent fact"}})
	hash, err := domain.MemoryDigestHash(digest)
	if err != nil {
		test.Fatal(err)
	}
	fixture.projection.memories[hash] = digest
	fixture.projection.snapshots = append(fixture.projection.snapshots, domain.Snapshot{RepoID: fixture.repo, ID: parent, MemoryHash: hash})
	fixture.projection.snapshots[0].Parents = []domain.ContentHash{parent}
	fixture.projection.snapshots[0].GraftParents = []domain.ContentHash{parent}
	var previous domain.ContentHash
	for attempt := 1; attempt <= 2; attempt++ {
		projection, err := fixture.queries.GetMemoryProjection(context.Background(), fixture.repo, fixture.id)
		if err != nil || !projection.Found || projection.Digest.SnapshotID != fixture.id || domain.ValidateContentHash(projection.StateHash) != nil {
			test.Fatal("actual projection failed", projection, err)
		}
		for _, summary := range []string{"SELECTED DECISION", "PARENT DECISION"} {
			if strings.Count(projection.Digest.Summary, summary) != 1 {
				test.Fatal("projection omitted or duplicated reachable contributions", projection.Digest)
			}
		}
		if len(projection.Digest.Fragments) != 2 || len(projection.Digest.KeyFacts) != 2 || projection.Digest.PreviousMemoryHash != "" || projection.Digest.GraftCoverage != nil {
			test.Fatal("projection was returned as a stored attachment", projection.Digest)
		}
		if previous != "" && previous != projection.StateHash {
			test.Fatal("unchanged dependencies produced a different state")
		}
		previous = projection.StateHash
		if fixture.projection.lists != 2*attempt || len(fixture.projection.reads) != 2*attempt || fixture.scope.resources != attempt || fixture.scope.snapshots != attempt {
			test.Fatal("projection lost batch reads or request-local object reuse", fixture.projection.lists, fixture.projection.reads)
		}
		counts := map[domain.ContentHash]int{}
		for _, loaded := range fixture.projection.reads {
			counts[loaded]++
		}
		if counts[fixture.hash] != attempt || counts[hash] != attempt || fixture.metadata.snapshotReads != 0 || fixture.metadata.metaReads != 0 || len(fixture.objects.reads) != 0 {
			test.Fatal("projection used another capability or retained a cross-request cache", counts)
		}
	}
}

func TestSnapshotOwnerActualProjectionRejectsBrokenSource(test *testing.T) {
	for _, sample := range []struct {
		name string
		want error
	}{
		{"metadata-error", domain.ErrForbidden},
		{"missing-parent", domain.ErrIntegrity},
		{"missing-object", domain.ErrNotFound},
		{"corrupt-object", domain.ErrIntegrity},
	} {
		test.Run(sample.name, func(test *testing.T) {
			fixture := newSnapshotOwnerFixture(test)
			switch sample.name {
			case "metadata-error":
				fixture.projection.failure = domain.ErrForbidden
			case "missing-parent":
				fixture.projection.snapshots[0].Parents = []domain.ContentHash{domain.HashContent([]byte("absent parent"))}
			case "missing-object":
				delete(fixture.projection.memories, fixture.hash)
			case "corrupt-object":
				fixture.projection.memories[fixture.hash] = domain.MemoryDigest{SnapshotID: fixture.id, Summary: "tampered bytes"}
			}
			projection, err := fixture.queries.GetMemoryProjection(context.Background(), fixture.repo, fixture.id)
			requireSnapshotOwnerZero(test, "projection", projection, err, sample.want)
			if fixture.projection.lists != 1 || fixture.metadata.snapshotReads != 0 || fixture.metadata.metaReads != 0 || len(fixture.objects.reads) != 0 {
				test.Fatal("broken projection fell back or read another state")
			}
		})
	}
}
