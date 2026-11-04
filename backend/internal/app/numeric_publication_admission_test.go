package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const numericPublicationGenericToolCall = `{"call_id":"numeric-call","input":{"exact":9007199254740992,"fraction":0.5,"negative_zero":-0,"nested":[null,true,"9007199254740993",{"value":1e+21}]},"kind":"tool_call","seq":1,"tool_name":"numeric"}`

type numericPublicationStore interface {
	outbound.MetadataStore
	outbound.BlobStore
	outbound.DocReadStore
	outbound.VerifiedDocReader
	outbound.RepositoryRevisions
	outbound.HistoryStore
	outbound.RepositoryStore
	outbound.OrganizationStore
	outbound.TeamStore
}

type numericPublicationFixture struct {
	service      *Service
	storage      numericPublicationStore
	ctx          context.Context
	repo         domain.ContentHash
	organization string
	existing     domain.SessionDoc
}

func newNumericPublicationFixture(test *testing.T, service *Service, storage numericPublicationStore, repo domain.ContentHash) numericPublicationFixture {
	test.Helper()
	team := makeTeamFixture(test, storage)
	if _, err := storage.PutRepo(systemTestContext(), domain.Repo{ID: repo, RepositoryID: team.repository.ID, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	fixture := numericPublicationFixture{
		service: service, storage: storage, repo: repo, organization: team.organization.ID,
		ctx:      inbound.WithRepositoryActor(context.Background(), team.owner.ID),
		existing: makeCommitDoc(test, string(repo)+"/existing numeric admission anchor"),
	}
	output, err := service.Commit(fixture.ctx, inbound.CommitInput{
		RepoID: repo, Docs: []domain.SessionDoc{fixture.existing},
		Snapshots: []domain.Snapshot{numericPublicationSnapshot(repo, fixture.existing)},
	})
	if err != nil || output != (inbound.CommitOutput{StoredDocs: 1, StoredSnapshots: 1}) {
		test.Fatalf("public seed commit: %+v %v", output, err)
	}
	if _, err := service.UpdateRef(fixture.ctx, inbound.UpdateRefInput{
		RepoID: repo, Ref: domain.Ref{Kind: domain.RefBranch, Name: "main", Target: fixture.existing.Hash},
	}); err != nil {
		test.Fatal(err)
	}
	audits, err := storage.ListOrganizationAudit(systemTestContext(), fixture.organization, 100)
	if err != nil {
		test.Fatal(err)
	}
	found := false
	for _, audit := range audits {
		if audit.Action == "context.objects.stored" && audit.TargetID == team.repository.ID {
			found = true
		}
	}
	if !found {
		test.Fatal("public seed commit did not exercise organization auditing")
	}
	return fixture
}

func numericPublicationSnapshot(repo domain.ContentHash, document domain.SessionDoc) domain.Snapshot {
	return domain.Snapshot{
		ID: document.Hash, RepoID: repo, DocHash: document.Hash, Branch: "main",
		Provider: document.CIR.Envelope.SourceProvider, Fidelity: document.CIR.Envelope.Fidelity,
	}
}

func numericPublicationDocument(test *testing.T, marker string, events ...domain.CIREvent) domain.SessionDoc {
	test.Helper()
	document := domain.SessionDoc{CIR: domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull, GitBranch: "main", SessionOriginID: marker},
		Events:   events,
	}}
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	document.Hash = domain.HashContent(canonical)
	return document
}

