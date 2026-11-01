package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func capturedMemoryFixture(test *testing.T) (*CapturedEffectiveMemory, domain.EffectiveMemoryRequest) {
	test.Helper()
	repo, snapshot := hh(test.Name()), hh("captured-memory-tip")
	digest := domain.MemoryDigest{SnapshotID: snapshot, Summary: "fixed summary", KeyFacts: []string{"one", "two"}, OpenTasks: []string{"three"}}
	raw, err := json.Marshal(digest)
	if err != nil {
		test.Fatal(err)
	}
	hash := domain.HashContent(raw)
	generation := domain.RepositoryMemorySourceGeneration{Version: 1, Graph: testProjectionGeneration(repo), Epoch: "23456789-abcd-4321-9876-123456789abc", Sequence: 7}
	view := domain.RepositoryView{Snapshots: []domain.Snapshot{{ID: snapshot, RepoID: repo, MemoryHash: hash}}, Refs: []domain.Ref{{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: snapshot}}}
	reader := &capturedMemoryReader{repo: repo, view: view, pinsCaptured: true, objects: map[domain.ContentHash]json.RawMessage{hash: raw}}
	evidence, err := store.NewGraphSourceReader(context.Background(), generation.Graph, domain.Repo{ID: repo, GitRemoteURL: "https://github.com/example/frozen"}, nil)
	if err != nil {
		test.Fatal(err)
	}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, CodeCommit: effectiveOID(3)}, Limit: 1}
	return &CapturedEffectiveMemory{generation: generation, selection: request.Selection, reader: reader, evidence: evidence}, request
}

type poisonedMemoryBlob struct{ outbound.BlobStore }

func (*poisonedMemoryBlob) GetMemory(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error) {
	panic("detached memory projection read live blob")
}

func TestCapturedMemoryProjectionNeverFallsBack(test *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"lineage", "branch", "stored"} {
		for _, content := range []string{"", "claims", "prompt"} {
			test.Run(mode+"/"+content, func(test *testing.T) {
				captured, request := capturedMemoryFixture(test)
				request.Content, captured.content = content, content
				if mode == "branch" {
					request.Selection.Branch = "main"
				}
				if mode == "stored" {
					request.Selection.MemoryHash = captured.Dependencies()[0].Hash
				}
				captured.selection = request.Selection
				projector := &Service{meta: &poisonedGraphMetadata{}, blobs: &poisonedMemoryBlob{}, projectionEvidence: captured.evidence, projectionMemory: captured.reader, projectionMemorySource: &captured.generation}
				allRequest := request
				allRequest.Limit = 50
				all, err := captured.Project(ctx, allRequest)
				if err != nil {
					test.Fatal(err)
				}
				items := []domain.EffectiveMemoryItem{}
				for pages := 0; ; pages++ {
					if pages >= 20 {
						test.Fatal("cursor failed to advance")
					}
					page, err := projector.queryEffectiveMemory(ctx, captured.reader.repo, request)
					if err != nil || page.StateHash != all.StateHash || page.DeliveryStateHash != all.DeliveryStateHash {
						test.Fatalf("frozen page changed source: %+v %v", page, err)
					}
					items = append(items, page.Items...)
					if page.NextCursor == "" {
						break
					}
					request.Cursor = page.NextCursor
				}
				if !reflect.DeepEqual(items, all.Items) {
					test.Fatal("page size changed delivery")
				}
			})
		}
	}
}

