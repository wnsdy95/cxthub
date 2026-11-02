//go:build postgres

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type documentPublicationStorePG interface {
	outbound.MetadataStore
	outbound.BlobStore
	outbound.RepositoryStore
}

func documentPublicationServicePG(storage documentPublicationStorePG) *Service {
	return NewService(storage, storage, nil, gitengine.NewEngine(storage), storage)
}

func documentPublicationOwnerPG(test *testing.T, service *Service) *DocumentPublicationCommands {
	test.Helper()
	public, err := service.DocumentPublicationCommands()
	if err != nil {
		test.Fatal(err)
	}
	commands, ok := public.(*DocumentPublicationCommands)
	if !ok || commands == nil {
		test.Fatalf("public complete factory returned %T, not the independent publication owner", public)
	}
	service.meta, service.blobs, service.repositories, service.engine, service.auth = nil, nil, nil, nil, nil
	return commands
}

type documentPublicationProbePG struct {
	*store.PostgresStore
	calls         map[string]int
	transactions  int
	failAt        string
	failure       error
	sourceFailure error
	cancel        context.CancelFunc
}

func (storage *documentPublicationProbePG) WithinRepository(ctx context.Context, repo domain.ContentHash, operation func(context.Context) error) error {
	if !storage.InTransaction(ctx) {
		storage.transactions++
	}
	return storage.PostgresStore.WithinRepository(ctx, repo, operation)
}

func (storage *documentPublicationProbePG) note(ctx context.Context, phase string) error {
	storage.calls[phase]++
	if !storage.InTransaction(ctx) || storage.InReadOnlyTransaction(ctx) {
		return errors.New("publication port escaped the writable repository transaction")
	}
	return nil
}

func (storage *documentPublicationProbePG) after(phase string) error {
	if storage.failAt == phase {
		if storage.cancel != nil {
			storage.cancel()
			return nil
		}
		return storage.failure
	}
	return nil
}

func (storage *documentPublicationProbePG) PutDoc(ctx context.Context, repo domain.ContentHash, document domain.SessionDoc) (bool, error) {
	if err := storage.note(ctx, "legacy_doc"); err != nil {
		return false, err
	}
	return false, errors.New("complete publication fell back to the legacy document writer")
}

func (storage *documentPublicationProbePG) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	if err := storage.note(ctx, "legacy_read"); err != nil {
		return domain.SessionDoc{}, err
	}
	return storage.PostgresStore.GetDoc(ctx, repo, hash)
}

func (storage *documentPublicationProbePG) VerifyStoredDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedDocReference, error) {
	if err := storage.note(ctx, "source"); err != nil {
		return domain.VerifiedDocReference{}, err
	}
	if storage.sourceFailure != nil {
		return domain.VerifiedDocReference{}, storage.sourceFailure
	}
	return storage.PostgresStore.VerifyStoredDoc(ctx, repo, hash)
}

func (storage *documentPublicationProbePG) PutVerifiedDoc(ctx context.Context, repo domain.ContentHash, document domain.VerifiedSessionDoc) (bool, error) {
	if err := storage.note(ctx, "verified_doc"); err != nil {
		return false, err
	}
	stored, err := storage.PostgresStore.PutVerifiedDoc(ctx, repo, document)
	if err != nil {
		return false, err
	}
	return stored, storage.after("verified_doc")
}

func (storage *documentPublicationProbePG) PutSnapshot(ctx context.Context, snapshot domain.Snapshot) error {
	if err := storage.note(ctx, "snapshot"); err != nil {
		return err
	}
	if err := storage.PostgresStore.PutSnapshot(ctx, snapshot); err != nil {
		return err
	}
	return storage.after("snapshot")
}

func (storage *documentPublicationProbePG) PutChunks(ctx context.Context, repo domain.ContentHash, chunks map[domain.ContentHash][]byte) (int, int, error) {
	if err := storage.note(ctx, "chunks"); err != nil {
		return 0, 0, err
	}
	stored, deduped, err := storage.PostgresStore.PutChunks(ctx, repo, chunks)
	if err != nil {
		return 0, 0, err
	}
	return stored, deduped, storage.after("chunks")
}

func (storage *documentPublicationProbePG) AdvanceRepositoryRevision(ctx context.Context, repo domain.ContentHash, pending bool) error {
	if err := storage.note(ctx, "revision"); err != nil {
		return err
	}
	if err := storage.PostgresStore.AdvanceRepositoryRevision(ctx, repo, pending); err != nil {
		return err
	}
	return storage.after("revision")
}

func (storage *documentPublicationProbePG) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	if err := storage.note(ctx, "audit"); err != nil {
		return err
	}
	if err := storage.PostgresStore.AppendOrganizationAudit(ctx, event); err != nil {
		return err
	}
	return storage.after("audit")
}

func (storage *documentPublicationProbePG) PutMemory(ctx context.Context, repo domain.ContentHash, digest domain.MemoryDigest) (domain.ContentHash, error) {
	if err := storage.note(ctx, "memory"); err != nil {
		return "", err
	}
	hash, err := storage.PostgresStore.PutMemory(ctx, repo, digest)
	if err != nil {
		return "", err
	}
	return hash, storage.after("memory")
}

func (storage *documentPublicationProbePG) CompareAndSwapSnapshotMemory(ctx context.Context, repo, snapshot, expected, next domain.ContentHash) error {
	if err := storage.note(ctx, "memory_cas"); err != nil {
		return err
	}
	if err := storage.PostgresStore.CompareAndSwapSnapshotMemory(ctx, repo, snapshot, expected, next); err != nil {
		return err
	}
	return storage.after("memory_cas")
}

func documentPublicationAuditsPG(test *testing.T, fixture settingsCodexFixturePG) []domain.OrganizationAuditEvent {
	test.Helper()
	events, err := fixture.storage.ListOrganizationAudit(fixture.ctx, fixture.team.organization.ID, 100)
	if err != nil {
		test.Fatal(err)
	}
	selected := []domain.OrganizationAuditEvent{}
	for _, event := range events {
		if strings.HasPrefix(event.Action, "context.") {
			selected = append(selected, event)
		}
	}
	return selected
}

