package repositoryquery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func capturedQueryJSON(test *testing.T, value any) json.RawMessage {
	test.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func capturedQueryInputs(test *testing.T, protocol int) (domain.RepositoryViewSourceCheckpoint, []domain.RepositoryViewSourceEntry) {
	test.Helper()
	repo := domain.HashContent([]byte(test.Name() + "/repo"))
	root, tip := domain.HashContent([]byte("captured-root")), domain.HashContent([]byte("captured-tip"))
	checkpoint := domain.RepositoryViewSourceCheckpoint{
		Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope,
		RepoID: repo, Epoch: "12345678-abcd-4321-9876-123456789abc", Sequence: 7,
	}
	stamp := time.Unix(1234567890, 0).UTC()
	entries := []domain.RepositoryViewSourceEntry{
		{Sequence: 3, Kind: "git_tree", Key: strings.Repeat("a", 40), Value: json.RawMessage(`{"tree":{}}`)},
		{Sequence: 1, Kind: "protocol", Key: string(repo), Value: capturedQueryJSON(test, map[string]int{"context_protocol": protocol})},
		{Sequence: 1, Kind: "repository", Key: string(repo), Value: capturedQueryJSON(test, map[string]any{
			"id": repo, "context_protocol": protocol, "default_branch": "main", "git_remote_url": "https://example.invalid/captured.git",
			"remote_url_supported": true, "git_remote_url_supported": true,
		})},
		{Sequence: 2, Kind: "revision", Key: string(repo), Value: capturedQueryJSON(test, domain.RepositoryRevision{Graph: 3, Pending: 2, Evidence: 1})},
		{Sequence: 2, Kind: "snapshot", Key: string(root), Value: capturedQueryJSON(test, domain.Snapshot{
			RepoID: repo, ID: root, DocHash: root, Branch: "main", Branches: []string{"untrusted"}, CreatedAt: stamp,
		})},
		{Sequence: 3, Kind: "snapshot", Key: string(tip), Value: capturedQueryJSON(test, domain.Snapshot{
			RepoID: repo, ID: tip, DocHash: tip, Branch: "topic", Branches: []string{"untrusted"}, Parents: []domain.ContentHash{root}, CreatedAt: stamp.Add(time.Second),
		})},
		{Sequence: 3, Kind: "ref", Key: `["branch","main"]`, Value: capturedQueryJSON(test, domain.Ref{
			RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "main-id", Target: tip,
		})},
		{Sequence: 2, Kind: "history", Key: fmt.Sprintf("%032x", 1), Value: capturedQueryJSON(test, domain.HistoryEvent{
			ID: fmt.Sprintf("%032x", 1), RepoID: string(repo), Kind: "birth", Branch: "main", BranchID: "main-id", Source: root, Target: root, CreatedAt: stamp,
		})},
		{Sequence: 3, Kind: "history", Key: fmt.Sprintf("%032x", 2), Value: capturedQueryJSON(test, domain.HistoryEvent{
			ID: fmt.Sprintf("%032x", 2), RepoID: string(repo), Kind: "position", Branch: "main", BranchID: "main-id", Target: tip, CreatedAt: stamp.Add(time.Second),
		})},
	}
	for index, log := range []domain.RefLogEntry{
		{Kind: domain.RefBranch, Name: "zeta", Old: root, New: tip, CreatedAt: stamp},
		{Kind: domain.RefBranch, Name: "alpha", New: tip, CreatedAt: stamp},
		{Kind: domain.RefBranch, Name: "HEAD", New: tip, CreatedAt: stamp},
		{Kind: domain.RefTag, Name: "not-membership", New: tip, CreatedAt: stamp},
	} {
		entries = append(entries, domain.RepositoryViewSourceEntry{Sequence: 3, Kind: "reflog", Key: fmt.Sprint(index + 1), Value: capturedQueryJSON(test, log)})
	}
	return checkpoint, entries
}

func TestSplitCapturedGraphInputsPreservesClassificationOrderAndOwnership(test *testing.T) {
	checkpoint, _ := capturedQueryInputs(test, 1)
	var entries, wantMetadata, wantEvidence []domain.RepositoryViewSourceEntry
	for _, kind := range []string{
		"snapshot", "ref", "history", "protocol", "repository", "pending", "unsync", "session_archive", "reflog", "revision",
		"git_commit", "git_tree", "git_delta", "git_observation", "pr_checkpoint", "pr_participation", "pr_contribution",
		"git_contribution", "git_receipt", "git_admission", "git_context_head", "git_contribution_proof", "unknown",
	} {
		entry := domain.RepositoryViewSourceEntry{Sequence: -1, Kind: kind, Key: kind, Deleted: true, Value: json.RawMessage(`{"image":1}`)}
		entries = append(entries, entry)
		if domain.RepositoryViewSourceKindValid(kind) {
			wantMetadata = append(wantMetadata, entry)
		} else {
			wantEvidence = append(wantEvidence, entry)
		}
	}
	metadata, evidence, err := SplitCapturedGraphInputs(context.Background(), checkpoint, entries)
	if err != nil || !reflect.DeepEqual(metadata, wantMetadata) || !reflect.DeepEqual(evidence, wantEvidence) {
		test.Fatal("split changed source classification, ordering, or entry fields", err)
	}
	if &metadata[0].Value[0] != &entries[0].Value[0] {
		test.Fatal("split copied immutable metadata images")
	}
	for index := range evidence {
		if &evidence[index].Value[0] == &wantEvidence[index].Value[0] {
			test.Fatal("split retained an evidence image", index)
		}
		evidence[index].Value[0] = '['
	}
	metadata[0].Key = "caller metadata"
	evidence[0].Key = "caller evidence"
	if entries[0].Key != "snapshot" || wantEvidence[0].Key != "git_commit" {
		test.Fatal("split retained caller entry slices")
	}
	_, fresh, err := SplitCapturedGraphInputs(context.Background(), checkpoint, entries)
	if err != nil || !reflect.DeepEqual(fresh, wantEvidence) {
		test.Fatal("caller evidence mutation changed later inputs", err)
	}
}

func TestDecodeCapturedGraphInputsMatchesNativeDecoderAndOwnsResults(test *testing.T) {
	for _, protocol := range []int{0, 1} {
		test.Run(fmt.Sprint(protocol), func(test *testing.T) {
			checkpoint, entries := capturedQueryInputs(test, protocol)
			before := capturedQueryJSON(test, entries)
			wantRepo, wantView, err := domain.DecodeRepositoryViewSource(context.Background(), checkpoint.RepoID, entries[1:])
			if err != nil {
				test.Fatal(err)
			}
			repo, view, evidence, err := DecodeCapturedGraphInputs(context.Background(), checkpoint, entries)
			if err != nil || !reflect.DeepEqual(repo, wantRepo) || !reflect.DeepEqual(view, wantView) || !reflect.DeepEqual(evidence, entries[:1]) {
				test.Fatal("captured decoder changed native metadata or evidence", err)
			}
			view.Snapshots[0].Branches[0] = "caller branch"
			evidence[0].Value[0] = '['
			_, fresh, freshEvidence, err := DecodeCapturedGraphInputs(context.Background(), checkpoint, entries)
			if err != nil || !reflect.DeepEqual(fresh, wantView) || !reflect.DeepEqual(freshEvidence, entries[:1]) || !bytes.Equal(before, capturedQueryJSON(test, entries)) {
				test.Fatal("captured decoder retained caller output", err)
			}
		})
	}
}

func TestCapturedGraphInputValidationAndCancellationOrder(test *testing.T) {
	checkpoint, entries := capturedQueryInputs(test, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, target := range []string{"version", "scope", "repository", "epoch", "sequence"} {
		test.Run(target, func(test *testing.T) {
			invalid := checkpoint
			switch target {
			case "version":
				invalid.Version++
			case "scope":
				invalid.Scope = domain.RepositoryViewSourceScope
			case "repository":
				invalid.RepoID = "invalid"
			case "epoch":
				invalid.Epoch = ""
			case "sequence":
				invalid.Sequence = -1
			}
			metadata, evidence, err := SplitCapturedGraphInputs(ctx, invalid, entries)
			if !errors.Is(err, domain.ErrIntegrity) || metadata != nil || evidence != nil {
				test.Fatal("split changed checkpoint validation precedence", err)
			}
			repo, view, evidence, err := DecodeCapturedGraphInputs(ctx, invalid, entries)
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(repo, domain.Repo{}) || !reflect.DeepEqual(view, domain.RepositoryView{}) || evidence != nil {
				test.Fatal("decoder exposed inputs from an invalid checkpoint", err)
			}
		})
	}
	metadata, evidence, err := SplitCapturedGraphInputs(ctx, checkpoint, entries)
	if !errors.Is(err, context.Canceled) || metadata != nil || evidence != nil {
		test.Fatal("canceled split exposed partial inputs", err)
	}
	metadata, evidence, err = SplitCapturedGraphInputs(ctx, checkpoint, nil)
	if err != nil || metadata != nil || evidence != nil {
		test.Fatal("empty split changed its original context-check boundary", err)
	}
	repo, view, evidence, err := DecodeCapturedGraphInputs(ctx, checkpoint, nil)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(repo, domain.Repo{}) || !reflect.DeepEqual(view, domain.RepositoryView{}) || evidence != nil {
		test.Fatal("native decoder lost cancellation after an empty split", err)
	}
}

func TestDecodeCapturedGraphInputsPreservesDecoderErrorsAndLimits(test *testing.T) {
	for _, scenario := range []string{"missing", "duplicate", "foreign", "unsupported origin", "malformed", "rows", "bytes"} {
		test.Run(scenario, func(test *testing.T) {
			checkpoint, entries := capturedQueryInputs(test, 1)
			want := domain.ErrIntegrity
			switch scenario {
			case "missing":
				entries = entries[:1]
			case "duplicate":
				entries = append(entries, entries[1])
			case "foreign":
				checkpoint.RepoID = domain.HashContent([]byte("foreign"))
			case "unsupported origin":
				entries[2].Value = capturedQueryJSON(test, domain.Repo{ID: checkpoint.RepoID, ContextProtocol: 1, DefaultBranch: "main"})
			case "malformed":
				entries[2].Value = json.RawMessage(`{"id":`)
			case "rows":
				entries = make([]domain.RepositoryViewSourceEntry, domain.MaxPendingProjectionInputRows+1)
				for index := range entries {
					entries[index].Kind = "repository"
				}
				want = domain.ErrRepositoryViewSourceLimit
			case "bytes":
				entries[2].Value = json.RawMessage(strings.Repeat(" ", domain.MaxPendingProjectionInputBytes+1))
				want = domain.ErrRepositoryViewSourceLimit
			}
			repo, view, evidence, err := DecodeCapturedGraphInputs(context.Background(), checkpoint, entries)
			if !errors.Is(err, want) || !reflect.DeepEqual(repo, domain.Repo{}) || !reflect.DeepEqual(view, domain.RepositoryView{}) || evidence != nil {
				test.Fatal("decoder changed validation or returned partial inputs", err)
			}
		})
	}
}

type capturedQueryEvidence struct {
	test     *testing.T
	repo     domain.ContentHash
	metadata domain.Repo
	calls    []string
	contexts []context.Context
	errors   map[string]error
	cancel   context.CancelFunc
	cancelOn string
}

var _ outbound.GraphEvidenceReader = (*capturedQueryEvidence)(nil)
var _ CapturedEvidenceReader = (outbound.GraphEvidenceReader)(nil)
var _ outbound.GraphEvidenceReader = (CapturedEvidenceReader)(nil)

type capturedQueryWideEvidence struct {
	CapturedEvidenceReader
}

func (capturedQueryWideEvidence) IsPinnedRead(context.Context) bool {
	return true
}

func capturedQueryRead[Value any](ctx context.Context, reader *capturedQueryEvidence, repo domain.ContentHash, origin, operation string, value Value, found bool) (Value, error) {
	reader.test.Helper()
	reader.calls = append(reader.calls, operation)
	reader.contexts = append(reader.contexts, ctx)
	var empty Value
	if repo != reader.repo || origin != "" && origin != reader.metadata.GitRemoteURL {
		reader.test.Fatal("captured query changed repository or origin", repo, origin)
	}
	if operation == reader.cancelOn {
		reader.cancel()
	}
	if err := reader.errors[operation]; err != nil {
		return empty, err
	}
	if !found {
		return empty, domain.ErrNotFound
	}
	return value, nil
}

func (reader *capturedQueryEvidence) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return capturedQueryRead(ctx, reader, repo, "", "repository", reader.metadata, true)
}

func (reader *capturedQueryEvidence) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, commit string) (domain.GitCommitTree, error) {
	return capturedQueryRead(ctx, reader, repo, origin, "commit", domain.GitCommitTree{}, false)
}