func TestCapturedMemoryScopeAndIsolation(test *testing.T) {
	ctx := context.Background()
	captured, request := capturedMemoryFixture(test)
	first, err := captured.Project(ctx, request)
	if err != nil || first.NextCursor == "" {
		test.Fatal("fixture needs a continuation", err)
	}
	for _, changed := range []domain.EffectiveMemoryRequest{
		{Selection: domain.EffectiveMemorySelection{SnapshotID: hh("foreign"), CodeCommit: request.Selection.CodeCommit}},
		{Selection: domain.EffectiveMemorySelection{SnapshotID: request.Selection.SnapshotID, CodeCommit: effectiveOID(4)}},
		{Selection: request.Selection, Content: "claims"},
	} {
		if _, err := captured.Project(ctx, changed); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("allowed capture scope widening", err)
		}
	}
	request.Cursor = first.NextCursor
	captured.generation.Sequence++
	if _, err := captured.Project(ctx, request); !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) {
		test.Fatal("accepted foreign generation cursor", err)
	}
	captured.generation.Sequence--
	request.Cursor = ""
	rows, _ := captured.reader.ListSnapshots(ctx, captured.reader.repo, "")
	rows[0].MemoryHash = hh("mutated")
	hash := captured.Dependencies()[0].Hash
	digest, _ := captured.reader.GetMemory(ctx, captured.reader.repo, hash)
	digest.KeyFacts[0] = "mutated"
	first.Items[0].Text = "mutated"
	again, err := captured.Project(ctx, request)
	if err != nil || again.Items[0].Text == "mutated" {
		test.Fatal("caller mutated captured inputs", err)
	}
	if _, err := captured.reader.GetMemory(ctx, hh("foreign"), hash); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("foreign object lookup accepted", err)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			page, err := captured.Project(ctx, request)
			if err != nil || !reflect.DeepEqual(page, again) {
				test.Error("concurrent replay changed input", err)
			}
		})
	}
	workers.Wait()
}

func TestRecordingMemoryAbsenceIntegrityAndBudget(test *testing.T) {
	ctx := context.Background()
	captured, _ := capturedMemoryFixture(test)
	hash := captured.Dependencies()[0].Hash
	digest, _ := captured.reader.GetMemory(ctx, captured.reader.repo, hash)
	for _, mode := range []string{"missing", "corrupt", "bytes", "count", "cancel", "storage", "success"} {
		test.Run(mode, func(test *testing.T) {
			reads := 0
			bound, cancel := context.WithCancel(ctx)
			defer cancel()
			reader := &recordingMemoryReader{capturedMemoryReader: &capturedMemoryReader{repo: captured.reader.repo, objects: map[domain.ContentHash]json.RawMessage{}}, read: func(context.Context, domain.ContentHash, domain.ContentHash, int) (domain.MemoryDigest, error) {
				reads++
				switch mode {
				case "missing":
					return domain.MemoryDigest{}, domain.ErrNotFound
				case "corrupt":
					return domain.MemoryDigest{SnapshotID: digest.SnapshotID, Summary: "corrupt"}, nil
				case "cancel":
					cancel()
				case "storage":
					return domain.MemoryDigest{}, domain.ErrForbidden
				}
				return digest, nil
			}}
			want := error(nil)
			switch mode {
			case "missing":
				want = domain.ErrNotFound
			case "corrupt":
				want = domain.ErrIntegrity
			case "bytes":
				reader.bytes = maxProjectionMemoryBytes - 1
				want = domain.ErrMemoryProjectionLimit
			case "count":
				for index := range maxProjectionSnapshots {
					reader.objects[domain.ContentHash(effectiveOID(index))] = nil
				}
				want = domain.ErrMemoryProjectionLimit
			case "cancel":
				want = context.Canceled
			case "storage":
				want = domain.ErrForbidden
			}
			_, err := reader.GetMemory(bound, reader.repo, hash)
			if !errors.Is(err, want) {
				test.Fatalf("got %v want %v", err, want)
			}
			if mode == "missing" || mode == "success" {
				_, err = reader.GetMemory(bound, reader.repo, hash)
				if !errors.Is(err, want) || reads != 1 {
					test.Fatal("captured read was repeated against live store", err)
				}
			} else if _, present := reader.objects[hash]; present {
				test.Fatal("failed read sealed a dependency")
			}
			if _, err := reader.capturedMemoryReader.GetMemory(ctx, reader.repo, hh("uncaptured")); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("uncaptured object became observed absence", err)
			}
		})
	}
}