func documentPublicationImagePG(test *testing.T, fixture settingsCodexFixturePG) string {
	test.Helper()
	metadata, err := fixture.storage.GetRepo(fixture.ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	revision, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	snapshots, err := fixture.storage.ListSnapshots(fixture.ctx, fixture.repo, "")
	if err != nil {
		test.Fatal(err)
	}
	refs, err := fixture.storage.ListRefs(fixture.ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	reflog, err := fixture.storage.ReadReflog(fixture.ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	history, err := fixture.storage.ListHistoryEvents(fixture.ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	pending, err := fixture.storage.ListPendings(fixture.ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	notifications, err := fixture.storage.ListNotifications(fixture.ctx, fixture.team.repository.ID)
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := json.Marshal([]any{metadata, revision, snapshots, refs, reflog, history, pending, notifications, documentPublicationAuditsPG(test, fixture)})
	if err != nil {
		test.Fatal(err)
	}
	return string(encoded)
}

func documentPublicationAuditPG(test *testing.T, fixture settingsCodexFixturePG, before []domain.OrganizationAuditEvent, action string) {
	test.Helper()
	after := documentPublicationAuditsPG(test, fixture)
	if len(after) != len(before)+1 {
		test.Fatalf("publication audits=%d, want %d", len(after), len(before)+1)
	}
	previous := make(map[string]domain.OrganizationAuditEvent, len(before))
	for _, event := range before {
		previous[event.ID] = event
	}
	for _, event := range after {
		if prior, found := previous[event.ID]; found {
			if !reflect.DeepEqual(prior, event) {
				test.Fatal("publication changed a prior audit")
			}
			continue
		}
		if event.Action != action || event.ActorID != fixture.team.member.ID || event.OrganizationID != fixture.team.organization.ID || event.TargetType != "repository" || event.TargetID != fixture.team.repository.ID {
			test.Fatalf("incorrect publication audit: %+v", event)
		}
	}
}

func documentPublicationChunkCountsPG(test *testing.T, observer *pgxpool.Pool, fixture settingsCodexFixturePG, hash domain.ContentHash) [2]int {
	test.Helper()
	return documentPublicationBlobCountsPG(test, observer, fixture, "chunk", hash)
}

func documentPublicationBlobCountsPG(test *testing.T, observer *pgxpool.Pool, fixture settingsCodexFixturePG, kind string, hash domain.ContentHash) [2]int {
	test.Helper()
	var counts [2]int
	if err := observer.QueryRow(fixture.ctx, `SELECT
		(SELECT count(*) FROM blobs WHERE hash=$1),
		(SELECT count(*) FROM repo_blobs WHERE repo_id=$2 AND kind=$3 AND hash=$1)`, string(hash), string(fixture.repo), kind).Scan(&counts[0], &counts[1]); err != nil {
		test.Fatal(err)
	}
	return counts
}

func TestPGDocumentPublicationCommandsIndependentCommitMetadata(test *testing.T) {
	fixture := settingsCodexPG(test)
	probe := &documentPublicationProbePG{PostgresStore: fixture.storage, calls: make(map[string]int)}
	commands := documentPublicationOwnerPG(test, documentPublicationServicePG(probe))
	member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	document := makeCommitDoc(test, string(fixture.repo)+" independent publication")
	snapshot := numericPublicationSnapshot(fixture.repo, document)
	snapshot.Branch, snapshot.Message = domain.StashBranchLabel, "capture"
	snapshot.Branches = []string{"forged-membership"}
	snapshot.MemoryHash = hh(string(fixture.repo) + " forged-memory")
	input := inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{snapshot}}
	revision, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	audits := documentPublicationAuditsPG(test, fixture)
	output, err := commands.Commit(member, input)
	if err != nil || output != (inbound.CommitOutput{StoredDocs: 1, StoredSnapshots: 1}) || probe.calls["verified_doc"] != 1 || probe.calls["legacy_doc"] != 0 {
		test.Fatal("independent complete commit did not use the verified native writer", output, probe.calls, err)
	}
	documentPublicationAuditPG(test, fixture, audits, "context.objects.stored")
	stored, err := fixture.storage.GetSnapshot(fixture.ctx, fixture.repo, document.Hash)
	if err != nil || stored.MemoryHash != "" || len(stored.Branches) != 0 || stored.Branch != domain.StashBranchLabel {
		test.Fatal("client-only branches or memory became authoritative", stored, err)
	}
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	proof, err := fixture.storage.ReadVerifiedDoc(fixture.ctx, fixture.repo, document.Hash)
	if err != nil || !bytes.Equal(proof.Bytes(), canonical) {
		test.Fatal("verified publication changed canonical CAS bytes", err)
	}
	digest := domain.MemoryDigest{SnapshotID: document.Hash, Summary: "server-owned memory"}
	memory, err := fixture.storage.PutMemory(fixture.ctx, fixture.repo, digest)
	if err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.CompareAndSwapSnapshotMemory(fixture.ctx, fixture.repo, document.Hash, "", memory); err != nil {
		test.Fatal(err)
	}
	input.Docs = nil
	input.Snapshots[0].Branch, input.Snapshots[0].Message = "main", "promoted commit"
	for attempt := 0; attempt < 2; attempt++ {
		audits := documentPublicationAuditsPG(test, fixture)
		output, err := commands.Commit(member, input)
		if err != nil || output != (inbound.CommitOutput{DedupedSnapshots: 1}) {
			test.Fatal("metadata promotion or replay failed", output, err)
		}
		documentPublicationAuditPG(test, fixture, audits, "context.objects.stored")
		stored, err := fixture.storage.GetSnapshot(fixture.ctx, fixture.repo, document.Hash)
		if err != nil || stored.Branch != "main" || stored.Message != "promoted commit" || stored.MemoryHash != memory || stored.DocumentRef() != document.DocumentRef() || len(stored.Branches) != 0 {
			test.Fatal("metadata promotion lost identity or overwrote the memory CAS", stored, err)
		}
		current, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo)
		expected := revision
		expected.Graph++
		if err != nil || current != expected || probe.calls["revision"] != 1 {
			test.Fatal("logical metadata must dirty the graph exactly once, not on replay", current, expected, probe.calls, err)
		}
	}
	before := documentPublicationImagePG(test, fixture)
	for _, invalid := range []string{"natural-parents", "graft-sequence", "graft-parent"} {
		forged := input
		forged.Snapshots = append([]domain.Snapshot(nil), input.Snapshots...)
		switch invalid {
		case "natural-parents":
			forged.Snapshots[0].Parents = []domain.ContentHash{hh(string(fixture.repo) + invalid)}
		case "graft-sequence":
			forged.Snapshots[0].GraftSeq = 1
		case "graft-parent":
			forged.Snapshots[0].GraftParents = []domain.ContentHash{hh(string(fixture.repo) + invalid)}
		}
		if output, err := commands.Commit(member, forged); !errors.Is(err, domain.ErrIntegrity) || output != (inbound.CommitOutput{}) {
			test.Fatal("publication accepted forged graph metadata", invalid, output, err)
		}
		if after := documentPublicationImagePG(test, fixture); after != before {
			test.Fatal("rejected graph metadata changed durable state", invalid)
		}
	}
	if probe.calls["source"] != 5 || probe.calls["legacy_read"] != 0 {
		test.Fatal("metadata attachment did not verify current owned sources directly", probe.calls)
	}
}

func TestPGDocumentPublicationCommandsChunksValidateBeforeEffects(test *testing.T) {
	fixture := settingsCodexPG(test)
	probe := &documentPublicationProbePG{PostgresStore: fixture.storage, calls: make(map[string]int)}
	commands := documentPublicationOwnerPG(test, documentPublicationServicePG(probe))
	observer := authorityScopeObserver(test, fixture.ctx)
	member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	body := []byte(string(fixture.repo) + " staged chunk")
	chunk := inbound.ChunkObject{Hash: domain.HashContent(body), Data: body}
	revision, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	for attempt, expected := range []inbound.StoreChunksOutput{{Stored: 1}, {Deduped: 1}} {
		audits := documentPublicationAuditsPG(test, fixture)
		output, err := commands.StoreChunks(member, inbound.StoreChunksInput{RepoID: fixture.repo, Chunks: []inbound.ChunkObject{chunk, chunk}})
		if err != nil || output != expected {
			test.Fatal("duplicate chunk publication or retry acknowledgement", attempt, output, expected, err)
		}
		documentPublicationAuditPG(test, fixture, audits, "context.chunks.stored")
	}
	if current, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo); err != nil || current != revision || probe.calls["revision"] != 0 {
		test.Fatal("chunk staging dirtied graph or pending versions", current, revision, probe.calls, err)
	}
	if stored, err := fixture.storage.GetChunk(fixture.ctx, fixture.repo, chunk.Hash); err != nil || !bytes.Equal(stored, body) {
		test.Fatal("stored chunk differs from its CAS body", err)
	}
	for _, scenario := range []struct {
		name   string
		chunks []inbound.ChunkObject
		cause  error
	}{
		{name: "empty-batch", cause: domain.ErrValidation},
		{name: "empty-body", chunks: []inbound.ChunkObject{{Hash: domain.HashContent(nil)}}, cause: domain.ErrValidation},
		{name: "bad-hash", chunks: []inbound.ChunkObject{{Hash: "invalid", Data: body}}, cause: domain.ErrIntegrity},
		{name: "valid-prefix-corrupt-body", chunks: []inbound.ChunkObject{chunk, {Hash: hh(string(fixture.repo) + "wrong-body"), Data: body}}, cause: domain.ErrIntegrity},
		{name: "duplicate-disagreement", chunks: []inbound.ChunkObject{chunk, {Hash: chunk.Hash, Data: []byte("different")}}, cause: domain.ErrIntegrity},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			candidate := []byte(string(fixture.repo) + scenario.name)
			prefix := inbound.ChunkObject{Hash: domain.HashContent(candidate), Data: candidate}
			chunks := append([]inbound.ChunkObject(nil), scenario.chunks...)
			if len(chunks) > 1 {
				chunks = append([]inbound.ChunkObject{prefix}, chunks...)
			}
			before := documentPublicationImagePG(test, fixture)
			objects := documentPublicationChunkCountsPG(test, observer, fixture, prefix.Hash)
			writes := probe.calls["chunks"]
			output, err := commands.StoreChunks(member, inbound.StoreChunksInput{RepoID: fixture.repo, Chunks: chunks})
			if !errors.Is(err, scenario.cause) || output != (inbound.StoreChunksOutput{}) || probe.calls["chunks"] != writes {
				test.Fatal("invalid chunk batch reached the native writer or returned payload", output, probe.calls, err)
			}
			if after := documentPublicationImagePG(test, fixture); after != before {
				test.Fatal("invalid chunk batch changed repository metadata or audit")
			}
			if after := documentPublicationChunkCountsPG(test, observer, fixture, prefix.Hash); after != objects {
				test.Fatal("invalid chunk batch leaked a valid prefix CAS object or grant", objects, after)
			}
		})
	}
}

