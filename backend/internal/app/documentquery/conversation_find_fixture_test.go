package documentquery

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type conversationFindAdmissionKey struct{}
type conversationFindSnapshotKey struct{}

type conversationFindAdmission struct {
	live bool
}

type conversationFindSnapshot struct {
	live      bool
	admission *conversationFindAdmission
}

type conversationFindFixture struct {
	test              *testing.T
	repo, hash        domain.ContentHash
	identity          domain.DocumentIdentity
	binding           domain.DocReadGenerationBinding
	generation        domain.DocRangeGeneration
	objects           map[domain.ContentHash][]byte
	manifest          domain.ConversationManifest
	bodies            map[domain.ContentHash][]byte
	visible           domain.DocVisibleGeneration
	visibleReader     *domain.DocVisibleReader
	ready             bool
	readOnly          bool
	readFailure       error
	generationFailure error
	displayFailure    error
	job               domain.DocRangeIndexJob
	active            int
	admissions        int
	snapshots         int
	preparations      int
	enqueued          int
	pages             int
	chunks            int
	displays          int
}

func newConversationFindFixture(test *testing.T, legacy bool, events []domain.CIREvent) (*Queries, *conversationFindFixture) {
	test.Helper()
	ctx := context.Background()
	for index := range events {
		events[index].Seq = index
	}
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex}, Events: events}
	manifest, bodies, err := domain.ConversationManifestForCIR(cir)
	if err != nil {
		test.Fatal(err)
	}
	fixture := &conversationFindFixture{test: test, repo: domain.HashContent([]byte("conversation-find-repository")),
		identity: domain.DocumentIdentityRootV1, manifest: manifest, bodies: bodies, objects: map[domain.ContentHash][]byte{},
		ready: true, readOnly: true, displayFailure: domain.ErrNotFound, job: domain.DocRangeIndexJob{State: "waiting"}}
	var objects []domain.DocRangeObject
	if legacy {
		fixture.identity = domain.DocumentIdentityLegacy
		raw, err := domain.CanonicalBytes(cir)
		if err != nil {
			test.Fatal(err)
		}
		fixture.hash = domain.HashContent(raw)
		verified, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: fixture.hash, CIR: cir})
		if err != nil {
			test.Fatal(err)
		}
		fixture.generation, objects, err = domain.BuildLegacyDocRangeGeneration(ctx, verified)
		if err != nil {
			test.Fatal(err)
		}
	} else {
		fixture.hash, err = domain.ConversationManifestHash(manifest)
		if err != nil {
			test.Fatal(err)
		}
		var verifier domain.CanonicalDocVerifier
		verified, err := verifier.VerifyConversationManifestDoc(ctx, fixture.hash, manifest, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
			return bodies[hash], nil
		})
		if err != nil {
			test.Fatal(err)
		}
		index, err := domain.BuildDocRangeIndex(ctx, verified)
		if err != nil {
			test.Fatal(err)
		}
		proof, generated, err := domain.BuildVerifiedDocRangeGeneration(ctx, index)
		if err != nil {
			test.Fatal(err)
		}
		fixture.generation, objects = proof.Descriptor(), generated
		for page, hash := range index.Header().Pages {
			raw, err := index.Page(ctx, page)
			if err != nil {
				test.Fatal(err)
			}
			fixture.objects[hash] = raw
		}
		fixture.visible, err = domain.BuildDocVisibleGeneration(ctx, proof, func(_ context.Context, object domain.DocRangeObject) error {
			fixture.objects[object.Hash] = object.Bytes
			return nil
		})
		if err != nil {
			test.Fatal(err)
		}
		fixture.visibleReader, err = domain.NewDocVisibleReader(fixture.visible, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
			return fixture.objects[hash], nil
		})
		if err != nil {
			test.Fatal(err)
		}
	}
	for _, object := range objects {
		fixture.objects[object.Hash] = object.Bytes
	}
	raw, err := fixture.generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	fixture.binding = domain.DocReadGenerationBinding{SourceID: "11111111-1111-4111-8111-111111111111", RangeGeneration: domain.HashContent(raw)}
	queries, err := New(Config{Profile: LegacyCompatibility, Scope: fixture, Snapshots: fixture, Legacy: fixture, AtomicRead: true,
		View: &ViewDependencies{Range: fixture, RangeGenerations: fixture, RangeReadiness: fixture, RangeJobs: fixture,
			VisibleGenerations: fixture, VisiblePreparation: fixture, Chunks: fixture, Resources: fixture},
		Find: &FindDependencies{DisplayRead: fixture, Display: fixture}})
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if fixture.active != 0 {
			test.Fatal("conversation find leaked admission", fixture.active)
		}
	})
	return queries, fixture
}