func TestCapturedMemoryRejectsUnknownAndWriteSources(test *testing.T) {
	fixture := newEffectiveFixture(test)
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: fixture.root, CodeCommit: effectiveOID(3)}}
	if captured, err := fixture.svc.CaptureEffectiveMemorySource(systemTestContext(), fixture.repo, request, nil); captured != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("unknown transaction source accepted", err)
	}
	request.Cursor = "continuation"
	if _, err := fixture.svc.CaptureEffectiveMemorySource(systemTestContext(), fixture.repo, request, nil); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("capture resumed from page cursor", err)
	}
}

func TestCapturedMemoryDoesNotTrustCorruptOrMissingDependencies(test *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"corrupt", "uncaptured", "observed absent", "canceled"} {
		test.Run(mode, func(test *testing.T) {
			captured, request := capturedMemoryFixture(test)
			hash := captured.Dependencies()[0].Hash
			request.Selection.MemoryHash = hash
			captured.selection = request.Selection
			if _, err := captured.Project(ctx, request); err != nil {
				test.Fatal(err)
			}
			bound, cancel := context.WithCancel(ctx)
			defer cancel()
			want := domain.ErrIntegrity
			switch mode {
			case "corrupt":
				captured.reader.objects[hash] = json.RawMessage(`{"summary":"tampered"}`)
			case "uncaptured":
				delete(captured.reader.objects, hash)
			case "observed absent":
				captured.reader.objects[hash] = nil
				want = domain.ErrNotFound
			case "canceled":
				cancel()
				want = context.Canceled
			}
			if _, err := captured.Project(bound, request); !errors.Is(err, want) {
				test.Fatalf("dependency failure returned %v want %v", err, want)
			}
		})
	}
}

func TestCapturedMemorySnapshotFilterMatchesMetadataContract(test *testing.T) {
	captured, _ := capturedMemoryFixture(test)
	repo := captured.reader.repo
	captured.reader.view.Snapshots = []domain.Snapshot{
		{ID: hh("main-capture"), RepoID: repo, Branch: "main", SessionID: "session-a"},
		{ID: hh("feature-capture"), RepoID: repo, Branch: "feature", SessionID: "main"},
	}
	rows, err := captured.reader.ListSnapshots(context.Background(), repo, "main")
	if err != nil || len(rows) != 1 || rows[0].ID != hh("main-capture") {
		test.Fatal("snapshot branch filter became session filter", rows, err)
	}
	captured.reader.pinsCaptured = false
	if _, err := captured.reader.ListPRContributionMemories(context.Background(), repo); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("uncaptured PR pins became an empty ledger", err)
	}
	if _, err := captured.reader.ListGitContributionMemories(context.Background(), repo); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("uncaptured Git pins became an empty ledger", err)
	}
}

type capturedMemoryCancelAfterCheck struct {
	context.Context
	checks int
	after  int
	cancel context.CancelFunc
}

func (bound *capturedMemoryCancelAfterCheck) Err() error {
	bound.checks++
	err := bound.Context.Err()
	if bound.checks == bound.after && bound.cancel != nil {
		bound.cancel()
	}
	return err
}

func TestCapturedMemoryEmptyPageRejectsLateCancellation(test *testing.T) {
	captured, request := capturedMemoryFixture(test)
	request.Content, captured.content = "claims", "claims"
	projector := &Service{projectionEvidence: captured.evidence, projectionMemory: captured.reader, projectionMemorySource: &captured.generation}
	measured := &capturedMemoryCancelAfterCheck{Context: context.Background()}
	page, err := projector.queryEffectiveMemory(measured, captured.reader.repo, request)
	if err != nil || page.Total != 0 || measured.checks == 0 {
		test.Fatal("fixture must complete an empty claims page", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bound := &capturedMemoryCancelAfterCheck{Context: ctx, after: measured.checks + 1, cancel: cancel}
	page, err = captured.Project(bound, request)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) || ctx.Err() == nil {
		test.Fatal("cancellation after proof completion returned a completed empty page", err)
	}
}