func TestPGDocumentPublicationCommandsRollbackNativeAndBoundTransactions(test *testing.T) {
	for _, operation := range []string{"commit", "chunks"} {
		phases := []string{"body", "audit", "outer"}
		if operation == "commit" {
			phases = append(phases, "verified_doc", "cancel")
		}
		for _, phase := range phases {
			test.Run(operation+"/"+phase, func(test *testing.T) {
				fixture := settingsCodexPG(test)
				observer := authorityScopeObserver(test, fixture.ctx)
				failure := errors.New("publication rollback after native " + phase)
				probe := &documentPublicationProbePG{PostgresStore: fixture.storage, calls: make(map[string]int), failAt: phase, failure: failure}
				if phase == "body" {
					probe.failAt = "snapshot"
					if operation == "chunks" {
						probe.failAt = "chunks"
					}
				}
				member, cancel := context.WithCancel(inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID))
				defer cancel()
				if phase == "cancel" {
					probe.failAt, probe.cancel, failure = "verified_doc", cancel, context.Canceled
				}
				commands := documentPublicationOwnerPG(test, documentPublicationServicePG(probe))
				document := makeCommitDoc(test, string(fixture.repo)+" rollback candidate")
				input := inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{numericPublicationSnapshot(fixture.repo, document)}}
				body := []byte(string(fixture.repo) + " rollback chunk")
				chunk := inbound.ChunkObject{Hash: domain.HashContent(body), Data: body}
				before := documentPublicationImagePG(test, fixture)
				objects := numericPublicationPGObjectCounts(test, observer, fixture.repo, input.Docs)
				chunkObjects := documentPublicationChunkCountsPG(test, observer, fixture, chunk.Hash)
				run := func(ctx context.Context) error {
					if operation == "commit" {
						output, err := commands.Commit(ctx, input)
						if err != nil && output != (inbound.CommitOutput{}) {
							return errors.New("failed commit exposed partial acknowledgement")
						}
						return err
					}
					output, err := commands.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: fixture.repo, Chunks: []inbound.ChunkObject{chunk}})
					if err != nil && output != (inbound.StoreChunksOutput{}) {
						return errors.New("failed chunk staging exposed partial acknowledgement")
					}
					return err
				}
				var err error
				if phase == "outer" {
					err = probe.WithinRepository(member, fixture.repo, func(bound context.Context) error {
						if err := run(bound); err != nil {
							return err
						}
						visible := fixture
						visible.ctx = bound
						if documentPublicationImagePG(test, visible) == before {
							return errors.New("bound outer transaction did not see native body and audit writes")
						}
						if documentPublicationImagePG(test, fixture) != before {
							return errors.New("bound publication committed ahead of its outer transaction")
						}
						return failure
					})
				} else {
					err = run(member)
				}
				if !errors.Is(err, failure) || probe.transactions != 1 || probe.calls["legacy_doc"] != 0 {
					test.Fatal("native rollback boundary or transaction ownership changed", probe.calls, probe.transactions, err)
				}
				if operation == "commit" && (probe.calls["verified_doc"] != 1 || (phase != "verified_doc" && phase != "cancel" && probe.calls["snapshot"] != 1)) {
					test.Fatal("rollback did not reach actual verified document and snapshot writes", probe.calls)
				}
				if operation == "chunks" && probe.calls["chunks"] != 1 {
					test.Fatal("rollback did not reach actual chunk storage", probe.calls)
				}
				if after := documentPublicationImagePG(test, fixture); after != before {
					test.Fatal("rollback changed snapshot, graph/pending/evidence versions, audit, refs or outbox")
				}
				if after := numericPublicationPGObjectCounts(test, observer, fixture.repo, input.Docs); after != objects {
					test.Fatal("rollback leaked global document CAS, indexes or ownership", objects, after)
				}
				if after := documentPublicationChunkCountsPG(test, observer, fixture, chunk.Hash); after != chunkObjects {
					test.Fatal("rollback leaked global staged chunk CAS or ownership", chunkObjects, after)
				}
			})
		}
	}
}