func (reader *capturedQueryEvidence) GetGitTreeNode(ctx context.Context, repo domain.ContentHash, origin, tree string) (domain.GitTreeNode, error) {
	return capturedQueryRead(ctx, reader, repo, origin, "tree", domain.GitTreeNode{}, false)
}

func (reader *capturedQueryEvidence) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, commit, parent string) (domain.GitCommitDelta, error) {
	return capturedQueryRead(ctx, reader, repo, origin, "delta", domain.GitCommitDelta{}, false)
}

func (reader *capturedQueryEvidence) GetPRHistoryCheckpoint(ctx context.Context, repo domain.ContentHash, origin string, provider int64, number int) (domain.PRHistoryCheckpoint, error) {
	return capturedQueryRead(ctx, reader, repo, origin, "pr checkpoint", domain.PRHistoryCheckpoint{}, false)
}

func (reader *capturedQueryEvidence) GetPRRefObservation(ctx context.Context, repo domain.ContentHash, origin, identifier string) (domain.GitRefObservation, error) {
	return capturedQueryRead(ctx, reader, repo, origin, "observation", domain.GitRefObservation{}, false)
}

func (reader *capturedQueryEvidence) ListPRParticipations(ctx context.Context, repo domain.ContentHash, origin string, number int) ([]domain.PRParticipation, error) {
	return capturedQueryRead(ctx, reader, repo, origin, "participations", []domain.PRParticipation{}, true)
}

