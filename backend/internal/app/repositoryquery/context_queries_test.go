package repositoryquery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type contextQueriesPinKey struct{}

type contextQueriesProbe struct {
	test             *testing.T
	repo             domain.ContentHash
	metadata         domain.Repo
	evidenceMetadata domain.Repo
	view             domain.RepositoryView
	archives         []domain.SessionArchive
	generation       domain.RepositoryViewSourceCheckpoint
	indexes          map[domain.ContentHash]domain.DocReadIndex
	commits          map[string]domain.GitCommitTree
	prs              []evidencequery.VerifiedPRContribution
	git              []evidencequery.VerifiedGitContribution
	failures         map[string]error
	calls            map[string]int
	order            []string
	indexReads       []domain.ContentHash
	commitReads      []string
	pinned           bool
	active           bool
	pins             int
	releases         int
	scopeErr         error
	finishErr        error
}

func (probe *contextQueriesProbe) ReadContext(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	probe.test.Helper()
	probe.pins++
	if repo != probe.repo || probe.active {
		probe.test.Fatal("context query changed repository or nested its read scope", repo)
	}
	if probe.scopeErr != nil {
		return probe.scopeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	probe.active = true
	defer func() {
		probe.active = false
		probe.releases++
	}()
	if err := read(context.WithValue(ctx, contextQueriesPinKey{}, probe)); err != nil {
		return err
	}
	return probe.finishErr
}

func (probe *contextQueriesProbe) read(ctx context.Context, repo domain.ContentHash, operation string) error {
	probe.test.Helper()
	if !probe.active || ctx.Value(contextQueriesPinKey{}) != probe || repo != probe.repo {
		probe.test.Fatal("context query port escaped its repository read", operation, repo)
	}
	probe.calls[operation]++
	probe.order = append(probe.order, operation)
	if err := probe.failures[operation]; err != nil {
		return err
	}
	return ctx.Err()
}

func (probe *contextQueriesProbe) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return probe.metadata, probe.read(ctx, repo, "repo")
}

func (probe *contextQueriesProbe) ListRefs(ctx context.Context, repo domain.ContentHash) ([]domain.Ref, error) {
	return probe.view.Refs, probe.read(ctx, repo, "refs")
}

func (probe *contextQueriesProbe) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	if branch != "" {
		probe.test.Fatal("context query prefiltered the repository generation", branch)
	}
	return probe.view.Snapshots, probe.read(ctx, repo, "snapshots")
}

func (probe *contextQueriesProbe) ReadReflog(ctx context.Context, repo domain.ContentHash) ([]domain.RefLogEntry, error) {
	return probe.view.Reflog, probe.read(ctx, repo, "reflog")
}

func (probe *contextQueriesProbe) ListPendings(ctx context.Context, repo domain.ContentHash) ([]domain.Pending, error) {
	return probe.view.Pending, probe.read(ctx, repo, "pending")
}

func (probe *contextQueriesProbe) ListUnsyncs(ctx context.Context, repo domain.ContentHash) ([]domain.Unsync, error) {
	return probe.view.Unsync, probe.read(ctx, repo, "unsync")
}

func (probe *contextQueriesProbe) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	return probe.view.History, probe.read(ctx, repo, "history")
}

func (probe *contextQueriesProbe) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	return probe.view.Revision, probe.read(ctx, repo, "revision")
}

func (probe *contextQueriesProbe) ListSessionArchives(ctx context.Context, repo domain.ContentHash) ([]domain.SessionArchive, error) {
	return probe.archives, probe.read(ctx, repo, "archives")
}

func (probe *contextQueriesProbe) IsPinnedRead(ctx context.Context) bool {
	if err := probe.read(ctx, probe.repo, "pinned"); err != nil {
		probe.test.Fatal("cache scope inspected an invalid read", err)
	}
	return probe.pinned
}

func (probe *contextQueriesProbe) ReadRepositoryGraphSourceGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, error) {
	return probe.generation, probe.read(ctx, repo, "generation")
}

func (probe *contextQueriesProbe) ReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	probe.indexReads = append(probe.indexReads, hash)
	if err := probe.read(ctx, repo, "index"); err != nil {
		return domain.DocReadIndex{}, err
	}
	index, found := probe.indexes[hash]
	if !found {
		return domain.DocReadIndex{}, domain.ErrNotFound
	}
	return index, nil
}

func (probe *contextQueriesProbe) VerifiedPR(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) ([]evidencequery.VerifiedPRContribution, error) {
	if !reflect.DeepEqual(history, probe.view.History) {
		probe.test.Fatal("PR contribution reader did not receive the captured history")
	}
	return probe.prs, probe.read(ctx, repo, "pr")
}

func (probe *contextQueriesProbe) VerifiedGit(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) ([]evidencequery.VerifiedGitContribution, error) {
	if !reflect.DeepEqual(history, probe.view.History) {
		probe.test.Fatal("Git contribution reader did not receive the captured history")
	}
	return probe.git, probe.read(ctx, repo, "git")
}