func TestPGDocumentPublicationCommandsNestedScopeKeepsOneAudit(test *testing.T) {
	fixture := settingsCodexPG(test)
	probe := &documentPublicationProbePG{PostgresStore: fixture.storage, calls: make(map[string]int)}
	commands := documentPublicationOwnerPG(test, documentPublicationServicePG(probe))
	work := newRepositoryWork(probe, probe, ReadDocumentIdentities(probe, probe))
	member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	document := makeCommitDoc(test, string(fixture.repo)+" nested owner")
	body := []byte(string(fixture.repo) + " nested chunk")
	chunk := inbound.ChunkObject{Hash: domain.HashContent(body), Data: body}
	before := documentPublicationImagePG(test, fixture)
	audits := documentPublicationAuditsPG(test, fixture)
	revision, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	callbacks := 0
	abort := errors.New("outer nested publication abort")
	for _, rollback := range []bool{true, false} {
		_, err := repositoryWriteWithWork(member, work, fixture.repo, func(bound context.Context) (struct{}, error) {
			if output, err := commands.Commit(bound, inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{numericPublicationSnapshot(fixture.repo, document)}}); err != nil || output != (inbound.CommitOutput{StoredDocs: 1, StoredSnapshots: 1}) {
				return struct{}{}, errors.New("nested commit did not publish inside its enclosing scope")
			}
			if output, err := commands.StoreChunks(bound, inbound.StoreChunksInput{RepoID: fixture.repo, Chunks: []inbound.ChunkObject{chunk}}); err != nil || output != (inbound.StoreChunksOutput{Stored: 1}) {
				return struct{}{}, errors.New("nested chunk command did not publish inside its enclosing scope")
			}
			afterRepositoryCommit(bound, func() { callbacks++ })
			if callbacks != 0 || len(documentPublicationAuditsPG(test, fixture)) != len(audits) {
				return struct{}{}, errors.New("nested publication audited or ran completion before outer commit")
			}
			if rollback {
				return struct{}{}, abort
			}
			return struct{}{}, nil
		})
		if rollback {
			if !errors.Is(err, abort) || callbacks != 0 || documentPublicationImagePG(test, fixture) != before {
				test.Fatal("nested rollback leaked effects or completion", callbacks, err)
			}
		} else if err != nil || callbacks != 1 {
			test.Fatal("nested publication did not complete once with its outer scope", callbacks, err)
		}
	}
	documentPublicationAuditPG(test, fixture, audits, "context.updated")
	expected := revision
	expected.Graph++
	if current, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo); err != nil || current != expected || probe.transactions != 2 || probe.calls["audit"] != 1 {
		test.Fatal("nested owners created additional transaction, revision or audit scopes", current, expected, probe.transactions, probe.calls, err)
	}
}

