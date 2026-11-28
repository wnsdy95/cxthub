package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// commitDocument is a conversation large enough to travel in several parts,
// with those parts in both formats a client may send.
type commitDocument struct {
	hash      domain.ContentHash
	canonical []byte
	plan      domain.DocChunkPlan // the format written now
	first     domain.DocChunkPlan // the format written first
}

func commitChunkedDocument(t *testing.T, label string) commitDocument {
	t.Helper()
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull, GitBranch: "main"}}
	for seq := 0; seq < 3; seq++ {
		cir.Events = append(cir.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: seq, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("%s %d ", label, seq) + strings.Repeat("x", 600<<10)}}})
	}
	canonical, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := domain.PlanDocChunks(canonical)
	first, okFirst := domain.PlanDocChunksV1(canonical)
	if !ok || !okFirst || len(plan.Manifest.Chunks) < 2 || len(first.Manifest.Chunks) < 2 || plan.Manifest.Format == first.Manifest.Format {
		t.Fatalf("the fixture does not travel in parts: %d and %d", len(plan.Manifest.Chunks), len(first.Manifest.Chunks))
	}
	return commitDocument{hash: domain.HashContent(canonical), canonical: canonical, plan: plan, first: first}
}

func (document commitDocument) chunked(plan domain.DocChunkPlan) inbound.ChunkedDoc {
	return inbound.ChunkedDoc{Hash: document.hash, Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}
}

func commitChunkObjects(plan domain.DocChunkPlan, hashes ...domain.ContentHash) []inbound.ChunkObject {
	if len(hashes) == 0 {
		hashes = plan.Order
	}
	objects := make([]inbound.ChunkObject, 0, len(hashes))
	for _, hash := range hashes {
		objects = append(objects, inbound.ChunkObject{Hash: hash, Data: plan.Bodies[hash]})
	}
	return objects
}

func commitStoredAs(t *testing.T, storage *store.FSStore, repo domain.ContentHash, document commitDocument) {
	t.Helper()
	stored, err := storage.GetDoc(systemTestContext(), repo, document.hash)
	if err != nil {
		t.Fatalf("the document was not stored: %v", err)
	}
	canonical, err := domain.CanonicalBytes(stored.CIR)
	if err != nil || stored.Hash != document.hash || string(canonical) != string(document.canonical) {
		t.Fatalf("the stored document is not the one that was sent: %s %v", stored.Hash, err)
	}
}

func commitAbsent(t *testing.T, name string, storage *store.FSStore, repo domain.ContentHash, hashes ...domain.ContentHash) {
	t.Helper()
	for _, hash := range hashes {
		if _, err := storage.GetDoc(systemTestContext(), repo, hash); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: a refused batch stored document %s: %v", name, hash, err)
		}
	}
}

// A conversation arrives as a list of parts. The parts may already be in the
// repository, come with the request, or both; the server puts them together,
// checks the result against the hash the client named, and stores it once.
func TestCommitPutsADocumentTogetherFromItsParts(t *testing.T) {
	for name, send := range map[string]func(t *testing.T, service *Service, storage *store.FSStore, repo domain.ContentHash, document commitDocument) inbound.CommitInput{
		"parts sent ahead, as cxt sends them": func(t *testing.T, service *Service, _ *store.FSStore, repo domain.ContentHash, document commitDocument) inbound.CommitInput {
			if out, err := service.StoreChunks(systemTestContext(), inbound.StoreChunksInput{RepoID: repo, Chunks: commitChunkObjects(document.plan)}); err != nil || out.Stored != len(document.plan.Bodies) {
				t.Fatalf("parts sent ahead: %+v %v", out, err)
			}
			return inbound.CommitInput{RepoID: repo, ChunkedDocs: []inbound.ChunkedDoc{document.chunked(document.plan)}}
		},
		"parts in the same request": func(_ *testing.T, _ *Service, _ *store.FSStore, repo domain.ContentHash, document commitDocument) inbound.CommitInput {
			return inbound.CommitInput{RepoID: repo, ChunkedDocs: []inbound.ChunkedDoc{document.chunked(document.plan)}, ChunkObjects: commitChunkObjects(document.plan)}
		},
		"some parts ahead, the rest with the request": func(t *testing.T, _ *Service, storage *store.FSStore, repo domain.ContentHash, document commitDocument) inbound.CommitInput {
			ahead := document.plan.Order[0]
			if _, _, err := storage.PutChunks(systemTestContext(), repo, map[domain.ContentHash][]byte{ahead: document.plan.Bodies[ahead]}); err != nil {
				t.Fatal(err)
			}
			return inbound.CommitInput{RepoID: repo, ChunkedDocs: []inbound.ChunkedDoc{document.chunked(document.plan)}, ChunkObjects: commitChunkObjects(document.plan, document.plan.Order[1:]...)}
		},
		"the first format": func(_ *testing.T, _ *Service, _ *store.FSStore, repo domain.ContentHash, document commitDocument) inbound.CommitInput {
			return inbound.CommitInput{RepoID: repo, ChunkedDocs: []inbound.ChunkedDoc{document.chunked(document.first)}, ChunkObjects: commitChunkObjects(document.first)}
		},
		"the first format without its name": func(_ *testing.T, _ *Service, _ *store.FSStore, repo domain.ContentHash, document commitDocument) inbound.CommitInput {
			unnamed := document.chunked(document.first)
			unnamed.Format = ""
			return inbound.CommitInput{RepoID: repo, ChunkedDocs: []inbound.ChunkedDoc{unnamed}, ChunkObjects: commitChunkObjects(document.first)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			service, storage := newFsckSvc(t)
			repo := hh("commit from parts: " + name)
			bindCommitTestRepo(t, storage, repo)
			document := commitChunkedDocument(t, name)
			input := send(t, service, storage, repo, document)
			out, err := service.Commit(systemTestContext(), input)
			if err != nil || out != (inbound.CommitOutput{StoredDocs: 1}) {
				t.Fatalf("first time: %+v %v", out, err)
			}
			commitStoredAs(t, storage, repo, document)
			// The same document again is not stored again.
			out, err = service.Commit(systemTestContext(), input)
			if err != nil || out != (inbound.CommitOutput{DedupedDocs: 1}) {
				t.Fatalf("second time: %+v %v", out, err)
			}
			commitStoredAs(t, storage, repo, document)
		})
	}
}

