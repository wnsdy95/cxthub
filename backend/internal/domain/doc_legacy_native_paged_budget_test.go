package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unsafe"
)

type legacyNativePagedBudgetStorage struct {
	allocated int64
	copied    int64
	retained  int64
}

func legacyNativePagedBudgetPages[Value any](test *testing.T, values *legacyCanonicalPages[Value]) legacyNativePagedBudgetStorage {
	test.Helper()
	if !values.valid() || legacyCanonicalPageValues != 64 {
		test.Fatal("actual collection lost the existing fixed-page core")
	}
	if values.single != nil && (values.inline != nil || values.root != nil) || values.inline != nil && values.root != nil {
		test.Fatal("actual singleton, inline, and tree storage overlap")
	}
	var storage legacyNativePagedBudgetStorage
	add := func(width uintptr) {
		if width == 0 || width > 64<<10 {
			test.Fatal("actual singleton, inline, page, or node allocation exceeds 64 KiB", width)
		}
		storage.allocated += int64(width)
		storage.retained += 64 + 2*int64(width)
	}
	if values.single != nil {
		if values.count > 1 || values.count == 1 && values.at(0) != values.single {
			test.Fatal("actual singleton lost its one-value capacity or backing address")
		}
		add(unsafe.Sizeof(*values.single))
	}
	if values.inline != nil {
		if len(values.inline[:]) != 8 || cap(values.inline[:]) != 8 {
			test.Fatal("actual inline capacity changed")
		}
		add(unsafe.Sizeof(*values.inline))
	}
	stack := []*legacyCanonicalPageNode[Value]{}
	if values.root != nil {
		stack = append(stack, values.root)
	}
	seen := make(map[*legacyCanonicalPageNode[Value]]bool)
	pages := 0
	for len(stack) != 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[node] {
			test.Fatal("actual fixed-page tree contains an alias or cycle")
		}
		seen[node] = true
		add(unsafe.Sizeof(*node))
		if node.values != nil {
			if len(node.values[:]) != 64 || cap(node.values[:]) != 64 {
				test.Fatal("actual leaf capacity is not a complete 64-value page")
			}
			add(unsafe.Sizeof(*node.values))
			pages++
		}
		for _, child := range node.children {
			if child != nil {
				stack = append(stack, child)
			}
		}
	}
	if len(seen) != values.nodes || pages != values.pages {
		test.Fatal("actual page/node graph differs from its counters", len(seen), values.nodes, pages, values.pages)
	}
	owned, err := values.ownedBytes()
	if err != nil || owned != storage.retained {
		test.Fatal("page heap omitted full unused page capacity or counted only a pointer", owned, storage.retained, err)
	}
	inlineBytes := int64(unsafe.Sizeof([legacyCanonicalInlineValues]Value{}))
	singleBytes := int64(unsafe.Sizeof(*new(Value)))
	if (values.inline != nil || values.root != nil) && singleBytes <= (64<<10)-inlineBytes {
		storage.allocated += singleBytes
		storage.copied += singleBytes
	}
	if values.root != nil {
		storage.allocated += inlineBytes
		storage.copied += inlineBytes
	}
	return storage
}

func legacyNativePagedBudgetValues(test *testing.T, values *legacyNativeEventValues) legacyNativePagedBudgetStorage {
	test.Helper()
	if values == nil {
		return legacyNativePagedBudgetStorage{}
	}
	storage := legacyNativePagedBudgetPages(test, &values.records)
	width := int64(unsafe.Sizeof(*values))
	storage.allocated += width
	storage.retained += 64 + 2*width
	backing, heap := values.backing(), values.heap()
	if backing.err != nil || backing.bytes != storage.retained || values.payload.err != nil ||
		heap.err != nil || heap.bytes != storage.retained+values.payload.bytes ||
		values.length() < 0 || values.length() > values.records.length() || values.at(-1) != nil || values.at(values.length()) != nil {
		test.Fatal("actual replacement backing/heap/getter lost page capacity or its logical boundary", backing, heap, storage.retained)
	}
	for index := 0; index < values.length(); index += 64 {
		if values.at(index) == nil || values.at(index) != values.records.at(index) {
			test.Fatal("replacement getter copied or lost a real page-boundary record", index)
		}
	}
	return storage
}

type legacyNativePagedBudgetFixture struct {
	normalizer *LegacyNormalizationDecoder
	buffer     *legacySourceProofBuffer
	current    context.Context
	readHook   func(context.Context, int, int, []byte, error) ([]byte, error)
}