func TestPGDocumentPublicationCommandsRejectIncompatibleBoundTransactions(test *testing.T) {
	fixture := settingsCodexPG(test)
	peer, _ := authorityScopePeer(test, fixture.ctx)
	probe := &documentPublicationProbePG{PostgresStore: fixture.storage, calls: make(map[string]int)}
	commands := documentPublicationOwnerPG(test, documentPublicationServicePG(probe))
	foreign := documentPublicationOwnerPG(test, documentPublicationServicePG(peer))
	member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	document := makeCommitDoc(test, string(fixture.repo)+" incompatible bound publication")
	input := inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{numericPublicationSnapshot(fixture.repo, document)}}
	before := documentPublicationImagePG(test, fixture)
	for _, boundary := range []string{"other-repository", "other-store", "read-only"} {
		test.Run(boundary, func(test *testing.T) {
			check := func(bound context.Context) error {
				selected, candidate := commands, input
				if boundary == "other-store" {
					selected = foreign
				}
				if boundary == "other-repository" {
					candidate.RepoID = hh(string(fixture.repo) + " incompatible repository scope")
				}
				output, err := selected.Commit(bound, candidate)
				if !errors.Is(err, domain.ErrConflict) || output != (inbound.CommitOutput{}) {
					return errors.New("publication escaped an incompatible bound transaction")
				}
				return nil
			}
			var err error
			if boundary == "read-only" {
				err = fixture.storage.WithinReadSnapshot(member, check)
			} else {
				err = fixture.storage.WithinRepository(member, fixture.repo, check)
			}
			if err != nil || len(probe.calls) != 0 || documentPublicationImagePG(test, fixture) != before {
				test.Fatal("incompatible transaction returned payload or left logical effects", probe.calls, err)
			}
		})
	}
}

func TestPGDocumentPublicationCommandsCurrentSourcesAndCAS(test *testing.T) {
	for _, state := range []string{"corrupt-source", "revoked-source", "foreign-repository", "source-error", "corrupt-cas"} {
		test.Run(state, func(test *testing.T) {
			fixture := settingsCodexPG(test)
			observer := authorityScopeObserver(test, fixture.ctx)
			probe := &documentPublicationProbePG{PostgresStore: fixture.storage, calls: make(map[string]int)}
			commands := documentPublicationOwnerPG(test, documentPublicationServicePG(probe))
			member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
			document := makeCommitDoc(test, string(fixture.repo)+" current source")
			ownerRepo := fixture.repo
			if state == "foreign-repository" {
				ownerRepo = hh(string(fixture.repo) + " foreign owner")
				foreign, err := fixture.team.identity.CreateOrganizationRepository(fixture.ctx, fixture.team.owner, fixture.team.organization.ID, "Foreign")
				if err != nil {
					test.Fatal(err)
				}
				if _, err := fixture.storage.PutRepo(fixture.ctx, domain.Repo{ID: ownerRepo, RepositoryID: foreign.ID}); err != nil {
					test.Fatal(err)
				}
			}
			if _, err := fixture.storage.PutDoc(fixture.ctx, ownerRepo, document); err != nil {
				test.Fatal(err)
			}
			if _, err := fixture.storage.VerifyStoredDoc(fixture.ctx, ownerRepo, document.Hash); err != nil {
				test.Fatal("failed to warm real source proof", err)
			}
			failure := domain.ErrIntegrity
			switch state {
			case "corrupt-source", "corrupt-cas":
				changed := makeCommitDoc(test, string(fixture.repo)+" replaced under existing CAS key")
				raw, err := domain.CanonicalBytes(changed.CIR)
				if err != nil {
					test.Fatal(err)
				}
				if updated, err := observer.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(document.Hash), raw); err != nil || updated.RowsAffected() != 1 {
					test.Fatal("source tamper did not change the actual stored object", updated, err)
				}
			case "revoked-source":
				if removed, err := observer.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, string(fixture.repo), string(document.Hash)); err != nil || removed.RowsAffected() != 1 {
					test.Fatal("source revocation did not remove actual ownership", removed, err)
				}
			case "source-error":
				failure = errors.New("strong stored-source verifier unavailable")
				probe.sourceFailure = failure
			}
			input := inbound.CommitInput{RepoID: fixture.repo, Snapshots: []domain.Snapshot{numericPublicationSnapshot(fixture.repo, document)}}
			if state == "corrupt-cas" {
				input.Docs = []domain.SessionDoc{document}
			}
			before := documentPublicationImagePG(test, fixture)
			objects := numericPublicationPGObjectCounts(test, observer, fixture.repo, []domain.SessionDoc{document})
			var original []byte
			if err := observer.QueryRow(fixture.ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(document.Hash)).Scan(&original); err != nil {
				test.Fatal(err)
			}
			output, err := commands.Commit(member, input)
			if !errors.Is(err, failure) || output != (inbound.CommitOutput{}) || probe.calls["legacy_doc"] != 0 || probe.calls["legacy_read"] != 0 {
				test.Fatal("stale source proof, foreign grant or corrupt CAS was accepted or downgraded", output, probe.calls, err)
			}
			expectedSource, expectedWriter := 1, 0
			if state == "corrupt-cas" {
				expectedSource, expectedWriter = 0, 1
			}
			if probe.calls["source"] != expectedSource || probe.calls["verified_doc"] != expectedWriter || probe.calls["snapshot"] != 0 || probe.calls["audit"] != 0 {
				test.Fatal("refusal did not occur at the actual current-source or CAS boundary", probe.calls)
			}
			if after := documentPublicationImagePG(test, fixture); after != before {
				test.Fatal("source refusal changed logical repository state")
			}
			if after := numericPublicationPGObjectCounts(test, observer, fixture.repo, []domain.SessionDoc{document}); after != objects {
				test.Fatal("source refusal created ownership or repaired corrupt CAS behind the caller", objects, after)
			}
			var current []byte
			if err := observer.QueryRow(fixture.ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(document.Hash)).Scan(&current); err != nil || !bytes.Equal(current, original) {
				test.Fatal("source refusal silently rewrote the actual stored CAS bytes", err)
			}
		})
	}
}

