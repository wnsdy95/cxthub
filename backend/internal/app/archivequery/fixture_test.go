package archivequery_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/archivequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type archiveBoundKey struct{}

type archiveScope struct {
	test      *testing.T
	active    bool
	bound     context.Context
	repo      domain.ContentHash
	calls     []string
	before    error
	after     error
	authorize func(context.Context) error
	releases  int
}

func (scope *archiveScope) read(ctx context.Context, repo domain.ContentHash, kind string, read func(context.Context) error) error {
	scope.test.Helper()
	if scope.active {
		scope.test.Fatal("archive query recursively entered an admission or authorization scope")
	}
	scope.calls = append(scope.calls, kind)
	if scope.before != nil {
		return scope.before
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	scope.active = true
	scope.bound = context.WithValue(ctx, archiveBoundKey{}, scope)
	scope.repo = repo
	defer func() { scope.active = false }()
	if scope.authorize != nil {
		if err := scope.authorize(scope.bound); err != nil {
			return err
		}
	}
	err := read(scope.bound)
	if err == nil {
		err = scope.after
	}
	release := func() { scope.releases++ }
	if kind != "document" || err != nil || !inbound.RetainDocumentResponse(scope.bound, release) {
		release()
	}
	return err
}

func (scope *archiveScope) ReadRepositories(ctx context.Context, read func(context.Context) error) error {
	return scope.read(ctx, "", "repositories", read)
}

func (scope *archiveScope) ReadRepository(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope.read(ctx, repo, "repository", read)
}

func (scope *archiveScope) ReadDocument(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope.read(ctx, repo, "document", read)
}

type archivePage struct {
	hash  domain.ContentHash
	after int
	limit int
}

type archivePorts struct {
	test          *testing.T
	scope         *archiveScope
	repo          domain.ContentHash
	repositories  []domain.Repo
	snapshots     []domain.Snapshot
	refs          []domain.Ref
	logs          []domain.RefLogEntry
	documents     map[domain.ContentHash]domain.SessionDoc
	indexes       map[domain.ContentHash]domain.DocReadIndex
	proofs        map[domain.ContentHash]domain.VerifiedSessionDoc
	candidates    map[domain.ContentHash]bool
	calls         []string
	pages         []archivePage
	query         string
	team          string
	failStage     string
	failure       error
	verifiedAfter func()
}

func (ports *archivePorts) call(ctx context.Context, repo domain.ContentHash, stage string) error {
	ports.test.Helper()
	if !ports.scope.active || ctx != ports.scope.bound || ctx.Value(archiveBoundKey{}) != ports.scope || repo != "" && repo != ports.repo {
		ports.test.Fatal("archive read escaped its pinned scope or repository")
	}
	ports.calls = append(ports.calls, stage)
	if err := ctx.Err(); err != nil {
		return err
	}
	if ports.failStage == stage {
		return ports.failure
	}
	return nil
}

func (ports *archivePorts) ListRepos(ctx context.Context, team string) ([]domain.Repo, error) {
	ports.team = team
	err := ports.call(ctx, "", "repos")
	return append([]domain.Repo(nil), ports.repositories...), err
}

func (ports *archivePorts) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	ports.calls = append(ports.calls, "repo")
	if repo != ports.repo {
		return domain.Repo{}, domain.ErrNotFound
	}
	if ports.failStage == "repo" {
		return domain.Repo{}, ports.failure
	}
	return ports.repositories[0], ctx.Err()
}

func (ports *archivePorts) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	if branch != "" {
		ports.test.Fatal("archive list filtered stored birth labels instead of reflog membership")
	}
	err := ports.call(ctx, repo, "snapshots")
	return append([]domain.Snapshot(nil), ports.snapshots...), err
}

func (ports *archivePorts) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	if err := ports.call(ctx, repo, "snapshot"); err != nil {
		return domain.Snapshot{}, err
	}
	for _, snapshot := range ports.snapshots {
		if snapshot.ID == hash {
			return snapshot, nil
		}
	}
	return domain.Snapshot{}, domain.ErrNotFound
}

func (ports *archivePorts) ListRefs(ctx context.Context, repo domain.ContentHash) ([]domain.Ref, error) {
	err := ports.call(ctx, repo, "refs")
	return append([]domain.Ref(nil), ports.refs...), err
}

func (ports *archivePorts) ReadReflog(ctx context.Context, repo domain.ContentHash) ([]domain.RefLogEntry, error) {
	err := ports.call(ctx, repo, "reflog")
	return append([]domain.RefLogEntry(nil), ports.logs...), err
}

