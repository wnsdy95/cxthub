//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docCanonicalPagesPGContext struct {
	context.Context
	native           atomic.Bool
	inspectionChecks atomic.Int64
}

func (ctx *docCanonicalPagesPGContext) Err() error {
	var callers [4]uintptr
	frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
	caller, _ := frames.Next()
	consumer, _ := frames.Next()
	if strings.HasSuffix(caller.Function, "domain.newLegacyCanonicalEncodingNative") ||
		strings.HasSuffix(caller.Function, "domain.finishLegacyCanonicalEncodingNative") {
		ctx.native.Store(true)
	}
	if strings.HasSuffix(caller.Function, "domain.(*legacyCanonicalPreparation).step") &&
		strings.HasSuffix(consumer.Function, "domain.(*LegacyCanonicalEncoding).Next") {
		ctx.inspectionChecks.Add(1)
	}
	return ctx.Context.Err()
}

type docCanonicalPagesPGSpoolState struct {
	spool    *legacyDocSpool
	logical  int
	physical int64
}

type docCanonicalPagesPGFixture struct {
	replacementPhase string
}

type docCanonicalPagesPGNativeState struct {
	encoding         uintptr
	owner            uintptr
	wrapper          uintptr
	source           reflect.Value
	sourceShape      [5]int64
	sourceBacking    [3]uintptr
	heapBytes        int64
	inspectionWork   uint64
	keyWork          uint64
	orderCount       int64
	orderWork        uint64
	actions          int64
	bytes            int64
	storageBytes     int64
	lastStorageBytes int64
}

func docCanonicalPagesPGNativeSnapshot(test *testing.T, work *preparedLegacyCaptureWork, replacement bool) docCanonicalPagesPGNativeState {
	test.Helper()
	preparation := reflect.ValueOf(work.normalization.preparation).Elem()
	pending, next := preparation.FieldByName("pending"), preparation.FieldByName("next").Int()
	if next >= int64(pending.Len()) {
		return docCanonicalPagesPGNativeState{}
	}
	encoding := pending.Index(int(next)).FieldByName("Encoding")
	if encoding.IsNil() || encoding.Elem().FieldByName("preparation").IsNil() {
		return docCanonicalPagesPGNativeState{}
	}
	if preparation.FieldByName("written").Int() != 0 {
		test.Fatal("active native preparation exposed event bytes before completing its projection and sort")
	}
	value := encoding.Elem()
	owner := value.FieldByName("nativeOwner")
	if owner.IsNil() {
		test.Fatal("active preparation did not retain the actual native owner")
	}
	active := value.FieldByName("preparation").Elem()
	state := docCanonicalPagesPGNativeState{encoding: encoding.Pointer(), owner: owner.Pointer(),
		heapBytes:      value.FieldByName("rootHeap").FieldByName("bytes").Int(),
		inspectionWork: value.FieldByName("inspectWork").Uint(), keyWork: value.FieldByName("keyWork").Uint(),
		actions: active.FieldByName("lastActions").Int(), bytes: active.FieldByName("lastBytes").Int(),
		storageBytes: active.FieldByName("lastStorageBytes").Int(), lastStorageBytes: value.FieldByName("lastStorageBytes").Int()}
	if replacement {
		field := value.FieldByName("root").Elem().MapIndex(reflect.ValueOf("replacement"))
		if !field.IsValid() || field.IsNil() || field.Elem().Kind() != reflect.Pointer ||
			field.Elem().IsNil() || field.Elem().Type().Elem().Name() != "legacyCanonicalReplacement" {
			test.Fatal("native replacement did not retain its deferred wrapper")
		}
		wrapper := field.Elem()
		source := wrapper.Elem().FieldByName("native")
		owned := owner.Elem().FieldByName("replacement")
		if source.IsNil() || owned.IsNil() || source.Pointer() != owned.Pointer() ||
			source.Elem().Type().Name() != "legacyNativeEventValues" || !wrapper.Elem().FieldByName("events").IsNil() ||
			!owner.Elem().FieldByName("event").FieldByName("Replacement").IsNil() {
			test.Fatal("replacement wrapper detached from its original native-owned pages")
		}
		state.wrapper, state.source = wrapper.Pointer(), source
		records := source.Elem().FieldByName("records")
		state.sourceShape = [5]int64{source.Elem().FieldByName("count").Int(), records.FieldByName("count").Int(),
			records.FieldByName("pages").Int(), records.FieldByName("nodes").Int(), records.FieldByName("depth").Int()}
		state.sourceBacking = [3]uintptr{records.FieldByName("single").Pointer(), records.FieldByName("inline").Pointer(), records.FieldByName("root").Pointer()}
		order := wrapper.Elem().FieldByName("order")
		state.orderCount, state.orderWork = order.FieldByName("count").Int(), order.FieldByName("work").Uint()
	}
	return state
}