func TestPGDocumentPublicationCommandsAuthorityLockOrdering(test *testing.T) {
	for _, operation := range []string{"commit", "chunks"} {
		for _, order := range []string{"revoke-first", "write-first"} {
			test.Run(operation+"/"+order, func(test *testing.T) {
				fixture := settingsCodexPG(test)
				observer := authorityScopeObserver(test, fixture.ctx)
				writer, writerName := authorityScopePeer(test, fixture.ctx)
				authority, authorityName := authorityScopePeer(test, fixture.ctx)
				member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
				document := makeCommitDoc(test, string(fixture.repo)+" authority candidate")
				body := []byte(string(fixture.repo) + " authority chunk")
				chunk := inbound.ChunkObject{Hash: domain.HashContent(body), Data: body}
				run := func(commands *DocumentPublicationCommands) error {
					if operation == "commit" {
						output, err := commands.Commit(member, inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{numericPublicationSnapshot(fixture.repo, document)}})
						if err != nil && output != (inbound.CommitOutput{}) {
							return errors.New("revoked commit exposed partial output")
						}
						return err
					}
					output, err := commands.StoreChunks(member, inbound.StoreChunksInput{RepoID: fixture.repo, Chunks: []inbound.ChunkObject{chunk}})
					if err != nil && output != (inbound.StoreChunksOutput{}) {
						return errors.New("revoked chunk command exposed partial output")
					}
					return err
				}
				before := documentPublicationImagePG(test, fixture)
				objects := numericPublicationPGObjectCounts(test, observer, fixture.repo, []domain.SessionDoc{document})
				chunkObjects := documentPublicationChunkCountsPG(test, observer, fixture, chunk.Hash)
				release := make(chan struct{})
				var released sync.Once
				defer released.Do(func() { close(release) })
				first, second := make(chan error, 1), make(chan error, 1)
				var commands *DocumentPublicationCommands
				if order == "revoke-first" {
					paused := &authorityScopeHoldPG{PostgresStore: authority, ready: make(chan struct{}), release: release}
					commands = documentPublicationOwnerPG(test, documentPublicationServicePG(writer))
					go func() {
						first <- authorityScopeMutation(fixture.ctx, NewIdentityService(nil, paused), fixture.team, "remove")
					}()
					authorityScopeAwait(test, fixture.ctx, paused.ready, first)
					go func() { second <- run(commands) }()
					authorityScopeBlocked(test, fixture.ctx, observer, writerName, authorityName, "repositories", second)
				} else {
					paused := &authorityScopeWriteHoldPG{PostgresStore: writer, ready: make(chan struct{}), release: release}
					commands = documentPublicationOwnerPG(test, documentPublicationServicePG(paused))
					go func() { first <- run(commands) }()
					authorityScopeAwait(test, fixture.ctx, paused.ready, first)
					go func() {
						second <- authorityScopeMutation(fixture.ctx, NewIdentityService(nil, authority), fixture.team, "remove")
					}()
					authorityScopeBlocked(test, fixture.ctx, observer, authorityName, writerName, "repositories", second)
				}
				if documentPublicationImagePG(test, fixture) != before || numericPublicationPGObjectCounts(test, observer, fixture.repo, []domain.SessionDoc{document}) != objects || documentPublicationChunkCountsPG(test, observer, fixture, chunk.Hash) != chunkObjects {
					test.Fatal("held publication exposed uncommitted metadata, audit or physical CAS")
				}
				released.Do(func() { close(release) })
				if err := authorityScopeDone(test, fixture.ctx, first); err != nil {
					test.Fatal(err)
				}
				secondError := authorityScopeDone(test, fixture.ctx, second)
				if order == "revoke-first" {
					if !errors.Is(secondError, domain.ErrForbidden) || documentPublicationImagePG(test, fixture) != before || numericPublicationPGObjectCounts(test, observer, fixture.repo, []domain.SessionDoc{document}) != objects || documentPublicationChunkCountsPG(test, observer, fixture, chunk.Hash) != chunkObjects {
						test.Fatal("queued independent owner retained revoked authority or left effects", secondError)
					}
				} else if secondError != nil || documentPublicationImagePG(test, fixture) == before {
					test.Fatal("in-flight authorized publication did not finish before revocation", secondError)
				}
				committed := documentPublicationImagePG(test, fixture)
				if err := run(commands); !errors.Is(err, domain.ErrForbidden) || documentPublicationImagePG(test, fixture) != committed {
					test.Fatal("reused public owner did not recheck current membership", err)
				}
			})
		}
	}
}

func TestPGDocumentPublicationCommandsCancellationAtSQLLock(test *testing.T) {
	fixture := settingsCodexPG(test)
	observer := authorityScopeObserver(test, fixture.ctx)
	holder, holderName := authorityScopePeer(test, fixture.ctx)
	writer, writerName := authorityScopePeer(test, fixture.ctx)
	probe := &documentPublicationProbePG{PostgresStore: writer, calls: make(map[string]int)}
	commands := documentPublicationOwnerPG(test, documentPublicationServicePG(probe))
	before := documentPublicationImagePG(test, fixture)
	ready, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	defer released.Do(func() { close(release) })
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- holder.WithinRepository(fixture.ctx, fixture.repo, func(bound context.Context) error {
			close(ready)
			select {
			case <-release:
				return nil
			case <-bound.Done():
				return bound.Err()
			}
		})
	}()
	authorityScopeAwait(test, fixture.ctx, ready, holderDone)
	request, cancel := context.WithCancel(inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID))
	defer cancel()
	document := makeCommitDoc(test, string(fixture.repo)+" cancelled at graph lock")
	done := make(chan error, 1)
	go func() {
		output, err := commands.Commit(request, inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{numericPublicationSnapshot(fixture.repo, document)}})
		if output != (inbound.CommitOutput{}) {
			done <- errors.New("cancelled SQL admission returned publication payload")
			return
		}
		done <- err
	}()
	authorityScopeBlocked(test, fixture.ctx, observer, writerName, holderName, "pg_advisory_xact_lock", done)
	cancel()
	if err := authorityScopeDone(test, fixture.ctx, done); !errors.Is(err, context.Canceled) || len(probe.calls) != 0 {
		test.Fatal("cancelled owner passed the real PostgreSQL lock boundary", probe.calls, err)
	}
	released.Do(func() { close(release) })
	if err := authorityScopeDone(test, fixture.ctx, holderDone); err != nil {
		test.Fatal(err)
	}
	if documentPublicationImagePG(test, fixture) != before {
		test.Fatal("cancelled lock waiter left publication effects")
	}
}

