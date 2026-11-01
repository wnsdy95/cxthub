package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type memoryDigestCommandPorts struct {
	test           *testing.T
	repo           domain.ContentHash
	target         domain.ContentHash
	previousHash   domain.ContentHash
	previous       domain.MemoryDigest
	digest         domain.MemoryDigest
	expected       domain.ContentHash
	next           domain.ContentHash
	calls          []string
	snapshotErr    error
	parentErr      error
	objectErr      error
	attachmentErr  error
	objectScopeErr error
	attachScopeErr error
	observe        func(string, context.Context)
}

func (ports *memoryDigestCommandPorts) record(stage string, ctx context.Context, repo domain.ContentHash) {
	ports.test.Helper()
	if repo != ports.repo {
		ports.test.Fatal("command changed repository", repo)
	}
	ports.calls = append(ports.calls, stage)
	if ports.observe != nil {
		ports.observe(stage, ctx)
	}
}

func (ports *memoryDigestCommandPorts) GetSnapshot(ctx context.Context, repo, target domain.ContentHash) (domain.Snapshot, error) {
	ports.record("snapshot", ctx, repo)
	if target != ports.target {
		ports.test.Fatal("command changed target", target)
	}
	return domain.Snapshot{ID: target, RepoID: repo}, ports.snapshotErr
}

func (ports *memoryDigestCommandPorts) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	ports.record("parent", ctx, repo)
	if hash != ports.previousHash {
		ports.test.Fatal("command changed causal parent", hash)
	}
	return ports.previous, ports.parentErr
}

func (ports *memoryDigestCommandPorts) PutMemory(ctx context.Context, repo domain.ContentHash, digest domain.MemoryDigest) (domain.ContentHash, error) {
	ports.record("object", ctx, repo)
	ports.digest = digest
	return domain.HashContent([]byte("stored memory object")), ports.objectErr
}

func (ports *memoryDigestCommandPorts) CompareAndSwapSnapshotMemory(ctx context.Context, repo, target, expected, next domain.ContentHash) error {
	ports.record("attachment", ctx, repo)
	if target != ports.target {
		ports.test.Fatal("CAS changed target", target)
	}
	ports.expected, ports.next = expected, next
	return ports.attachmentErr
}

func (ports *memoryDigestCommandPorts) WithinMemoryObjectWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) (domain.ContentHash, error)) (domain.ContentHash, error) {
	ports.record("object scope", ctx, repo)
	if ports.objectScopeErr != nil {
		return "", ports.objectScopeErr
	}
	return write(context.WithValue(ctx, memoryDigestCommandScopeKey{}, "object"))
}

func (ports *memoryDigestCommandPorts) WithinMemoryAttachmentWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	ports.record("attachment scope", ctx, repo)
	if ports.attachScopeErr != nil {
		return ports.attachScopeErr
	}
	return write(context.WithValue(ctx, memoryDigestCommandScopeKey{}, "attachment"))
}

type memoryDigestCommandScopeKey struct{}

type memoryDigestAfterObjectScope struct {
	MemoryDigestWriteScope
	after func() error
}

func (scope *memoryDigestAfterObjectScope) WithinMemoryObjectWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) (domain.ContentHash, error)) (domain.ContentHash, error) {
	hash, err := scope.MemoryDigestWriteScope.WithinMemoryObjectWrite(ctx, repo, write)
	if err != nil {
		return "", err
	}
	if err := scope.after(); err != nil {
		return "", err
	}
	return hash, nil
}