func (probe *contextQueriesProbe) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, identifier string) (domain.GitCommitTree, error) {
	probe.commitReads = append(probe.commitReads, identifier)
	if origin != probe.evidenceMetadata.GitRemoteURL {
		probe.test.Fatal("Git evidence used a different metadata origin", origin)
	}
	if err := probe.read(ctx, repo, "tree"); err != nil {
		return domain.GitCommitTree{}, err
	}
	commit, found := probe.commits[identifier]
	if !found {
		return domain.GitCommitTree{}, domain.ErrNotFound
	}
	return commit, nil
}

func (probe *contextQueriesProbe) GetGitTreeNode(ctx context.Context, repo domain.ContentHash, origin, identifier string) (domain.GitTreeNode, error) {
	if err := probe.read(ctx, repo, "node"); err != nil {
		return domain.GitTreeNode{}, err
	}
	return domain.GitTreeNode{}, domain.ErrNotFound
}

func (probe *contextQueriesProbe) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, identifier, parent string) (domain.GitCommitDelta, error) {
	if err := probe.read(ctx, repo, "delta"); err != nil {
		return domain.GitCommitDelta{}, err
	}
	return domain.GitCommitDelta{}, domain.ErrNotFound
}

func (probe *contextQueriesProbe) ListPRContributions(ctx context.Context, repo domain.ContentHash) ([]domain.PRContribution, error) {
	return nil, probe.read(ctx, repo, "pr-ledger")
}

func (probe *contextQueriesProbe) ListGitContributions(ctx context.Context, repo domain.ContentHash) ([]domain.GitContribution, error) {
	return nil, probe.read(ctx, repo, "git-ledger")
}

type contextQueriesEvidenceMetadata struct {
	probe *contextQueriesProbe
}

func (metadata *contextQueriesEvidenceMetadata) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return metadata.probe.evidenceMetadata, metadata.probe.read(ctx, repo, "evidence-metadata")
}

func contextQueriesIndex(test *testing.T, words ...string) domain.DocReadIndex {
	test.Helper()
	document := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{SourceProvider: domain.ProviderCodex, SessionOriginID: "context-query-stream"}}}
	for position, word := range words {
		document.CIR.Events = append(document.CIR.Events, domain.CIREvent{Seq: position, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: word}}})
	}
	raw, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	document.Hash = domain.HashContent(raw)
	index, err := domain.BuildDocReadIndex(document)
	if err != nil {
		test.Fatal(err)
	}
	return index
}

func newContextQueriesProbe(test *testing.T) *contextQueriesProbe {
	test.Helper()
	repo := domain.HashContent([]byte(test.Name() + "/repo"))
	root := domain.HashContent([]byte(test.Name() + "/root"))
	tip := domain.HashContent([]byte(test.Name() + "/tip"))
	first := contextQueriesIndex(test, "first")
	second := contextQueriesIndex(test, "first", "second")
	stamp := time.Unix(1234567890, 0).UTC()
	probe := &contextQueriesProbe{
		test: test, repo: repo, pinned: true,
		metadata: domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://example.invalid/context.git"},
		indexes:  map[domain.ContentHash]domain.DocReadIndex{first.Hash: first, second.Hash: second},
		commits:  map[string]domain.GitCommitTree{}, failures: map[string]error{}, calls: map[string]int{},
		generation: domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope,
			RepoID: repo, Epoch: "12345678-abcd-4321-9876-123456789abc", Sequence: 7},
	}
	probe.evidenceMetadata = probe.metadata
	probe.view = domain.RepositoryView{
		Revision: domain.RepositoryRevision{Graph: 8, Pending: 4, Evidence: 2},
		Refs:     []domain.Ref{{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "main-id", Target: tip}},
		Snapshots: []domain.Snapshot{
			{RepoID: repo, ID: root, DocHash: first.Hash, Branch: "main", Provider: domain.ProviderCodex, SessionID: "context-query-stream", CreatedAt: stamp},
			{RepoID: repo, ID: tip, DocHash: second.Hash, Branch: "main", Provider: domain.ProviderCodex, SessionID: "context-query-stream", Parents: []domain.ContentHash{root}, CreatedAt: stamp.Add(time.Second)},
		},
		History: []domain.HistoryEvent{
			{RepoID: string(repo), ID: fmt.Sprintf("%032x", 1), BranchID: "main-id", Branch: "main", Kind: "birth", Source: root, Target: root, CreatedAt: stamp},
			{RepoID: string(repo), ID: fmt.Sprintf("%032x", 2), BranchID: "main-id", Branch: "main", Kind: "position", Source: tip, Target: tip, GitAfter: contextQueriesOID(4), CreatedAt: stamp.Add(time.Second)},
		},
	}
	for number := 1; number <= 4; number++ {
		commit := domain.GitCommitTree{Commit: contextQueriesOID(number), Tree: contextQueriesOID(number + 100), Parents: []string{}}
		if number > 1 {
			commit.Parents = []string{contextQueriesOID(number - 1)}
		}
		probe.commits[commit.Commit] = commit
	}
	test.Cleanup(func() {
		if probe.active {
			test.Fatal("context query leaked its repository read scope")
		}
	})
	return probe
}

