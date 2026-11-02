package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unsafe"
)

type legacyNativeBlockPagedBudgetSnapshot struct {
	data    *legacyNativeBlockValues
	records int
	nodes   int
	pages   int
	single  *legacyNativeBlockValue
	inline  *[legacyCanonicalInlineValues]legacyNativeBlockValue
	storage int
	values  [legacyCanonicalInlineValues]legacyNativeBlockValue
	anchors map[int]*legacyNativeBlockValue
}

type legacyNativeBlockPagedBudgetRun struct {
	test       *testing.T
	normalizer *LegacyNormalizationDecoder
	buffer     *legacySourceProofBuffer
	raw        []byte
	current    context.Context
	cancel     context.CancelFunc
	snapshots  map[*legacyNativeBlockList]*legacyNativeBlockPagedBudgetSnapshot
	data       *legacyNativeBlockValues
	single     bool
	inline     bool
	singleCopy bool
	inlineCopy bool
	depth      int
	steps      int
	cancelRead bool
	triggered  bool
}

type legacyNativeBlockPagedBudgetContext struct {
	context.Context
	run *legacyNativeBlockPagedBudgetRun
}

func (current *legacyNativeBlockPagedBudgetContext) Err() error {
	if err := current.Context.Err(); err != nil {
		return err
	}
	if native := current.run.normalizer.nativeCache; native != nil {
		current.run.observe(native.object)
	}
	return nil
}

func legacyNativeBlockPagedBudgetStart(test *testing.T, nested bool) *legacyNativeBlockPagedBudgetRun {
	test.Helper()
	block := `{"type":"text","text":"` + strings.Repeat("x", 257) + `"}`
	event := `{"kind":"message","seq":7,"role":"user","blocks":[` + strings.Repeat(block+",", 4096) + block + `]}`
	if nested {
		event = `{"kind":"compaction","seq":7,"replacement_complete":true,"replacement":[` + event + `]}`
	}
	raw := legacyNativeEventDocument("2", event)
	if len(raw) <= 2*(512<<10) || ConversationManifestChunkBytes != 512<<10 || LegacyCanonicalEncodingBytes != 64<<10 ||
		legacyCanonicalEncodingActions != 1024 || legacyCanonicalInlineValues != 8 || legacyCanonicalPageValues != 64 {
		test.Fatal("paged block fixture or original source/output/page quanta changed", len(raw))
	}
	run := &legacyNativeBlockPagedBudgetRun{
		test: test, raw: raw, buffer: &legacySourceProofBuffer{body: bytes.Clone(raw)},
		snapshots: make(map[*legacyNativeBlockList]*legacyNativeBlockPagedBudgetSnapshot),
	}
	source := legacyNormalizationPreparationInput(test, run.buffer, true)
	readSource := source.Read
	source.Read = func(current context.Context, offset, length int) ([]byte, error) {
		if current != run.current || current.Err() != nil {
			test.Fatal("public paged blocks read using a previous or canceled call context", offset, length)
		}
		body, err := readSource(current, offset, length)
		if run.cancelRead && run.data != nil && run.data.records.length() > 64 {
			run.triggered = true
			run.cancel()
			return nil, errors.New("paged block source failure concurrent with cancellation")
		}
		return body, err
	}
	created, cancel := context.WithCancel(context.Background())
	var err error
	run.normalizer, err = NewLegacyNormalizationDecoder(created, source)
	cancel()
	if err != nil || run.normalizer == nil || len(run.buffer.reads) != 0 {
		test.Fatal("public paged block constructor read source or failed", err)
	}
	return run
}