func (reader *capturedQueryEvidence) ListPRContributions(ctx context.Context, repo domain.ContentHash) ([]domain.PRContribution, error) {
	return capturedQueryRead(ctx, reader, repo, "", "pr", []domain.PRContribution{}, true)
}

func (reader *capturedQueryEvidence) ListGitContributions(ctx context.Context, repo domain.ContentHash) ([]domain.GitContribution, error) {
	return capturedQueryRead(ctx, reader, repo, "", "git", []domain.GitContribution{}, true)
}

func (reader *capturedQueryEvidence) GetGitHeadReceipt(ctx context.Context, repo domain.ContentHash, origin string, version int64) (domain.GitHeadReceipt, error) {
	return capturedQueryRead(ctx, reader, repo, origin, "receipt", domain.GitHeadReceipt{}, false)
}

func (reader *capturedQueryEvidence) GetGitContributionAdmission(ctx context.Context, repo domain.ContentHash, identifier string) (domain.GitContributionAdmission, error) {
	return capturedQueryRead(ctx, reader, repo, "", "admission", domain.GitContributionAdmission{}, false)
}

func (reader *capturedQueryEvidence) GetGitContextHead(ctx context.Context, repo domain.ContentHash, identifier string) (domain.GitContextHead, error) {
	return capturedQueryRead(ctx, reader, repo, "", "head", domain.GitContextHead{}, false)
}

