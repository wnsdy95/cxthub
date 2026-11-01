package memoryquery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryAssessmentEvidence struct {
	repo    domain.ContentHash
	origin  string
	commits map[string]domain.GitCommitTree
	nodes   map[string]domain.GitTreeNode
	deltas  map[string]domain.GitCommitDelta
}

func (reader *memoryAssessmentEvidence) check(ctx context.Context, repo domain.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if repo != reader.repo {
		return domain.ErrIntegrity
	}
	return nil
}

func (reader *memoryAssessmentEvidence) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return domain.Repo{ID: reader.repo, GitRemoteURL: reader.origin}, reader.check(ctx, repo)
}

func (reader *memoryAssessmentEvidence) ListPRContributions(ctx context.Context, repo domain.ContentHash) ([]domain.PRContribution, error) {
	return nil, reader.check(ctx, repo)
}

func (reader *memoryAssessmentEvidence) ListGitContributions(ctx context.Context, repo domain.ContentHash) ([]domain.GitContribution, error) {
	return nil, reader.check(ctx, repo)
}

func (*memoryAssessmentEvidence) GetPRHistoryCheckpoint(context.Context, domain.ContentHash, string, int64, int) (domain.PRHistoryCheckpoint, error) {
	panic("empty-ledger fixture queried a PR history checkpoint")
}

func (*memoryAssessmentEvidence) GetPRRefObservation(context.Context, domain.ContentHash, string, string) (domain.GitRefObservation, error) {
	panic("empty-ledger fixture queried a PR ref observation")
}

func (*memoryAssessmentEvidence) ListPRParticipations(context.Context, domain.ContentHash, string, int) ([]domain.PRParticipation, error) {
	panic("empty-ledger fixture queried PR participations")
}

func (*memoryAssessmentEvidence) GetGitContextHead(context.Context, domain.ContentHash, string) (domain.GitContextHead, error) {
	panic("empty-ledger fixture queried a Git context head")
}

func (*memoryAssessmentEvidence) GetGitContributionAdmission(context.Context, domain.ContentHash, string) (domain.GitContributionAdmission, error) {
	panic("empty-ledger fixture queried a Git contribution admission")
}

func (reader *memoryAssessmentEvidence) ListGitContributionProofs(ctx context.Context, repo domain.ContentHash) ([]domain.GitContributionProof, error) {
	return nil, reader.check(ctx, repo)
}

func (*memoryAssessmentEvidence) GetGitHeadReceipt(context.Context, domain.ContentHash, string, int64) (domain.GitHeadReceipt, error) {
	panic("empty-ledger fixture queried a Git head receipt")
}

func (reader *memoryAssessmentEvidence) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, identifier string) (domain.GitCommitTree, error) {
	if err := reader.check(ctx, repo); err != nil {
		return domain.GitCommitTree{}, err
	}
	if origin != reader.origin {
		return domain.GitCommitTree{}, domain.ErrIntegrity
	}
	commit, found := reader.commits[identifier]
	if !found {
		return domain.GitCommitTree{}, domain.ErrNotFound
	}
	return commit, nil
}

func (reader *memoryAssessmentEvidence) GetGitTreeNode(ctx context.Context, repo domain.ContentHash, origin, identifier string) (domain.GitTreeNode, error) {
	if err := reader.check(ctx, repo); err != nil {
		return domain.GitTreeNode{}, err
	}
	if origin != reader.origin {
		return domain.GitTreeNode{}, domain.ErrIntegrity
	}
	node, found := reader.nodes[identifier]
	if !found {
		return domain.GitTreeNode{}, domain.ErrNotFound
	}
	return node, nil
}

func (reader *memoryAssessmentEvidence) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, identifier, parent string) (domain.GitCommitDelta, error) {
	if err := reader.check(ctx, repo); err != nil {
		return domain.GitCommitDelta{}, err
	}
	if origin != reader.origin {
		return domain.GitCommitDelta{}, domain.ErrIntegrity
	}
	delta, found := reader.deltas[identifier]
	if !found || parent != "" && parent != delta.Parent {
		return domain.GitCommitDelta{}, domain.ErrNotFound
	}
	return delta, nil
}