func (run *legacyNativeBlockPagedBudgetRun) observe(object *legacyNativeObject) {
	if object == nil {
		return
	}
	for index, list := range object.blocks {
		if list == nil {
			continue
		}
		if !object.pagedEvents || !list.paged || list.decoded != nil || list.previous != nil || list.payloads != nil ||
			list.previousHeaps != nil || list.excess != nil || list.previousExtra != nil ||
			!object.destination.Elem().Field(object.metadata.fields[index].index).IsNil() {
			run.test.Fatal("actual public block list retained a typed block/payload/excess/previous array")
		}
		if list.data == nil {
			continue
		}
		data := list.data
		if run.data != nil && run.data != data {
			run.test.Fatal("single-list public fixture acquired a copied block data container")
		}
		run.data = data
		records := &data.records
		if !records.valid() || data.length() < 0 || data.length() > records.length() ||
			data.at(-1) != nil || data.at(data.length()) != nil {
			run.test.Fatal("actual public block pages lost logical/highwater access bounds")
		}
		switch {
		case records.length() == 0:
			if records.single != nil || records.inline != nil || records.root != nil {
				run.test.Fatal("unstarted public block source allocated value storage")
			}
		case records.length() == 1:
			if records.single == nil || records.inline != nil || records.root != nil || records.at(0) != records.single {
				run.test.Fatal("first public block source record did not use its actual single slot")
			}
		case records.length() <= legacyCanonicalInlineValues:
			if records.single != nil || records.inline == nil || records.root != nil || records.at(0) != &records.inline[0] {
				run.test.Fatal("second through eighth public source records did not use the actual inline slots")
			}
		default:
			if records.single != nil || records.inline != nil || records.root == nil {
				run.test.Fatal("ninth and later public source records retained small-list storage")
			}
		}
		snapshot := run.snapshots[list]
		if snapshot == nil {
			snapshot = &legacyNativeBlockPagedBudgetSnapshot{data: data, anchors: make(map[int]*legacyNativeBlockValue)}
			run.snapshots[list] = snapshot
		}
		added := records.length() - snapshot.records
		if snapshot.data != data || added < 0 || added > 1 || records.nodes < snapshot.nodes || records.pages < snapshot.pages ||
			records.pages-snapshot.pages > added || records.nodes-snapshot.nodes > added*(records.depth+1) {
			run.test.Fatal("public block growth replaced data or escaped fixed page/node geometry")
		}
		if snapshot.single != nil && records.single != nil && snapshot.single != records.single ||
			snapshot.inline != nil && records.inline != nil && snapshot.inline != records.inline {
			run.test.Fatal("public block source replaced stable single/inline storage before promotion")
		}
		storage := (records.nodes-snapshot.nodes)*int(unsafe.Sizeof(legacyCanonicalPageNode[legacyNativeBlockValue]{})) +
			(records.pages-snapshot.pages)*int(unsafe.Sizeof([legacyCanonicalPageValues]legacyNativeBlockValue{}))
		if records.single != nil && snapshot.single == nil {
			storage += int(unsafe.Sizeof(*records.single))
		}
		if records.inline != nil && snapshot.inline == nil {
			storage += int(unsafe.Sizeof(*records.inline))
		}
		if snapshot.single != nil && records.single == nil {
			first := records.at(0)
			if snapshot.records != 1 || records.length() != 2 || records.inline == nil || first == nil ||
				first == snapshot.single || *first != snapshot.values[0] ||
				unsafe.StringData(first.block.Type) != unsafe.StringData(snapshot.values[0].block.Type) ||
				unsafe.StringData(first.block.Text) != unsafe.StringData(snapshot.values[0].block.Text) {
				run.test.Fatal("second public source record did not promote exactly one unchanged single-slot value")
			}
			storage += int(unsafe.Sizeof(snapshot.values[0]))
			run.singleCopy = true
		}
		if snapshot.inline != nil && records.inline == nil {
			if snapshot.records != legacyCanonicalInlineValues || records.length() != legacyCanonicalInlineValues+1 ||
				records.root == nil || records.nodes != 1 || records.pages != 1 || records.depth != 0 {
				run.test.Fatal("ninth public source record did not promote inline storage to one fixed page")
			}
			for position, previous := range snapshot.values {
				record := records.at(position)
				if record == nil || record == &snapshot.inline[position] || *record != previous ||
					unsafe.StringData(record.block.Type) != unsafe.StringData(previous.block.Type) ||
					unsafe.StringData(record.block.Text) != unsafe.StringData(previous.block.Text) {
					run.test.Fatal("public source page promotion changed an inline value or its payload backing", position)
				}
			}
			storage += int(unsafe.Sizeof(*snapshot.inline))
			run.inlineCopy = true
		}
		nextStorage, storageErr := records.appendBytes()
		if storage < 0 || storage > max(1, added)*(64<<10) || added == 1 && storage != snapshot.storage ||
			storageErr != nil || nextStorage < 0 || nextStorage > 64<<10 ||
			unsafe.Sizeof([legacyCanonicalPageValues]legacyNativeBlockValue{}) > 64<<10 ||
			unsafe.Sizeof(legacyCanonicalPageNode[legacyNativeBlockValue]{}) > 64<<10 {
			run.test.Fatal("actual block page allocation/copy reservation exceeds the original fixed storage quantum", storage, added)
		}
		if records.root != nil {
			for _, position := range []int{0, 7, 8, 63, 64, 4095, 4096} {
				if position >= snapshot.records {
					continue
				}
				record := records.at(position)
				if record == nil || snapshot.anchors[position] != nil && snapshot.anchors[position] != record {
					run.test.Fatal("actual public block growth copied a previously paged record", position)
				}
				snapshot.anchors[position] = record
			}
		}
		if records.root == nil {
			for position := 0; position < records.length(); position++ {
				record := records.at(position)
				if record == nil {
					run.test.Fatal("public source small-list storage omitted a reserved record", position)
				}
				snapshot.values[position] = *record
			}
		}
		run.single = run.single || records.single != nil
		run.inline = run.inline || records.inline != nil
		run.depth = max(run.depth, records.depth)
		snapshot.records, snapshot.nodes, snapshot.pages = records.length(), records.nodes, records.pages
		snapshot.single, snapshot.inline, snapshot.storage = records.single, records.inline, nextStorage
	}
	for _, list := range object.events {
		if list != nil {
			run.observe(list.object)
		}
	}
}