func TestMemoryDigestCommandsConstructor(test *testing.T) {
	ports := &memoryDigestCommandPorts{test: test}
	for _, config := range []MemoryDigestCommandsConfig{
		{Snapshots: ports, Scope: ports}, {Blobs: ports, Scope: ports}, {Blobs: ports, Snapshots: ports},
		{Blobs: (*memoryDigestCommandPorts)(nil), Snapshots: ports, Scope: ports},
		{Blobs: ports, Snapshots: (*memoryDigestCommandPorts)(nil), Scope: ports},
		{Blobs: ports, Snapshots: ports, Scope: (*memoryDigestCommandPorts)(nil)},
	} {
		if commands, err := NewMemoryDigestCommands(config); commands != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("accepted missing/typed-nil port", commands, err)
		}
	}
	if commands, err := NewMemoryDigestCommands(MemoryDigestCommandsConfig{Blobs: ports, Snapshots: ports, Scope: ports}); commands == nil || err != nil {
		test.Fatal(commands, err)
	}
	if len(ports.calls) != 0 {
		test.Fatal("constructor performed IO", ports.calls)
	}
	filesystem := store.NewFSStore(test.TempDir())
	for _, service := range []*Service{nil, NewService(filesystem, filesystem, nil, nil, nil)} {
		if commands, err := service.MemoryDigestCommands(); commands != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
			test.Fatal("complete factory accepted absent authority", commands, err)
		}
	}
	failure := errors.New("strong capability unavailable")
	for _, work := range []*repositoryWork{nil, {configurationError: failure}} {
		commands, err := memoryDigestsForWork(ports, ports, work)
		want := failure
		if work == nil {
			want = domain.ErrValidation
		}
		if commands != nil || !errors.Is(err, want) {
			test.Fatal("factory weakened configuration error", commands, err)
		}
	}
}