func (probe *contextQueriesProbe) config() ContextConfig {
	return ContextConfig{Profile: PinnedSnapshots, Scope: probe, Metadata: probe, History: probe, Revisions: probe,
		Archives: probe, Indexes: probe, CacheScope: probe, Generations: probe, Cache: &SegmentCache{},
		Contributions: probe, EvidenceMetadata: &contextQueriesEvidenceMetadata{probe: probe}, Trees: probe, Deltas: probe, AtomicRead: true}
}

func contextQueriesNew(test *testing.T, config ContextConfig) *ContextQueries {
	test.Helper()
	queries, err := NewContext(config)
	if err != nil {
		test.Fatal(err)
	}
	return queries
}

func contextQueriesOID(number int) string {
	return fmt.Sprintf("%040x", number)
}

func contextQueriesIDs(snapshots []domain.Snapshot) []domain.ContentHash {
	identifiers := make([]domain.ContentHash, 0, len(snapshots))
	for _, snapshot := range snapshots {
		identifiers = append(identifiers, snapshot.ID)
	}
	return identifiers
}

func TestContextQueriesConstructorBindsExplicitPortsWithoutIO(test *testing.T) {
	for _, profile := range []Profile{LegacyCompatibility, PinnedSnapshots} {
		for _, strict := range []bool{false, true} {
			test.Run(fmt.Sprintf("profile=%d/strict=%t", profile, strict), func(test *testing.T) {
				probe := newContextQueriesProbe(test)
				config := probe.config()
				config.Profile, config.RequireComplete, config.Captured = profile, strict, true
				config.Archives, config.Generations, config.Trees, config.Deltas = nil, nil, nil, nil
				queries := contextQueriesNew(test, config)
				if queries.scope != config.Scope || queries.metadata != config.Metadata || queries.indexes != config.Indexes || queries.cache != config.Cache || queries.cacheScope != config.CacheScope || queries.contributions != config.Contributions || queries.evidenceMetadata != config.EvidenceMetadata || queries.requireComplete != strict || !queries.captured || !queries.atomicRead {
					test.Fatal("constructor replaced ports, shared cache, or explicit policy")
				}
				if probe.pins != 0 || len(probe.order) != 0 {
					test.Fatal("constructor performed repository I/O", probe.order)
				}
			})
		}
	}
}

func TestContextQueriesConstructorRejectsMissingAndTypedNilPorts(test *testing.T) {
	required := []string{"Scope", "Metadata", "Indexes", "CacheScope", "Cache", "Contributions", "EvidenceMetadata"}
	optional := []string{"History", "Revisions", "Archives", "Generations", "Trees", "Deltas"}
	for _, name := range append(slices.Clone(required), optional...) {
		for _, typedNil := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/typed-nil=%t", name, typedNil), func(test *testing.T) {
				probe := newContextQueriesProbe(test)
				config := probe.config()
				config.Profile, config.AtomicRead = LegacyCompatibility, false
				field := reflect.ValueOf(&config).Elem().FieldByName(name)
				field.SetZero()
				if typedNil && field.Kind() == reflect.Interface {
					var missing *contextQueriesProbe
					field.Set(reflect.ValueOf(missing))
				}
				queries, err := NewContext(config)
				if slices.Contains(optional, name) && !typedNil {
					if err != nil || queries == nil {
						test.Fatal("omitted compatibility capability was rejected", err)
					}
				} else if queries != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
					test.Fatal("invalid port declaration created a context query", queries, err)
				}
				if probe.pins != 0 || len(probe.order) != 0 {
					test.Fatal("constructor attempted to probe a missing capability", probe.order)
				}
			})
		}
	}
	for _, scenario := range []string{"missing profile", "unknown profile", "pinned without history", "pinned without revision", "pinned without atomic read"} {
		test.Run(scenario, func(test *testing.T) {
			probe := newContextQueriesProbe(test)
			config := probe.config()
			want := domain.ErrCatalogUnsupported
			switch scenario {
			case "missing profile":
				config.Profile, want = 0, domain.ErrValidation
			case "unknown profile":
				config.Profile, want = Profile(255), domain.ErrValidation
			case "pinned without history":
				config.History = nil
			case "pinned without revision":
				config.Revisions = nil
			case "pinned without atomic read":
				config.AtomicRead = false
			}
			queries, err := NewContext(config)
			if queries != nil || !errors.Is(err, want) || probe.pins != 0 || len(probe.order) != 0 {
				test.Fatal("invalid profile assembly was accepted or performed I/O", queries, err, probe.order)
			}
		})
	}
}