func legacyNativePagedBudgetStart(test *testing.T, count int) *legacyNativePagedBudgetFixture {
	test.Helper()
	var event strings.Builder
	event.WriteString(`{"kind":"compaction","seq":7,"replacement_complete":true,"replacement":[`)
	for index := 0; index < count; index++ {
		if index != 0 {
			event.WriteByte(',')
		}
		fmt.Fprintf(&event, `{"kind":"turn","seq":%d,"role":"user","id":%q}`, (count-index)/3,
			fmt.Sprintf("child-%05d-%s", index, strings.Repeat("x", 97)))
	}
	event.WriteString(`]}`)
	fixture := &legacyNativePagedBudgetFixture{buffer: &legacySourceProofBuffer{body: legacyNativeEventDocument("2", event.String())}}
	source := legacyNormalizationPreparationInput(test, fixture.buffer, true)
	read := source.Read
	source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		if ctx != fixture.current || ctx.Err() != nil {
			test.Fatal("public paged source used a constructor, previous, or canceled context", offset, length)
		}
		body, err := read(ctx, offset, length)
		if fixture.readHook != nil {
			return fixture.readHook(ctx, offset, length, body, err)
		}
		return body, err
	}
	constructor, cancel := context.WithCancel(context.Background())
	var err error
	fixture.normalizer, err = NewLegacyNormalizationDecoder(constructor, source)
	cancel()
	if err != nil || fixture.normalizer == nil || len(fixture.buffer.reads) != 0 {
		test.Fatal("public paged constructor failed or eagerly loaded source", err)
	}
	return fixture
}

func (fixture *legacyNativePagedBudgetFixture) reads(test *testing.T, before int) {
	test.Helper()
	loads := 0
	for _, read := range fixture.buffer.reads[before:] {
		if read[0] == len(fixture.buffer.body) && (read[1] == 0 || read[1] == 1) {
			continue
		}
		if read[0] < 0 || read[0] >= len(fixture.buffer.body) || read[0]%(512<<10) != 0 ||
			read[1] != min(512<<10, len(fixture.buffer.body)-read[0]) {
			test.Fatal("public paged source bypassed authenticated full parts", read)
		}
		loads++
	}
	if loads > 2 {
		test.Fatal("public paged call exceeded its two source loads", loads)
	}
}

func (fixture *legacyNativePagedBudgetFixture) step(test *testing.T, ctx context.Context) (LegacyNormalizationStep, error) {
	test.Helper()
	fixture.current = ctx
	before, position := len(fixture.buffer.reads), fixture.normalizer.position
	result, err := fixture.normalizer.Step(ctx)
	fixture.current = nil
	fixture.reads(test, before)
	if fixture.normalizer.position < position || fixture.normalizer.position-position > 512<<10 || len(result.Events) > 64 {
		test.Fatal("public paged Step escaped its existing source/event quantum")
	}
	if err != nil {
		legacyNativeEventZero(test, result)
	}
	return result, err
}

func (fixture *legacyNativePagedBudgetFixture) list(test *testing.T) *legacyNativeEventList {
	test.Helper()
	native := fixture.normalizer.native
	if native == nil {
		return nil
	}
	if !native.object.pagedEvents {
		test.Fatal("public normalizer used the old typed internal constructor mode")
	}
	field := legacyEventNativeFields.match("replacement")
	if field < 0 || field >= len(native.object.events) {
		return nil
	}
	list := native.object.events[field]
	if list != nil && !list.paged {
		test.Fatal("public replacement list lost its inherited paged mode")
	}
	return list
}

func legacyNativePagedBudgetEncodingStorage(test *testing.T, encoding *LegacyCanonicalEncoding, preparation *legacyCanonicalPreparation) legacyNativePagedBudgetStorage {
	test.Helper()
	storage := legacyNativePagedBudgetPages(test, &encoding.mapKeys)
	add := func(child legacyNativePagedBudgetStorage) {
		storage.allocated += child.allocated
		storage.copied += child.copied
	}
	add(legacyNativePagedBudgetPages(test, &encoding.frames))
	if preparation != nil {
		add(legacyNativePagedBudgetPages(test, &preparation.frames))
	}
	for index := 0; index < encoding.mapKeys.length(); index++ {
		add(legacyNativePagedBudgetPages(test, &encoding.mapKeys.at(index).fields))
	}
	return storage
}