func (reader *memoryAssessmentEvidence) queries() (*evidencequery.Queries, error) {
	return evidencequery.New(evidencequery.Config{Profile: evidencequery.CompleteEvidence, Metadata: reader,
		PRLedger: reader, PRHistory: reader, GitLedger: reader, GitHeads: reader, GitAdmission: reader, GitReceipts: reader})
}

type memoryAssessmentInputs struct {
	reader    *memoryIntegrationInputs
	reads     int
	rows      int
	err       error
	afterRead func()
}

func (reader *memoryAssessmentInputs) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	reader.reads++
	if reader.err != nil {
		return nil, reader.err
	}
	rows, err := reader.reader.ListSnapshots(ctx, repo, branch)
	reader.rows += len(rows)
	if reader.afterRead != nil {
		reader.afterRead()
	}
	return rows, err
}

func (reader *memoryAssessmentInputs) GetSnapshot(ctx context.Context, repo, identity domain.ContentHash) (domain.Snapshot, error) {
	return reader.reader.GetSnapshot(ctx, repo, identity)
}

func (reader *memoryAssessmentInputs) GetRef(ctx context.Context, repo domain.ContentHash, kind domain.RefKind, name string) (domain.Ref, error) {
	return reader.reader.GetRef(ctx, repo, kind, name)
}

type memoryAssessmentSource struct {
	generation domain.RepositoryMemorySourceGeneration
	selection  domain.EffectiveMemorySelection
	reader     *memoryIntegrationInputs
	evidence   *memoryAssessmentEvidence
	objects    map[domain.ContentHash]json.RawMessage
}

var _ outbound.MemoryMetadataReader = (*memoryAssessmentInputs)(nil)
var _ outbound.MemoryProjectionReader = (*memoryAssessmentSource)(nil)

func (captured *memoryAssessmentSource) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if err := captured.reader.check(ctx, repo); err != nil {
		return domain.MemoryDigest{}, err
	}
	raw, found := captured.objects[hash]
	if !found {
		return domain.MemoryDigest{}, domain.ErrIntegrity
	}
	var digest domain.MemoryDigest
	if err := json.Unmarshal(raw, &digest); err != nil {
		return domain.MemoryDigest{}, domain.ErrIntegrity
	}
	actual, err := domain.MemoryDigestHash(digest)
	if err != nil || actual != hash {
		return domain.MemoryDigest{}, domain.ErrIntegrity
	}
	return digest, ctx.Err()
}

func (captured *memoryAssessmentSource) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	return captured.reader.ListSnapshots(ctx, repo, branch)
}

func (captured *memoryAssessmentSource) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	if err := captured.reader.check(ctx, repo); err != nil {
		return nil, err
	}
	history := make([]domain.HistoryEvent, len(captured.reader.view.History))
	for ordinal, event := range captured.reader.view.History {
		history[ordinal] = event.Clone()
	}
	return history, ctx.Err()
}

func (captured *memoryAssessmentSource) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	return captured.reader.view.Revision, captured.reader.check(ctx, repo)
}

func (captured *memoryAssessmentSource) queries(complete bool, metadata outbound.MemoryMetadataReader) (*LiveQueries, error) {
	contributions, err := captured.evidence.queries()
	if err != nil {
		return nil, err
	}
	generation := captured.generation
	return &LiveQueries{mode: LiveFrozen, metadata: metadata, projection: captured, history: captured, revisions: captured,
		repository: captured.evidence, evidence: contributions, trees: captured.evidence, deltas: captured.evidence,
		frozenGeneration: &generation, capturedEvidence: true, requireComplete: complete}, nil
}

