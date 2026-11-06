package repositoryquery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type bootstrapTestReader struct {
	*stateTestPorts
	ref       domain.Ref
	snapshots map[domain.ContentHash]domain.Snapshot
	archive   *domain.SessionArchive
	archiveAt domain.ContentHash
}

type bootstrapTestMetadata struct {
	*stateTestMetadata
	protocol int
}

func (metadata *bootstrapTestMetadata) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	value, err := metadata.stateTestMetadata.GetRepo(ctx, repo)
	value.ContextProtocol = metadata.protocol
	return value, err
}

func (reader *bootstrapTestReader) GetRef(ctx context.Context, repo domain.ContentHash, kind domain.RefKind, name string) (domain.Ref, error) {
	if err := reader.read(ctx, repo, "ref"); err != nil {
		return domain.Ref{}, err
	}
	if kind != domain.RefBranch || name != reader.ref.Name {
		return domain.Ref{}, domain.ErrNotFound
	}
	return reader.ref, nil
}

func (reader *bootstrapTestReader) GetSnapshot(ctx context.Context, repo, id domain.ContentHash) (domain.Snapshot, error) {
	if err := reader.read(ctx, repo, "snapshot"); err != nil {
		return domain.Snapshot{}, err
	}
	snapshot, exists := reader.snapshots[id]
	if !exists {
		return domain.Snapshot{}, domain.ErrNotFound
	}
	return snapshot, nil
}

func (reader *bootstrapTestReader) GetSessionArchive(ctx context.Context, repo, key domain.ContentHash) (domain.SessionArchive, error) {
	if err := reader.read(ctx, repo, "archive"); err != nil {
		return domain.SessionArchive{}, err
	}
	if reader.archive == nil || reader.archiveAt != "" && reader.archiveAt != key {
		return domain.SessionArchive{}, domain.ErrNotFound
	}
	return *reader.archive, nil
}

func bootstrapFixture(test *testing.T) (stateTestFixture, *bootstrapTestReader) {
	test.Helper()
	fixture := newStateTestFixture(test, false)
	parent := domain.HashContent([]byte("bootstrap parent"))
	head := domain.HashContent([]byte("bootstrap head"))
	reader := &bootstrapTestReader{stateTestPorts: fixture.ports,
		ref: domain.Ref{RepoID: fixture.repo, Kind: domain.RefBranch, Name: "main", Target: head},
		snapshots: map[domain.ContentHash]domain.Snapshot{
			head:   {ID: head, RepoID: fixture.repo, DocHash: head, Parents: []domain.ContentHash{parent}, Provider: domain.ProviderCodex, SessionID: "active"},
			parent: {ID: parent, RepoID: fixture.repo, DocHash: parent},
		}}
	fixture.queries.bootstrap = reader
	fixture.queries.metadata = &bootstrapTestMetadata{stateTestMetadata: fixture.metadata, protocol: 1}
	fixture.metadata.view.DefaultBranch = "main"
	return fixture, reader
}

func TestBootstrapUsesOnlyPointReadsInOneAuthorizedSnapshot(test *testing.T) {
	for _, count := range []int{0, 10000} {
		test.Run(fmt.Sprintf("history-%d", count), func(test *testing.T) {
			fixture, reader := bootstrapFixture(test)
			fixture.metadata.view.Snapshots = make([]domain.Snapshot, count)
			fixture.metadata.errors["snapshots"] = errors.New("full scan forbidden")
			fixture.metadata.errors["refs"] = errors.New("full ref scan forbidden")
			fixture.ports.errors["history"] = errors.New("full history scan forbidden")
			view, err := fixture.queries.GetRepositoryBootstrap(inbound.WithSystemActor(context.Background()), fixture.repo, "")
			if err != nil || view.Ref == nil || view.Snapshot == nil || view.Parent == nil || view.Archived ||
				view.Snapshot.ID != reader.ref.Target || view.Parent.ID != view.Snapshot.Parents[0] || view.Revision != fixture.ports.revision {
				test.Fatal("invalid bounded selection", view, err)
			}
			if !reflect.DeepEqual(fixture.metadata.calls, []string{"repo"}) ||
				!reflect.DeepEqual(fixture.ports.calls, []string{"revision", "ref", "snapshot", "archive", "archive", "snapshot"}) ||
				fixture.scope.pins != 1 || fixture.resources.acquired != 1 || fixture.resources.released != 1 {
				test.Fatal("bootstrap scanned history, split snapshots, or leaked admission", fixture.metadata.calls, fixture.ports.calls)
			}
		})
	}
}