func TestContextQueriesValidateBeforeEnteringReadScope(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		selection domain.ContextSelection
		badRepo   bool
		want      error
	}{
		{name: "repository", badRepo: true, want: domain.ErrIntegrity},
		{name: "code", selection: domain.ContextSelection{CodeCommit: "not-an-oid"}, want: domain.ErrValidation},
		{name: "negative limit", selection: domain.ContextSelection{SegmentLimit: -1}, want: domain.ErrValidation},
		{name: "limit bound", selection: domain.ContextSelection{SegmentLimit: domain.MaxContextSegmentPage + 1}, want: domain.ErrValidation},
		{name: "negative offset", selection: domain.ContextSelection{SegmentLimit: 1, SegmentOffset: -1}, want: domain.ErrValidation},
		{name: "unpaged offset", selection: domain.ContextSelection{SegmentOffset: 1}, want: domain.ErrValidation},
		{name: "unfenced continuation", selection: domain.ContextSelection{SegmentLimit: 1, SegmentOffset: 1}, want: domain.ErrValidation},
		{name: "invalid fence", selection: domain.ContextSelection{SegmentLimit: 1, SegmentStateHash: "wrong"}, want: domain.ErrIntegrity},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			probe := newContextQueriesProbe(test)
			queries := contextQueriesNew(test, probe.config())
			repo := probe.repo
			if scenario.badRepo {
				repo = "invalid"
			}
			result, err := queries.QueryContext(context.Background(), repo, scenario.selection)
			if !errors.Is(err, scenario.want) || !reflect.DeepEqual(result, domain.ContextQueryView{}) || probe.pins != 0 || len(probe.order) != 0 {
				test.Fatal("invalid context request reached scoped I/O", result, err, probe.order)
			}
		})
	}
	probe := newContextQueriesProbe(test)
	probe.scopeErr = domain.ErrForbidden
	queries := contextQueriesNew(test, probe.config())
	result, err := queries.QueryContext(context.Background(), probe.repo, domain.ContextSelection{})
	if !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(result, domain.ContextQueryView{}) || len(probe.order) != 0 {
		test.Fatal("context query bypassed its read authority", result, err, probe.order)
	}
}

func TestContextQueriesSelectMetadataAndRevisionInsideOneScope(test *testing.T) {
	probe := newContextQueriesProbe(test)
	probe.metadata.DefaultBranch = "trunk"
	probe.view.Refs[0].Name = "trunk"
	probe.view.Refs = append(probe.view.Refs, domain.Ref{RepoID: probe.repo, Kind: domain.RefBranch, Name: "main", Target: probe.view.Snapshots[0].ID})
	queries := contextQueriesNew(test, probe.config())
	result, err := queries.QueryContext(context.Background(), probe.repo, domain.ContextSelection{Scope: "all", Position: "HEAD"})
	if err != nil || result.Position != probe.view.Snapshots[1].ID || result.Revision != probe.view.Revision || !slices.Equal(contextQueriesIDs(result.Snapshots), []domain.ContentHash{probe.view.Snapshots[1].ID, probe.view.Snapshots[0].ID}) || !reflect.DeepEqual(result.History, probe.view.History) {
		test.Fatal("context selection lost its default branch or captured repository revision", result, err)
	}
	if probe.pins != 1 || probe.releases != 1 || probe.calls["repo"] == 0 || probe.calls["revision"] == 0 || probe.calls["snapshots"] != 1 || probe.calls["reflog"] != 1 || probe.calls["history"] != 1 || probe.calls["index"] != 0 || probe.calls["generation"] != 0 || probe.calls["pr"] != 0 || probe.calls["git"] != 0 || probe.calls["tree"] != 0 {
		test.Fatal("unpaged selection performed nested reads, document reads, or Git projection", probe.order)
	}
	if result.Inclusion != nil || result.Segments != nil || domain.ValidateContentHash(result.StateHash) != nil || domain.ValidateContentHash(result.DeliveryStateHash) != nil {
		test.Fatal("unpaged selection omitted delivery proof or invented paging/inclusion", result)
	}
}

func TestContextQueriesRejectCrossRepositoryMetadataAtomically(test *testing.T) {
	for _, source := range []string{"repo", "ref", "snapshot", "history", "pending", "unsync", "archive", "evidence metadata"} {
		test.Run(source, func(test *testing.T) {
			probe := newContextQueriesProbe(test)
			foreign := domain.HashContent([]byte("foreign context repository"))
			selection := domain.ContextSelection{Scope: "all"}
			switch source {
			case "repo":
				probe.metadata.ID = foreign
			case "ref":
				probe.view.Refs[0].RepoID = foreign
			case "snapshot":
				probe.view.Snapshots[0].RepoID = foreign
			case "history":
				probe.view.History[0].RepoID = string(foreign)
			case "pending":
				probe.view.Pending = []domain.Pending{{RepoID: foreign, Target: probe.view.Snapshots[0].ID}}
			case "unsync":
				probe.view.Unsync = []domain.Unsync{{RepoID: foreign}}
			case "archive":
				probe.archives = []domain.SessionArchive{{RepoID: foreign, SnapshotID: probe.view.Snapshots[0].ID}}
			case "evidence metadata":
				probe.evidenceMetadata.ID = foreign
				selection = domain.ContextSelection{Scope: "current", Position: "HEAD"}
			}
			queries := contextQueriesNew(test, probe.config())
			result, err := queries.QueryContext(context.Background(), probe.repo, selection)
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(result, domain.ContextQueryView{}) || probe.pins != 1 || probe.releases != 1 || probe.calls["pr"] != 0 || probe.calls["git"] != 0 || probe.calls["tree"] != 0 || probe.calls["index"] != 0 {
				test.Fatal("foreign repository metadata reached projection or escaped atomic read", result, err, probe.order)
			}
		})
	}
}