func (reader *capturedQueryEvidence) ListGitContributionProofs(ctx context.Context, repo domain.ContentHash) ([]domain.GitContributionProof, error) {
	return capturedQueryRead(ctx, reader, repo, "", "proofs", []domain.GitContributionProof{}, true)
}

func capturedQueryFixture(test *testing.T) (domain.RepositoryViewSourceCheckpoint, domain.RepositoryView, *capturedQueryEvidence, *legacyProjectResources) {
	test.Helper()
	checkpoint, entries := capturedQueryInputs(test, 1)
	repo, view, _, err := DecodeCapturedGraphInputs(context.Background(), checkpoint, entries)
	if err != nil {
		test.Fatal(err)
	}
	reader := &capturedQueryEvidence{test: test, repo: repo.ID, metadata: repo, errors: map[string]error{}}
	return checkpoint, view, reader, &legacyProjectResources{test: test}
}

func TestCapturedEvidenceReaderMatchesPortAndNarrowsCapabilities(test *testing.T) {
	local := reflect.TypeFor[CapturedEvidenceReader]()
	port := reflect.TypeFor[outbound.GraphEvidenceReader]()
	if local.NumMethod() != port.NumMethod() {
		test.Fatal("captured evidence method set differs from its existing read-only port")
	}
	for index := 0; index < port.NumMethod(); index++ {
		method := port.Method(index)
		actual, found := local.MethodByName(method.Name)
		if !found || actual.Type != method.Type {
			test.Fatal("captured evidence method changed", method.Name)
		}
	}
	_, _, reader, resources := capturedQueryFixture(test)
	wide := capturedQueryWideEvidence{CapturedEvidenceReader: reader}
	narrow := struct{ CapturedEvidenceReader }{wide}
	projection, err := NewCapturedProjection(narrow, resources)
	if err != nil {
		test.Fatal(err)
	}
	if _, extra := projection.metadata.(ProjectionCacheScope); extra {
		test.Fatal("captured evidence narrowing retained an additional capability")
	}
	if projection.scope.IsPinnedRead(context.Background()) || projection.resources != resources || len(reader.calls) != 0 || resources.attempts != 0 {
		test.Fatal("evidence narrowing changed captured scope, resources, or constructor I/O")
	}
}