func legacyNativePagedBudgetEncode(test *testing.T, fixture *legacyNativePagedBudgetFixture, encoding *LegacyCanonicalEncoding, count int) []byte {
	test.Helper()
	defer encoding.release()
	if encoding.nativeOwner == nil || encoding.nativeOwner.replacement == nil || encoding.preparation == nil || encoding.inspectWork != 0 {
		test.Fatal("public emitted encoding did not carry an unvisited actual paged owner")
	}
	values := encoding.nativeOwner.replacement
	legacyNativePagedBudgetValues(test, values)
	if values.length() != count || values.records.pages != (count+63)/64 || values.records.depth == 0 {
		test.Fatal("large public source did not cross real 64-value leaf and tree boundaries", values.length(), values.records.pages, values.records.depth)
	}
	reads := len(fixture.buffer.reads)
	var output []byte
	preparing, ordering := 0, 0
	for stepIndex := 0; stepIndex < 1<<16; stepIndex++ {
		preparation, inspect, keys := encoding.preparation, encoding.inspectWork, encoding.keyWork
		before := legacyNativePagedBudgetEncodingStorage(test, encoding, preparation)
		ctx, cancel := context.WithCancel(context.Background())
		chunk, complete, err := encoding.Next(ctx)
		cancel()
		if err != nil || len(chunk) > 64<<10 || cap(chunk) > 64<<10 || encoding.lastStorageBytes < 0 || encoding.lastStorageBytes > 64<<10 ||
			encoding.inspectWork < inspect || encoding.keyWork < keys || len(fixture.buffer.reads) != reads {
			test.Fatal("public paged Next escaped its output/storage quantum, reread source, or lost work", stepIndex, err)
		}
		if preparation != nil {
			after := legacyNativePagedBudgetEncodingStorage(test, encoding, preparation)
			growth := after.allocated + after.copied - before.allocated - before.copied
			if len(chunk) != 0 || complete || preparation.lastActions <= 0 || preparation.lastActions > 1024 ||
				encoding.inspectWork-inspect != uint64(preparation.lastActions) || preparation.lastBytes < 0 || preparation.lastBytes > 64<<10 ||
				preparation.lastStorageBytes < 0 || preparation.lastStorageBytes > preparation.lastBytes ||
				encoding.lastStorageBytes != preparation.lastStorageBytes || growth < 0 || growth > int64(preparation.lastStorageBytes) {
				test.Fatal("paged preparation did not reserve its actual full-page allocation/copy work", stepIndex, growth, preparation.lastStorageBytes)
			}
			preparing++
		} else if encoding.inspectWork != inspect {
			test.Fatal("paged output restarted completed inspection")
		}
		if encoding.keyWork > keys {
			ordering++
		}
		output = append(output, chunk...)
		if complete {
			if preparing < 2 || ordering < 2 || encoding.nativeOwner != nil || encoding.preparation != nil || encoding.root != nil {
				test.Fatal("public paged stream skipped bounded preparation/ordering or retained its owner", preparing, ordering)
			}
			chunk, done, err := encoding.Next(context.Background())
			if err != nil || !done || len(chunk) != 0 || len(fixture.buffer.reads) != reads {
				test.Fatal("completed public paged encoding changed EOF or reread source", done, err)
			}
			return output
		}
		if len(chunk) == 0 && encoding.inspectWork == inspect && encoding.keyWork == keys {
			test.Fatal("public paged Next made no finite preparation, ordering, or output progress", stepIndex)
		}
	}
	test.Fatal("public paged encoding exhausted its unchanged finite Next allowance")
	return nil
}