func TestContextQueriesScopeFinalizationRespectsAtomicPolicy(test *testing.T) {
	for _, atomic := range []bool{false, true} {
		test.Run(fmt.Sprintf("atomic=%t", atomic), func(test *testing.T) {
			probe := newContextQueriesProbe(test)
			config := probe.config()
			config.Profile, config.AtomicRead = LegacyCompatibility, atomic
			queries := contextQueriesNew(test, config)
			baseline, err := queries.QueryContext(context.Background(), probe.repo, domain.ContextSelection{})
			if err != nil {
				test.Fatal(err)
			}
			failure := errors.New("context read finalization failed")
			probe.finishErr = failure
			result, err := queries.QueryContext(context.Background(), probe.repo, domain.ContextSelection{})
			want := baseline
			if atomic {
				want = domain.ContextQueryView{}
			}
			if !errors.Is(err, failure) || !reflect.DeepEqual(result, want) || probe.pins != 2 || probe.releases != 2 {
				test.Fatal("scope finalization changed explicit compatibility/atomic result semantics", result, err)
			}
		})
	}
}

func TestContextQueriesCompatibilityOmitsOptionalReadersWithoutChangingHistoryErrors(test *testing.T) {
	for _, historyAvailable := range []bool{false, true} {
		test.Run(fmt.Sprintf("history=%t", historyAvailable), func(test *testing.T) {
			probe := newContextQueriesProbe(test)
			config := probe.config()
			config.Profile, config.AtomicRead = LegacyCompatibility, false
			config.Revisions, config.Archives, config.Generations, config.Trees, config.Deltas = nil, nil, nil, nil, nil
			if !historyAvailable {
				config.History = nil
			}
			queries := contextQueriesNew(test, config)
			result, err := queries.QueryContext(context.Background(), probe.repo, domain.ContextSelection{})
			if !historyAvailable {
				if err == nil || err.Error() != "context history storage unavailable" || !reflect.DeepEqual(result, domain.ContextQueryView{}) {
					test.Fatal("omitted history changed the established unavailable-storage error", result, err)
				}
				return
			}
			if err != nil || result.Revision != (domain.RepositoryRevision{}) || len(result.Snapshots) != 2 || len(result.History) != len(probe.view.History) || probe.calls["revision"] != 0 || probe.calls["archives"] != 0 || probe.calls["generation"] != 0 {
				test.Fatal("compatibility context query required absent optional readers", result, err, probe.order)
			}
		})
	}
}

func TestContextQueriesCompatibilityKeepsSelectedResultOnProjectionError(test *testing.T) {
	for _, phase := range []string{"tree", "pr", "git", "index"} {
		test.Run(phase, func(test *testing.T) {
			probe := newContextQueriesProbe(test)
			config := probe.config()
			config.Profile, config.AtomicRead = LegacyCompatibility, false
			queries := contextQueriesNew(test, config)
			failure := errors.New("selected projection failed")
			probe.failures[phase] = failure
			selection := domain.ContextSelection{Scope: "current", Position: "main"}
			if phase == "index" {
				selection = domain.ContextSelection{Scope: "all", SegmentLimit: 1}
			}
			result, err := queries.QueryContext(context.Background(), probe.repo, selection)
			if !errors.Is(err, failure) || result.Version != 1 || result.Revision != probe.view.Revision || len(result.Snapshots) != 2 || len(result.History) != len(probe.view.History) || result.Inclusion != nil || result.Segments != nil {
				test.Fatal("compatibility projection error discarded its selected view or published an incomplete page", result, err)
			}
			if phase != "index" && (result.Position != probe.view.Snapshots[1].ID || result.DeliveryStateHash != "") {
				test.Fatal("failed branch projection changed position or fabricated delivery proof", result)
			}
		})
	}
}

func contextQueriesAddContributions(probe *contextQueriesProbe) (domain.ContentHash, domain.ContentHash) {
	prCapture := domain.HashContent([]byte("context PR capture"))
	gitCapture := domain.HashContent([]byte("context Git capture"))
	root := probe.view.Snapshots[0]
	probe.view.Snapshots = append(probe.view.Snapshots,
		domain.Snapshot{RepoID: probe.repo, ID: prCapture, DocHash: root.DocHash, Parents: []domain.ContentHash{root.ID}, CreatedAt: root.CreatedAt.Add(20 * time.Second)},
		domain.Snapshot{RepoID: probe.repo, ID: gitCapture, DocHash: root.DocHash, Parents: []domain.ContentHash{root.ID}, CreatedAt: root.CreatedAt.Add(10 * time.Second)},
	)
	probe.view.Snapshots[1].GraftParents = []domain.ContentHash{gitCapture}
	probe.prs = []evidencequery.VerifiedPRContribution{{Record: domain.PRContribution{
		ID: "context-pr", RepoID: probe.repo, DestinationBranchID: "main-id", Capture: prCapture,
		PR: domain.PullRequestMerge{Number: 17, BaseBranch: "main", HeadBranch: "feature-pr", MergeSHA: contextQueriesOID(2)},
	}, Valid: true}}
	probe.git = []evidencequery.VerifiedGitContribution{{Record: domain.GitContribution{
		ID: "context-git", RepoID: probe.repo, DestinationBranchID: "main-id", Capture: gitCapture, IntegrationSHA: contextQueriesOID(3),
	}, Valid: true, SourceBranch: "feature-git", DestinationBranch: "main"}}
	return prCapture, gitCapture
}