func TestPGDocumentPublicationCommandsNumericAdmissionThroughPublicOwner(test *testing.T) {
	fixture := settingsCodexPG(test)
	observer := authorityScopeObserver(test, fixture.ctx)
	probe := &documentPublicationProbePG{PostgresStore: fixture.storage, calls: make(map[string]int)}
	commands := documentPublicationOwnerPG(test, documentPublicationServicePG(probe))
	member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	anchor := makeCommitDoc(test, string(fixture.repo)+" numeric anchor")
	if output, err := commands.Commit(member, inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{anchor}, Snapshots: []domain.Snapshot{numericPublicationSnapshot(fixture.repo, anchor)}}); err != nil || output != (inbound.CommitOutput{StoredDocs: 1, StoredSnapshots: 1}) {
		test.Fatal(output, err)
	}
	numeric := numericPublicationFixture{storage: fixture.storage, ctx: member, repo: fixture.repo, existing: anchor}
	for _, scenario := range []struct{ location, layout string }{
		{location: "tool_input", layout: "valid_prefix"},
		{location: "tool_output", layout: "duplicate_claim"},
		{location: "nested_compaction", layout: "invalid_first"},
	} {
		test.Run(scenario.location+"/"+scenario.layout, func(test *testing.T) {
			input := numericPublicationRejectedInput(test, numeric, scenario.location, scenario.layout)
			before := documentPublicationImagePG(test, fixture)
			objects := numericPublicationPGObjectCounts(test, observer, fixture.repo, input.Docs)
			writes := probe.calls["verified_doc"]
			if output, err := commands.Commit(member, input); !errors.Is(err, domain.ErrIntegrity) || output != (inbound.CommitOutput{}) || probe.calls["verified_doc"] != writes {
				test.Fatal("numeric admission did not reject the whole batch before native writes", output, probe.calls, err)
			}
			if documentPublicationImagePG(test, fixture) != before || numericPublicationPGObjectCounts(test, observer, fixture.repo, input.Docs) != objects {
				test.Fatal("numeric refusal leaked a valid prefix, CAS grant, index or logical metadata")
			}
			numericPublicationAssertAbsent(test, numeric, input.Docs)
		})
	}
	for _, lexeme := range []string{"1787683260.123456789", "1.2300E+003", "1e999"} {
		test.Run(lexeme, func(test *testing.T) {
			document := numericPublicationCompatibleDocument(test, string(fixture.repo)+lexeme, lexeme)
			canonical, err := domain.CanonicalBytes(document.CIR)
			if err != nil || !bytes.Contains(canonical, []byte(`"create_time":`+lexeme)) {
				test.Fatal("numeric fixture lost its original typed metadata lexeme", err)
			}
			input := inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{numericPublicationSnapshot(fixture.repo, document)}}
			for _, expected := range []inbound.CommitOutput{{StoredDocs: 1, StoredSnapshots: 1}, {DedupedDocs: 1, DedupedSnapshots: 1}} {
				if output, err := commands.Commit(member, input); err != nil || output != expected {
					test.Fatal("compatible numeric public publication or replay failed", output, expected, err)
				}
			}
			proof, err := fixture.storage.ReadVerifiedDoc(fixture.ctx, fixture.repo, document.Hash)
			if err != nil || !proof.Valid() || !bytes.Equal(proof.Bytes(), canonical) {
				test.Fatal("native numeric publication changed exact canonical bytes", err)
			}
			loaded, err := fixture.storage.GetDoc(fixture.ctx, fixture.repo, document.Hash)
			if err != nil {
				test.Fatal(err)
			}
			reloaded, err := domain.CanonicalBytes(loaded.CIR)
			if err != nil || !bytes.Equal(reloaded, canonical) || domain.HashContent(reloaded) != document.Hash {
				test.Fatal("numeric reload changed admitted byte identity", err)
			}
			index, err := fixture.storage.DocReadIndex(fixture.ctx, fixture.repo, document.Hash)
			if err != nil || len(index.Events) != 4 || index.Events[1].Hash != domain.HashContent([]byte(numericPublicationGenericToolCall)) {
				test.Fatal("numeric publication changed native event hashes", index, err)
			}
		})
	}
}

func TestPGDocumentPublicationCommandsExistingRootMetadataWithAdmissionOff(test *testing.T) {
	if !conversationRootReleaseReady {
		test.Skip("requires released root document readers")
	}
	fixture := settingsCodexPG(test)
	root := domain.DocumentIdentityRootV1
	if !hasDocumentIdentity(fixture.service.DocumentIdentitiesSupported(), root) {
		test.Skip("complete PostgreSQL root readers unavailable")
	}
	ctx := inbound.WithDocumentIdentities(inbound.WithSystemActor(fixture.ctx), []domain.DocumentIdentity{root})
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{root}, fixture.service.DocumentIdentitiesSupported())
	if err := fixture.storage.WithinRepository(ctx, fixture.repo, func(bound context.Context) error {
		return fixture.storage.RequireDocumentIdentity(bound, fixture.repo, root)
	}); err != nil {
		test.Fatal(err)
	}
	representation, bodies := a11RootRepresentation(test, "already-owned root metadata")
	if _, _, err := fixture.storage.PutChunks(ctx, fixture.repo, bodies); err != nil {
		test.Fatal(err)
	}
	job, err := domain.NewDocFinalizationJobForRepresentation(fixture.repo, representation, time.Now().UTC())
	if err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.EnqueueDocJob(ctx, job); err != nil {
		test.Fatal(err)
	}
	claimed, err := fixture.storage.ClaimDocJob(ctx, fixture.repo, time.Now().UTC(), time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	if err := fixture.service.runDocJob(ctx, fixture.storage, claimed); err != nil {
		test.Fatal(err)
	}
	completed, err := fixture.storage.GetDocJob(ctx, fixture.repo, job.ID)
	if err != nil || completed.State != "completed" {
		test.Fatal("root fixture did not prepare actual owned bytes", completed, err)
	}
	if err := fixture.service.ConfigureConversationRootPublication(false); err != nil || fixture.service.RootPublicationEnabled() {
		test.Fatal("root metadata fixture enabled new-document admission", err)
	}
	observer := authorityScopeObserver(test, fixture.ctx)
	var jobsBefore int
	if err := observer.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_finalization_jobs WHERE repo_id=$1`, string(fixture.repo)).Scan(&jobsBefore); err != nil {
		test.Fatal(err)
	}
	commands := documentPublicationOwnerPG(test, fixture.service)
	member := inbound.WithDocumentIdentities(inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID), []domain.DocumentIdentity{root})
	snapshot := domain.Snapshot{ID: representation.Hash, RepoID: fixture.repo, DocHash: representation.Hash, DocIdentity: root, Branch: "main", Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull}
	input := inbound.CommitInput{RepoID: fixture.repo, Snapshots: []domain.Snapshot{snapshot}}
	before := documentPublicationImagePG(test, fixture)
	oldPeer := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	if output, err := commands.Commit(oldPeer, input); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || output != (inbound.CommitOutput{}) || documentPublicationImagePG(test, fixture) != before {
		test.Fatal("root metadata owner skipped current peer identity negotiation", output, err)
	}
	for _, expected := range []inbound.CommitOutput{{StoredSnapshots: 1}, {DedupedSnapshots: 1}} {
		if output, err := commands.Commit(member, input); err != nil || output != expected {
			test.Fatal("admission-off owner refused verified existing root metadata", output, expected, err)
		}
	}
	stored, err := fixture.storage.GetSnapshot(ctx, fixture.repo, representation.Hash)
	if err != nil || stored.DocumentRef() != representation.DocumentRef() {
		test.Fatal("root metadata attachment lost its exact identity", stored, err)
	}
	if current, err := fixture.storage.GetDocJob(ctx, fixture.repo, job.ID); err != nil || !reflect.DeepEqual(current, completed) {
		test.Fatal("metadata attachment changed the root publication job or version", current, err)
	}
	var jobsAfter int
	if err := observer.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_finalization_jobs WHERE repo_id=$1`, string(fixture.repo)).Scan(&jobsAfter); err != nil || jobsAfter != jobsBefore {
		test.Fatal("metadata attachment opened a new root admission job", jobsBefore, jobsAfter, err)
	}
}