func (run *legacyNativeBlockPagedBudgetRun) step() (LegacyNormalizationStep, error) {
	run.test.Helper()
	before := run.normalizer.position
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	step, err := legacyNativeScalarReviewStep(run.test, ctx, run.normalizer, run.buffer, func(current context.Context) context.Context {
		observed := &legacyNativeBlockPagedBudgetContext{Context: current, run: run}
		run.current = observed
		return observed
	})
	cancel()
	run.steps++
	if advanced := run.normalizer.position - before; advanced < 0 || advanced > 512<<10 || !bytes.Equal(run.buffer.body, run.raw) {
		run.test.Fatal("public paged block Step escaped source progress bounds or mutated source", advanced)
	}
	if run.data != nil && err == nil && run.normalizer.native != nil {
		retained, retainedErr := run.normalizer.RetainedBytes()
		backing := run.data.backing()
		if retainedErr != nil || backing.err != nil || retained < backing.bytes {
			run.test.Fatal("public active block pages disappeared from retained storage", retained, backing.bytes, retainedErr, backing.err)
		}
	}
	return step, err
}

func (run *legacyNativeBlockPagedBudgetRun) exhausted() {
	run.test.Helper()
	native := run.normalizer.native
	if native == nil {
		run.test.Fatal("shared budget probe has no actual active public native driver")
	}
	position, records, reads := native.object.progressPosition(), run.data.records.length(), len(run.buffer.reads)
	for _, exhausted := range []struct {
		work   int
		fields int
	}{
		{work: 512 << 10},
		{fields: native.object.fieldActionLimit()},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		input := &legacyNormalizationInput{ctx: ctx, source: run.normalizer.source, workBytes: exhausted.work, fields: exhausted.fields}
		_, complete, err := native.step(ctx, input, &run.normalizer.frame)
		cancel()
		if err != nil || complete || input.workBytes != exhausted.work || input.fields != exhausted.fields || input.loads != 0 ||
			input.unbounded || input.failure != nil || native.object.progressPosition() != position ||
			run.data.records.length() != records || len(run.buffer.reads) != reads {
			run.test.Fatal("actual public block list ignored exhausted shared bytes/actions", complete, err, input.workBytes, input.fields)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	observed := &legacyNativeBlockPagedBudgetContext{Context: ctx, run: run}
	run.current = observed
	initialWork := (512 << 10) - (64 << 10)
	input := &legacyNormalizationInput{ctx: observed, source: run.normalizer.source, workBytes: initialWork}
	step, err := run.normalizer.scan(observed, input)
	cancel()
	advanced := run.normalizer.position - position
	if err != nil || input.failure != nil || step.Complete || len(step.Events) != 0 || input.workBytes <= initialWork ||
		input.workBytes > 512<<10 || input.loads < 0 || input.loads > 2 || input.unbounded ||
		input.fields < 0 || input.fields > native.object.fieldActionLimit() ||
		len(run.buffer.reads)-reads != input.loads || advanced <= 0 || advanced > input.workBytes-initialWork ||
		run.normalizer.native != native || !bytes.Equal(run.buffer.body, run.raw) {
		run.test.Fatal("actual public block driver reset or exceeded the caller's remaining 64KiB/shared two-load budget", err, input.workBytes, input.loads, advanced)
	}
}

func (run *legacyNativeBlockPagedBudgetRun) encoding() *LegacyCanonicalEncoding {
	run.test.Helper()
	for range 2048 {
		step, err := run.step()
		if err != nil || step.Complete || len(step.Events) > 1 {
			run.test.Fatal("public paged block source failed before its single event", err)
		}
		if len(step.Events) == 1 {
			encoding := step.Events[0].Encoding
			if encoding == nil || encoding.preparation == nil || encoding.inspectWork != 0 || encoding.nativeOwner == nil ||
				run.data == nil || run.data.length() != 4097 || run.data.records.length() != 4097 ||
				run.data.records.pages != 65 || run.depth != 2 || !run.single || !run.inline || !run.singleCopy || !run.inlineCopy {
				run.test.Fatal("public constructor did not hand off unvisited real fixed block pages")
			}
			return encoding
		}
	}
	run.test.Fatal("public paged block event exhausted its finite source allowance")
	return nil
}

func legacyNativeBlockPagedBudgetFrame(test *testing.T, encoding *LegacyCanonicalEncoding, data *legacyNativeBlockValues) (bool, bool) {
	test.Helper()
	prepared, emitted := false, false
	if encoding.preparation != nil {
		for position := 0; position < encoding.preparation.frames.length(); position++ {
			frame := encoding.preparation.frames.at(position)
			if frame.kind == 'd' {
				if frame.value != data || frame.length != data.length() || frame.count != data.records.length() {
					test.Fatal("actual block preparation copied data or replaced logical/highwater bounds")
				}
				prepared = true
			}
		}
	}
	for position := 0; position < encoding.frames.length(); position++ {
		frame := encoding.frames.at(position)
		if frame.kind == 'd' {
			if frame.value != data {
				test.Fatal("actual block output copied the native data reference")
			}
			emitted = true
		}
	}
	return prepared, emitted
}

func TestLegacyNativeBlockPagedBudgetPublicQuanta(test *testing.T) {
	for _, nested := range []bool{false, true} {
		test.Run(fmt.Sprintf("nested=%t", nested), func(test *testing.T) {
			run := legacyNativeBlockPagedBudgetStart(test, nested)
			shared := false
			for range 2048 {
				if run.data != nil && run.data.records.length() > 64 && run.normalizer.native != nil {
					run.exhausted()
					shared = true
					break
				}
				step, err := run.step()
				if err != nil || step.Complete || len(step.Events) != 0 {
					test.Fatal("public block fixture skipped its active multi-page source phase", err)
				}
			}
			if !shared {
				test.Fatal("actual public block source never exposed its shared budget boundary")
			}
			encoding := run.encoding()
			defer encoding.release()
			if !nested {
				root, ok := encoding.root.(map[string]any)
				if !ok || root["blocks"] != run.data {
					test.Fatal("public root projection did not borrow the actual pagelist data reference")
				}
			}
			reads := len(run.buffer.reads)
			var body []byte
			preparationSteps, outputSteps := 0, 0
			preparedPages, emittedPages, finished := false, false, false
			for range 1 << 16 {
				preparation, before := encoding.preparation, encoding.inspectWork
				prepared, emitted := legacyNativeBlockPagedBudgetFrame(test, encoding, run.data)
				preparedPages, emittedPages = preparedPages || prepared, emittedPages || emitted
				current, cancel := context.WithCancel(context.Background())
				chunk, complete, err := encoding.Next(current)
				cancel()
				if err != nil || len(chunk) > 64<<10 || cap(chunk) > 64<<10 || encoding.lastStorageBytes < 0 ||
					encoding.lastStorageBytes > 64<<10 || len(run.buffer.reads) != reads || !bytes.Equal(run.buffer.body, run.raw) {
					test.Fatal("public paged block Next failed, reread source, or escaped output/storage quanta", err, len(chunk), cap(chunk))
				}
				if preparation != nil {
					preparationSteps++
					if len(chunk) != 0 || cap(chunk) != 0 || complete || preparation.lastActions <= 0 || preparation.lastActions > 1024 ||
						preparation.lastBytes < 0 || preparation.lastBytes > 64<<10 || preparation.lastStorageBytes < 0 ||
						preparation.lastStorageBytes > preparation.lastBytes || encoding.lastStorageBytes != preparation.lastStorageBytes ||
						encoding.inspectWork-before != uint64(preparation.lastActions) {
						test.Fatal("actual paged block preparation made no accounted progress or exceeded 64KiB/1024")
					}
				} else {
					outputSteps++
					if encoding.inspectWork != before || !complete && len(chunk) == 0 {
						test.Fatal("actual paged block emission restarted inspection or stalled")
					}
				}
				body = append(body, chunk...)
				if complete {
					finished = true
					break
				}
			}
			if !finished || !preparedPages || !emittedPages || preparationSteps < 2 || outputSteps < 2 || !json.Valid(body) ||
				bytes.Count(body, []byte(`"type":"text"`)) != 4097 || encoding.nativeOwner != nil || encoding.root != nil {
				test.Fatal("actual public block preparation/output did not make finite multi-call page progress", finished, preparedPages, emittedPages, preparationSteps, outputSteps)
			}
			chunk, complete, err := encoding.Next(context.Background())
			if err != nil || !complete || len(chunk) != 0 || len(run.buffer.reads) != reads {
				test.Fatal("completed public paged block output revived source work", complete, err)
			}
			completed := false
			for range 128 {
				step, err := run.step()
				if err != nil || len(step.Events) != 0 {
					test.Fatal("public block source failed after its encoding", err)
				}
				if step.Complete {
					if step.Count != 1 || !json.Valid(step.Envelope) {
						test.Fatal("public paged blocks lost their final event count/header")
					}
					completed = true
					break
				}
			}
			if !completed || run.steps < 2 {
				test.Fatal("public paged block source did not complete within its finite allowance")
			}
			legacyCanonicalBlocksPublicSourceComplete(test, run.buffer)
		})
	}
}

func TestLegacyNativeBlockPagedBudgetCurrentSourceCancellation(test *testing.T) {
	run := legacyNativeBlockPagedBudgetStart(test, false)
	for range 2048 {
		if run.data != nil && run.data.records.length() > 64 && run.normalizer.native != nil {
			break
		}
		step, err := run.step()
		if err != nil || step.Complete || len(step.Events) != 0 {
			test.Fatal("source cancellation fixture skipped active public block pages", err)
		}
	}
	if run.data == nil || run.data.records.length() <= 64 || run.normalizer.native == nil {
		test.Fatal("source cancellation fixture never reached real active block pages")
	}
	reads, position, records := len(run.buffer.reads), run.normalizer.position, run.data.records.length()
	retained, retainedErr := run.normalizer.RetainedBytes()
	prior, cancel := context.WithCancel(context.Background())
	cancel()
	step, err := run.normalizer.Step(prior)
	legacyNativeEventZero(test, step)
	after, afterErr := run.normalizer.RetainedBytes()
	if !errors.Is(err, context.Canceled) || len(run.buffer.reads) != reads || run.normalizer.position != position ||
		run.data.records.length() != records || retainedErr != nil || afterErr != nil || after != retained || run.normalizer.terminal != nil {
		test.Fatal("pre-canceled public call changed active pages or poisoned fresh-context resumption", err)
	}
	step, err = run.step()
	if err != nil || step.Complete || len(step.Events) != 0 || run.data.records.length() <= records || len(run.buffer.reads) <= reads {
		test.Fatal("fresh public call failed to resume pages with its current source context", err)
	}
	run.cancelRead = true
	reads = len(run.buffer.reads)
	step, err = run.step()
	legacyNativeEventZero(test, step)
	if !run.triggered || !errors.Is(err, context.Canceled) || len(run.buffer.reads) != reads+1 ||
		run.normalizer.native != nil || !errors.Is(run.normalizer.terminal, context.Canceled) {
		test.Fatal("current source-read cancellation lost priority, continued loading, or exposed a partial public event", err)
	}
	reads = len(run.buffer.reads)
	step, err = run.normalizer.Step(context.Background())
	legacyNativeEventZero(test, step)
	if !errors.Is(err, context.Canceled) || len(run.buffer.reads) != reads {
		test.Fatal("terminal paged source cancellation restarted authenticated reads", err)
	}
}

func TestLegacyNativeBlockPagedBudgetCurrentNextCancellation(test *testing.T) {
	for _, stage := range []string{"preparation", "emission"} {
		test.Run(stage, func(test *testing.T) {
			run := legacyNativeBlockPagedBudgetStart(test, true)
			encoding := run.encoding()
			defer encoding.release()
			reads := len(run.buffer.reads)
			reached := false
			for range 1 << 16 {
				prepared, emitted := legacyNativeBlockPagedBudgetFrame(test, encoding, run.data)
				if stage == "preparation" && prepared || stage == "emission" && emitted {
					reached = true
					break
				}
				current, cancel := context.WithCancel(context.Background())
				_, complete, err := encoding.Next(current)
				cancel()
				if err != nil || complete {
					test.Fatal("Next cancellation fixture skipped its actual page traversal", stage, err)
				}
			}
			if !reached {
				test.Fatal("Next cancellation fixture exhausted its finite traversal allowance", stage)
			}
			current, cancel := context.WithCancel(context.Background())
			checks := 0
			observed := legacyCanonicalBlocksOwnershipCancellationContext{Context: current, cancelWhen: func() bool {
				checks++
				if checks == 3 {
					cancel()
				}
				return false
			}}
			chunk, complete, err := encoding.Next(observed)
			cancel()
			if checks < 3 || !errors.Is(err, context.Canceled) || complete || len(chunk) != 0 || len(run.buffer.reads) != reads ||
				encoding.nativeOwner != nil || encoding.preparation != nil || encoding.root != nil || encoding.frames.length() != 0 {
				test.Fatal("current Next cancellation retained live traversal or leaked partial bytes/source work", stage, checks, err)
			}
			chunk, complete, err = encoding.Next(context.Background())
			if !errors.Is(err, context.Canceled) || complete || len(chunk) != 0 || len(run.buffer.reads) != reads {
				test.Fatal("terminal Next cancellation resumed paged preparation/emission", stage, err)
			}
		})
	}
}