func TestContextQueriesBranchCurrentUsesCodeEvidenceAndVerifiedContributions(test *testing.T) {
	for _, scenario := range []string{"resolved HEAD", "explicit earlier code", "derived branch identity"} {
		test.Run(scenario, func(test *testing.T) {
			probe := newContextQueriesProbe(test)
			prCapture, gitCapture := contextQueriesAddContributions(probe)
			selection := domain.ContextSelection{Scope: "current", Position: "HEAD"}
			code := contextQueriesOID(4)
			want := []domain.ContentHash{probe.view.Snapshots[1].ID, gitCapture, prCapture, probe.view.Snapshots[0].ID}
			if scenario == "explicit earlier code" {
				code, selection.CodeCommit = contextQueriesOID(2), contextQueriesOID(2)
				want = []domain.ContentHash{probe.view.Snapshots[1].ID, prCapture, probe.view.Snapshots[0].ID}
			}
			if scenario == "derived branch identity" {
				probe.view.Refs[0].BranchID = ""
			}
			queries := contextQueriesNew(test, probe.config())
			result, err := queries.QueryContext(context.Background(), probe.repo, selection)
			if err != nil || result.Inclusion == nil {
				test.Fatal("branch-current query did not finish the real code-evidence plan", result, err)
			}
			if result.Branch != "main" || result.Position != probe.view.Snapshots[1].ID || result.Inclusion.BranchID != "main-id" || result.Inclusion.CodeCommit != code || !slices.Equal(contextQueriesIDs(result.Snapshots), want) || !slices.Equal(result.Inclusion.SnapshotIDs, want) {
				test.Fatal("branch-current selection substituted timestamps, names, or mutable grafts for selected code", result)
			}
			if len(result.Inclusion.Merges) != 2 || probe.calls["pr"] != 1 || probe.calls["git"] != 1 || probe.calls["evidence-metadata"] != 1 || probe.calls["tree"] == 0 || probe.calls["index"] != 0 || probe.pins != 1 || probe.releases != 1 {
				test.Fatal("branch-current projection bypassed narrow evidence ports", result.Inclusion, probe.order)
			}
			for _, merge := range result.Inclusion.Merges {
				state, reason := "included", "verified_git_order"
				if scenario == "explicit earlier code" && merge.Source == gitCapture {
					state, reason = "not_selected", "merge_not_in_selected_code"
				}
				if merge.State != state || merge.Reason != reason || !merge.DeliveryAcknowledged {
					test.Fatal("verified participation overrode code selection or lost acknowledgment", merge)
				}
			}
			if domain.ValidateContentHash(result.StateHash) != nil || domain.ValidateContentHash(result.DeliveryStateHash) != nil || slices.Index(probe.order, "tree") > slices.Index(probe.order, "pr") {
				test.Fatal("contributions ran before branch preparation or omitted delivery proof", probe.order, result)
			}
		})
	}
}

func contextQueriesRealEvidence(test *testing.T, probe *contextQueriesProbe) ContributionEvidence {
	test.Helper()
	queries, err := evidencequery.New(evidencequery.Config{Profile: evidencequery.Compatibility,
		Metadata: &contextQueriesEvidenceMetadata{probe: probe}, PRLedger: probe, GitLedger: probe})
	if err != nil {
		test.Fatal(err)
	}
	return queries
}

func TestContextQueriesDoNotReadLedgersUntilBranchPlanIsValid(test *testing.T) {
	for _, scenario := range []string{"Git read failure", "cyclic Git", "valid plan"} {
		test.Run(scenario, func(test *testing.T) {
			probe := newContextQueriesProbe(test)
			config := probe.config()
			config.Contributions = contextQueriesRealEvidence(test, probe)
			var want error
			switch scenario {
			case "Git read failure":
				want = errors.New("cannot prepare selected code")
				probe.failures["tree"] = want
			case "cyclic Git":
				want = domain.ErrIntegrity
				commit := probe.commits[contextQueriesOID(4)]
				commit.Parents = []string{commit.Commit}
				probe.commits[commit.Commit] = commit
			}
			queries := contextQueriesNew(test, config)
			result, err := queries.QueryContext(context.Background(), probe.repo, domain.ContextSelection{Scope: "current", Position: "main"})
			if want != nil {
				if !errors.Is(err, want) || !reflect.DeepEqual(result, domain.ContextQueryView{}) || probe.calls["pr-ledger"] != 0 || probe.calls["git-ledger"] != 0 {
					test.Fatal("invalid branch plan reached a contribution ledger", result, err, probe.order)
				}
				return
			}
			if err != nil || result.Inclusion == nil || result.Inclusion.CodeCommit != contextQueriesOID(4) || probe.calls["pr-ledger"] != 1 || probe.calls["git-ledger"] != 1 || slices.Index(probe.order, "git-ledger") < slices.Index(probe.order, "pr-ledger") {
				test.Fatal("valid plan did not prepare before querying the real evidence owner", result, err, probe.order)
			}
			for position, operation := range probe.order {
				if operation == "tree" && position > slices.Index(probe.order, "pr-ledger") {
					test.Fatal("contribution ledger was read before Git plan preparation finished", probe.order)
				}
			}
		})
	}
}