func (fixture *conversationFindFixture) requireAdmission(ctx context.Context) *conversationFindAdmission {
	fixture.test.Helper()
	admission, _ := ctx.Value(conversationFindAdmissionKey{}).(*conversationFindAdmission)
	if admission == nil || !admission.live || fixture.readOnly && !outbound.DocReadOnly(ctx) {
		fixture.test.Fatal("conversation find lost live admission or its query read-only flag")
	}
	return admission
}

func (fixture *conversationFindFixture) requireRead(ctx context.Context) *conversationFindSnapshot {
	fixture.test.Helper()
	fixture.requireAdmission(ctx)
	snapshot, _ := ctx.Value(conversationFindSnapshotKey{}).(*conversationFindSnapshot)
	if snapshot == nil || !snapshot.live || !snapshot.admission.live {
		fixture.test.Fatal("conversation find escaped its pinned read scope")
	}
	return snapshot
}

func (fixture *conversationFindFixture) AcquireInteractive(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	if admission, _ := ctx.Value(conversationFindAdmissionKey{}).(*conversationFindAdmission); admission != nil && admission.live {
		return ctx, func() {}, nil
	}
	fixture.active++
	fixture.admissions++
	admission := &conversationFindAdmission{live: true}
	return context.WithValue(ctx, conversationFindAdmissionKey{}, admission), func() {
		if !admission.live {
			fixture.test.Fatal("conversation find released an admission twice")
		}
		admission.live = false
		fixture.active--
	}, nil
}

func (fixture *conversationFindFixture) AcquireBulk(ctx context.Context) (context.Context, func(), error) {
	return fixture.AcquireInteractive(ctx)
}

func (fixture *conversationFindFixture) AuthorizeDocumentRead(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	fixture.requireAdmission(ctx)
	if repo != fixture.repo {
		return ctx, domain.ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return ctx, err
	}
	return ctx, fixture.readFailure
}

func (fixture *conversationFindFixture) AuthorizeDocumentPreparation(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	fixture.requireAdmission(ctx)
	fixture.preparations++
	if outbound.DocReadOnly(ctx) || ctx.Value(conversationFindSnapshotKey{}) != nil || repo != fixture.repo {
		fixture.test.Fatal("conversation find prepared inside a read-only or pinned read scope")
	}
	return ctx, ctx.Err()
}

func (fixture *conversationFindFixture) WithinDocumentRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return fixture.WithinDocumentSnapshot(ctx, repo, read)
}

func (fixture *conversationFindFixture) WithinDocumentSnapshot(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	bound, err := fixture.AuthorizeDocumentRead(ctx, repo)
	if err != nil {
		return err
	}
	fixture.snapshots++
	snapshot := &conversationFindSnapshot{live: true, admission: fixture.requireAdmission(bound)}
	defer func() { snapshot.live = false }()
	return read(context.WithValue(bound, conversationFindSnapshotKey{}, snapshot))
}

func (fixture *conversationFindFixture) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	fixture.requireRead(ctx)
	if repo != fixture.repo || hash != fixture.hash {
		return domain.Snapshot{}, domain.ErrNotFound
	}
	return domain.Snapshot{ID: hash, RepoID: repo, DocHash: hash, DocIdentity: fixture.identity}, nil
}

func (fixture *conversationFindFixture) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	fixture.test.Fatal("conversation find materialized a full document")
	return domain.SessionDoc{}, domain.ErrIntegrity
}