// A batch that is wrong anywhere stores nothing, whatever else it holds.
func TestCommitRefusesABatchItCannotTrustAndStoresNothing(t *testing.T) {
	good := commitChunkedDocument(t, "good")
	other := commitChunkedDocument(t, "other")
	whole := makeCommitDoc(t, "sent whole")
	unbound, missing := hh("commit: a store without a repository"), hh("commit: no such store")

	// Bytes that fit together and match their hash but are not a conversation.
	notADocument := inbound.ChunkObject{Data: []byte(`not json`)}
	notADocument.Hash = domain.HashContent(notADocument.Data)
	undecodable := inbound.ChunkedDoc{Format: domain.ChunkFormatV2, Envelope: []byte(`{}`), Chunks: []domain.ContentHash{notADocument.Hash}}
	undecodable.Hash = domain.HashContent([]byte(`{"envelope":{},"events":[not json]}`))

	swapped := good.chunked(good.plan)
	swapped.Chunks = append([]domain.ContentHash{}, good.plan.Manifest.Chunks...)
	swapped.Chunks[0], swapped.Chunks[1] = swapped.Chunks[1], swapped.Chunks[0]
	renamed := good.chunked(good.plan)
	renamed.Hash = other.hash
	otherEnvelope := good.chunked(good.plan)
	otherEnvelope.Envelope = []byte(`{"cir_version":"1"}`)
	future := good.chunked(good.plan)
	future.Format = "cxt-doc-chunks-v9"
	badList := good.chunked(good.plan)
	badList.Chunks = append(append([]domain.ContentHash{}, good.plan.Manifest.Chunks...), "sha256:nope")
	badName := good.chunked(good.plan)
	badName.Hash = "not-a-hash"
	second := good.chunked(good.plan)
	second.Identity = domain.DocumentIdentityRootV1
	wholeSecond := whole
	wholeSecond.Identity = domain.DocumentIdentityRootV1
	corrupt := commitChunkObjects(good.plan)
	corrupt[0].Data = append([]byte{}, corrupt[0].Data...)
	corrupt[0].Data[len(corrupt[0].Data)/2] ^= 1
	misnamed := commitChunkObjects(good.plan)
	misnamed[0].Hash = "sha256:nope"

	for name, s := range map[string]struct {
		repo  domain.ContentHash
		input inbound.CommitInput
		want  error
		words string
	}{
		"a part that is neither sent nor stored": {input: inbound.CommitInput{ChunkedDocs: []inbound.ChunkedDoc{good.chunked(good.plan)}, ChunkObjects: commitChunkObjects(good.plan, good.plan.Order[1:]...)}, want: domain.ErrIntegrity, words: "references unknown chunk"},
		"parts in another order":                 {input: inbound.CommitInput{ChunkedDocs: []inbound.ChunkedDoc{swapped}, ChunkObjects: commitChunkObjects(good.plan)}, want: domain.ErrIntegrity, words: "reassembly mismatch"},
		"the parts of another document":          {input: inbound.CommitInput{ChunkedDocs: []inbound.ChunkedDoc{renamed}, ChunkObjects: commitChunkObjects(good.plan)}, want: domain.ErrIntegrity, words: "reassembly mismatch"},
		"another envelope":                       {input: inbound.CommitInput{ChunkedDocs: []inbound.ChunkedDoc{otherEnvelope}, ChunkObjects: commitChunkObjects(good.plan)}, want: domain.ErrIntegrity, words: "reassembly mismatch"},
		"a format this server does not read":     {input: inbound.CommitInput{ChunkedDocs: []inbound.ChunkedDoc{future}, ChunkObjects: commitChunkObjects(good.plan)}, want: domain.ErrValidation, words: "unsupported format"},
		"a part whose bytes are not its hash":    {input: inbound.CommitInput{ChunkedDocs: []inbound.ChunkedDoc{good.chunked(good.plan)}, ChunkObjects: corrupt}, want: domain.ErrIntegrity, words: "body hash mismatch"},
		"a part whose name is not a hash":        {input: inbound.CommitInput{ChunkedDocs: []inbound.ChunkedDoc{good.chunked(good.plan)}, ChunkObjects: misnamed}, want: domain.ErrIntegrity, words: "invalid content hash"},
		"a list that names something not a hash": {input: inbound.CommitInput{ChunkedDocs: []inbound.ChunkedDoc{badList}, ChunkObjects: commitChunkObjects(good.plan)}, want: domain.ErrIntegrity, words: "invalid content hash"},
		"a document whose name is not a hash":    {input: inbound.CommitInput{ChunkedDocs: []inbound.ChunkedDoc{badName}, ChunkObjects: commitChunkObjects(good.plan)}, want: domain.ErrIntegrity, words: "invalid content hash"},
		"parts that are not a conversation":      {input: inbound.CommitInput{ChunkedDocs: []inbound.ChunkedDoc{undecodable}, ChunkObjects: []inbound.ChunkObject{notADocument}}, want: domain.ErrIntegrity, words: "undecodable"},
		"the second format in parts":             {input: inbound.CommitInput{ChunkedDocs: []inbound.ChunkedDoc{second}, ChunkObjects: commitChunkObjects(good.plan)}, want: domain.ErrUnsupportedDocumentIdentity},
		"the second format sent whole":           {input: inbound.CommitInput{Docs: []domain.SessionDoc{wholeSecond}}, want: domain.ErrUnsupportedDocumentIdentity},
		// The format is refused before anything is put together: what else is wrong with the batch is not looked at.
		"the second format sent whole, beside parts that are missing": {input: inbound.CommitInput{Docs: []domain.SessionDoc{wholeSecond}, ChunkedDocs: []inbound.ChunkedDoc{good.chunked(good.plan)}}, want: domain.ErrUnsupportedDocumentIdentity},
		"the second format in parts that are missing":                 {input: inbound.CommitInput{ChunkedDocs: []inbound.ChunkedDoc{second}}, want: domain.ErrUnsupportedDocumentIdentity},
		// One good document does not get in beside a bad one.
		"a good document beside a bad one": {input: inbound.CommitInput{Docs: []domain.SessionDoc{whole},
			ChunkedDocs: []inbound.ChunkedDoc{good.chunked(good.plan), renamed}, ChunkObjects: commitChunkObjects(good.plan)}, want: domain.ErrIntegrity, words: "reassembly mismatch"},
		"a store that is not there":             {repo: missing, input: inbound.CommitInput{ChunkedDocs: []inbound.ChunkedDoc{good.chunked(good.plan)}, ChunkObjects: commitChunkObjects(good.plan)}, want: domain.ErrNotFound},
		"a store that belongs to no repository": {repo: unbound, input: inbound.CommitInput{ChunkedDocs: []inbound.ChunkedDoc{good.chunked(good.plan)}, ChunkObjects: commitChunkObjects(good.plan)}, want: domain.ErrForbidden, words: "not bound"},
		"a store whose name is not a hash":      {repo: "not-a-hash", input: inbound.CommitInput{Docs: []domain.SessionDoc{whole}}, want: domain.ErrIntegrity, words: "invalid content hash"},
	} {
		service, storage := newFsckSvc(t)
		repo := hh("commit refuses: " + name)
		bindCommitTestRepo(t, storage, repo)
		if _, err := storage.PutRepo(systemTestContext(), domain.Repo{ID: unbound, DefaultBranch: "main"}); err != nil {
			t.Fatal(err)
		}
		if s.repo != "" {
			repo = s.repo
		}
		s.input.RepoID = repo
		out, err := service.Commit(systemTestContext(), s.input)
		if !errors.Is(err, s.want) || !strings.Contains(fmt.Sprint(err), s.words) || out != (inbound.CommitOutput{}) {
			t.Errorf("%s: %+v %v", name, out, err)
		}
		if domain.ValidateContentHash(repo) == nil {
			commitAbsent(t, name, storage, repo, good.hash, other.hash, whole.Hash, undecodable.Hash)
		}
	}
}