func TestContextQueriesHistoryAndDetachedSelectionsAvoidGitLedgerIO(test *testing.T) {
	for _, scenario := range []string{"all", "previous", "archived", "detached", "tag"} {
		test.Run(scenario, func(test *testing.T) {
			probe := newContextQueriesProbe(test)
			config := probe.config()
			config.Contributions = contextQueriesRealEvidence(test, probe)
			for _, operation := range []string{"pr-ledger", "git-ledger", "tree", "node", "delta", "evidence-metadata"} {
				probe.failures[operation] = errors.New("unnecessary Git I/O")
			}
			selection := domain.ContextSelection{Scope: scenario}
			switch scenario {
			case "previous":
				selection.Position = string(probe.view.Snapshots[0].ID)
			case "detached":
				selection.Scope, selection.Position = "current", string(probe.view.Snapshots[1].ID)
			case "tag":
				selection.Scope, selection.Position = "current", "release"
				probe.view.Refs = append(probe.view.Refs, domain.Ref{RepoID: probe.repo, Kind: domain.RefTag, Name: "release", Target: probe.view.Snapshots[1].ID})
			}
			queries := contextQueriesNew(test, config)
			result, err := queries.QueryContext(context.Background(), probe.repo, selection)
			if err != nil || result.Inclusion != nil || domain.ValidateContentHash(result.DeliveryStateHash) != nil {
				test.Fatal("history-only selection required branch code evidence", result, err, probe.order)
			}
			for operation := range probe.failures {
				if probe.calls[operation] != 0 {
					test.Fatal("non-branch-current selection read an unnecessary Git port", operation, probe.order)
				}
			}
		})
	}
}

func TestContextQueriesSegmentCacheRechecksCurrentProofBeforeServingHit(test *testing.T) {
	for _, phase := range []string{"repo", "revision", "generation", "foreign generation", "scope finalization"} {
		test.Run(phase, func(test *testing.T) {
			probe := newContextQueriesProbe(test)
			queries := contextQueriesNew(test, probe.config())
			selection := domain.ContextSelection{Scope: "all", SegmentLimit: 1}
			first, err := queries.QueryContext(context.Background(), probe.repo, selection)
			if err != nil || first.Segments == nil {
				test.Fatal("could not warm the direct context segment cache", first, err)
			}
			catalogReads := probe.calls["snapshots"]
			hit, err := queries.QueryContext(context.Background(), probe.repo, selection)
			if err != nil || !reflect.DeepEqual(hit, first) || probe.calls["snapshots"] != catalogReads || probe.calls["generation"] < 2 {
				test.Fatal("pinned segment projection did not reuse its source generation", hit, err, probe.order)
			}
			failure := errors.New("current context read proof failed")
			indexReads := probe.calls["index"]
			switch phase {
			case "foreign generation":
				probe.generation.RepoID = domain.HashContent([]byte("foreign source checkpoint"))
				failure = domain.ErrIntegrity
			case "scope finalization":
				probe.finishErr = failure
			default:
				probe.failures[phase] = failure
			}
			result, err := queries.QueryContext(context.Background(), probe.repo, selection)
			if !errors.Is(err, failure) || !reflect.DeepEqual(result, domain.ContextQueryView{}) || probe.pins != 3 || probe.releases != 3 {
				test.Fatal("cached projection bypassed current proof or published after scope failure", result, err, probe.order)
			}
			if phase != "scope finalization" && probe.calls["index"] != indexReads {
				test.Fatal("failed current proof reached document I/O", probe.order)
			}
		})
	}
}

func TestContextQueriesSourceGenerationInvalidatesCachedContinuation(test *testing.T) {
	probe := newContextQueriesProbe(test)
	queries := contextQueriesNew(test, probe.config())
	selection := domain.ContextSelection{Scope: "all", SegmentLimit: 1}
	first, err := queries.QueryContext(context.Background(), probe.repo, selection)
	if err != nil || first.Segments == nil {
		test.Fatal(first, err)
	}
	reads := probe.calls["snapshots"]
	changed := contextQueriesIndex(test, "different immutable source")
	probe.indexes[changed.Hash] = changed
	probe.view.Snapshots[1].DocHash = changed.Hash
	probe.generation.Sequence++
	selection.SegmentOffset, selection.SegmentStateHash = first.Segments.Next, first.StateHash
	result, err := queries.QueryContext(context.Background(), probe.repo, selection)
	if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(result, domain.ContextQueryView{}) || probe.calls["snapshots"] != reads+1 {
		test.Fatal("unchanged repository revision hid a changed source generation", result, err, probe.order)
	}
	fresh, err := queries.QueryContext(context.Background(), probe.repo, domain.ContextSelection{Scope: "all", SegmentLimit: 1})
	if err != nil || fresh.Segments == nil || fresh.StateHash == first.StateHash || fresh.DeliveryStateHash == first.DeliveryStateHash || fresh.Snapshots[0].DocHash != changed.Hash || fresh.Revision != first.Revision {
		test.Fatal("fresh page reused an obsolete immutable source under the same revision", fresh, err)
	}
}