func TestBootstrapSelectionAndArchives(test *testing.T) {
	for _, name := range []string{"default", "explicit", "unset-default", "absent-branch", "archived", "legacy-archive", "root"} {
		test.Run(name, func(test *testing.T) {
			fixture, reader := bootstrapFixture(test)
			branch := ""
			switch name {
			case "default":
				fixture.metadata.view.DefaultBranch, reader.ref.Name = "trunk", "trunk"
			case "explicit":
				branch, reader.ref.Name = "feature/login", "feature/login"
			case "unset-default":
				fixture.metadata.view.DefaultBranch = ""
			case "absent-branch":
				branch = "missing"
			case "archived":
				reader.archive = &domain.SessionArchive{RepoID: fixture.repo, Key: domain.SessionArchiveKey(reader.snapshots[reader.ref.Target])}
			case "legacy-archive":
				reader.archiveAt = domain.SessionArchiveKey(domain.Snapshot{ID: reader.ref.Target})
				reader.archive = &domain.SessionArchive{RepoID: fixture.repo, Key: reader.archiveAt, SnapshotID: reader.ref.Target}
				archives := domain.ProjectSessionArchives([]domain.SessionArchive{*reader.archive}, []domain.Snapshot{reader.snapshots[reader.ref.Target]}, nil, "main")
				if len(archives) != 1 || len(archives[0].SnapshotIDs) != 1 || archives[0].SnapshotIDs[0] != reader.ref.Target {
					test.Fatal("legacy archive fixture does not match graph semantics")
				}
			case "root":
				snapshot := reader.snapshots[reader.ref.Target]
				snapshot.Parents = nil
				reader.snapshots[reader.ref.Target] = snapshot
			}
			view, err := fixture.queries.GetRepositoryBootstrap(inbound.WithSystemActor(context.Background()), fixture.repo, branch)
			if err != nil {
				test.Fatal(err)
			}
			if name == "absent-branch" {
				if view.Ref != nil || view.Snapshot != nil || view.Parent != nil || view.Branch != branch {
					test.Fatal("missing branch selected unrelated context", view)
				}
				return
			}
			if view.Ref == nil || view.Branch != reader.ref.Name || view.DefaultBranch == "" {
				test.Fatal("branch selection changed", view)
			}
			if name == "archived" || name == "legacy-archive" {
				if !view.Archived || view.Snapshot != nil || view.Parent != nil {
					test.Fatal("archived conversation reappeared", view)
				}
			} else if view.Snapshot == nil || (name == "root" && view.Parent != nil) {
				test.Fatal("incorrect conversation selection", view)
			}
		})
	}
}

func TestBootstrapFailsClosedWithoutPartialData(test *testing.T) {
	for _, failure := range []string{"admission", "authorization", "pin", "commit", "ref", "snapshot", "archive", "cross-repo", "wrong-id", "bad-doc", "bad-ref", "bad-archive", "missing-parent"} {
		test.Run(failure, func(test *testing.T) {
			fixture, reader := bootstrapFixture(test)
			ctx := inbound.WithSystemActor(context.Background())
			denied := domain.ErrForbidden
			switch failure {
			case "admission":
				fixture.resources.err = denied
			case "authorization":
				ctx = context.Background()
			case "pin":
				fixture.scope.entryErr = denied
			case "commit":
				fixture.scope.finishErr = denied
			case "ref", "snapshot", "archive":
				reader.errors[failure] = denied
			case "cross-repo", "wrong-id", "bad-doc":
				snapshot := reader.snapshots[reader.ref.Target]
				switch failure {
				case "cross-repo":
					snapshot.RepoID = domain.HashContent([]byte("other repo"))
				case "wrong-id":
					snapshot.ID = domain.HashContent([]byte("other snapshot"))
				case "bad-doc":
					snapshot.DocHash = "invalid"
				}
				reader.snapshots[reader.ref.Target] = snapshot
			case "bad-ref":
				reader.ref.RepoID = domain.HashContent([]byte("other repo"))
			case "bad-archive":
				reader.archive = &domain.SessionArchive{RepoID: fixture.repo, Key: domain.HashContent([]byte("other archive"))}
			case "missing-parent":
				delete(reader.snapshots, reader.snapshots[reader.ref.Target].Parents[0])
			}
			view, err := fixture.queries.GetRepositoryBootstrap(ctx, fixture.repo, "main")
			if err == nil || !reflect.DeepEqual(view, domain.RepositoryBootstrap{}) {
				test.Fatal("failed read leaked selection", view, err)
			}
			if failure != "admission" && fixture.resources.released != 1 {
				test.Fatal("resource admission leaked")
			}
		})
	}
}

func TestBootstrapRejectsInvalidSelectionBeforeReading(test *testing.T) {
	fixture, _ := bootstrapFixture(test)
	for _, branch := range []string{"../escape", "bad branch", "refs/heads/.."} {
		if _, err := fixture.queries.GetRepositoryBootstrap(context.Background(), fixture.repo, branch); !errors.Is(err, domain.ErrValidation) {
			test.Fatal(branch, err)
		}
	}
	if fixture.resources.acquired != 0 || fixture.scope.pins != 0 {
		test.Fatal("invalid input acquired resources")
	}
}

func TestBootstrapRejectsUnboundedOrUnpinnedStorage(test *testing.T) {
	for _, mode := range []string{"unpinned", "legacy-protocol"} {
		test.Run(mode, func(test *testing.T) {
			fixture, _ := bootstrapFixture(test)
			if mode == "unpinned" {
				fixture.queries.atomicRead = false
			} else {
				fixture.queries.metadata.(*bootstrapTestMetadata).protocol = 0
			}
			view, err := fixture.queries.GetRepositoryBootstrap(inbound.WithSystemActor(context.Background()), fixture.repo, "")
			if !errors.Is(err, domain.ErrCatalogUnsupported) || !reflect.DeepEqual(view, domain.RepositoryBootstrap{}) || len(fixture.ports.calls) != 0 {
				test.Fatal("unsupported store made a bounded-read claim", mode, view, err, fixture.ports.calls)
			}
		})
	}
}