func docCanonicalPagesPGRecordHeap(test *testing.T, records reflect.Value, inspect func(reflect.Value, int64)) rootPublicationHeapEstimate {
	test.Helper()
	single, inline, root := records.FieldByName("single"), records.FieldByName("inline"), records.FieldByName("root")
	count, depth := records.FieldByName("count").Int(), records.FieldByName("depth").Int()
	if count < 0 || depth < 0 || depth > (int64(reflect.TypeFor[uintptr]().Size())*8-1)/6 {
		test.Fatal("native records escaped their bounded count or page depth", count, depth)
	}
	var heap rootPublicationHeapEstimate
	var nodes, pages, visited int64
	checkRecord := func(record reflect.Value, ordinal int64) {
		if ordinal < count {
			if inspect != nil {
				inspect(record, ordinal)
			}
			visited++
		}
	}
	checkArray := func(array reflect.Value, width int, base int64) {
		if array.Kind() != reflect.Array || array.Len() != width || array.Type().Size() > domain.LegacyCanonicalEncodingBytes {
			test.Fatal("native records lost their fixed bounded value page", array.Type(), width)
		}
		heap.allocation(1, int64(array.Type().Size()))
		for offset := 0; offset < array.Len() && base+int64(offset) < count; offset++ {
			checkRecord(array.Index(offset), base+int64(offset))
		}
	}
	if root.IsNil() {
		if depth != 0 || !single.IsNil() && !inline.IsNil() {
			test.Fatal("native records retained incompatible direct backing", count, depth)
		}
		switch {
		case !single.IsNil():
			record := single.Elem()
			if count > 1 || record.Type().Size() > domain.LegacyCanonicalEncodingBytes ||
				inline.Type().Elem().Kind() != reflect.Array || inline.Type().Elem().Len() != 8 ||
				record.Type() != inline.Type().Elem().Elem() || inline.Type().Elem().Size() > domain.LegacyCanonicalEncodingBytes ||
				count == 0 && !record.IsZero() {
				test.Fatal("native records lost their bounded actual singleton or retained cleared slot", count)
			}
			heap.allocation(1, int64(record.Type().Size()))
			checkRecord(record, 0)
		case !inline.IsNil():
			if count > 8 {
				test.Fatal("native records exceeded their fixed inline backing", count)
			}
			checkArray(inline.Elem(), 8, 0)
		default:
			if count != 0 {
				test.Fatal("native records lost their allocated backing", count)
			}
		}
	} else {
		if !single.IsNil() || !inline.IsNil() || count > 0 && (count-1)>>(6*(depth+1)) != 0 {
			test.Fatal("native records retained direct backing alongside their tree or exceeded its depth", count, depth)
		}
		var visit func(reflect.Value, int64, int64)
		visit = func(node reflect.Value, level, base int64) {
			if node.Elem().Type().Size() > domain.LegacyCanonicalEncodingBytes {
				test.Fatal("native records allocated an oversized page node")
			}
			nodes++
			heap.allocation(1, int64(node.Elem().Type().Size()))
			children, page := node.Elem().FieldByName("children"), node.Elem().FieldByName("values")
			if children.Kind() != reflect.Array || children.Len() != 64 || level == 0 && page.IsNil() || level > 0 && !page.IsNil() {
				test.Fatal("native records lost their fixed paged-core node shape", level)
			}
			if !page.IsNil() {
				pages++
				checkArray(page.Elem(), 64, base)
			}
			for ordinal := 0; ordinal < children.Len(); ordinal++ {
				if child := children.Index(ordinal); !child.IsNil() {
					if level == 0 {
						test.Fatal("native records retained a child below their leaf page")
					}
					visit(child, level-1, base+int64(ordinal)*(int64(1)<<(6*level)))
				}
			}
		}
		visit(root, depth, 0)
	}
	if heap.err != nil || nodes != records.FieldByName("nodes").Int() || pages != records.FieldByName("pages").Int() || visited != count {
		test.Fatal("native record backing disagrees with its actual allocation or reserved slots", heap, nodes, pages, visited, count)
	}
	return heap
}

