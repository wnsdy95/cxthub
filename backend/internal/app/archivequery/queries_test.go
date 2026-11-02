package archivequery_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app/archivequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestArchiveMetadataUsesOnlyExplicitReadPorts(test *testing.T) {
	queries, ports, _ := archiveFixture(test)
	first := archiveDocument(test, ports, false, "origin", 0, archiveMessage("first", 0))
	second := archiveDocument(test, ports, false, "other", 1, archiveMessage("second", 1))
	ports.snapshots[0].Branches = []string{"stale"}
	ports.logs = []domain.RefLogEntry{
		{Kind: domain.RefBranch, Name: "shared", Old: first.Hash, New: second.Hash},
		{Kind: domain.RefBranch, Name: "alpha", New: first.Hash},
		{Kind: domain.RefTag, Name: "not-a-branch", New: first.Hash},
	}
	ports.refs = []domain.Ref{{Kind: domain.RefBranch, Name: "shared", Target: second.Hash}}
	repositories, err := queries.ListRepos(context.Background(), "exact-team")
	if err != nil || !reflect.DeepEqual(repositories, ports.repositories) || ports.team != "exact-team" || !reflect.DeepEqual(ports.scope.calls, []string{"repositories"}) {
		test.Fatal("repository listing lost its exact scope or team", err)
	}
	ports.scope.calls = nil
	snapshots, err := queries.List(context.Background(), inbound.ListSnapshotsInput{RepoID: ports.repo, Branch: "shared"})
	if err != nil || len(snapshots) != 2 || !reflect.DeepEqual(snapshots[0].Branches, []string{"alpha", "origin", "shared"}) || !reflect.DeepEqual(snapshots[1].Branches, []string{"other", "shared"}) {
		test.Fatal("reflog membership or branch ordering changed", snapshots, err)
	}
	if !reflect.DeepEqual(ports.snapshots[0].Branches, []string{"stale"}) || !reflect.DeepEqual(ports.scope.calls, []string{"repository"}) {
		test.Fatal("list changed source metadata or reentered its scope")
	}
	filtered, err := queries.List(context.Background(), inbound.ListSnapshotsInput{RepoID: ports.repo, Branch: "alpha"})
	if err != nil || len(filtered) != 1 || filtered[0].ID != first.Hash {
		test.Fatal("branch list reverted to scalar birth-label filtering", filtered, err)
	}
	refs, err := queries.ListRefs(context.Background(), ports.repo)
	if err != nil || !reflect.DeepEqual(refs, ports.refs) {
		test.Fatal("ref read changed", refs, err)
	}
	snapshot, err := queries.GetSnapshot(context.Background(), ports.repo, second.Hash)
	if err != nil || !reflect.DeepEqual(snapshot, ports.snapshots[1]) {
		test.Fatal("snapshot read changed", snapshot, err)
	}
}

func TestArchiveGetRepoIsRawValidatedAuthorizationMetadata(test *testing.T) {
	queries, ports, _ := archiveFixture(test)
	ports.scope.before = domain.ErrForbidden
	ports.repositories[0].RequiredDocIdentity = "future-identity"
	repository, err := queries.GetRepo(context.Background(), ports.repo)
	if err != nil || !reflect.DeepEqual(repository, ports.repositories[0]) || len(ports.scope.calls) != 0 || !reflect.DeepEqual(ports.calls, []string{"repo"}) {
		test.Fatal("GetRepo entered scope or changed raw metadata meaning", repository, err)
	}
	ports.calls = nil
	if _, err := queries.GetRepo(context.Background(), "invalid"); !errors.Is(err, domain.ErrIntegrity) || len(ports.calls) != 0 || len(ports.scope.calls) != 0 {
		test.Fatal("invalid raw repository ID reached a port", err)
	}
	ports.scope.before = nil
	ports.repositories[0].RequiredDocIdentity = domain.DocumentIdentityLegacy
	ports.scope.authorize = func(ctx context.Context) error {
		_, err := queries.GetRepo(ctx, ports.repo)
		return err
	}
	if _, err := queries.ListRefs(context.Background(), ports.repo); err != nil || !reflect.DeepEqual(ports.scope.calls, []string{"repository"}) || !reflect.DeepEqual(ports.calls, []string{"repo", "refs"}) {
		test.Fatal("GetRepo recursively authorized inside permission callback", err, ports.calls, ports.scope.calls)
	}
	ports.failStage, ports.failure = "repo", domain.ErrNotFound
	if _, err := queries.GetRepo(context.Background(), ports.repo); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("raw metadata error changed", err)
	}
}

