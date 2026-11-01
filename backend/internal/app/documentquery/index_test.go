package documentquery

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type indexOwnerBoundKey struct{}

type indexOwnerPorts struct {
	test      *testing.T
	ctx       context.Context
	repo      domain.ContentHash
	hash      domain.ContentHash
	snapshot  domain.Snapshot
	document  domain.SessionDoc
	index     domain.DocReadIndex
	verified  domain.VerifiedSessionDoc
	calls     []string
	failStage string
	failure   error
}

func (ports *indexOwnerPorts) call(ctx context.Context, stage string, repo, hash domain.ContentHash) error {
	ports.test.Helper()
	if ctx != ports.ctx || repo != ports.repo || hash != "" && hash != ports.hash {
		ports.test.Fatal("index delegation replaced the caller's pinned context or exact repository/document identity")
	}
	ports.calls = append(ports.calls, stage)
	if err := ctx.Err(); err != nil {
		return err
	}
	if stage == ports.failStage {
		return ports.failure
	}
	return nil
}

func (ports *indexOwnerPorts) AuthorizeDocumentRead(context.Context, domain.ContentHash) (context.Context, error) {
	ports.test.Fatal("index delegation repeated document authorization")
	return nil, domain.ErrIntegrity
}

func (ports *indexOwnerPorts) AuthorizeDocumentPreparation(context.Context, domain.ContentHash) (context.Context, error) {
	ports.test.Fatal("index delegation attempted preparation")
	return nil, domain.ErrIntegrity
}

func (ports *indexOwnerPorts) WithinDocumentRead(context.Context, domain.ContentHash, func(context.Context) error) error {
	ports.test.Fatal("index delegation acquired a second document admission")
	return domain.ErrIntegrity
}

func (ports *indexOwnerPorts) WithinDocumentSnapshot(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	if err := ports.call(ctx, "snapshot", repo, ""); err != nil {
		return err
	}
	return read(ctx)
}

func (ports *indexOwnerPorts) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	err := ports.call(ctx, "metadata", repo, hash)
	return ports.snapshot, err
}

func (ports *indexOwnerPorts) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	err := ports.call(ctx, "document", repo, hash)
	return ports.document, err
}

func (ports *indexOwnerPorts) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	ports.test.Fatal("index metadata read fetched a chunk manifest")
	return domain.DocChunkManifest{}, domain.ErrIntegrity
}

func (ports *indexOwnerPorts) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	ports.test.Fatal("index metadata read fetched a payload chunk")
	return nil, domain.ErrIntegrity
}

func (ports *indexOwnerPorts) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	err := ports.call(ctx, "index", repo, hash)
	return ports.index, err
}

func (ports *indexOwnerPorts) SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error) {
	ports.test.Fatal("index delegation attempted a search")
	return nil, domain.ErrIntegrity
}

func (ports *indexOwnerPorts) ReadVerifiedDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	err := ports.call(ctx, "verified", repo, hash)
	return ports.verified, err
}

func indexOwnerFixture(test *testing.T) (*Queries, *indexOwnerPorts) {
	test.Helper()
	document := domain.SessionDoc{CIR: domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "index-owner"},
		Events: []domain.CIREvent{
			{Seq: 20, Kind: domain.EventMessage, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: "answer 한글"}}},
			{Seq: 10, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "question"}}},
		},
	}}
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	document.Hash = domain.HashContent(canonical)
	index, err := domain.BuildDocReadIndex(document)
	if err != nil {
		test.Fatal(err)
	}
	ports := &indexOwnerPorts{test: test, ctx: context.WithValue(context.Background(), indexOwnerBoundKey{}, "already pinned"), repo: domain.HashContent([]byte("index owner repo")), hash: document.Hash, document: document, index: index}
	ports.snapshot = domain.Snapshot{ID: ports.hash, RepoID: ports.repo, DocHash: ports.hash}
	return &Queries{scope: ports, metadata: ports, legacy: ports, index: ports, verified: ports, atomicRead: true}, ports
}

func TestReadIndexDelegatesWithoutReadmissionAndPreservesCanonicalHashes(test *testing.T) {
	for _, indexed := range []bool{false, true} {
		name := "legacy canonical source"
		if indexed {
			name = "explicit index source"
		}
		test.Run(name, func(test *testing.T) {
			queries, ports := indexOwnerFixture(test)
			wantCalls := []string{"snapshot", "metadata", "index"}
			if !indexed {
				queries.index = nil
				wantCalls[2] = "document"
			}
			got, err := queries.ReadIndex(ports.ctx, ports.repo, ports.hash)
			if err != nil || !reflect.DeepEqual(got, ports.index) || !reflect.DeepEqual(ports.calls, wantCalls) {
				test.Fatalf("index delegation changed canonical output or read sequence: %+v %v %v", got, err, ports.calls)
			}
			ports.calls = nil
			direct, directErr := queries.docReadIndex(ports.ctx, ports.repo, ports.hash)
			if directErr != nil || !reflect.DeepEqual(got, direct) || !reflect.DeepEqual(ports.calls, wantCalls) {
				test.Fatal("exported reader diverged from its existing document source", directErr)
			}
		})
	}
}