func TestNewCapturedProjectionBindsCompleteEvidenceAndSharedResourcesWithoutIO(test *testing.T) {
	_, view, reader, resources := capturedQueryFixture(test)
	projection, err := NewCapturedProjection(reader, resources)
	if err != nil {
		test.Fatal(err)
	}
	if _, complete := projection.contributions.(*evidencequery.Queries); !complete {
		test.Fatal("captured projection replaced complete evidence queries")
	}
	if len(reader.calls) != 0 || resources.attempts != 0 || projection.metadata != reader || projection.trees != reader || projection.deltas != reader || projection.resources != resources {
		test.Fatal("constructor performed I/O or replaced its dependencies")
	}
	if !projection.captured || projection.requireComplete || projection.generations != nil || projection.scope.IsPinnedRead(context.Background()) {
		test.Fatal("captured projection changed generation or exhaustion policy")
	}
	another, err := NewCapturedProjection(reader, resources)
	if err != nil || another.graphs == projection.graphs || another.branches == projection.branches {
		test.Fatal("captured constructors shared ephemeral caches", err)
	}
	ctx := context.WithValue(context.Background(), legacyProjectBuildKey{}, "caller")
	graph, err := projection.Cached(ctx, reader.repo, view, "")
	if err != nil || graph.Version != 1 || resources.acquires != 1 || resources.releases != 1 || resources.active {
		test.Fatal("captured projection did not use the shared admission resource", err)
	}
	for _, bound := range reader.contexts {
		if bound.Value(legacyProjectBuildKey{}) != resources {
			test.Fatal("admitted evidence read lost its current context")
		}
	}
	if !slices.Contains(reader.calls, "pr") || !slices.Contains(reader.calls, "git") || projection.graphs.Stats().Entries != 0 || projection.branches.Stats().Entries != 0 {
		test.Fatal("captured projection skipped ledgers or populated reusable caches")
	}
	resources.err = domain.ErrResourceOverloaded
	graph, err = projection.Cached(ctx, reader.repo, view, "")
	if !errors.Is(err, domain.ErrResourceOverloaded) || !reflect.DeepEqual(graph, domain.GraphState{}) || resources.acquires != resources.releases {
		test.Fatal("captured cache bypassed shared resource admission", err)
	}
}

func TestNewCapturedProjectionRejectsMissingAndTypedNilDependencies(test *testing.T) {
	for _, scenario := range []string{"nil evidence", "typed nil evidence", "nil resources", "typed nil resources"} {
		test.Run(scenario, func(test *testing.T) {
			_, _, reader, shared := capturedQueryFixture(test)
			var evidence outbound.GraphEvidenceReader = reader
			var resources GraphResources = shared
			switch scenario {
			case "nil evidence":
				evidence = nil
			case "typed nil evidence":
				var missing *capturedQueryEvidence
				evidence = missing
			case "nil resources":
				resources = nil
			case "typed nil resources":
				var missing *legacyProjectResources
				resources = missing
			}
			projection, err := NewCapturedProjection(evidence, resources)
			if !errors.Is(err, domain.ErrCatalogUnsupported) || projection != nil || len(reader.calls) != 0 || shared.attempts != 0 {
				test.Fatal("invalid captured assembly performed I/O or succeeded", err)
			}
		})
	}
}