func TestLegacyNativePagedBudgetPublicSourceAndEncoding(test *testing.T) {
	if ConversationManifestChunkBytes != 512<<10 || LegacyCanonicalEncodingBytes != 64<<10 || legacyCanonicalEncodingActions != 1024 {
		test.Fatal("paged source changed the existing byte/action quantum")
	}
	const count = 4097
	fixture := legacyNativePagedBudgetStart(test, count)
	original := bytes.Clone(fixture.buffer.body)
	want, err := legacyNormalizationOracle(original)
	if err != nil || len(original) <= 512<<10 {
		test.Fatal("budget fixture lost its original typed acceptance or multi-part source", len(original), err)
	}
	var output []byte
	seen, partial := 0, false
	for quantum := 0; quantum < 1024; quantum++ {
		ctx, cancel := context.WithCancel(context.Background())
		step, err := fixture.step(test, ctx)
		cancel()
		if err != nil {
			test.Fatal("public paged source failed", quantum, err)
		}
		if list := fixture.list(test); list != nil && list.values != nil {
			legacyNativePagedBudgetValues(test, list.values)
			partial = partial || list.values.length() > 0 && list.values.length() < count
			retained, err := fixture.normalizer.RetainedBytes()
			if err != nil || retained < list.values.heap().bytes {
				test.Fatal("public source heap omitted its actual paged replacement", retained, err)
			}
		}
		for _, event := range step.Events {
			seen++
			output = legacyNativePagedBudgetEncode(test, fixture, event.Encoding, count)
		}
		if !step.Complete {
			continue
		}
		actual := AssembleCanonicalDoc(step.Envelope, [][]byte{output})
		zeroEOF, oneEOF := 0, 0
		for _, read := range fixture.buffer.reads {
			if read[0] == len(original) {
				if read[1] == 0 {
					zeroEOF++
				} else if read[1] == 1 {
					oneEOF++
				}
			}
		}
		if !partial || seen != 1 || step.Count != 1 || zeroEOF == 0 || zeroEOF != oneEOF ||
			!bytes.Equal(actual, want) || !bytes.Equal(fixture.buffer.body, original) {
			test.Fatal("bounded public source lost progress, source EOF, original bytes, or canonical output", partial, seen, zeroEOF, oneEOF)
		}
		reads := len(fixture.buffer.reads)
		repeated, err := fixture.step(test, context.Background())
		if err != nil || !repeated.Complete || repeated.Count != 1 || len(fixture.buffer.reads) != reads {
			test.Fatal("completed public normalizer resumed source work", err)
		}
		test.Log("actual fixed-page sizes/capacities and reserved structural work checked; no allocator, wall-clock, or RSS guarantee")
		return
	}
	test.Fatal("public paged source exhausted its original 1024-call allowance")
}

func TestLegacyNativePagedBudgetPublicSharedSourceReservation(test *testing.T) {
	fixture := legacyNativePagedBudgetStart(test, 4097)
	for quantum := 0; quantum < 1024; quantum++ {
		ctx, cancel := context.WithCancel(context.Background())
		step, err := fixture.step(test, ctx)
		cancel()
		if err != nil || step.Complete || len(step.Events) != 0 {
			test.Fatal("shared source fixture did not park in its actual public replacement", quantum, err)
		}
		list := fixture.list(test)
		if list == nil || list.values == nil || list.values.length() == 0 {
			continue
		}
		normalizer := fixture.normalizer
		limit := normalizer.native.object.fieldActionLimit()
		for _, exhausted := range []struct {
			work   int
			fields int
		}{
			{work: 512 << 10},
			{fields: limit},
		} {
			reads, position, count := len(fixture.buffer.reads), normalizer.position, list.values.length()
			ctx, cancel := context.WithCancel(context.Background())
			fixture.current = ctx
			input := &legacyNormalizationInput{ctx: ctx, source: normalizer.source, workBytes: exhausted.work, fields: exhausted.fields}
			step, err := normalizer.scan(ctx, input)
			fixture.current = nil
			cancel()
			if err != nil || len(step.Events) != 0 || step.Complete || input.workBytes != exhausted.work || input.fields != exhausted.fields ||
				input.loads != 0 || len(fixture.buffer.reads) != reads || normalizer.position != position || list.values.length() != count {
				test.Fatal("public native driver reset an exhausted shared source byte/field allowance", err)
			}
		}
		reads := len(fixture.buffer.reads)
		ctx, cancel = context.WithCancel(context.Background())
		fixture.current = ctx
		loaded := &legacyNormalizationInput{ctx: ctx, source: normalizer.source, loads: 2}
		step, err = normalizer.scan(ctx, loaded)
		fixture.current = nil
		cancel()
		if err != nil || loaded.failure != nil || step.Complete || len(step.Events) != 0 || loaded.loads != 2 ||
			len(fixture.buffer.reads) != reads || loaded.workBytes < 0 || loaded.workBytes > 512<<10 || loaded.fields > limit || loaded.unbounded {
			test.Fatal("public replacement reset or bypassed the parent's exhausted two-load window", err)
		}
		grew := false
		for reservation := 0; reservation < 64; reservation++ {
			before := legacyNativePagedBudgetValues(test, list.values)
			reads, count := len(fixture.buffer.reads), list.values.length()
			ctx, cancel = context.WithCancel(context.Background())
			fixture.current = ctx
			input := &legacyNormalizationInput{ctx: ctx, source: normalizer.source, workBytes: (512 << 10) - (64 << 10)}
			step, err = normalizer.scan(ctx, input)
			fixture.current = nil
			cancel()
			fixture.reads(test, reads)
			after := legacyNativePagedBudgetValues(test, list.values)
			growth := after.allocated + after.copied - before.allocated - before.copied
			used := input.workBytes - ((512 << 10) - (64 << 10))
			if err != nil || input.failure != nil || step.Complete || len(step.Events) != 0 || used <= 0 || used > 64<<10 ||
				input.loads > 2 || input.unbounded || input.fields > limit || input.values > legacyNativeAnyValueActions ||
				input.eventsRunning != nil || input.eventsPending != nil || growth < 0 || growth > int64(used) || list.values.length() <= count {
				test.Fatal("public replacement escaped the parent's remaining 64 KiB reservation or lost actual progress", used, growth, err)
			}
			if growth > 0 {
				grew = true
				break
			}
		}
		if !grew {
			test.Fatal("shared source reservation did not cross an actual full-page allocation boundary")
		}
		reads = len(fixture.buffer.reads)
		canceled, stop := context.WithCancel(context.Background())
		stop()
		step, err = fixture.step(test, canceled)
		if !errors.Is(err, context.Canceled) || len(fixture.buffer.reads) != reads || normalizer.terminal != nil {
			test.Fatal("pre-canceled current call read source or poisoned the resumable public decoder", err)
		}
		ctx, cancel = context.WithCancel(context.Background())
		step, err = fixture.step(test, ctx)
		cancel()
		for _, event := range step.Events {
			event.Encoding.release()
		}
		if err != nil || len(fixture.buffer.reads) == reads {
			test.Fatal("fresh public context did not resume actual source after pre-cancellation", err)
		}
		return
	}
	test.Fatal("shared public source never exposed a partial actual paged replacement")
}