func TestReadIndexPreservesSourceErrorsAndCancellation(test *testing.T) {
	for _, stage := range []string{"snapshot", "metadata", "index", "document"} {
		for _, failure := range []error{domain.ErrIntegrity, domain.ErrNotFound, domain.ErrResourceOverloaded, context.Canceled, context.DeadlineExceeded} {
			test.Run(stage+"/"+failure.Error(), func(test *testing.T) {
				queries, ports := indexOwnerFixture(test)
				if stage == "document" {
					queries.index = nil
				}
				ports.failStage, ports.failure = stage, failure
				got, err := queries.ReadIndex(ports.ctx, ports.repo, ports.hash)
				if stage == "metadata" && errors.Is(failure, domain.ErrNotFound) {
					if err != nil || !reflect.DeepEqual(got, ports.index) || !reflect.DeepEqual(ports.calls, []string{"snapshot", "metadata", "index"}) {
						test.Fatal("missing legacy snapshot lost its existing fallback", err)
					}
					return
				}
				if !errors.Is(err, failure) || !reflect.DeepEqual(got, domain.DocReadIndex{}) || ports.calls[len(ports.calls)-1] != stage {
					test.Fatalf("index error crossed a source boundary or leaked partial metadata: %+v %v %v", got, err, ports.calls)
				}
				ports.calls = nil
				direct, directErr := queries.docReadIndex(ports.ctx, ports.repo, ports.hash)
				if !errors.Is(directErr, failure) || !reflect.DeepEqual(got, direct) {
					test.Fatal("exported reader changed the original error/result contract", directErr)
				}
			})
		}
	}
	queries, ports := indexOwnerFixture(test)
	ctx, cancel := context.WithCancel(ports.ctx)
	cancel()
	ports.ctx = ctx
	if got, err := queries.ReadIndex(ctx, ports.repo, ports.hash); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, domain.DocReadIndex{}) || !reflect.DeepEqual(ports.calls, []string{"snapshot"}) {
		test.Fatal("already canceled pinned index read reached a source", err)
	}
}

func TestReadIndexPreservesRootIdentityAndRangeProjection(test *testing.T) {
	queries, ports := indexOwnerFixture(test)
	manifest, bodies, err := domain.ConversationManifestForCIR(ports.document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	root, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		test.Fatal(err)
	}
	verifier := domain.NewCanonicalDocVerifierWithoutCache()
	ports.verified, err = verifier.VerifyConversationManifestDoc(ports.ctx, root, manifest, func(_ context.Context, hash domain.ContentHash) ([]byte, error) { return bodies[hash], nil })
	if err != nil {
		test.Fatal(err)
	}
	ports.hash = root
	ports.snapshot = domain.Snapshot{ID: root, RepoID: ports.repo, DocHash: root, DocIdentity: domain.DocumentIdentityRootV1}
	plan, err := ports.verified.PlanReadIndexContext(ports.ctx)
	if err != nil {
		test.Fatal(err)
	}
	want, err := plan.BuildRangesContext(ports.ctx)
	if err != nil {
		test.Fatal(err)
	}
	got, err := queries.ReadIndex(ports.ctx, ports.repo, root)
	if err != nil || !reflect.DeepEqual(got, want) || got.Hash != root || root == ports.document.Hash || !reflect.DeepEqual(ports.calls, []string{"snapshot", "metadata", "verified"}) {
		test.Fatalf("root proof was replaced by legacy identity or a different projection: %+v %v %v", got, err, ports.calls)
	}
	ports.calls, ports.failStage, ports.failure = nil, "verified", domain.ErrIntegrity
	if got, err := queries.ReadIndex(ports.ctx, ports.repo, root); !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(got, domain.DocReadIndex{}) || !reflect.DeepEqual(ports.calls, []string{"snapshot", "metadata", "verified"}) {
		test.Fatal("root verification failure fell back to a legacy/index source", err)
	}
	ports.calls, ports.failStage, ports.verified = nil, "", domain.VerifiedSessionDoc{}
	if got, err := queries.ReadIndex(ports.ctx, ports.repo, root); !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(got, domain.DocReadIndex{}) {
		test.Fatal("unproven root metadata became an index", err)
	}
}

func TestReadIndexKeepsSnapshotAndLegacyCanonicalIntegrityFences(test *testing.T) {
	for _, damage := range []string{"snapshot id", "snapshot document hash", "canonical bytes", "canonical hash"} {
		test.Run(damage, func(test *testing.T) {
			queries, ports := indexOwnerFixture(test)
			queries.index = nil
			switch damage {
			case "snapshot id":
				ports.snapshot.ID = domain.HashContent([]byte("wrong snapshot"))
			case "snapshot document hash":
				ports.snapshot.DocHash = domain.HashContent([]byte("wrong document"))
			case "canonical bytes":
				ports.document.CIR.Events[0].Blocks[0].Text = "tampered"
			case "canonical hash":
				ports.document.Hash = domain.HashContent([]byte("wrong canonical hash"))
			}
			got, err := queries.ReadIndex(ports.ctx, ports.repo, ports.hash)
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(got, domain.DocReadIndex{}) {
				test.Fatal("existing document source integrity validation was bypassed", err)
			}
		})
	}
}