func TestPGDocumentPublicationCommandsPrivateMemoryDelegateAtomicity(test *testing.T) {
	for _, outcome := range []string{"pointer-rollback", "success"} {
		test.Run(outcome, func(test *testing.T) {
			fixture := settingsCodexPG(test)
			observer := authorityScopeObserver(test, fixture.ctx)
			probe := &documentPublicationProbePG{PostgresStore: fixture.storage, calls: make(map[string]int)}
			failure := errors.New("memory failure after native pointer CAS")
			if outcome == "pointer-rollback" {
				probe.failAt, probe.failure = "memory_cas", failure
			}
			service := documentPublicationServicePG(probe)
			public, err := service.DocumentPublicationCommands()
			if err != nil {
				test.Fatal(err)
			}
			if _, ok := public.(*DocumentPublicationCommands); !ok {
				test.Fatalf("memory fixture public factory returned %T", public)
			}
			document := makeCommitDoc(test, string(fixture.repo)+" memory delegate")
			input := inbound.MemoryPublication{
				Objects: inbound.CommitInput{RepoID: fixture.repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{numericPublicationSnapshot(fixture.repo, document)}},
				Memory:  domain.MemoryDigest{SnapshotID: document.Hash, Summary: "atomic independent memory"},
			}
			memoryHash, hashError := domain.MemoryDigestHash(input.Memory)
			if hashError != nil {
				test.Fatal(hashError)
			}
			before := documentPublicationImagePG(test, fixture)
			audits := documentPublicationAuditsPG(test, fixture)
			objects := numericPublicationPGObjectCounts(test, observer, fixture.repo, input.Objects.Docs)
			memoryObjects := documentPublicationBlobCountsPG(test, observer, fixture, "memory", memoryHash)
			revision, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo)
			if err != nil {
				test.Fatal(err)
			}
			admission, gate := admissionTestContext(10 * time.Millisecond)
			_, release, err := AcquireResources(admission, ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer release()
			member, cancel := context.WithTimeout(inbound.WithRepositoryActor(admission, fixture.team.member.ID), 10*time.Second)
			defer cancel()
			hash, err := service.PublishMemoryArchive(member, input)
			release()
			assertResourcesReleased(test, gate)
			if probe.transactions != 1 || probe.calls["verified_doc"] != 1 || probe.calls["snapshot"] != 1 || probe.calls["memory"] != 1 || probe.calls["memory_cas"] != 1 || probe.calls["legacy_doc"] != 0 {
				test.Fatal("private memory commit did not share the actual owner and one transaction", probe.transactions, probe.calls, err)
			}
			if outcome == "pointer-rollback" {
				if !errors.Is(err, failure) || hash != "" || probe.calls["audit"] != 0 || documentPublicationImagePG(test, fixture) != before || numericPublicationPGObjectCounts(test, observer, fixture.repo, input.Objects.Docs) != objects {
					test.Fatal("private delegate committed or audited before the final memory pointer", hash, probe.calls, err)
				}
				if _, err := fixture.storage.GetMemory(fixture.ctx, fixture.repo, memoryHash); !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("memory blob or ownership escaped archive rollback", err)
				}
				if current := documentPublicationBlobCountsPG(test, observer, fixture, "memory", memoryHash); current != memoryObjects {
					test.Fatal("private delegate leaked the global memory CAS blob or grant", memoryObjects, current)
				}
				return
			}
			if err != nil || hash != memoryHash || probe.calls["audit"] != 1 || probe.calls["revision"] != 1 {
				test.Fatal("memory archive opened an extra public command, revision or audit scope", hash, probe.calls, err)
			}
			documentPublicationAuditPG(test, fixture, audits, "context.updated")
			stored, err := fixture.storage.GetSnapshot(fixture.ctx, fixture.repo, document.Hash)
			if err != nil || stored.MemoryHash != hash {
				test.Fatal("atomic archive lost its final memory CAS pointer", stored, err)
			}
			expected := revision
			expected.Graph++
			if current, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo); err != nil || current != expected {
				test.Fatal("private delegate changed more than the archive's graph version", current, expected, err)
			}
		})
	}
}