func TestLegacyNativePagedBudgetPublicCurrentCancelAndSourceEOF(test *testing.T) {
	for _, failure := range []string{"cancel-part", "part-hash", "cancel-part-hash", "extra-eof", "cancel-eof"} {
		test.Run(failure, func(test *testing.T) {
			fixture := legacyNativePagedBudgetStart(test, 4097)
			var cancelCurrent context.CancelFunc
			triggered, failedAt := false, 0
			fixture.readHook = func(ctx context.Context, offset, length int, body []byte, err error) ([]byte, error) {
				eof := strings.HasSuffix(failure, "eof")
				if triggered || !eof && (fixture.list(test) == nil || length == 0 || offset == len(fixture.buffer.body)) ||
					eof && (offset != len(fixture.buffer.body) || length != 1) {
					return body, err
				}
				triggered, failedAt = true, len(fixture.buffer.reads)
				if strings.HasPrefix(failure, "cancel") {
					cancelCurrent()
				}
				if eof {
					return []byte("!"), nil
				}
				if strings.HasSuffix(failure, "hash") {
					body = bytes.Clone(body)
					body[0] ^= 1
				}
				return body, err
			}
			for quantum := 0; quantum < 1024; quantum++ {
				ctx, cancel := context.WithCancel(context.Background())
				cancelCurrent = cancel
				step, err := fixture.step(test, ctx)
				cancel()
				for _, event := range step.Events {
					event.Encoding.release()
				}
				if err == nil {
					if step.Complete {
						test.Fatal("paged source completed without its current cancellation/source EOF fence", failure)
					}
					continue
				}
				want := error(context.Canceled)
				if !strings.HasPrefix(failure, "cancel") {
					want = ErrPreparedDocRangeObject
				}
				if !triggered || !errors.Is(err, want) || want == context.Canceled && errors.Is(err, ErrPreparedDocRangeObject) || len(fixture.buffer.reads) != failedAt {
					test.Fatal("paged source lost cancellation priority, swallowed EOF, or continued reading", failure, triggered, err)
				}
				_, repeatedErr := fixture.step(test, context.Background())
				if !errors.Is(repeatedErr, want) || len(fixture.buffer.reads) != failedAt {
					test.Fatal("failed public paged source resumed source work", failure, repeatedErr)
				}
				return
			}
			test.Fatal("public paged source failure exhausted its finite allowance", failure)
		})
	}
}