func TestProjectCapturedRepositoryViewPreservesMembershipSemanticsAndInputs(test *testing.T) {
	checkpoint, view, reader, resources := capturedQueryFixture(test)
	view.Semantics = domain.ContextSemantics{Version: 99}
	view.Graph = &domain.GraphState{Version: -1}
	before := capturedQueryJSON(test, view)
	projected, err := ProjectCapturedRepositoryView(context.Background(), checkpoint, view, reader, resources)
	if err != nil || projected.Graph == nil {
		test.Fatal(err)
	}
	for _, snapshot := range projected.Snapshots {
		want := []string{"main", "zeta"}
		if snapshot.Branch == "topic" {
			want = []string{"alpha", "topic", "zeta"}
		}
		if !reflect.DeepEqual(snapshot.Branches, want) {
			test.Fatal("captured membership changed", snapshot.Branches, want)
		}
	}
	if !reflect.DeepEqual(projected.Semantics, domain.ProjectContextSemantics(view.Snapshots, view.History)) || projected.Revision != view.Revision || resources.attempts != 0 {
		test.Fatal("captured projection changed semantics, revision, or admission ownership")
	}
	contextual := projected.Graph.BranchContexts["main"]
	if contextual.BranchID != "main-id" || contextual.SnapshotID != view.Refs[0].Target || len(contextual.SnapshotIDs) != 2 || contextual.Reason != "code_position_unavailable" {
		test.Fatal("captured graph did not compute the native branch context", contextual)
	}
	if !bytes.Equal(before, capturedQueryJSON(test, view)) {
		test.Fatal("captured projection mutated its input view")
	}
	projected.Snapshots[0].Branches[0] = "caller output"
	projected.Graph.SnapshotIDs[0] = domain.HashContent([]byte("caller output"))
	again, err := ProjectCapturedRepositoryView(context.Background(), checkpoint, view, reader, resources)
	if err != nil || slices.Contains(again.Snapshots[0].Branches, "caller output") || !bytes.Equal(before, capturedQueryJSON(test, view)) {
		test.Fatal("captured output mutation changed later projection", err)
	}
}

func TestProjectCapturedRepositoryViewNormalizesOnlyOriginalNilCollections(test *testing.T) {
	checkpoint, _, reader, resources := capturedQueryFixture(test)
	projected, err := ProjectCapturedRepositoryView(context.Background(), checkpoint, domain.RepositoryView{DefaultBranch: reader.metadata.DefaultBranch}, reader, resources)
	if err != nil || projected.Graph == nil || projected.Refs == nil || projected.Snapshots == nil || projected.Reflog == nil || projected.History == nil || projected.Pending == nil || projected.Unsync == nil || projected.ArchivedSessions != nil {
		test.Fatal("captured projection changed nil collection semantics", err)
	}
}

func TestProjectCapturedRepositoryViewRejectsTypedNilEvidence(test *testing.T) {
	checkpoint, view, reader, resources := capturedQueryFixture(test)
	var missing *capturedQueryEvidence
	var evidence outbound.GraphEvidenceReader = missing
	projected, err := ProjectCapturedRepositoryView(context.Background(), checkpoint, view, evidence, resources)
	if !errors.Is(err, domain.ErrValidation) || !reflect.DeepEqual(projected, domain.RepositoryView{}) || len(reader.calls) != 0 || resources.attempts != 0 {
		test.Fatal("typed-nil evidence reached a reader or exposed a partial repository view", err)
	}
}

func TestProjectCapturedGraphClearsDerivedStateAndPreservesHistoricalSelection(test *testing.T) {
	checkpoint, view, reader, resources := capturedQueryFixture(test)
	ctx := context.WithValue(context.Background(), legacyProjectBuildKey{}, "caller")
	want, err := ProjectCapturedGraph(ctx, checkpoint, view, reader, "", resources)
	if err != nil {
		test.Fatal(err)
	}
	view.Semantics = domain.ContextSemantics{Version: 99}
	view.Graph = &domain.GraphState{Version: -1}
	before := capturedQueryJSON(test, view)
	graph, err := ProjectCapturedGraph(ctx, checkpoint, view, reader, "", resources)
	if err != nil || !reflect.DeepEqual(graph, want) || !bytes.Equal(before, capturedQueryJSON(test, view)) {
		test.Fatal("derived caller state changed captured graph projection", err)
	}
	position := fmt.Sprintf("%032x", 2)
	clean := view
	clean.Semantics, clean.Graph = domain.ContextSemantics{}, nil
	want, err = domain.ProjectGraphState(clean, clean.DefaultBranch, position)
	if err != nil {
		test.Fatal(err)
	}
	want.BranchContexts = map[string]domain.BranchContext{}
	want.Integrations = []domain.GraphIntegration{}
	reader.calls, reader.contexts = nil, nil
	graph, err = ProjectCapturedGraph(ctx, checkpoint, view, reader, position, resources)
	if err != nil || !reflect.DeepEqual(graph, want) || !reflect.DeepEqual(reader.calls, []string{"repository"}) || resources.attempts != 0 {
		test.Fatal("historical projection changed semantics or read contribution evidence", err)
	}
	if reader.contexts[0] != ctx {
		test.Fatal("captured graph replaced its current context")
	}
}