func (captured *memoryAssessmentSource) setDigest(test *testing.T, digest domain.MemoryDigest) {
	test.Helper()
	raw, err := json.Marshal(digest)
	if err != nil {
		test.Fatal(err)
	}
	identity := domain.HashContent(raw)
	captured.objects = map[domain.ContentHash]json.RawMessage{identity: raw}
	captured.selection.MemoryHash = identity
	for ordinal := range captured.reader.view.Snapshots {
		if captured.reader.view.Snapshots[ordinal].ID == digest.SnapshotID {
			captured.reader.view.Snapshots[ordinal].MemoryHash = identity
		}
	}
}

func buildMemoryAssessmentRead(ctx context.Context, captured *memoryAssessmentSource, captureID domain.ContentHash) (domain.MemoryReadBundle, error) {
	queries, err := captured.queries(true, captured.reader)
	if err != nil {
		return domain.MemoryReadBundle{}, err
	}
	dependencies := []domain.MemoryReadDependency{{Hash: captured.selection.MemoryHash, Available: true}}
	return BuildRead(ctx, queries, captured.reader.repo, captureID, captured.generation, captured.selection, "", dependencies)
}

func memoryAssessmentContext() context.Context {
	return inbound.WithSystemActor(context.Background())
}

func memoryAssessmentHash(text string) domain.ContentHash {
	return domain.HashContent([]byte(text))
}

func memoryAssessmentOID(ordinal int) string {
	return fmt.Sprintf("%040x", ordinal)
}

func memoryAssessmentGitFixture(test *testing.T, repo domain.ContentHash) *memoryAssessmentEvidence {
	test.Helper()
	reader := &memoryAssessmentEvidence{repo: repo, origin: "https://github.com/example/effective-memory",
		commits: map[string]domain.GitCommitTree{}, nodes: map[string]domain.GitTreeNode{}, deltas: map[string]domain.GitCommitDelta{}}
	old := domain.GitEntry{OID: memoryAssessmentOID(101), Mode: "100644"}
	fresh := domain.GitEntry{OID: memoryAssessmentOID(102), Mode: "100755"}
	other := domain.GitEntry{OID: memoryAssessmentOID(103), Mode: "100644"}
	feature := []domain.GitPathChange{{Path: "a", Before: old, After: fresh}, {Path: "b", Before: old, After: fresh}}
	for _, row := range []struct {
		ordinal int
		parent  int
		entries map[string]domain.GitEntry
		changes []domain.GitPathChange
	}{
		{ordinal: 1, entries: map[string]domain.GitEntry{"a": old, "b": old}, changes: []domain.GitPathChange{{Path: "a", After: old}, {Path: "b", After: old}}},
		{ordinal: 2, parent: 1, entries: map[string]domain.GitEntry{"a": fresh, "b": fresh}, changes: feature},
		{ordinal: 3, parent: 2, entries: map[string]domain.GitEntry{"a": fresh, "b": fresh, "x": other}, changes: []domain.GitPathChange{{Path: "x", After: other}}},
		{ordinal: 9, parent: 1, entries: map[string]domain.GitEntry{"a": fresh, "b": fresh}, changes: feature},
	} {
		node, err := domain.NewGitTreeNode(row.entries, 40)
		if err != nil {
			test.Fatal(err)
		}
		identifier := memoryAssessmentOID(row.ordinal)
		commit := domain.GitCommitTree{Commit: identifier, Tree: node.OID, Parents: []string{}}
		delta := domain.GitCommitDelta{Commit: identifier, Parents: []string{}, Changes: row.changes, Complete: true}
		if row.parent != 0 {
			delta.Parent = memoryAssessmentOID(row.parent)
			delta.Parents = []string{delta.Parent}
			commit.Parents = []string{delta.Parent}
		}
		reader.commits[identifier], reader.nodes[node.OID], reader.deltas[identifier] = commit, node, delta
	}
	return reader
}