func (ports *archivePorts) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	if err := ports.call(ctx, repo, "legacy"); err != nil {
		return domain.SessionDoc{}, err
	}
	document, exists := ports.documents[hash]
	if !exists {
		return domain.SessionDoc{}, domain.ErrNotFound
	}
	return document, nil
}

func (ports *archivePorts) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	ports.test.Fatal("archive search or raw export attempted a range manifest read")
	return domain.DocChunkManifest{}, domain.ErrIntegrity
}

func (ports *archivePorts) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	ports.test.Fatal("archive search or raw export attempted a chunk fallback")
	return nil, domain.ErrIntegrity
}

func (ports *archivePorts) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	err := ports.call(ctx, repo, "index")
	return ports.indexes[hash], err
}

func (ports *archivePorts) SearchDocEvents(ctx context.Context, repo, hash domain.ContentHash, query string, after, limit int) ([]domain.DocEventIndex, error) {
	if err := ports.call(ctx, repo, "events"); err != nil {
		return nil, err
	}
	ports.pages = append(ports.pages, archivePage{hash: hash, after: after, limit: limit})
	return searchFixtureIndex(ports.indexes[hash], query, after, limit), nil
}

func (ports *archivePorts) ReadVerifiedDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	if err := ports.call(ctx, repo, "verified"); err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	proof, exists := ports.proofs[hash]
	if !exists {
		return domain.VerifiedSessionDoc{}, domain.ErrNotFound
	}
	if ports.verifiedAfter != nil {
		ports.verifiedAfter()
	}
	return proof, nil
}

func (ports *archivePorts) MatchingDocHashes(ctx context.Context, repo domain.ContentHash, query string) (map[domain.ContentHash]bool, error) {
	ports.query = query
	err := ports.call(ctx, repo, "candidates")
	return ports.candidates, err
}

func archiveFixture(test *testing.T) (*archivequery.ArchiveQueries, *archivePorts, archivequery.Config) {
	test.Helper()
	scope := &archiveScope{test: test}
	repo := domain.HashContent([]byte("standalone archive repository"))
	ports := &archivePorts{test: test, scope: scope, repo: repo, repositories: []domain.Repo{{ID: repo}},
		documents: make(map[domain.ContentHash]domain.SessionDoc), indexes: make(map[domain.ContentHash]domain.DocReadIndex), proofs: make(map[domain.ContentHash]domain.VerifiedSessionDoc)}
	raw, err := documentquery.NewRawReader(documentquery.RawConfig{Profile: documentquery.LegacyCompatibility, Snapshots: ports, Legacy: ports, Index: ports, Verified: ports})
	if err != nil {
		test.Fatal(err)
	}
	config := archivequery.Config{Profile: documentquery.LegacyCompatibility, Scope: scope, Metadata: ports, Documents: raw, Candidates: ports, AtomicRead: true}
	queries, err := archivequery.New(config)
	if err != nil {
		test.Fatal(err)
	}
	return queries, ports, config
}

func archiveMessage(text string, sequence int) domain.CIREvent {
	return domain.CIREvent{Kind: domain.EventMessage, Role: domain.RoleUser, Seq: sequence, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}
}

func archiveDocument(test *testing.T, ports *archivePorts, root bool, branch string, minute int, events ...domain.CIREvent) domain.SessionDoc {
	test.Helper()
	document := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "standalone-archive"}, Events: append([]domain.CIREvent{}, events...)}}
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	document.Hash = domain.HashContent(canonical)
	if root {
		manifest, bodies, err := domain.ConversationManifestForCIR(document.CIR)
		if err != nil {
			test.Fatal(err)
		}
		document.Hash, err = domain.ConversationManifestHash(manifest)
		if err != nil {
			test.Fatal(err)
		}
		var verifier domain.CanonicalDocVerifier
		proof, err := verifier.VerifyConversationManifestDoc(context.Background(), document.Hash, manifest, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
			body, exists := bodies[hash]
			if !exists {
				return nil, fmt.Errorf("missing fixture chunk %s", hash)
			}
			return body, nil
		})
		if err != nil {
			test.Fatal(err)
		}
		document.Identity = domain.DocumentIdentityRootV1
		ports.proofs[document.Hash] = proof
	}
	index, err := domain.BuildDocReadIndex(domain.SessionDoc{Hash: domain.HashContent(canonical), CIR: document.CIR})
	if err != nil {
		test.Fatal(err)
	}
	ports.indexes[document.Hash] = index
	ports.documents[document.Hash] = document
	ports.snapshots = append(ports.snapshots, domain.Snapshot{ID: document.Hash, RepoID: ports.repo, DocHash: document.Hash, DocIdentity: document.Identity, Branch: branch,
		CreatedAt: time.Date(2026, time.October, 1, 0, minute, 0, 0, time.FixedZone("fixture", 9*60*60))})
	return document
}