func TestProjectCapturedGraphValidatesScopeBeforeEvidenceReads(test *testing.T) {
	for _, target := range []string{"version", "scope", "epoch", "sequence", "repository", "ref", "snapshot", "history", "pending", "unsync", "archive"} {
		test.Run(target, func(test *testing.T) {
			checkpoint, view, reader, resources := capturedQueryFixture(test)
			foreign := domain.HashContent([]byte("foreign"))
			want := domain.ErrIntegrity
			switch target {
			case "version":
				checkpoint.Version++
				want = domain.ErrCatalogResetRequired
			case "scope":
				checkpoint.Scope = domain.RepositoryViewSourceScope
				want = domain.ErrCatalogResetRequired
			case "epoch":
				checkpoint.Epoch = ""
				want = domain.ErrValidation
			case "sequence":
				checkpoint.Sequence = -1
				want = domain.ErrValidation
			case "repository":
				checkpoint.RepoID = "invalid"
				want = domain.ErrValidation
			case "ref":
				view.Refs[0].RepoID = foreign
			case "snapshot":
				view.Snapshots[0].RepoID = foreign
			case "history":
				view.History[0].RepoID = string(foreign)
			case "pending":
				view.Pending = []domain.Pending{{RepoID: foreign}}
			case "unsync":
				view.Unsync = []domain.Unsync{{RepoID: foreign}}
			case "archive":
				view.ArchivedSessions = []domain.SessionArchiveView{{SessionArchive: domain.SessionArchive{RepoID: foreign}}}
			}
			graph, err := ProjectCapturedGraph(context.Background(), checkpoint, view, reader, "historical", resources)
			if !errors.Is(err, want) || !reflect.DeepEqual(graph, domain.GraphState{}) || len(reader.calls) != 0 || resources.attempts != 0 {
				test.Fatal("invalid captured scope reached evidence or returned a graph", err)
			}
		})
	}
}

func TestProjectCapturedGraphPropagatesMetadataEvidenceAndCurrentContextErrors(test *testing.T) {
	for _, scenario := range []string{"nil evidence", "typed nil evidence", "missing repository", "foreign metadata", "different default", "pr error", "git error", "pre-canceled", "cancel repository", "cancel pr", "cancel git", "duplicate snapshot"} {
		test.Run(scenario, func(test *testing.T) {
			checkpoint, view, reader, resources := capturedQueryFixture(test)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var evidence outbound.GraphEvidenceReader = reader
			want := domain.ErrIntegrity
			switch scenario {
			case "nil evidence":
				evidence, want = nil, domain.ErrValidation
			case "typed nil evidence":
				var missing *capturedQueryEvidence
				evidence, want = missing, domain.ErrValidation
			case "missing repository":
				reader.errors["repository"], want = domain.ErrNotFound, domain.ErrNotFound
			case "foreign metadata":
				reader.metadata.ID = domain.HashContent([]byte("foreign"))
			case "different default":
				reader.metadata.DefaultBranch = "other"
			case "pr error":
				reader.errors["pr"] = want
			case "git error":
				reader.errors["git"] = want
			case "pre-canceled":
				cancel()
				checkpoint.Version++
				want = context.Canceled
			case "cancel repository", "cancel pr", "cancel git":
				reader.cancel, reader.cancelOn = cancel, strings.TrimPrefix(scenario, "cancel ")
				want = context.Canceled
			case "duplicate snapshot":
				view.Snapshots = append(view.Snapshots, view.Snapshots[0])
			}
			graph, err := ProjectCapturedGraph(ctx, checkpoint, view, evidence, "", resources)
			if !errors.Is(err, want) || !reflect.DeepEqual(graph, domain.GraphState{}) || resources.attempts != 0 {
				test.Fatal("failed captured projection exposed partial state or changed its error", err)
			}
			if (scenario == "pre-canceled" || scenario == "nil evidence" || scenario == "typed nil evidence") && len(reader.calls) != 0 {
				test.Fatal("canceled projection or absent evidence reached a captured reader")
			}
			for _, bound := range reader.contexts {
				if bound != ctx {
					test.Fatal("captured evidence escaped its current context")
				}
			}
		})
	}
}
