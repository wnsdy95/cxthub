//go:build postgres

package store

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docVisibleReadTracer struct {
	descriptors atomic.Int64
	objects     atomic.Int64
	forbidden   atomic.Int64
	unbounded   atomic.Int64
}

func (trace *docVisibleReadTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.Contains(query.SQL, "visible.descriptor") {
		trace.descriptors.Add(1)
		if len(query.Args) != 5 || query.Args[4] != domain.MaxDocVisibleGenerationBytes+1 || query.Args[3] != domain.MaxDocRangeGenerationBytes+1 {
			trace.unbounded.Add(1)
		}
	}
	if strings.Contains(query.SQL, "FROM doc_range_objects_v2") {
		trace.objects.Add(1)
		if len(query.Args) != 3 || query.Args[2] != domain.MaxDocVisibleObjectBytes+1 {
			trace.unbounded.Add(1)
		}
	}
	if strings.Contains(query.SQL, "doc_range_indexes_v1") || strings.Contains(query.SQL, "doc_range_pages_v1") ||
		strings.Contains(query.SQL, "root.bytes") || strings.Contains(query.SQL, "b.bytes") || strings.Contains(query.SQL, "kind='chunk'") {
		trace.forbidden.Add(1)
	}
	return ctx
}

func (*docVisibleReadTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func docVisibleHiddenFixture(t *testing.T, hidden int) rootReaderFixture {
	t.Helper()
	fixture := rangeFixture(t, 0)
	fixture.cir.Events = append(fixture.cir.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: fixture.cir.Envelope.SessionOriginID}}})
	for position := range hidden {
		event := domain.CIREvent{Kind: domain.EventReasoning, Seq: position + 2}
		if position%2 == 0 {
			event.Kind = domain.EventToolCall
			event.CallID = fmt.Sprint(position)
			event.ToolName = "hidden_tool"
			event.Input = map[string]any{"payload": strings.Repeat("hidden-payload-not-metadata", 32)}
		}
		fixture.cir.Events = append(fixture.cir.Events, event)
	}
	fixture.cir.Events = append(fixture.cir.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: hidden + 2, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: "done"}}})
	var err error
	fixture.manifest, fixture.bodies, err = domain.ConversationManifestForCIR(fixture.cir)
	if err != nil {
		t.Fatal(err)
	}
	fixture.hash, err = domain.ConversationManifestHash(fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	fixture.manifestBytes, err = domain.CanonicalConversationManifest(fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	fixture.canonical, err = domain.CanonicalBytes(fixture.cir)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestDocVisiblePGBoundedHiddenLookup(t *testing.T) {
	for _, hidden := range []int{1000, 10000} {
		t.Run(fmt.Sprint(hidden), func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := rootPGRepo(t, ctx, storage)
			fixture := docVisibleHiddenFixture(t, hidden)
			seedRootPG(t, ctx, storage, repo, fixture)
			if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.manifest.Chunks[0].Hash, []byte("damaged source")); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_pages_v1 SET bytes=$2 WHERE doc_hash=$1`, fixture.hash, []byte("damaged legacy pages")); err != nil {
				t.Fatal(err)
			}
			trace := &docVisibleReadTracer{}
			reader := docRangeBoundaryPGTracedStore(t, storage, ctx, trace)
			if err := reader.WithinReadSnapshot(ctx, func(bound context.Context) error {
				opened, err := reader.OpenDocVisibleGeneration(bound, repo, fixture.hash)
				if err != nil {
					return err
				}
				read := opened.(*postgresDocVisibleGenerationRead)
				for _, mode := range []string{"chat", "prompts"} {
					ordinal := 1
					want := domain.DocVisibleUnit{From: 1, To: hidden + 2, EventIndex: hidden + 1, TurnIndex: 0, RenderedBefore: 1, FoldRows: hidden / 2}
					if mode == "prompts" {
						ordinal = 0
						want = domain.DocVisibleUnit{From: 0, To: hidden + 2, EventIndex: 0, TurnIndex: 0, FoldRows: hidden/2 + 1}
					}
					before := trace.objects.Load()
					for range 3 {
						for _, byEvent := range []bool{false, true} {
							position := ordinal
							if byEvent {
								position = hidden / 2
							}
							unit, selected, err := read.Unit(bound, mode, position, byEvent)
							if err != nil || selected != ordinal || unit != want {
								t.Fatal("hidden unit", mode, byEvent, unit, selected, err)
							}
						}
					}
					if fetched := trace.objects.Load() - before; fetched != 2 {
						t.Fatal("hidden lookup scaled with source length", hidden, mode, fetched)
					}
				}
				metadataBytes := 0
				for _, raw := range read.objects.objects {
					metadataBytes += len(raw)
					if strings.Contains(string(raw), "hidden-payload-not-metadata") {
						t.Fatal("body copied into visible metadata")
					}
				}
				if metadataBytes > 2048 || trace.descriptors.Load() != 1 || trace.forbidden.Load() != 0 || trace.unbounded.Load() != 0 {
					t.Fatal("unbounded hidden lookup", metadataBytes, trace.descriptors.Load(), trace.forbidden.Load(), trace.unbounded.Load())
				}
				t.Logf("hidden=%d chat+prompts_objects=%d selected_metadata_bytes=%d", hidden, trace.objects.Load(), metadataBytes)
				for position := 0; position < hidden; position += domain.DocVisiblePageUnits {
					if _, _, err := read.Unit(bound, "all", position, false); err != nil {
						return err
					}
					if len(read.objects.objects) > docRangeGenerationCacheObjects {
						t.Fatal("unbounded visible object cache", len(read.objects.objects))
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