func numericPublicationRejectedInput(test *testing.T, fixture numericPublicationFixture, location, layout string) inbound.CommitInput {
	test.Helper()
	token := "9007199254740993"
	event := domain.CIREvent{Kind: domain.EventToolCall, Seq: 1, CallID: "numeric-call", ToolName: "numeric", Input: map[string]any{"payload": []any{map[string]any{"count": json.Number(token)}}}}
	switch location {
	case "tool_output":
		token = "1E+3"
		event = domain.CIREvent{Kind: domain.EventToolResult, Seq: 1, CallID: "numeric-call", Output: []any{map[string]any{"count": json.Number(token)}}}
	case "nested_compaction":
		token = "1e999"
		event = domain.CIREvent{Kind: domain.EventCompaction, Seq: 1, ReplacementComplete: true, Replacement: []domain.CIREvent{
			{Kind: domain.EventCompaction, Seq: 0, ReplacementComplete: true, Replacement: []domain.CIREvent{
				{Kind: domain.EventToolResult, Seq: 0, CallID: "nested-call", Output: map[string]any{"nested": []any{json.Number(token)}}},
			}},
		}}
	}
	invalid := numericPublicationDocument(test, string(fixture.repo)+"/"+test.Name(), event)
	canonical, err := domain.CanonicalBytes(invalid.CIR)
	if err != nil {
		test.Fatal(err)
	}
	if !bytes.Contains(canonical, []byte(token)) || domain.HashContent(canonical) != invalid.Hash {
		test.Fatal("numeric regression fixture does not claim its own canonical hash")
	}
	var verifier domain.CanonicalDocVerifier
	proof, err := verifier.Verify(systemTestContext(), invalid.Hash, canonical)
	if !errors.Is(err, domain.ErrIntegrity) || proof.Valid() {
		test.Fatalf("unchanged wire verifier must reject numeric fixture: valid=%t err=%v", proof.Valid(), err)
	}
	prefix := makeCommitDoc(test, string(fixture.repo)+"/"+test.Name()+"/valid prefix")
	documents := []domain.SessionDoc{invalid, prefix}
	if layout != "invalid_first" {
		documents = []domain.SessionDoc{prefix, invalid}
	}
	if layout == "duplicate_claim" {
		documents[1].Hash = prefix.Hash
	}
	input := inbound.CommitInput{RepoID: fixture.repo, Docs: documents}
	for position, document := range documents {
		if layout == "duplicate_claim" && position == 1 {
			continue
		}
		snapshot := numericPublicationSnapshot(fixture.repo, document)
		snapshot.Parents = []domain.ContentHash{fixture.existing.Hash}
		input.Snapshots = append(input.Snapshots, snapshot)
	}
	return input
}