func memoryAssessmentFixture(test *testing.T, count int) (*memoryIntegration, *memoryAssessmentInputs, *memoryAssessmentSource, domain.EffectiveMemoryItem) {
	test.Helper()
	repo, root, source, other := memoryAssessmentHash(test.Name()), memoryAssessmentHash("root"), memoryAssessmentHash("A"), memoryAssessmentHash("B")
	tip := memoryAssessmentHash("assessment-receipt-tip")
	snapshots := []domain.Snapshot{{ID: source, RepoID: repo, DocHash: source}, {ID: other, RepoID: repo, DocHash: other},
		{ID: root, RepoID: repo, DocHash: root, Parents: []domain.ContentHash{source, other}},
		{ID: tip, RepoID: repo, DocHash: tip, Parents: []domain.ContentHash{source}}}
	for len(snapshots) < count {
		identifier := memoryAssessmentHash(fmt.Sprintf("unrelated-%d", len(snapshots)))
		snapshots = append(snapshots, domain.Snapshot{ID: identifier, DocHash: identifier, RepoID: repo})
	}
	var history []domain.HistoryEvent
	for ordinal, row := range []struct {
		snapshot domain.ContentHash
		code     int
		branch   string
	}{{snapshot: source, code: 2, branch: "branch-a"}, {snapshot: other, code: 3, branch: "branch-b"}} {
		observation := domain.HistoryEvent{ID: fmt.Sprintf("%032x", ordinal+1), RepoID: string(repo), BranchID: row.branch, Branch: row.branch,
			Kind: "position", Target: row.snapshot, GitAfter: memoryAssessmentOID(row.code), CreatedAt: time.Date(2026, 1, 1, 0, 0, ordinal+1, 0, time.UTC)}
		history = append(history, observation, memoryIntegrationPublication(observation))
	}
	receipt := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 3), RepoID: string(repo), BranchID: "base", Branch: "main", SourceBranchID: "branch-a",
		Kind: "pr-merge", Source: tip, Target: tip, CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		PR: &domain.PullRequestMerge{Number: 1, HeadBranch: "branch-a", BaseBranch: "main", HeadSHA: memoryAssessmentOID(2), MergeSHA: memoryAssessmentOID(9)}}
	completion := receipt
	key := sha256.Sum256([]byte(receipt.ID + ":completed"))
	completion.ID, completion.PRCompleted = fmt.Sprintf("%x", key[:16]), true
	history = append(history, receipt, completion)
	for _, event := range history {
		if err := domain.ValidateHistoryEvent(event); err != nil {
			test.Fatal("invalid assessment fixture", err)
		}
	}
	digest := domain.MemoryDigest{SnapshotID: root, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{
		{SourceSnapshot: source, Summary: "old untyped prose", Claims: []domain.MemoryClaim{{Kind: "code", Text: "A feature", Code: &domain.MemoryCodeScope{Commit: memoryAssessmentOID(2), Paths: []string{"a", "b"}}}, {Kind: "rationale", Text: "why A was tried"}}},
		{SourceSnapshot: other, Claims: []domain.MemoryClaim{{Kind: "code", Text: "B feature", Code: &domain.MemoryCodeScope{Commit: memoryAssessmentOID(3), Paths: []string{"x"}}}}}}}
	generation := domain.RepositoryMemorySourceGeneration{Version: 1, Epoch: "23456789-abcd-4321-9876-123456789abc", Sequence: 1,
		Graph: domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope, RepoID: repo,
			Epoch: "12345678-abcd-4321-9876-123456789abc", Sequence: 7}}
	captured := &memoryAssessmentSource{generation: generation, evidence: memoryAssessmentGitFixture(test, repo),
		selection: domain.EffectiveMemorySelection{SnapshotID: root, CodeCommit: memoryAssessmentOID(9)},
		reader:    &memoryIntegrationInputs{repo: repo, view: domain.RepositoryView{Snapshots: snapshots, History: history}}}
	captured.setDigest(test, digest)
	inputs := &memoryAssessmentInputs{reader: captured.reader}
	queries, err := captured.queries(true, inputs)
	if err != nil {
		test.Fatal(err)
	}
	evidence := memoryIntegrationEvidence(test, evidencequery.GitEvidenceConfig{RepoID: repo, Origin: captured.evidence.origin,
		Trees: captured.evidence, Deltas: captured.evidence, RequireComplete: true})
	template, err := newMemoryIntegration(queries, evidence, repo, history)
	if err != nil {
		test.Fatal(err)
	}
	items, err := EffectiveItems(digest, "claims")
	if err != nil {
		test.Fatal(err)
	}
	return template, inputs, captured, items[0]
}