func docCanonicalPagesPGReplacementUnchanged(test *testing.T, source reflect.Value, expected []domain.CIREvent) {
	test.Helper()
	if !source.IsValid() || source.Kind() != reflect.Pointer || source.IsNil() ||
		source.Elem().Type().Name() != "legacyNativeEventValues" || source.Elem().FieldByName("count").Int() != int64(len(expected)) ||
		source.Elem().FieldByName("records").FieldByName("count").Int() != int64(len(expected)) {
		test.Fatal("native replacement lost its original paged children")
	}
	docCanonicalPagesPGRecordHeap(test, source.Elem().FieldByName("records"), nil)
	for ordinal, event := range expected {
		child := docCanonicalPagesPGReplacementChild(test, source, ordinal)
		if child.FieldByName("Kind").String() != string(event.Kind) || child.FieldByName("Seq").Int() != int64(event.Seq) ||
			child.FieldByName("CallID").String() != event.CallID || child.FieldByName("ToolName").String() != event.ToolName {
			test.Fatal("native replacement mutated its original child or stable tie order", ordinal)
		}
		input := child.FieldByName("Input")
		if input.Len() != len(event.Input) {
			test.Fatal("native replacement mutated its original child input", ordinal)
		}
		for key, want := range event.Input {
			got := input.MapIndex(reflect.ValueOf(key))
			if !got.IsValid() || got.IsNil() || got.Elem().Kind() != reflect.String || got.Elem().String() != want.(string) {
				test.Fatal("native replacement mutated its original child input field", ordinal, key)
			}
		}
	}
}

func docCanonicalPagesPGReplacementChild(test *testing.T, source reflect.Value, ordinal int) reflect.Value {
	test.Helper()
	records := source.Elem().FieldByName("records")
	single, inline, node := records.FieldByName("single"), records.FieldByName("inline"), records.FieldByName("root")
	depth := records.FieldByName("depth").Int()
	if ordinal < 0 || int64(ordinal) >= records.FieldByName("count").Int() || depth < 0 ||
		depth > (int64(reflect.TypeFor[uintptr]().Size())*8-1)/6 {
		test.Fatal("native replacement child escaped its original page bounds", ordinal, depth)
	}
	if node.IsNil() {
		if !single.IsNil() {
			if !inline.IsNil() || records.FieldByName("count").Int() != 1 || ordinal != 0 || depth != 0 ||
				records.FieldByName("nodes").Int() != 0 || records.FieldByName("pages").Int() != 0 ||
				single.Elem().Type().Size() > domain.LegacyCanonicalEncodingBytes {
				test.Fatal("native replacement lost its actual singleton child record", ordinal)
			}
			return single.Elem().FieldByName("event")
		}
		if inline.IsNil() || inline.Elem().Kind() != reflect.Array || inline.Elem().Len() != 8 || ordinal >= 8 || depth != 0 {
			test.Fatal("native replacement lost its fixed inline child page", ordinal)
		}
		return inline.Elem().Index(ordinal).FieldByName("event")
	}
	if !single.IsNil() || !inline.IsNil() {
		test.Fatal("native replacement retained a direct child record alongside its tree")
	}
	for ; depth > 0; depth-- {
		children := node.Elem().FieldByName("children")
		if children.Kind() != reflect.Array || children.Len() != 64 {
			test.Fatal("native replacement changed its fixed page directory")
		}
		node = children.Index((ordinal >> uint(6*depth)) & 63)
		if node.IsNil() {
			test.Fatal("native replacement lost its original child page", ordinal, depth)
		}
	}
	page := node.Elem().FieldByName("values")
	if page.IsNil() || page.Elem().Kind() != reflect.Array || page.Elem().Len() != 64 ||
		page.Elem().Type().Size() > domain.LegacyCanonicalEncodingBytes {
		test.Fatal("native replacement lost its fixed bounded child page", ordinal)
	}
	return page.Elem().Index(ordinal & 63).FieldByName("event")
}