func TestArchiveRepositoryListValidatesReturnedIdentityOnly(test *testing.T) {
	queries, ports, _ := archiveFixture(test)
	ports.repositories[0].RequiredDocIdentity = "future-identity"
	if output, err := queries.ListRepos(context.Background(), "team"); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) || output != nil {
		test.Fatal("unsupported listed repository identity survived", output, err)
	}
}

func TestArchiveReadValidationPreservesHashFences(test *testing.T) {
	queries, ports, _ := archiveFixture(test)
	valid := domain.HashContent([]byte("valid hash"))
	operations := []func() error{
		func() error {
			_, err := queries.List(context.Background(), inbound.ListSnapshotsInput{RepoID: "invalid"})
			return err
		},
		func() error { _, err := queries.ListRefs(context.Background(), "invalid"); return err },
		func() error { _, err := queries.GetSnapshot(context.Background(), ports.repo, "invalid"); return err },
		func() error { _, err := queries.GetSnapshot(context.Background(), "invalid", valid); return err },
		func() error { _, err := queries.GetDoc(context.Background(), ports.repo, "invalid"); return err },
		func() error { _, err := queries.GetDoc(context.Background(), "invalid", valid); return err },
	}
	for _, operation := range operations {
		if err := operation(); !errors.Is(err, domain.ErrIntegrity) || len(ports.calls) != 0 {
			test.Fatal("hash validation reached a read port", err, ports.calls)
		}
	}
}

func TestArchiveGetDocMaterializesWithoutIndexOrNestedScope(test *testing.T) {
	for _, root := range []bool{false, true} {
		test.Run(map[bool]string{false: "legacy", true: "root"}[root], func(test *testing.T) {
			queries, ports, _ := archiveFixture(test)
			want := archiveDocument(test, ports, root, "main", 0, archiveMessage("raw searchable body", 0), domain.CIREvent{Kind: domain.EventToolResult, CallID: "call", Output: "archival tool body"})
			if !root {
				ports.snapshots = nil
			}
			got, err := queries.GetDoc(context.Background(), ports.repo, want.Hash)
			wantCalls := []string{"snapshot", "legacy"}
			if root {
				wantCalls[1] = "verified"
			}
			if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(ports.calls, wantCalls) || !reflect.DeepEqual(ports.scope.calls, []string{"document"}) {
				test.Fatal("raw export changed identity, CIR, source, or scope", got, err, ports.calls)
			}
			if root && domain.HashContent(ports.proofs[want.Hash].Bytes()) == got.Hash {
				test.Fatal("root raw export relabeled its canonical bytes")
			}
		})
	}
}

func TestArchiveScopeOwnsResponseLifetimeAndFailureDiscard(test *testing.T) {
	for _, atomic := range []bool{false, true} {
		queries, ports, config := archiveFixture(test)
		document := archiveDocument(test, ports, true, "main", 0, archiveMessage("needle", 0))
		config.AtomicRead = atomic
		queries, err := archivequery.New(config)
		if err != nil {
			test.Fatal(err)
		}
		var release func()
		ctx := inbound.WithDocumentResponseRetainer(context.Background(), func(retained func()) bool { release = retained; return true })
		got, err := queries.GetDoc(ctx, ports.repo, document.Hash)
		if err != nil || !reflect.DeepEqual(got, document) || release == nil || ports.scope.releases != 0 {
			test.Fatal("archive replaced the scope's response retention", err)
		}
		release()
		if ports.scope.releases != 1 {
			test.Fatal("response release was lost")
		}
		ports.scope.after = domain.ErrConflict
		got, err = queries.GetDoc(context.Background(), ports.repo, document.Hash)
		want := document
		if atomic {
			want = domain.SessionDoc{}
		}
		if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(got, want) {
			test.Fatal("archive changed the explicit atomic-read error policy", got, err)
		}
	}
}