// commitWriters counts through which of the two ways a document is written.
type commitWriters struct {
	outbound.BlobStore
	verified, legacy int
}

func (writers *commitWriters) PutVerifiedDoc(ctx context.Context, repo domain.ContentHash, document domain.VerifiedSessionDoc) (bool, error) {
	writers.verified++
	return writers.BlobStore.(outbound.VerifiedDocStore).PutVerifiedDoc(ctx, repo, document)
}

func (writers *commitWriters) PutDoc(ctx context.Context, repo domain.ContentHash, document domain.SessionDoc) (bool, error) {
	writers.legacy++
	return writers.BlobStore.PutDoc(ctx, repo, document)
}

// commitLegacyOnly is a store that cannot take a document it was told is
// already checked.
type commitLegacyOnly struct {
	storage *store.FSStore
	writes  int
	answer  error
}

func (legacy *commitLegacyOnly) PutDoc(ctx context.Context, repo domain.ContentHash, document domain.SessionDoc) (bool, error) {
	legacy.writes++
	if legacy.answer != nil {
		return false, legacy.answer
	}
	return legacy.storage.PutDoc(ctx, repo, document)
}

type commitPassThrough struct{}

func (commitPassThrough) WithinDocumentCommit(ctx context.Context, _ domain.ContentHash, work func(context.Context) (inbound.CommitOutput, error)) (inbound.CommitOutput, error) {
	return work(ctx)
}