func docCanonicalPagesPGScratch(test *testing.T, work *preparedLegacyCaptureWork) [domain.LegacyNormalizationSlots]docCanonicalPagesPGSpoolState {
	test.Helper()
	if work.normalization == nil || work.normalization.storage == nil || work.normalization.preparation == nil ||
		work.normalization.canonical != nil || work.normalization.data != nil || work.work != nil ||
		work.canonical != nil || work.canonicalProof.Valid() || work.proofData != nil || work.original {
		test.Fatal("dense native preparation escaped its provisional normalization phase")
	}
	var state [domain.LegacyNormalizationSlots]docCanonicalPagesPGSpoolState
	for slot, spool := range work.normalization.storage.spools {
		if spool == nil || spool.file == nil {
			test.Fatal("dense native preparation lost its scratch slot", slot)
		}
		info, err := spool.file.Stat()
		if err != nil || info.Size() != int64(spool.size) {
			test.Fatal("dense native scratch physical and logical extents disagree", slot, err)
		}
		state[slot] = docCanonicalPagesPGSpoolState{spool: spool, logical: spool.size, physical: info.Size()}
	}
	return state
}

func TestDocCanonicalPagesPGActiveNativePreparationRetainsFences(test *testing.T) {
	docCanonicalPagesPGActiveNativePreparation(test, docCanonicalPagesPGFixture{})
	for _, phase := range []string{"projection", "sort"} {
		test.Run("replacement "+phase, func(test *testing.T) {
			docCanonicalPagesPGActiveNativePreparation(test, docCanonicalPagesPGFixture{replacementPhase: phase})
		})
	}
}

func docCanonicalPagesPGDrainCheckpoint(test *testing.T, work *preparedLegacyCaptureWork, ctx context.Context, job domain.DocRangeIndexJob, remaining *int) bool {
	test.Helper()
	drained := false
	var sequence int64
	if work.checkpoint != nil {
		sequence = work.checkpoint.state.sequence
	}
	for work.checkpoint.busy() {
		if *remaining == 0 {
			test.Fatal("native preparation exhausted its existing checkpoint processing budget")
		}
		*remaining--
		if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
			test.Fatal("native preparation checkpoint failed", progress, err, "sequence", work.checkpoint.state.sequence,
				"position", work.checkpoint.position, "written", work.checkpoint.next.State.Written, "remaining", *remaining)
		}
		drained = true
	}
	if drained && (work.checkpoint.state.sequence <= sequence || !work.checkpoint.root.valid(work.checkpoint.state)) {
		test.Fatal("checkpoint-only quanta did not acknowledge a valid durable continuation")
	}
	return drained
}