func (fixture *conversationFindFixture) GetDocManifest(ctx context.Context, repo, hash domain.ContentHash) (domain.DocChunkManifest, error) {
	fixture.requireRead(ctx)
	if repo != fixture.repo || hash != fixture.hash || fixture.identity != domain.DocumentIdentityLegacy {
		return domain.DocChunkManifest{}, domain.ErrIntegrity
	}
	manifest := domain.DocChunkManifest{Format: domain.ChunkFormatV2, Envelope: fixture.manifest.Envelope}
	for _, chunk := range fixture.manifest.Chunks {
		manifest.Chunks = append(manifest.Chunks, chunk.Hash)
	}
	return manifest, nil
}

func (fixture *conversationFindFixture) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	fixture.requireRead(ctx)
	fixture.chunks++
	if repo != fixture.repo || fixture.bodies[hash] == nil {
		return nil, domain.ErrNotFound
	}
	return fixture.bodies[hash], nil
}

func (fixture *conversationFindFixture) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	raw, err := fixture.GetChunk(ctx, repo, hash)
	if err == nil && int64(len(raw)) != length {
		return nil, domain.ErrIntegrity
	}
	return raw, err
}

func (fixture *conversationFindFixture) InReadOnlyTransaction(ctx context.Context) bool {
	snapshot, _ := ctx.Value(conversationFindSnapshotKey{}).(*conversationFindSnapshot)
	return snapshot != nil && snapshot.live
}

func (fixture *conversationFindFixture) HasDocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	fixture.requireRead(ctx)
	if repo != fixture.repo || hash != fixture.hash {
		return false, domain.ErrNotFound
	}
	return fixture.ready, nil
}

func (fixture *conversationFindFixture) HasDocVisibleIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	return fixture.HasDocRangeIndex(ctx, repo, hash)
}

func (fixture *conversationFindFixture) EnqueueDocRangeJob(ctx context.Context, repo, hash domain.ContentHash, _ time.Time) (domain.DocRangeIndexJob, error) {
	fixture.requireAdmission(ctx)
	if outbound.DocReadOnly(ctx) || ctx.Value(conversationFindSnapshotKey{}) != nil || repo != fixture.repo || hash != fixture.hash {
		fixture.test.Fatal("conversation find enqueued work with the wrong scope")
	}
	fixture.enqueued++
	return fixture.job, nil
}

func (fixture *conversationFindFixture) EnqueueDocVisibleIndex(ctx context.Context, repo, hash domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	return fixture.EnqueueDocRangeJob(ctx, repo, hash, now)
}

func (fixture *conversationFindFixture) DocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocRangeIndex, error) {
	fixture.test.Fatal("conversation find bypassed its pinned generation")
	return domain.DocRangeIndex{}, domain.ErrIntegrity
}

func (fixture *conversationFindFixture) DocRangePage(context.Context, domain.ContentHash, domain.DocRangeIndex, int) ([]domain.DocRangeEntry, error) {
	fixture.test.Fatal("conversation find bypassed its pinned range reader")
	return nil, domain.ErrIntegrity
}

type conversationFindRangeRead struct {
	fixture  *conversationFindFixture
	snapshot *conversationFindSnapshot
	binding  domain.DocReadGenerationBinding
}

func (fixture *conversationFindFixture) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	snapshot := fixture.requireRead(ctx)
	if repo != fixture.repo || hash != fixture.hash {
		return nil, domain.ErrNotFound
	}
	if fixture.generationFailure != nil {
		return nil, fixture.generationFailure
	}
	return &conversationFindRangeRead{fixture: fixture, snapshot: snapshot, binding: fixture.binding}, nil
}

func (read *conversationFindRangeRead) Descriptor() domain.DocRangeGeneration {
	if !read.snapshot.live || !read.snapshot.admission.live {
		read.fixture.test.Fatal("conversation find retained a range reader beyond its snapshot")
	}
	return read.fixture.generation
}

func (read *conversationFindRangeRead) Binding() domain.DocReadGenerationBinding {
	_ = read.Descriptor()
	return read.binding
}