func TestArchiveErrorsRemainExactAndScoped(test *testing.T) {
	for _, stage := range []string{"snapshots", "reflog", "refs", "snapshot", "legacy", "verified"} {
		for _, failure := range []error{domain.ErrNotFound, domain.ErrForbidden, domain.ErrIntegrity, domain.ErrResourceOverloaded, context.Canceled} {
			test.Run(stage+"/"+failure.Error(), func(test *testing.T) {
				queries, ports, _ := archiveFixture(test)
				document := archiveDocument(test, ports, stage == "verified", "main", 0, archiveMessage("fixture", 0))
				ports.failStage, ports.failure = stage, failure
				var err error
				switch stage {
				case "snapshots", "reflog":
					_, err = queries.List(context.Background(), inbound.ListSnapshotsInput{RepoID: ports.repo})
				case "refs":
					_, err = queries.ListRefs(context.Background(), ports.repo)
				case "snapshot":
					_, err = queries.GetSnapshot(context.Background(), ports.repo, document.Hash)
				default:
					_, err = queries.GetDoc(context.Background(), ports.repo, document.Hash)
				}
				if !errors.Is(err, failure) || len(ports.scope.calls) != 1 {
					test.Fatal("archive changed source error or retried admission", err, ports.scope.calls)
				}
			})
		}
	}
	queries, ports, _ := archiveFixture(test)
	ports.scope.before = domain.ErrForbidden
	if _, err := queries.Search(context.Background(), inbound.SearchInput{RepoID: ports.repo, Query: "needle"}); !errors.Is(err, domain.ErrForbidden) || len(ports.calls) != 0 {
		test.Fatal("denied scope reached archive ports", err, ports.calls)
	}
}

func TestArchiveConstructorRejectsNilAndTypedNilWithoutPortCalls(test *testing.T) {
	for _, dependency := range []string{"scope", "metadata", "documents", "candidates"} {
		for _, typedNil := range []bool{false, true} {
			test.Run(dependency+map[bool]string{false: "/nil", true: "/typed-nil"}[typedNil], func(test *testing.T) {
				_, ports, config := archiveFixture(test)
				var missingScope *archiveScope
				var missingPorts *archivePorts
				switch dependency {
				case "scope":
					config.Scope = nil
					if typedNil {
						config.Scope = missingScope
					}
				case "metadata":
					config.Metadata = nil
					if typedNil {
						config.Metadata = missingPorts
					}
				case "documents":
					config.Documents = nil
				case "candidates":
					config.Candidates = nil
					if typedNil {
						config.Candidates = missingPorts
					}
				}
				queries, err := archivequery.New(config)
				if dependency == "candidates" && !typedNil {
					if err != nil || queries == nil {
						test.Fatal("explicitly omitted candidate capability was rejected", err)
					}
				} else if queries != nil || !errors.Is(err, domain.ErrValidation) {
					test.Fatal("invalid archive assembly was accepted", queries, err)
				}
				if len(ports.calls) != 0 || len(ports.scope.calls) != 0 {
					test.Fatal("archive constructor probed ports before startup completed")
				}
			})
		}
	}
	_, ports, config := archiveFixture(test)
	config.Profile = documentquery.VerifiedRoots
	if queries, err := archivequery.New(config); queries != nil || !errors.Is(err, domain.ErrValidation) {
		test.Fatal("mismatched document profile was accepted", err)
	}
	config.Profile = 0
	if queries, err := archivequery.New(config); queries != nil || !errors.Is(err, domain.ErrValidation) || len(ports.calls) != 0 {
		test.Fatal("undeclared archive profile was accepted", err)
	}
}