func TestMemoryDigestCommandsValidationAndWriteOrder(test *testing.T) {
	repo, target := domain.HashContent([]byte("memory repository")), domain.HashContent([]byte("memory target"))
	parent := domain.HashContent([]byte("memory parent"))
	accepted := []string{"snapshot", "parent", "object scope", "object", "attachment scope", "attachment"}
	for _, scenario := range []struct {
		name   string
		legacy bool
		edit   func(*domain.MemoryDigest, *memoryDigestCommandPorts)
		want   error
		calls  []string
	}{
		{name: "causal", calls: accepted},
		{name: "typed upgrade", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) { digest.ClaimsVersion = 1 }, calls: accepted},
		{name: "legacy first", legacy: true, edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) { digest.PreviousMemoryHash = "" }, calls: []string{"snapshot", "object scope", "object", "attachment scope", "attachment"}},
		{name: "legacy typed precedes malformed hash", legacy: true, edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			digest.SnapshotID, digest.ClaimsVersion = "bad", 1
		}, want: domain.ErrValidation},
		{name: "legacy causal", legacy: true, want: domain.ErrValidation},
		{name: "unknown claims before hash", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			digest.SnapshotID, digest.ClaimsVersion = "bad", 2
		}, want: domain.ErrValidation},
		{name: "claims require version", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			digest.Fragments = []domain.MemoryFragment{{SourceSnapshot: target, Claims: []domain.MemoryClaim{{Kind: "decision", Text: "typed claim"}}}}
		}, want: domain.ErrValidation},
		{name: "invalid snapshot", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) { digest.SnapshotID = "bad" }, want: domain.ErrIntegrity},
		{name: "invalid parent", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) { digest.PreviousMemoryHash = "bad" }, want: domain.ErrIntegrity},
		{name: "invalid fragment", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			digest.Fragments = []domain.MemoryFragment{{SourceSnapshot: "bad"}}
		}, want: domain.ErrIntegrity},
		{name: "coverage version", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			digest.GraftCoverage = &domain.MemoryGraftCoverage{}
		}, want: domain.ErrIntegrity},
		{name: "coverage sequence", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			digest.GraftCoverage = &domain.MemoryGraftCoverage{ProjectionVersion: 1, GraftSeq: domain.MaxGraftSeq + 1}
		}, want: domain.ErrIntegrity},
		{name: "coverage completion", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			digest.GraftCoverage = &domain.MemoryGraftCoverage{ProjectionVersion: 1, ProjectionComplete: true}
		}, want: domain.ErrIntegrity},
		{name: "coverage fingerprint", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			digest.GraftCoverage = &domain.MemoryGraftCoverage{ProjectionVersion: 1, LineageFingerprint: "bad"}
		}, want: domain.ErrIntegrity},
		{name: "coverage parent", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			digest.GraftCoverage = &domain.MemoryGraftCoverage{ProjectionVersion: 1, GraftParents: []domain.ContentHash{"bad"}}
		}, want: domain.ErrIntegrity},
		{name: "coverage source", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			digest.GraftCoverage = &domain.MemoryGraftCoverage{ProjectionVersion: 1, PinnedSources: []domain.ContentHash{"bad"}}
		}, want: domain.ErrIntegrity},
		{name: "snapshot error before authorization", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			ports.snapshotErr, ports.objectScopeErr = domain.ErrNotFound, domain.ErrForbidden
		}, want: domain.ErrNotFound, calls: accepted[:1]},
		{name: "parent error", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			ports.parentErr = domain.ErrNotFound
		}, want: domain.ErrNotFound, calls: accepted[:2]},
		{name: "parent snapshot mismatch", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) { ports.previous.SnapshotID = parent }, want: domain.ErrIntegrity, calls: accepted[:2]},
		{name: "claims downgrade", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) { ports.previous.ClaimsVersion = 1 }, want: domain.ErrValidation, calls: accepted[:2]},
		{name: "current object authorization", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			ports.objectScopeErr = domain.ErrForbidden
		}, want: domain.ErrForbidden, calls: accepted[:3]},
		{name: "blob failure", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			ports.objectErr = domain.ErrIntegrity
		}, want: domain.ErrIntegrity, calls: accepted[:4]},
		{name: "current attachment authorization", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			ports.attachScopeErr = domain.ErrForbidden
		}, want: domain.ErrForbidden, calls: accepted[:5]},
		{name: "CAS conflict without retry", edit: func(digest *domain.MemoryDigest, ports *memoryDigestCommandPorts) {
			ports.attachmentErr = domain.ErrConflict
		}, want: domain.ErrConflict, calls: accepted},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			digest := domain.MemoryDigest{SnapshotID: target, PreviousMemoryHash: parent, Summary: "exact payload"}
			ports := &memoryDigestCommandPorts{test: test, repo: repo, target: target, previousHash: parent, previous: domain.MemoryDigest{SnapshotID: target}}
			ports.observe = func(stage string, ctx context.Context) {
				if ctx.Value(auditOperationKey{}) != "context.memory.updated" {
					test.Fatal("lost audit action", stage)
				}
				if stage == "object" || stage == "attachment" {
					if ctx.Value(memoryDigestCommandScopeKey{}) != stage {
						test.Fatal("write escaped bound scope", stage)
					}
				} else if ctx.Value(memoryDigestCommandScopeKey{}) != nil {
					test.Fatal("preflight or second scope borrowed first write")
				}
			}
			if scenario.edit != nil {
				scenario.edit(&digest, ports)
			}
			commands, err := NewMemoryDigestCommands(MemoryDigestCommandsConfig{Blobs: ports, Snapshots: ports, Scope: ports})
			if err != nil {
				test.Fatal(err)
			}
			write := commands.PutMemoryDigestCAS
			if scenario.legacy {
				write = commands.PutMemoryDigest
			}
			hash, err := write(test.Context(), repo, digest)
			if !errors.Is(err, scenario.want) || !reflect.DeepEqual(ports.calls, scenario.calls) || (err != nil && hash != "") {
				test.Fatalf("hash=%s err=%v calls=%v", hash, err, ports.calls)
			}
			if wave42Count(ports.calls, "object") != 0 && !reflect.DeepEqual(ports.digest, digest) {
				test.Fatal("command changed immutable payload")
			}
			if wave42Count(ports.calls, "attachment") != 0 && (ports.expected != digest.PreviousMemoryHash || ports.next != domain.HashContent([]byte("stored memory object"))) {
				test.Fatal("command changed exact CAS", ports.expected, ports.next)
			}
			if err == nil && hash != ports.next {
				test.Fatal("command changed result", hash)
			}
		})
	}
}