func TestMemoryReadAssessmentReusesOnlySnapshotInputs(test *testing.T) {
	for _, count := range []int{100, 1000} {
		test.Run(fmt.Sprintf("snapshots-%d", count), func(test *testing.T) {
			template, inputs, _, item := memoryAssessmentFixture(test, count)
			ctx := memoryAssessmentContext()
			for ordinal := range 20 {
				item.Text = fmt.Sprintf("independent claim %d", ordinal)
				got, err := template.assessReadItem(ctx, item, memoryAssessmentOID(9), domain.RepositoryRevision{})
				if err != nil || got.State != "applied" || got.IntegrationReceipt == "" {
					test.Fatal("completed squash did not apply", got, err)
				}
				fresh := *template
				fresh.snapshots = nil
				beforeReads, beforeRows := inputs.reads, inputs.rows
				want, err := fresh.assessReadItem(ctx, item, memoryAssessmentOID(9), domain.RepositoryRevision{})
				if err != nil || !reflect.DeepEqual(got, want) {
					test.Fatal("reuse changed independent assessment", err)
				}
				inputs.reads, inputs.rows = beforeReads, beforeRows
				if len(template.natural) != 0 || template.evidence.git.ReadCount() != 0 || template.evidence.Limited() || template.evidence.TraversalLimited() {
					test.Fatal("claim traversal or read budget escaped its scope")
				}
			}
			test.Logf("claims=20 snapshots=%d snapshot_list_reads=%d cloned_rows=%d", count, inputs.reads, inputs.rows)
			if inputs.reads != 1 || inputs.rows != count {
				test.Fatal("independent claims repeatedly cloned the entire snapshot list")
			}
		})
	}
}