func docCanonicalPagesPGActiveNativePreparation(test *testing.T, activeFixture docCanonicalPagesPGFixture) {
	test.Helper()
	for _, scenario := range []struct {
		name          string
		change        string
		canceled      bool
		argumentFence bool
	}{
		{name: "unchanged"},
		{name: "source version", change: "source version"},
		{name: "source incarnation", change: "source incarnation"},
		{name: "ownership", change: "ownership"},
		{name: "lease expiry", change: "lease expiry"},
		{name: "job fence", change: "job fence"},
		{name: "snapshot identity", change: "snapshot identity"},
		{name: "argument fence", argumentFence: true},
		{name: "cancellation", canceled: true},
		{name: "cancellation before source conflict", change: "source version", canceled: true},
		{name: "cancellation before ownership failure", change: "ownership", canceled: true},
		{name: "cancellation before argument fence", canceled: true, argumentFence: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			repo, _ := docRangeJobPGRepo(test, storage, ctx)
			const fields = 4*1024 + 1
			replacement := activeFixture.replacementPhase != ""
			preparationLimit := docCaptureWorkPGPreparationStepLimit
			var event domain.CIREvent
			if replacement {
				children := make([]domain.CIREvent, fields)
				for ordinal := range children {
					children[ordinal] = domain.CIREvent{Kind: domain.EventToolCall, Seq: (fields-1-ordinal)/2 - fields/4,
						CallID: fmt.Sprintf("replacement-%05d", ordinal), ToolName: "replacement-pages",
						Input: map[string]any{"z-last": fmt.Sprintf("child-%05d \ud55c🙂\"\\\n<>&", ordinal), "a-first": "first", "m-middle": "middle"}}
				}
				event = domain.CIREvent{Kind: domain.EventCompaction, Seq: 0, Replacement: children, ReplacementComplete: true}
				preparationLimit += len(children)
			} else {
				input := make(map[string]any, fields)
				for position := 0; position < fields; position++ {
					input[fmt.Sprintf("field-%05d", position)] = []any{fmt.Sprintf("value-%05d", position), "\ud55c🙂\"\\\n<>&", position%2 == 0, nil}
				}
				event = domain.CIREvent{Kind: domain.EventToolCall, Seq: 0, CallID: "dense-pages", ToolName: "dense-pages", Input: input}
			}
			document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
				SessionOriginID: string(repo), Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{event}}
			fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
			raw := fixture.raw
			if replacement {
				var err error
				raw, err = json.Marshal(document)
				if err != nil || bytes.Equal(raw, fixture.raw) || len(event.Replacement) != fields ||
					event.Replacement[0].Seq <= event.Replacement[fields-1].Seq || event.Replacement[1].Seq != event.Replacement[2].Seq {
					test.Fatal("replacement fixture lost its unsorted children or stable sequence ties", err)
				}
			}
			var stored bytes.Buffer
			if err := json.Indent(&stored, raw, "", "  "); err != nil || bytes.Equal(stored.Bytes(), fixture.raw) {
				test.Fatal("dense map fixture did not produce a noncanonical stored representation", err)
			}
			work, job, trace := docNormalizationPGStartSource(test, storage, ctx, fixture, stored.Bytes())
			bodyReads := trace.bodyReads.Load()
			private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			inspection := &docCanonicalPagesPGContext{Context: ctx}
			witnessed := 0
			checkpointSteps := normalizationCheckpointPGStepLimit
			var originalReplacement reflect.Value
			for quantum := 0; quantum < preparationLimit && witnessed < 2; quantum++ {
				if docCanonicalPagesPGDrainCheckpoint(test, work, ctx, job, &checkpointSteps) {
					private = legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
				}
				before := docCanonicalPagesPGScratch(test, work)
				nativeBefore := docCanonicalPagesPGNativeSnapshot(test, work, replacement)
				checks := inspection.inspectionChecks.Load()
				progress, err := work.Step(inspection, job)
				if err != nil || progress != outbound.DocRangeWorkPending {
					test.Fatal("dense native preparation did not yield pending", quantum, progress, err)
				}
				after := docCanonicalPagesPGScratch(test, work)
				nativeAfter := docCanonicalPagesPGNativeSnapshot(test, work, replacement)
				if replacement && nativeAfter.encoding != 0 && !originalReplacement.IsValid() {
					originalReplacement = nativeAfter.source
					docCanonicalPagesPGReplacementUnchanged(test, originalReplacement, event.Replacement)
				}
				checks = inspection.inspectionChecks.Load() - checks
				if checks == 0 {
					if witnessed != 0 {
						test.Fatal("dense native inspection did not span consecutive work quanta", quantum)
					}
					continue
				}
				if !inspection.native.Load() || checks > 1024+1 || after != before {
					test.Fatal("native inspection exceeded its action bound or advanced output scratch", quantum, checks)
				}
				if nativeBefore.encoding == 0 || nativeAfter.encoding != nativeBefore.encoding || nativeAfter.owner != nativeBefore.owner ||
					nativeAfter.actions <= 0 || nativeAfter.actions > 1024 || nativeAfter.bytes < 0 || nativeAfter.bytes > 64<<10 ||
					nativeAfter.storageBytes < 0 || nativeAfter.storageBytes > nativeAfter.bytes || nativeAfter.lastStorageBytes != nativeAfter.storageBytes ||
					nativeAfter.inspectionWork < nativeBefore.inspectionWork || nativeAfter.inspectionWork-nativeBefore.inspectionWork != uint64(nativeAfter.actions) ||
					nativeAfter.keyWork < nativeBefore.keyWork || nativeAfter.keyWork-nativeBefore.keyWork > 1024 {
					test.Fatal("active native Next exceeded its original 64KiB/1024 preparation quantum", quantum, nativeBefore, nativeAfter)
				}
				if replacement {
					projecting := nativeAfter.orderCount == 0 && nativeAfter.heapBytes > nativeBefore.heapBytes && nativeAfter.keyWork == nativeBefore.keyWork
					sorting := nativeAfter.orderCount == fields && nativeAfter.orderWork > nativeBefore.orderWork && nativeAfter.keyWork > nativeBefore.keyWork
					if nativeAfter.wrapper != nativeBefore.wrapper || nativeAfter.source.Pointer() != nativeBefore.source.Pointer() {
						test.Fatal("replacement preparation replaced its active wrapper or native source")
					}
					if activeFixture.replacementPhase == "projection" && !projecting || activeFixture.replacementPhase == "sort" && !sorting {
						if witnessed != 0 {
							test.Fatal("replacement preparation did not span consecutive active quanta", activeFixture.replacementPhase, quantum)
						}
						continue
					}
				}
				witnessed++
			}
			if witnessed != 2 {
				test.Fatal("dense native preparation was not witnessed across two bounded Next quanta")
			}
			if replacement {
				docCanonicalPagesPGReplacementUnchanged(test, originalReplacement, event.Replacement)
			}
			if trace.bodyReads.Load() != bodyReads || private != legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) ||
				public != legacyByteStagePGPublicImage(test, storage, ctx, fixture) {
				test.Fatal("active native page preparation reread SQL payload or changed persisted state")
			}
			if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
				test.Fatal("unfinished dense native preparation became publishable", err)
			}
			if scenario.name == "unchanged" {
				original := legacyDerivedOriginalImagePG(test, storage, ctx, fixture)
				progress := outbound.DocRangeWorkPending
				var err error
				for quantum := 0; quantum < preparationLimit && work.work == nil; quantum++ {
					docCanonicalPagesPGDrainCheckpoint(test, work, ctx, job, &checkpointSteps)
					progress, err = work.Step(ctx, job)
					if err != nil || progress != outbound.DocRangeWorkPending {
						break
					}
				}
				if err != nil || progress != outbound.DocRangeWorkPending || work.work == nil {
					test.Fatal("dense native preparation did not reach its same-process verified handoff", progress, err)
				}
				if replacement {
					docCanonicalPagesPGReplacementUnchanged(test, originalReplacement, event.Replacement)
				}
				for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && progress != outbound.DocRangeWorkPublishable; quantum++ {
					progress, err = work.Step(ctx, job)
					if err != nil || progress == outbound.DocRangeWorkCompatibilityRequired {
						test.Fatal("dense native control failed delegated preparation", quantum, progress, err)
					}
				}
				if progress != outbound.DocRangeWorkPublishable || trace.bodyReads.Load() != bodyReads ||
					original != legacyDerivedOriginalImagePG(test, storage, ctx, fixture) {
					test.Fatal("dense native control reread SQL body or changed its immutable source before publication", progress, trace.bodyReads.Load(), bodyReads)
				}
				if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
					_, err := storage.OpenDocRangeGeneration(read, fixture.repo, fixture.hash)
					if errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
						return nil
					}
					return errors.Join(domain.ErrIntegrity, err)
				}); err != nil {
					test.Fatal("private page preparation became readable before publication", err)
				}
				if err := work.Publish(ctx, job); err != nil {
					test.Fatal("publish dense native control", err)
				}
				legacyDerivedPublishedPG(test, storage, ctx, fixture)
				if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
					reader, err := storage.OpenDocRangeGeneration(read, fixture.repo, fixture.hash)
					if err != nil {
						return err
					}
					generation := reader.Descriptor()
					if generation.Hash != fixture.hash || generation.Identity() != domain.DocumentIdentityLegacy ||
						generation.ByteStorage() != domain.DocRangeByteStorageDerived || generation.EventCount != 1 ||
						generation.StreamBytes != int64(len(fixture.stream)) || !bytes.Equal(generation.Envelope, fixture.envelope) {
						test.Fatal("dense native publication changed its canonical descriptor", generation)
					}
					var stream []byte
					for ordinal := 0; ordinal < generation.ChunkCount(); ordinal++ {
						want, descriptor := legacyByteStagePGBlock(fixture, ordinal)
						body, err := storage.ReadLegacyDerivedByteChunk(read, fixture.repo, fixture.hash, reader.Binding(), ordinal, descriptor)
						if err != nil {
							return err
						}
						if !bytes.Equal(body, want) || domain.HashContent(body) != descriptor.Hash {
							test.Fatal("dense native publication changed canonical block bytes or hash", ordinal)
						}
						stream = append(stream, body...)
					}
					raw := domain.AssembleCanonicalDoc(generation.Envelope, [][]byte{stream})
					if !bytes.Equal(raw, fixture.raw) || domain.HashContent(raw) != fixture.hash {
						test.Fatal("published dense native ranges did not reconstruct the original exact bytes and hash")
					}
					return nil
				}); err != nil {
					test.Fatal(err)
				}
				proof, err := storage.ReadVerifiedDoc(ctx, fixture.repo, fixture.hash)
				if err != nil || proof.Hash() != fixture.hash || !bytes.Equal(proof.Bytes(), fixture.raw) {
					test.Fatal("dense native control changed the original verified document", err)
				}
				if trace.bodyReads.Load() != bodyReads || original != legacyDerivedOriginalImagePG(test, storage, ctx, fixture) {
					test.Fatal("dense native publication reread its SQL source body or rewrote archival tuples")
				}
				if replacement {
					docCanonicalPagesPGReplacementUnchanged(test, originalReplacement, event.Replacement)
				}
				return
			}
			if scenario.change != "" {
				legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, scenario.change)
				if scenario.change == "source incarnation" {
					if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, stored.Bytes()); err != nil {
						test.Fatal(err)
					}
					var sourceID string
					var sourceVersion int64
					var sourceBytes []byte
					if err := storage.pool.QueryRow(ctx, `SELECT incarnation.incarnation::text,source.doc_range_source_version,source.bytes
 FROM blobs source JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash WHERE source.hash=$1`, fixture.hash).
						Scan(&sourceID, &sourceVersion, &sourceBytes); err != nil || sourceID == work.witness.stage.sourceID ||
						sourceVersion != work.witness.stage.sourceVersion || !bytes.Equal(sourceBytes, stored.Bytes()) {
						test.Fatal("source reincarnation did not preserve the original bytes and source version", sourceID, sourceVersion, err)
					}
				}
			}
			candidate := job
			if scenario.argumentFence {
				candidate.Version++
			}
			selected, cancel := context.WithCancel(ctx)
			defer cancel()
			want := error(domain.ErrConflict)
			if scenario.change == "ownership" || scenario.change == "snapshot identity" {
				want = domain.ErrNotFound
			}
			if scenario.canceled {
				cancel()
				want = context.Canceled
			}
			fenced := &docCanonicalPagesPGContext{Context: selected}
			private = legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			public = legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			scratch := docCanonicalPagesPGScratch(test, work)
			resources, err := work.RetainedResources()
			if err != nil {
				test.Fatal(err)
			}
			normalization, preparation := work.normalization, work.normalization.preparation
			native := docCanonicalPagesPGNativeSnapshot(test, work, replacement)
			queries := trace.queries.Load()
			progress, err := work.Step(fenced, candidate)
			if !errors.Is(err, want) || progress != 0 || scenario.canceled && err != context.Canceled ||
				fenced.inspectionChecks.Load() != 0 || fenced.native.Load() || work.normalization != normalization ||
				work.normalization.preparation != preparation || docCanonicalPagesPGScratch(test, work) != scratch {
				test.Fatal("revoked dense native quantum resumed inspection, scratch, or authority", progress, err, want)
			}
			if after, err := work.RetainedResources(); err != nil || after != resources {
				test.Fatal("rejected dense native quantum changed retained local resources", resources, after, err)
			}
			after := docCanonicalPagesPGNativeSnapshot(test, work, replacement)
			if after.encoding != native.encoding || after.owner != native.owner || after.wrapper != native.wrapper ||
				after.heapBytes != native.heapBytes || after.inspectionWork != native.inspectionWork || after.keyWork != native.keyWork ||
				after.orderCount != native.orderCount || after.orderWork != native.orderWork || after.actions != native.actions ||
				after.bytes != native.bytes || after.storageBytes != native.storageBytes || after.lastStorageBytes != native.lastStorageBytes {
				test.Fatal("rejected native quantum advanced its retained preparation or replacement sort")
			}
			if replacement {
				if after.source.Pointer() != native.source.Pointer() || after.sourceShape != native.sourceShape || after.sourceBacking != native.sourceBacking {
					test.Fatal("rejected replacement quantum replaced or resized its retained native source pages")
				}
				docCanonicalPagesPGReplacementUnchanged(test, originalReplacement, event.Replacement)
			}
			if trace.bodyReads.Load() != bodyReads || (scenario.canceled || scenario.argumentFence) && trace.queries.Load() != queries {
				test.Fatal("rejected dense native quantum reread SQL payload or a pre-SQL rejection touched the database")
			}
			if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
				test.Fatal("rejected dense native preparation became publishable", err)
			}
			if private != legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) || public != legacyByteStagePGPublicImage(test, storage, ctx, fixture) {
				test.Fatal("rejected dense native quantum changed persisted source, jobs, staging, or public ranges")
			}
		})
	}
}