func TestContextQueriesUnpinnedAndCapturedReadsDoNotUseGenerationCache(test *testing.T) {
	for _, scenario := range []string{"unpinned", "captured", "generation unavailable"} {
		test.Run(scenario, func(test *testing.T) {
			probe := newContextQueriesProbe(test)
			config := probe.config()
			switch scenario {
			case "unpinned":
				probe.pinned = false
			case "captured":
				config.Captured = true
			case "generation unavailable":
				probe.failures["generation"] = domain.ErrCatalogUnsupported
			}
			queries := contextQueriesNew(test, config)
			selection := domain.ContextSelection{Scope: "all", SegmentLimit: 1}
			first, err := queries.QueryContext(context.Background(), probe.repo, selection)
			if err != nil {
				test.Fatal(err)
			}
			second, err := queries.QueryContext(context.Background(), probe.repo, selection)
			if err != nil || !reflect.DeepEqual(first, second) || probe.calls["snapshots"] != 2 {
				test.Fatal("unqualified source generation reused a segment projection", second, err, probe.order)
			}
			if scenario != "generation unavailable" && probe.calls["generation"] != 0 {
				test.Fatal("unpinned or captured read consulted a live source generation", probe.order)
			}
		})
	}
}

func TestContextQueriesUnpagedAndSegmentDeliveryStayStableAcrossContinuation(test *testing.T) {
	probe := newContextQueriesProbe(test)
	queries := contextQueriesNew(test, probe.config())
	full, err := queries.QueryContext(context.Background(), probe.repo, domain.ContextSelection{Scope: "all"})
	if err != nil || full.Segments != nil || len(full.Snapshots) != 2 || len(full.History) != len(probe.view.History) || probe.calls["index"] != 0 {
		test.Fatal("ordinary context query read indexes or dropped full metadata", full, err, probe.order)
	}
	selection := domain.ContextSelection{Scope: "all", SegmentLimit: 1}
	first, err := queries.QueryContext(context.Background(), probe.repo, selection)
	if err != nil || first.Segments == nil {
		test.Fatal("first direct segment page failed", first, err)
	}
	page := first.Segments
	if page.Offset != 0 || page.Next != 1 || page.Total != 2 || page.Complete || len(page.Entries) != 1 || page.Entries[0].SnapshotID != full.Snapshots[0].ID || page.Entries[0].Kind != "full_source" || page.Entries[0].Segment == nil || page.Entries[0].Segment.DocHash != full.Snapshots[0].DocHash || len(page.Entries[0].Bindings) != 0 {
		test.Fatal("first page invented a binding or changed full-selection ordering", page)
	}
	if first.DeliveryStateHash != full.DeliveryStateHash || page.StateHash != first.StateHash || domain.ValidateContentHash(page.PageHash) != nil || len(first.Snapshots) != 1 || len(first.History) != 0 || first.Inclusion != nil || !reflect.DeepEqual(first.Semantics, domain.ContextSemantics{}) || !slices.Equal(probe.indexReads, []domain.ContentHash{full.Snapshots[0].DocHash}) {
		test.Fatal("segment page changed delivery proof, leaked full metadata, or read off-page documents", first, probe.indexReads)
	}
	selection.SegmentOffset, selection.SegmentStateHash = page.Next, page.StateHash
	last, err := queries.QueryContext(context.Background(), probe.repo, selection)
	if err != nil || last.Segments == nil {
		test.Fatal("direct segment continuation failed", last, err)
	}
	if !last.Segments.Complete || last.Segments.Next != -1 || last.Segments.Offset != 1 || last.Segments.Total != 2 || last.StateHash != first.StateHash || last.DeliveryStateHash != full.DeliveryStateHash || last.Segments.PageHash == page.PageHash || len(last.Snapshots) != 1 || last.Snapshots[0].ID != full.Snapshots[1].ID || len(last.History) != 0 || len(last.Segments.Entries) != 1 || last.Segments.Entries[0].SnapshotID != full.Snapshots[1].ID {
		test.Fatal("continuation lost stable delivery identity or page-local ordering", last)
	}
	if probe.calls["snapshots"] != 2 || !slices.Equal(probe.indexReads, []domain.ContentHash{full.Snapshots[0].DocHash, full.Snapshots[1].DocHash}) {
		test.Fatal("continuation rescanned catalog metadata or unrelated indexes", probe.order, probe.indexReads)
	}
	selection.SegmentStateHash = domain.HashContent([]byte("stale context cursor"))
	stale, err := queries.QueryContext(context.Background(), probe.repo, selection)
	if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(stale, domain.ContextQueryView{}) {
		test.Fatal("cached continuation accepted an unrelated state fence", stale, err)
	}
}