func TestMemoryDigestCommandsExistingWorkAndAuthority(test *testing.T) {
	repo, target := domain.HashContent([]byte("memory work")), domain.HashContent([]byte("target"))
	for _, scenario := range []struct {
		name      string
		system    bool
		nested    bool
		cancelled bool
		commitErr error
		writeErr  error
		want      error
		calls     []string
	}{
		{name: "separate writes", system: true, calls: []string{"write", "metadata", "write", "metadata", "graph"}},
		{name: "borrowed transaction", system: true, nested: true, calls: []string{"write", "metadata", "write", "metadata", "write", "metadata", "graph"}},
		{name: "anonymous", want: domain.ErrUnauthorized, calls: []string{"write"}},
		{name: "cancelled", system: true, cancelled: true, want: context.Canceled, calls: []string{"write"}},
		{name: "object commit failure", system: true, commitErr: domain.ErrConflict, want: domain.ErrConflict, calls: []string{"write", "metadata"}},
		{name: "CAS failure", system: true, writeErr: domain.ErrConflict, want: domain.ErrConflict, calls: []string{"write", "metadata", "write", "metadata"}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			metadata := &wave42WorkStore{commitError: scenario.commitErr, get: func(ctx context.Context, actual domain.ContentHash) (domain.Repo, error) {
				if actual != repo || ctx.Value(wave42TransactionKey{}) != "write" {
					test.Fatal("identity read escaped transaction")
				}
				return domain.Repo{ID: repo}, nil
			}}
			work := newRepositoryWork(metadata, nil, nil)
			ports := &memoryDigestCommandPorts{test: test, repo: repo, target: target, attachmentErr: scenario.writeErr}
			ports.observe = func(stage string, ctx context.Context) {
				if stage == "snapshot" {
					return
				}
				if ctx.Value(wave42TransactionKey{}) != "write" || ctx.Value(afterCommitKey{}) == nil || ctx.Value(auditOperationKey{}) != "context.memory.updated" {
					test.Fatal("write escaped original work", stage)
				}
				if (stage == "object" && ctx.Value(revisionScopeKey{}) != "none") || (stage == "attachment" && ctx.Value(revisionScopeKey{}) != nil) {
					test.Fatal("revision scope changed", stage)
				}
			}
			commands, err := memoryDigestsForWork(ports, ports, work)
			if err != nil {
				test.Fatal(err)
			}
			ctx, cancel := context.WithCancel(test.Context())
			defer cancel()
			if scenario.system {
				ctx = inbound.WithSystemActor(ctx)
			}
			if scenario.cancelled {
				cancel()
			}
			write := func(bound context.Context) (domain.ContentHash, error) {
				return commands.PutMemoryDigestCAS(bound, repo, domain.MemoryDigest{SnapshotID: target})
			}
			var hash domain.ContentHash
			if scenario.nested {
				hash, err = repositoryWriteWithWork(ctx, work, repo, write)
			} else {
				hash, err = write(ctx)
			}
			if !errors.Is(err, scenario.want) || !reflect.DeepEqual(metadata.calls, scenario.calls) || (err != nil && hash != "") {
				test.Fatal(hash, err, metadata.calls)
			}
		})
	}
}

func TestMemoryDigestCommandsCapturedOwnerAndCompatibilityReplay(test *testing.T) {
	for _, direct := range []bool{false, true} {
		test.Run(map[bool]string{false: "compatibility", true: "owner"}[direct], func(test *testing.T) {
			storage := store.NewFSStore(test.TempDir())
			service := NewService(storage, storage, nil, nil, storage)
			var writer MemoryDigestWriter = service
			if direct {
				commands, err := service.MemoryDigestCommands()
				if err != nil {
					test.Fatal(err)
				}
				writer = commands
				*service = Service{}
			}
			ctx := inbound.WithSystemActor(test.Context())
			repo, target := domain.HashContent([]byte(test.Name())), domain.HashContent([]byte("snapshot"))
			if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: target, RepoID: repo, DocHash: target}); err != nil {
				test.Fatal(err)
			}
			root := domain.MemoryDigest{SnapshotID: target, Summary: "legacy root"}
			rootHash, err := writer.PutMemoryDigest(ctx, repo, root)
			if err != nil {
				test.Fatal(err)
			}
			if replay, err := writer.PutMemoryDigest(ctx, repo, root); err != nil || replay != rootHash {
				test.Fatal("immediate legacy replay", replay, err)
			}
			typed := domain.MemoryDigest{SnapshotID: target, PreviousMemoryHash: rootHash, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: target, Claims: []domain.MemoryClaim{{Kind: "decision", Text: "Keep history"}}}}}
			typedHash, err := writer.PutMemoryDigestCAS(ctx, repo, typed)
			if err != nil {
				test.Fatal(err)
			}
			if replay, err := writer.PutMemoryDigestCAS(ctx, repo, typed); err != nil || replay != typedHash {
				test.Fatal("immediate causal replay", replay, err)
			}
			loser := root
			loser.Summary = "concurrent legacy loser"
			loserHash, err := domain.MemoryDigestHash(loser)
			if err != nil {
				test.Fatal(err)
			}
			if hash, err := writer.PutMemoryDigest(ctx, repo, loser); hash != "" || !errors.Is(err, domain.ErrConflict) {
				test.Fatal("legacy replaced nonempty pointer", hash, err)
			}
			if retained, err := storage.GetMemory(ctx, repo, loserHash); err != nil || retained.Summary != loser.Summary {
				test.Fatal("CAS loser blob was discarded", retained, err)
			}
			if hash, err := writer.PutMemoryDigest(ctx, repo, root); hash != "" || !errors.Is(err, domain.ErrConflict) {
				test.Fatal("stale replay rewound memory", hash, err)
			}
			downgrade := domain.MemoryDigest{SnapshotID: target, PreviousMemoryHash: typedHash, Summary: "downgrade"}
			if hash, err := writer.PutMemoryDigestCAS(ctx, repo, downgrade); hash != "" || !errors.Is(err, domain.ErrValidation) {
				test.Fatal("claims downgrade accepted", hash, err)
			}
			current, err := storage.GetSnapshot(ctx, repo, target)
			if err != nil || current.MemoryHash != typedHash {
				test.Fatal("failed commands changed pointer", current, err)
			}
			next := typed
			next.PreviousMemoryHash, next.Summary = typedHash, "later causal memory"
			nextHash, err := writer.PutMemoryDigestCAS(ctx, repo, next)
			if err != nil {
				test.Fatal(err)
			}
			if hash, err := writer.PutMemoryDigestCAS(ctx, repo, typed); hash != "" || !errors.Is(err, domain.ErrConflict) {
				test.Fatal("stale causal replay accepted", hash, err)
			}
			current, err = storage.GetSnapshot(ctx, repo, target)
			if err != nil || current.MemoryHash != nextHash {
				test.Fatal("stale causal replay rewound memory", current, err)
			}
		})
	}
}