func (commitPassThrough) WithinChunkStore(ctx context.Context, _ domain.ContentHash, work func(context.Context) (inbound.StoreChunksOutput, error)) (inbound.StoreChunksOutput, error) {
	return work(ctx)
}

// A document that was checked is handed to the store as checked, once. A
// store that cannot take it that way gets the document itself; without either
// way there is nothing to commit to. What the store answers is the answer.
func TestCommitWritesEachDocumentOnceThroughTheWayTheStoreHas(t *testing.T) {
	document := commitChunkedDocument(t, "written once")
	whole := makeCommitDoc(t, "written once, whole")
	input := func(repo domain.ContentHash) inbound.CommitInput {
		return inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{whole},
			ChunkedDocs: []inbound.ChunkedDoc{document.chunked(document.plan)}, ChunkObjects: commitChunkObjects(document.plan)}
	}

	service, storage := newFsckSvc(t)
	repo := hh("commit writes once")
	bindCommitTestRepo(t, storage, repo)
	writers := &commitWriters{BlobStore: storage}
	service.blobs = writers
	if out, err := service.Commit(systemTestContext(), input(repo)); err != nil || out != (inbound.CommitOutput{StoredDocs: 2}) || writers.verified != 2 || writers.legacy != 0 {
		t.Fatalf("a store that takes checked documents: %+v %v, checked writes %d, other writes %d", out, err, writers.verified, writers.legacy)
	}
	commitStoredAs(t, storage, repo, document)

	refusal := errors.New("synthetic: the store refused the document")
	for name, s := range map[string]struct {
		legacy *commitLegacyOnly
		want   error
		out    inbound.CommitOutput
		writes int
	}{
		"a store that only takes the document itself": {legacy: &commitLegacyOnly{}, out: inbound.CommitOutput{StoredDocs: 2}, writes: 2},
		"what the store answers is the answer":        {legacy: &commitLegacyOnly{answer: refusal}, want: refusal, writes: 1},
		"no way to write at all":                      {want: domain.ErrCatalogUnsupported},
	} {
		storage := store.NewFSStore(t.TempDir())
		repo := hh("commit writes: " + name)
		bindCommitTestRepo(t, storage, repo)
		config := DocumentPublicationCommandsConfig{Profile: DocumentPublicationCompatibility, Store: storage, Chunks: storage, Scope: commitPassThrough{},
			SupportedIdentities: []domain.DocumentIdentity{domain.DocumentIdentityLegacy}}
		if s.legacy != nil {
			s.legacy.storage = storage
			config.LegacyWriter = s.legacy
		}
		commands, err := NewDocumentPublicationCommands(config)
		if err != nil {
			t.Fatal(name, err)
		}
		out, err := commands.Commit(systemTestContext(), input(repo))
		if s.want == nil && err != nil || s.want != nil && !errors.Is(err, s.want) || out != s.out {
			t.Errorf("%s: %+v %v", name, out, err)
		}
		if s.legacy != nil && s.legacy.writes != s.writes {
			t.Errorf("%s: %d writes", name, s.legacy.writes)
		}
	}
}