func TestMemoryReadAssessmentErrorsAndCaptureIsolation(test *testing.T) {
	template, inputs, _, item := memoryAssessmentFixture(test, 100)
	ctx := memoryAssessmentContext()
	inputs.err = errors.New("transient metadata error")
	if _, err := template.assessReadItem(ctx, item, memoryAssessmentOID(9), domain.RepositoryRevision{}); !errors.Is(err, inputs.err) || template.snapshots != nil {
		test.Fatal("failed input read was retained", err)
	}
	inputs.err = nil
	if _, err := template.assessReadItem(ctx, item, memoryAssessmentOID(9), domain.RepositoryRevision{}); err != nil {
		test.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := template.assessReadItem(canceled, item, memoryAssessmentOID(9), domain.RepositoryRevision{}); !errors.Is(err, context.Canceled) {
		test.Fatal("warm input bypassed cancellation", err)
	}
	fresh, err := newMemoryIntegration(template.queries, template.evidence, template.repo, template.history)
	if err != nil {
		test.Fatal(err)
	}
	inputs.err = domain.ErrIntegrity
	if _, err := fresh.assessReadItem(ctx, item, memoryAssessmentOID(9), domain.RepositoryRevision{}); !errors.Is(err, domain.ErrIntegrity) || fresh.snapshots != nil {
		test.Fatal("new build reused old input authority", err)
	}
}

func TestMemoryReadAssessmentKeepsSnapshotInputsLazy(test *testing.T) {
	for _, scenario := range []string{"missing-publication", "direct-ancestry", "exact-receipt"} {
		test.Run(scenario, func(test *testing.T) {
			template, inputs, _, item := memoryAssessmentFixture(test, 100)
			inputs.err = domain.ErrIntegrity
			code := memoryAssessmentOID(9)
			switch scenario {
			case "missing-publication":
				item.SourceSnapshot = memoryAssessmentHash("unpublished")
			case "direct-ancestry":
				code = memoryAssessmentOID(3)
			case "exact-receipt":
				template.receipts["branch-a"][0].Source = item.SourceSnapshot
			}
			got, err := template.assessReadItem(memoryAssessmentContext(), item, code, domain.RepositoryRevision{})
			if err != nil || inputs.reads != 0 || template.snapshots != nil {
				test.Fatal("loaded unrelated snapshot metadata", inputs.reads, err)
			}
			if scenario == "missing-publication" && got.Reason != "source_publication_missing" {
				test.Fatal("missing publication changed state", got)
			}
			if scenario != "missing-publication" && got.State != "applied" {
				test.Fatal("confirmed ancestry changed state", got)
			}
		})
	}
}

func TestMemoryReadAssessmentRejectsPartialSnapshotIndex(test *testing.T) {
	template, inputs, captured, item := memoryAssessmentFixture(test, 100)
	captured.reader.view.Snapshots = append(captured.reader.view.Snapshots, domain.Snapshot{ID: memoryAssessmentHash("foreign"), RepoID: memoryAssessmentHash("other-repository")})
	if _, err := template.assessReadItem(memoryAssessmentContext(), item, memoryAssessmentOID(9), domain.RepositoryRevision{}); !errors.Is(err, domain.ErrIntegrity) || template.snapshots != nil {
		test.Fatal("retained partially validated metadata", err)
	}
	captured.reader.view.Snapshots = captured.reader.view.Snapshots[:len(captured.reader.view.Snapshots)-1]
	if _, err := template.assessReadItem(memoryAssessmentContext(), item, memoryAssessmentOID(9), domain.RepositoryRevision{}); err != nil || inputs.reads != 2 {
		test.Fatal("retry did not validate the complete input again", inputs.reads, err)
	}
}

func TestMemoryReadAssessmentCancellationDiscardsLoadedInputs(test *testing.T) {
	template, inputs, _, item := memoryAssessmentFixture(test, 100)
	ctx, cancel := context.WithCancel(memoryAssessmentContext())
	defer cancel()
	inputs.afterRead = cancel
	if _, err := template.assessReadItem(ctx, item, memoryAssessmentOID(9), domain.RepositoryRevision{}); !errors.Is(err, context.Canceled) || template.snapshots != nil || inputs.reads != 1 {
		test.Fatal("cancellation published loaded metadata", inputs.reads, err)
	}
	inputs.afterRead = nil
	if got, err := template.assessReadItem(memoryAssessmentContext(), item, memoryAssessmentOID(9), domain.RepositoryRevision{}); err != nil || got.State != "applied" || inputs.reads != 2 {
		test.Fatal("canceled input changed retry behavior", inputs.reads, err)
	}
}

func TestMemoryReadAssessmentKeepsNaturalTraversalBound(test *testing.T) {
	for _, depth := range []int{MaxProjectionSnapshots, MaxProjectionSnapshots + 1} {
		test.Run(fmt.Sprintf("depth-%d", depth), func(test *testing.T) {
			template, inputs, captured, item := memoryAssessmentFixture(test, 100)
			chain := make([]domain.ContentHash, depth)
			for ordinal := range chain {
				chain[ordinal] = memoryAssessmentHash(fmt.Sprintf("natural-chain-%d", ordinal))
			}
			for ordinal, identifier := range chain {
				parent := item.SourceSnapshot
				if ordinal+1 < len(chain) {
					parent = chain[ordinal+1]
				}
				captured.reader.view.Snapshots = append(captured.reader.view.Snapshots, domain.Snapshot{ID: identifier, RepoID: template.repo, Parents: []domain.ContentHash{parent}})
			}
			template.receipts["branch-a"][0].Source = chain[0]
			near := item
			near.SourceSnapshot = chain[1]
			publication := template.publications[[2]string{string(item.SourceSnapshot), item.Code.Commit}][0]
			publication.Source, publication.Target = near.SourceSnapshot, near.SourceSnapshot
			template.publications[[2]string{string(near.SourceSnapshot), item.Code.Commit}] = []domain.HistoryEvent{publication}
			if got, err := template.assessReadItem(memoryAssessmentContext(), near, memoryAssessmentOID(9), domain.RepositoryRevision{}); err != nil || got.State != "applied" {
				test.Fatal("could not warm metadata with a bounded claim", err)
			}
			for range 2 {
				got, err := template.assessReadItem(memoryAssessmentContext(), item, memoryAssessmentOID(9), domain.RepositoryRevision{})
				if depth == MaxProjectionSnapshots {
					if err != nil || got.State != "applied" {
						test.Fatal("inclusive traversal bound changed", err)
					}
				} else if !errors.Is(err, domain.ErrMemoryProjectionLimit) || !reflect.DeepEqual(got, domain.EffectiveMemoryItem{}) {
					test.Fatal("cached metadata bypassed the per-claim bound", err)
				}
			}
			if inputs.reads != 1 || len(template.natural) != 0 || template.evidence.TraversalLimited() {
				test.Fatal("claim-specific traversal leaked across assessments")
			}
		})
	}
}

func TestMemoryReadAssessmentBundleMatchesLegacyProof(test *testing.T) {
	_, _, captured, _ := memoryAssessmentFixture(test, 1000)
	ctx := memoryAssessmentContext()
	digest, err := captured.GetMemory(ctx, captured.reader.repo, captured.selection.MemoryHash)
	if err != nil {
		test.Fatal(err)
	}
	for ordinal := range 12 {
		claim := digest.Fragments[0].Claims[0]
		claim.Text = fmt.Sprintf("inherited source A claim %d", ordinal)
		digest.Fragments[0].Claims = append(digest.Fragments[0].Claims, claim)
	}
	captured.setDigest(test, digest)
	bundle, err := buildMemoryAssessmentRead(ctx, captured, memoryAssessmentHash("synthetic-capture"))
	if err != nil {
		test.Fatal(err)
	}
	if err := domain.ValidateMemoryReadBundle(ctx, bundle); err != nil {
		test.Fatal(err)
	}
	queries, err := captured.queries(false, captured.reader)
	if err != nil {
		test.Fatal(err)
	}
	want, err := queries.Project(ctx, captured.reader.repo, domain.EffectiveMemoryRequest{Selection: captured.selection, Limit: 50})
	if err != nil || bundle.Header.View.DeliveryStateHash != want.DeliveryStateHash {
		test.Fatal("complete result changed legacy delivery proof", err)
	}
	var items []domain.EffectiveMemoryItem
	for ordinal, chunk := range bundle.Chunks {
		decoded, err := domain.DecodeMemoryReadChunk(ctx, bundle.Header, ordinal, chunk.Bytes)
		if err != nil {
			test.Fatal(err)
		}
		items = append(items, decoded...)
	}
	if !reflect.DeepEqual(items, want.Items) {
		test.Fatal("prepared bundle changed content, assessments or provenance")
	}
	confirmed := 0
	for _, item := range items {
		if item.IntegrationReceipt != "" && item.State == "applied" {
			confirmed++
		}
	}
	if confirmed != 13 {
		test.Fatal("fixture did not exercise repeated inherited-source assessments", confirmed)
	}
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			got, err := buildMemoryAssessmentRead(ctx, captured, memoryAssessmentHash("synthetic-capture"))
			if err != nil || !reflect.DeepEqual(got, bundle) {
				test.Error("concurrent build reused mutable assessment state", err)
			}
		})
	}
	workers.Wait()
	pristine, err := buildMemoryAssessmentRead(ctx, captured, memoryAssessmentHash("synthetic-capture"))
	if err != nil {
		test.Fatal(err)
	}
	bundle.Chunks[0].Bytes[0] ^= 1
	again, err := buildMemoryAssessmentRead(ctx, captured, memoryAssessmentHash("synthetic-capture"))
	if err != nil || !reflect.DeepEqual(again, pristine) {
		test.Fatal("returned bundle changed captured input or subsequent builds", err)
	}
}