func numericPublicationState(test *testing.T, fixture numericPublicationFixture) string {
	test.Helper()
	ctx := systemTestContext()
	repository, err := fixture.storage.GetRepo(ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	snapshots, err := fixture.storage.ListSnapshots(ctx, fixture.repo, "")
	if err != nil {
		test.Fatal(err)
	}
	refs, err := fixture.storage.ListRefs(ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	reflog, err := fixture.storage.ReadReflog(ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	revision, err := fixture.storage.RepositoryRevision(ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	history, err := fixture.storage.ListHistoryEvents(ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	pending, err := fixture.storage.ListPendings(ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	audits, err := fixture.storage.ListOrganizationAudit(ctx, fixture.organization, 100)
	if err != nil {
		test.Fatal(err)
	}
	existing, err := fixture.storage.GetDoc(ctx, fixture.repo, fixture.existing.Hash)
	if err != nil {
		test.Fatal(err)
	}
	canonical, err := domain.CanonicalBytes(existing.CIR)
	if err != nil {
		test.Fatal(err)
	}
	proof, err := fixture.storage.ReadVerifiedDoc(ctx, fixture.repo, fixture.existing.Hash)
	if err != nil || !proof.Valid() || !bytes.Equal(proof.Bytes(), canonical) {
		test.Fatalf("existing valid body/proof changed: %v", err)
	}
	index, err := fixture.storage.DocReadIndex(ctx, fixture.repo, fixture.existing.Hash)
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := json.Marshal([]any{repository, snapshots, refs, reflog, revision, history, pending, audits, existing, index, canonical})
	if err != nil {
		test.Fatal(err)
	}
	return string(encoded)
}

func numericPublicationObjectHashes(test *testing.T, documents []domain.SessionDoc) []domain.ContentHash {
	test.Helper()
	seen := make(map[domain.ContentHash]bool)
	var hashes []domain.ContentHash
	for _, document := range documents {
		canonical, err := domain.CanonicalBytes(document.CIR)
		if err != nil {
			test.Fatal(err)
		}
		candidates := []domain.ContentHash{document.Hash, domain.HashContent(canonical)}
		if plan, chunked := domain.PlanDocChunks(canonical); chunked {
			candidates = append(candidates, plan.Order...)
		}
		for _, hash := range candidates {
			if !seen[hash] {
				seen[hash] = true
				hashes = append(hashes, hash)
			}
		}
	}
	return hashes
}

func numericPublicationAssertAbsent(test *testing.T, fixture numericPublicationFixture, documents []domain.SessionDoc) {
	test.Helper()
	ctx := systemTestContext()
	hashes := numericPublicationObjectHashes(test, documents)
	for _, repo := range []domain.ContentHash{fixture.repo, hh(string(fixture.repo) + "/unrelated")} {
		if have, err := fixture.storage.HasDocs(ctx, repo, hashes); err != nil || len(have) != 0 {
			test.Errorf("rejected batch left document ownership in %s: %v %v", repo, have, err)
		}
		if have, err := fixture.storage.HasChunks(ctx, repo, hashes); err != nil || len(have) != 0 {
			test.Errorf("rejected batch left chunk CAS ownership in %s: %v %v", repo, have, err)
		}
	}
	for _, document := range documents {
		if _, err := fixture.storage.GetDoc(ctx, fixture.repo, document.Hash); !errors.Is(err, domain.ErrNotFound) {
			test.Errorf("rejected document %s is stored or unreadable: %v", document.Hash, err)
		}
		if _, err := fixture.storage.GetSnapshot(ctx, fixture.repo, document.Hash); !errors.Is(err, domain.ErrNotFound) {
			test.Errorf("rejected snapshot %s was published: %v", document.Hash, err)
		}
		if _, err := fixture.storage.DocReadIndex(ctx, fixture.repo, document.Hash); !errors.Is(err, domain.ErrNotFound) {
			test.Errorf("rejected document %s left a read index: %v", document.Hash, err)
		}
	}
}

func TestNumericPublicationAdmissionFSRejectsBeforeWrites(test *testing.T) {
	service, storage := newFsckSvc(test)
	fixture := newNumericPublicationFixture(test, service, storage, hh(test.Name()))
	writer := &verifiedCommitBlobs{BlobStore: storage}
	service.blobs = writer
	for _, location := range []string{"tool_input", "tool_output", "nested_compaction"} {
		for _, layout := range []string{"invalid_first", "valid_prefix", "duplicate_claim"} {
			test.Run(location+"/"+layout, func(test *testing.T) {
				input := numericPublicationRejectedInput(test, fixture, location, layout)
				before := numericPublicationState(test, fixture)
				writes := writer.writes
				output, err := service.Commit(fixture.ctx, input)
				if !errors.Is(err, domain.ErrIntegrity) || output != (inbound.CommitOutput{}) {
					test.Errorf("public numeric admission: output=%+v err=%v", output, err)
				}
				if writer.writes != writes {
					test.Errorf("commit wrote %d verified documents before validating the full batch", writer.writes-writes)
				}
				if after := numericPublicationState(test, fixture); after != before {
					test.Errorf("rejection changed existing body, snapshots, refs, revisions, history or audit:\nbefore=%s\nafter=%s", before, after)
				}
				numericPublicationAssertAbsent(test, fixture, input.Docs)
			})
		}
	}
}

func numericPublicationCompatibleDocument(test *testing.T, marker, lexeme string) domain.SessionDoc {
	test.Helper()
	created := json.Number(lexeme)
	return numericPublicationDocument(test, marker,
		domain.CIREvent{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "numeric publication"}}, ProviderMetadata: &domain.ProviderMetadata{TurnID: "numeric-turn", CreateTime: &created}},
		domain.CIREvent{Kind: domain.EventToolCall, Seq: 1, CallID: "numeric-call", ToolName: "numeric", Input: map[string]any{
			"exact": json.Number("9007199254740992"), "negative_zero": math.Copysign(0, -1), "fraction": float64(0.5),
			"nested": []any{nil, true, "9007199254740993", map[string]any{"value": json.Number("1e+21")}},
		}},
		domain.CIREvent{Kind: domain.EventToolResult, Seq: 2, CallID: "numeric-call", Output: []any{json.Number("42"), map[string]any{"ratio": json.Number("0.125")}}},
		domain.CIREvent{Kind: domain.EventCompaction, Seq: 3, ReplacementComplete: true, Replacement: []domain.CIREvent{
			{Kind: domain.EventToolResult, Seq: 0, CallID: "nested-call", Output: map[string]any{"count": json.Number("1000")}, ProviderMetadata: &domain.ProviderMetadata{TurnID: "nested-turn", CreateTime: &created}},
		}},
	)
}

func runNumericPublicationRoundTrips(test *testing.T, service *Service, storage numericPublicationStore, ctx context.Context, repo domain.ContentHash, view bool) {
	test.Helper()
	for _, publication := range []string{"put_doc", "commit"} {
		for _, lexeme := range []string{"1787683260.123456789", "1.2300E+003", "1e999"} {
			test.Run(publication+"/"+lexeme, func(test *testing.T) {
				document := numericPublicationCompatibleDocument(test, string(repo)+"/"+test.Name(), lexeme)
				canonical, err := domain.CanonicalBytes(document.CIR)
				if err != nil {
					test.Fatal(err)
				}
				if !bytes.Contains(canonical, []byte(`"create_time":`+lexeme)) || !bytes.Contains(canonical, []byte(numericPublicationGenericToolCall)) {
					test.Fatal("canonical bytes rounded typed metadata or compatible generic numbers")
				}
				var verifier domain.CanonicalDocVerifier
				proof, err := verifier.Verify(systemTestContext(), document.Hash, canonical)
				if err != nil || !proof.Valid() {
					test.Fatalf("compatible fixture rejected by existing wire verifier: %v", err)
				}
				input := inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{numericPublicationSnapshot(repo, document)}}
				if publication == "put_doc" {
					if stored, err := storage.PutDoc(ctx, repo, document); err != nil || !stored {
						test.Fatalf("native PutDoc: stored=%t err=%v", stored, err)
					}
				} else if output, err := service.Commit(ctx, input); err != nil || output != (inbound.CommitOutput{StoredDocs: 1, StoredSnapshots: 1}) {
					test.Fatalf("public compatible commit: %+v %v", output, err)
				}
				var expected domain.CIRDocument
				if err := json.Unmarshal(canonical, &expected); err != nil {
					test.Fatal(err)
				}
				for _, read := range []func(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error){storage.GetDoc, service.GetDoc} {
					loaded, err := read(systemTestContext(), repo, document.Hash)
					if err != nil || loaded.Hash != document.Hash || !reflect.DeepEqual(loaded.CIR, expected) {
						test.Fatalf("actual reload changed generic values or typed numeric metadata: %+v %v", loaded, err)
					}
					reloaded, err := domain.CanonicalBytes(loaded.CIR)
					if err != nil || !bytes.Equal(reloaded, canonical) || domain.HashContent(reloaded) != document.Hash {
						test.Fatalf("reload changed original canonical bytes/hash: %v", err)
					}
				}
				storedProof, err := storage.ReadVerifiedDoc(systemTestContext(), repo, document.Hash)
				if err != nil || !storedProof.Valid() || storedProof.Hash() != document.Hash || !bytes.Equal(storedProof.Bytes(), canonical) {
					test.Fatalf("stored verifier disagrees with admitted body: %v", err)
				}
				queries, err := service.DocumentQueries()
				if err != nil {
					test.Fatal(err)
				}
				index, err := storage.DocReadIndex(systemTestContext(), repo, document.Hash)
				wantIndex, indexErr := proof.ReadIndex()
				for position := range wantIndex.Events {
					wantIndex.Events[position].Text = ""
				}
				if err != nil || indexErr != nil || !reflect.DeepEqual(index, wantIndex) {
					test.Fatalf("stored read index changed canonical event hashes: %+v %v %v", index, err, indexErr)
				}
				if index.Events[1].Hash != domain.HashContent([]byte(numericPublicationGenericToolCall)) {
					test.Fatal("compatible generic payload changed its original canonical event hash")
				}
				page, err := queries.ReadDocEvents(systemTestContext(), repo, document.Hash, "", 1, 2)
				if err != nil || page.Hash != document.Hash || page.Offset != 1 || page.Total != 4 || page.Next != 3 || !reflect.DeepEqual(page.Events, expected.Events[1:3]) {
					test.Fatalf("selected tool page changed numeric payloads: %+v %v", page, err)
				}
				tail, err := queries.ReadDocEvents(systemTestContext(), repo, document.Hash, "", 3, 1)
				if err != nil || tail.Next != -1 || !reflect.DeepEqual(tail.Events, expected.Events[3:]) {
					test.Fatalf("compaction page changed nested typed metadata: %+v %v", tail, err)
				}
				if view {
					selected, err := queries.ReadDocViewEvents(systemTestContext(), repo, document.Hash, "", 0, 4, "raw")
					if err != nil || selected.Hash != document.Hash || selected.Total != 4 || selected.Next != -1 || len(selected.Events) != 4 {
						test.Fatalf("raw numeric viewer page: %+v %v", selected, err)
					}
					for position, event := range selected.Events {
						if event.Index != position || event.Deferred || !reflect.DeepEqual(event.Event, expected.Events[position]) {
							test.Fatalf("viewer event %d changed numeric data: %+v", position, event)
						}
					}
				}
				foreign := hh(string(repo) + "/foreign numeric owner")
				if _, err := storage.GetDoc(systemTestContext(), foreign, document.Hash); !errors.Is(err, domain.ErrNotFound) {
					test.Fatalf("admitted document escaped repository ownership: %v", err)
				}
				if publication == "put_doc" {
					if stored, err := storage.PutDoc(ctx, repo, document); err != nil || stored {
						test.Fatalf("native numeric retry: stored=%t err=%v", stored, err)
					}
				} else if output, err := service.Commit(ctx, input); err != nil || output != (inbound.CommitOutput{DedupedDocs: 1, DedupedSnapshots: 1}) {
					test.Fatalf("public numeric retry: %+v %v", output, err)
				}
			})
		}
	}
}

func TestNumericPublicationAdmissionFSCompatibleRoundTrips(test *testing.T) {
	service, storage := newFsckSvc(test)
	repo := hh(test.Name())
	bindCommitTestRepo(test, storage, repo)
	runNumericPublicationRoundTrips(test, service, storage, systemTestContext(), repo, true)
}