func (read *conversationFindRangeRead) Envelope(ctx context.Context) (json.RawMessage, error) {
	if read.fixture.requireRead(ctx) != read.snapshot {
		read.fixture.test.Fatal("conversation find changed the selected envelope snapshot")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := read.Binding().Validate(); err != nil {
		return nil, err
	}
	return bytes.Clone(read.Descriptor().Envelope), nil
}

func (read *conversationFindRangeRead) PageHash(ctx context.Context, page int) (domain.ContentHash, error) {
	if read.fixture.requireRead(ctx) != read.snapshot {
		read.fixture.test.Fatal("conversation find changed the selected range snapshot")
	}
	return domain.ResolveDocRangeLeaf(ctx, read.Descriptor().EventPages, page, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		return read.fixture.objects[hash], nil
	})
}

func (read *conversationFindRangeRead) ReadPage(ctx context.Context, page int) ([]domain.DocRangeEntry, error) {
	read.fixture.pages++
	hash, err := read.PageHash(ctx, page)
	if err != nil {
		return nil, err
	}
	return read.Descriptor().DecodePage(page, hash, read.fixture.objects[hash])
}

func (read *conversationFindRangeRead) Chunk(ctx context.Context, position int) (domain.ConversationManifestChunk, error) {
	if read.fixture.requireRead(ctx) != read.snapshot {
		read.fixture.test.Fatal("conversation find changed the selected chunk snapshot")
	}
	if position < 0 || position >= len(read.fixture.manifest.Chunks) {
		return domain.ConversationManifestChunk{}, domain.ErrIntegrity
	}
	return read.fixture.manifest.Chunks[position], nil
}

type conversationFindVisibleRead struct {
	fixture  *conversationFindFixture
	snapshot *conversationFindSnapshot
	binding  domain.DocReadGenerationBinding
}

func (fixture *conversationFindFixture) OpenDocVisibleGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocVisibleGenerationRead, error) {
	snapshot := fixture.requireRead(ctx)
	if repo != fixture.repo || hash != fixture.hash {
		return nil, domain.ErrNotFound
	}
	if fixture.generationFailure != nil {
		return nil, fixture.generationFailure
	}
	return &conversationFindVisibleRead{fixture: fixture, snapshot: snapshot, binding: fixture.binding}, nil
}

func (read *conversationFindVisibleRead) Descriptor() domain.DocVisibleGeneration {
	if !read.snapshot.live || !read.snapshot.admission.live {
		read.fixture.test.Fatal("conversation find retained a visible reader beyond its snapshot")
	}
	return read.fixture.visible
}

func (read *conversationFindVisibleRead) Binding() domain.DocReadGenerationBinding {
	_ = read.Descriptor()
	return read.binding
}

func (read *conversationFindVisibleRead) Unit(ctx context.Context, mode string, position int, byEvent bool) (domain.DocVisibleUnit, int, error) {
	if read.fixture.requireRead(ctx) != read.snapshot {
		read.fixture.test.Fatal("conversation find changed the selected visible snapshot")
	}
	_ = read.Descriptor()
	return read.fixture.visibleReader.Unit(ctx, mode, position, byEvent)
}

func (fixture *conversationFindFixture) ReadDocDisplayGeneration(ctx context.Context, target domain.DocDisplayTarget) (domain.DocDisplayStringGeneration, error) {
	fixture.requireRead(ctx)
	fixture.displays++
	if target.RepoID != fixture.repo || target.DocHash != fixture.hash || target.Binding != fixture.binding {
		return domain.DocDisplayStringGeneration{}, domain.ErrIntegrity
	}
	return domain.DocDisplayStringGeneration{}, fixture.displayFailure
}

func (fixture *conversationFindFixture) ReadDocDisplayPage(context.Context, domain.DocDisplayTarget, int) (domain.DocDisplayPage, error) {
	fixture.test.Fatal("conversation find loaded a missing display generation")
	return domain.DocDisplayPage{}, domain.ErrIntegrity
}

func (fixture *conversationFindFixture) EnqueueDocDisplay(ctx context.Context, target domain.DocDisplayTarget, now time.Time) (domain.DocDisplayJob, error) {
	job, err := fixture.EnqueueDocRangeJob(ctx, target.RepoID, target.DocHash, now)
	return domain.DocDisplayJob{DocRangeIndexJob: job}, err
}