func TestMemoryDigestCommandsRechecksMembershipBetweenWrites(test *testing.T) {
	repo, target := domain.HashContent([]byte(test.Name())), domain.HashContent([]byte("target"))
	repositoryID := domain.NewID("ws_")
	storage := &wave42WorkStore{FSStore: store.NewFSStore(test.TempDir()), get: func(context.Context, domain.ContentHash) (domain.Repo, error) {
		return domain.Repo{ID: repo, RepositoryID: repositoryID}, nil
	}}
	if err := storage.CreateRepository(test.Context(), domain.Repository{ID: repositoryID, OwnerID: "owner", Slug: "memory", Name: "Memory"}); err != nil {
		test.Fatal(err)
	}
	if err := storage.AddMember(test.Context(), domain.Membership{RepositoryID: repositoryID, UserID: "member", Role: domain.RoleMember}); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSnapshot(test.Context(), domain.Snapshot{ID: target, RepoID: repo, DocHash: target}); err != nil {
		test.Fatal(err)
	}
	service := NewService(storage, storage, nil, nil, storage)
	commands, err := service.MemoryDigestCommands()
	if err != nil {
		test.Fatal(err)
	}
	*service = Service{}
	commands.scope = &memoryDigestAfterObjectScope{MemoryDigestWriteScope: commands.scope, after: func() error {
		return storage.RemoveMember(test.Context(), repositoryID, "member")
	}}
	digest := domain.MemoryDigest{SnapshotID: target, Summary: "retained before revocation"}
	wantHash, err := domain.MemoryDigestHash(digest)
	if err != nil {
		test.Fatal(err)
	}
	ctx := inbound.WithRepositoryActor(test.Context(), "member")
	if hash, err := commands.PutMemoryDigestCAS(ctx, repo, digest); hash != "" || !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("cached authority survived object commit", hash, err)
	}
	if retained, err := storage.GetMemory(test.Context(), repo, wantHash); err != nil || retained.Summary != digest.Summary {
		test.Fatal("earlier object write was lost", retained, err)
	}
	current, err := storage.GetSnapshot(test.Context(), repo, target)
	if err != nil || current.MemoryHash != "" || wave42Count(storage.calls, "graph") != 0 || wave42Count(storage.calls, "lock:"+repositoryID+":member") != 2 {
		test.Fatal("revoked attachment changed pointer/revision or skipped current locks", current, err, storage.calls)
	}
}
