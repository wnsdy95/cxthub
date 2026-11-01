package domain

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"unsafe"
)

const legacyCanonicalPreparedNumbersTestBudget = int64(64 << 20)

func legacyCanonicalPreparedNumbersSave(test *testing.T, numbers *legacyCanonicalNumberValidation, binding ContentHash, store legacyNativeCheckpointIO) legacyCanonicalPreparedNumberState {
	test.Helper()
	ctx := context.Background()
	written := 0
	put := store.Put
	store.Put = func(current context.Context, hash ContentHash, body []byte) error {
		written += len(body)
		return put(current, hash, body)
	}
	work, err := newLegacyCanonicalPreparedNumbersCheckpoint(ctx, numbers, binding, store, legacyCanonicalPreparedNumbersTestBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	for step := 0; step < 1<<17; step++ {
		written = 0
		before := work.position
		ready, err := work.step(ctx)
		if work.keepTexts || work.texts != (legacyCanonicalSourceNumberTexts{}) {
			test.Fatal("ordinary number checkpoint retained source bindings")
		}
		if err != nil || written > LegacyCanonicalEncodingBytes || work.position-before > legacyCanonicalEncodingActions {
			test.Fatal("checkpoint exceeded its quantum or failed", step, written, err)
		}
		retained, err := work.retainedBytes()
		if err != nil || retained > work.budget {
			test.Fatal("checkpoint omitted its owned workspace", retained, err)
		}
		if ready {
			state, err := work.take(ctx)
			if err != nil {
				test.Fatal(err)
			}
			if retained, err := work.retainedBytes(); err != nil || retained != 0 || work.numbers != nil || work.text != nil || work.rows != nil {
				test.Fatal("checkpoint take retained borrowed or temporary owners", retained, err)
			}
			return state
		}
	}
	test.Fatal("checkpoint did not finish")
	return legacyCanonicalPreparedNumberState{}
}

func legacyCanonicalPreparedNumbersLoad(test *testing.T, state legacyCanonicalPreparedNumberState, binding ContentHash, store legacyNativeCheckpointIO) (legacyCanonicalNumberValidation, docRangeHeaderHeapEstimate) {
	test.Helper()
	ctx := context.Background()
	readBytes, reads := 0, 0
	read := store.Read
	store.Read = func(current context.Context, hash ContentHash, length int) ([]byte, error) {
		readBytes += length
		reads++
		return read(current, hash, length)
	}
	work, err := newLegacyCanonicalPreparedNumbersRestore(ctx, state, binding, store, legacyCanonicalPreparedNumbersTestBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	for step := 0; step < 1<<17; step++ {
		readBytes = 0
		before := work.position
		ready, err := work.step(ctx)
		if work.keepTexts || work.texts != (legacyCanonicalSourceNumberTexts{}) {
			test.Fatal("ordinary number restore retained source bindings")
		}
		if err != nil || readBytes > LegacyCanonicalEncodingBytes || work.position-before > legacyCanonicalEncodingActions {
			test.Fatal("restore exceeded its quantum or failed", step, readBytes, err)
		}
		retained, err := work.retainedBytes()
		if err != nil || retained > work.budget {
			test.Fatal("restore omitted its newly owned workspace", retained, err)
		}
		if ready {
			expected := work.payload
			work.result.retained(&expected)
			result, heap, err := work.take(ctx)
			if err != nil || heap.err != nil || expected.err != nil || heap.bytes != expected.bytes || reads == 0 {
				test.Fatal("take lost result ownership or bypassed current IO", heap, expected, reads, err)
			}
			if retained, err := work.retainedBytes(); err != nil || retained != 0 || work.rows != nil || work.scalar != nil || work.value != nil || work.result != (legacyCanonicalNumberValidation{}) {
				test.Fatal("restore take retained temporary owners", retained, err)
			}
			return result, heap
		}
	}
	test.Fatal("restore did not finish")
	return legacyCanonicalNumberValidation{}, docRangeHeaderHeapEstimate{}
}

func legacyCanonicalPreparedNumbersReject(test *testing.T, state legacyCanonicalPreparedNumberState, binding ContentHash, store legacyNativeCheckpointIO) {
	test.Helper()
	ctx := context.Background()
	work, err := newLegacyCanonicalPreparedNumbersRestore(ctx, state, binding, store, legacyCanonicalPreparedNumbersTestBudget)
	if err != nil {
		return
	}
	defer work.close()
	for step := 0; step < 1<<12; step++ {
		ready, err := work.step(ctx)
		if err != nil {
			if retained, retainedErr := work.retainedBytes(); retainedErr != nil || retained != 0 || work.rows != nil || work.scalar != nil || work.value != nil || work.result != (legacyCanonicalNumberValidation{}) {
				test.Fatal("failed restore retained owners", retained, retainedErr)
			}
			if _, retry := work.step(ctx); !errors.Is(retry, err) {
				test.Fatal("restore failure was not sticky", err, retry)
			}
			return
		}
		if ready {
			test.Fatal("accepted damaged prepared numeric state")
		}
	}
	test.Fatal("damaged numeric state stalled instead of rejecting")
}

func TestLegacyCanonicalPreparedNumbersPendingCursorAndIdentity(test *testing.T) {
	ctx := context.Background()
	store, objects := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent([]byte("prepared numeric cursor"))
	first, second := strings.Clone("12345"), strings.Clone("12345")
	emptyFirst, emptySecond := strings.Clone("first")[:0], strings.Clone("second")[:0]
	values := []string{first, second, strings.Repeat("7", 3*LegacyCanonicalEncodingBytes+17), "invalid", emptyFirst, emptySecond, ""}
	var numbers legacyCanonicalNumberValidation
	for _, text := range values {
		if err := numbers.prepare(text); err != nil {
			test.Fatal(err)
		}
	}
	if err := numbers.prepare(first); err != nil || numbers.pending.length() != len(values) {
		test.Fatal("registration lost identity deduplication", err)
	}
	if ready, err := numbers.step(ctx); err != nil || ready || numbers.next != 2 || numbers.position == 0 {
		test.Fatal("fixture did not stop inside the pending number", numbers.next, numbers.position, ready, err)
	}
	state := legacyCanonicalPreparedNumbersSave(test, &numbers, binding, store)
	currentIO := legacyNativeCheckpointIO{Read: func(current context.Context, hash ContentHash, length int) ([]byte, error) {
		if err := current.Err(); err != nil {
			return nil, err
		}
		body, exists := objects[hash]
		if !exists || len(body) != length {
			return nil, ErrNotFound
		}
		return bytes.Clone(body), nil
	}}
	restored, heap := legacyCanonicalPreparedNumbersLoad(test, state, binding, currentIO)
	if restored.next != numbers.next || restored.position != numbers.position || restored.mode != numbers.mode || restored.pending.length() != len(values) {
		test.Fatal("restore changed the pending lexical cursor")
	}
	var structural docRangeHeaderHeapEstimate
	restored.retained(&structural)
	if heap.bytes <= structural.bytes {
		test.Fatal("restore result did not charge owned numeric bodies", heap, structural)
	}
	for ordinal, original := range values {
		text := restored.pending.at(ordinal)
		if text == nil || *text != original {
			test.Fatal("restore changed pending order", ordinal)
		}
		index, err := restored.index(*text)
		if err != nil || index != ordinal || restored.valid(*text) != (ordinal < restored.next) {
			test.Fatal("restore merged distinct equal-text identities or lost proof", ordinal, index, err)
		}
		if legacyCanonicalNumberKey(*text) == legacyCanonicalNumberKey(original) {
			test.Fatal("restore retained an old text identity", ordinal)
		}
	}
	if _, err := restored.index(strings.Clone(first)); !errors.Is(err, ErrIntegrity) {
		test.Fatal("ordinal lookup compared content instead of identity", err)
	}
	for step := 0; step < 8; step++ {
		originalReady, originalErr := numbers.step(ctx)
		restoredReady, restoredErr := restored.step(ctx)
		if originalReady != restoredReady || !errors.Is(restoredErr, originalErr) ||
			numbers.next != restored.next || numbers.position != restored.position || numbers.mode != restored.mode {
			test.Fatal("resumed validation replayed or skipped lexical work", step, originalErr, restoredErr)
		}
		if originalErr != nil {
			if !errors.Is(originalErr, errLegacyCanonicalNumber) {
				test.Fatal(originalErr)
			}
			return
		}
	}
	test.Fatal("resumed validation did not reach the invalid pending text")
}

func TestLegacyCanonicalPreparedNumbersProofDoesNotReplayParsing(test *testing.T) {
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent([]byte("prepared numeric proof"))
	var numbers legacyCanonicalNumberValidation
	text := strings.Clone("already-admitted-not-lexically-replayed")
	if err := numbers.prepare(text); err != nil {
		test.Fatal(err)
	}
	if err := numbers.proven.prove(legacyCanonicalNumberKey(text)); err != nil {
		test.Fatal(err)
	}
	numbers.next = 1
	state := legacyCanonicalPreparedNumbersSave(test, &numbers, binding, store)
	restored, _ := legacyCanonicalPreparedNumbersLoad(test, state, binding, store)
	if ready, err := restored.step(context.Background()); err != nil || !ready || !restored.valid(*restored.pending.at(0)) {
		test.Fatal("restore parsed a previously proven entry", ready, err)
	}
}

func TestLegacyCanonicalPreparedNumbersPagedRegistryAndEmpty(test *testing.T) {
	for _, count := range []int{0, 1, 8, 9, 65, 1025} {
		store, _ := legacyNativeCheckpointStoreFixture(test)
		binding := HashContent([]byte("prepared numeric pages"))
		var numbers legacyCanonicalNumberValidation
		for ordinal := 0; ordinal < count; ordinal++ {
			text := strings.Clone("123456789")
			if err := numbers.prepare(text); err != nil {
				test.Fatal(count, ordinal, err)
			}
			if ordinal < count/2 {
				if err := numbers.proven.prove(legacyCanonicalNumberKey(text)); err != nil {
					test.Fatal(err)
				}
			}
		}
		numbers.next = count / 2
		state := legacyCanonicalPreparedNumbersSave(test, &numbers, binding, store)
		restored, heap := legacyCanonicalPreparedNumbersLoad(test, state, binding, store)
		if restored.pending.length() != count || restored.next != numbers.next || count == 0 && heap.bytes != 0 {
			test.Fatal("paged numeric restore changed queue ownership", count, heap)
		}
		for ordinal := 0; ordinal < count; ordinal++ {
			text := *restored.pending.at(ordinal)
			index, err := restored.index(text)
			if err != nil || index != ordinal || restored.valid(text) != (ordinal < numbers.next) {
				test.Fatal("radix split lost the pending ordinal or proof", count, ordinal, index, err)
			}
		}
		legacyCanonicalNumberRegistryShape(test, &restored.proven)
	}
}

func TestLegacyCanonicalPreparedNumbersRejectBindingCursorAndRecords(test *testing.T) {
	ctx := context.Background()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent([]byte("prepared numeric fences"))
	var numbers legacyCanonicalNumberValidation
	for _, text := range []string{strings.Clone("12"), strings.Clone("34")} {
		if err := numbers.prepare(text); err != nil {
			test.Fatal(err)
		}
	}
	state := legacyCanonicalPreparedNumbersSave(test, &numbers, binding, store)
	legacyCanonicalPreparedNumbersReject(test, state, HashContent([]byte("other binding")), store)
	changed := state
	changed.Next = 1
	legacyCanonicalPreparedNumbersReject(test, changed, binding, store)
	changed = state
	changed.Position, changed.Mode = 1, legacyJSONValueInteger
	legacyCanonicalPreparedNumbersReject(test, changed, binding, store)
	for _, mutate := range []func([]legacyCanonicalPreparedNumberRow){
		func(rows []legacyCanonicalPreparedNumberRow) { rows[0].Binding = HashContent([]byte("foreign")) },
		func(rows []legacyCanonicalPreparedNumberRow) { rows[0].Next = 1 },
		func(rows []legacyCanonicalPreparedNumberRow) { rows[1].Ordinal = 1 },
		func(rows []legacyCanonicalPreparedNumberRow) { rows[1].Proven = true },
		func(rows []legacyCanonicalPreparedNumberRow) { rows[1].Version++ },
		func(rows []legacyCanonicalPreparedNumberRow) { rows[1].Text.Bytes++ },
		func(rows []legacyCanonicalPreparedNumberRow) { rows[1].Next = 1 },
	} {
		var page legacyNativeCheckpointListPage[legacyCanonicalPreparedNumberRow]
		if state.Pending.Height != 0 {
			test.Fatal("small fence fixture unexpectedly branched")
		}
		if err := legacyNativeCheckpointReadJSON(ctx, store, state.Pending.Ref, &page); err != nil {
			test.Fatal(err)
		}
		mutate(page.Values)
		ref, err := legacyNativeCheckpointWriteJSON(ctx, store, page)
		if err != nil {
			test.Fatal(err)
		}
		changed = state
		changed.Pending.Ref = ref
		legacyCanonicalPreparedNumbersReject(test, changed, binding, store)
	}
	read := store.Read
	corrupt := store
	corrupt.Read = func(current context.Context, hash ContentHash, length int) ([]byte, error) {
		body, err := read(current, hash, length)
		if err == nil && len(body) > 0 {
			body[0] ^= 1
		}
		return body, err
	}
	legacyCanonicalPreparedNumbersReject(test, state, binding, corrupt)
	missing := store
	missing.Read = func(context.Context, ContentHash, int) ([]byte, error) { return nil, ErrNotFound }
	legacyCanonicalPreparedNumbersReject(test, state, binding, missing)
}

func TestLegacyCanonicalPreparedNumbersRejectInconsistentRegistry(test *testing.T) {
	for _, corruption := range []string{"proof", "ordinal", "identity"} {
		ctx := context.Background()
		store, _ := legacyNativeCheckpointStoreFixture(test)
		binding := HashContent([]byte("inconsistent numeric registry"))
		var numbers legacyCanonicalNumberValidation
		for _, text := range []string{strings.Clone("12"), strings.Clone("34")} {
			if err := numbers.prepare(text); err != nil {
				test.Fatal(err)
			}
		}
		entry, err := numbers.proven.lookup(legacyCanonicalNumberKey(*numbers.pending.at(1)))
		if err != nil {
			test.Fatal(err)
		}
		switch corruption {
		case "proof":
			entry.proven = true
		case "ordinal":
			entry.ordinal = 0
		case "identity":
			*numbers.pending.at(1) = *numbers.pending.at(0)
		}
		work, err := newLegacyCanonicalPreparedNumbersCheckpoint(ctx, &numbers, binding, store, legacyCanonicalPreparedNumbersTestBudget)
		if err != nil {
			test.Fatal(err)
		}
		failed := false
		for step := 0; step < 100; step++ {
			ready, err := work.step(ctx)
			if err != nil {
				failed = true
				if !errors.Is(err, ErrIntegrity) || work.numbers != nil || work.rows != nil || work.text != nil {
					test.Fatal("inconsistent registry retained checkpoint owners", corruption, err)
				}
				break
			}
			if ready {
				test.Fatal("checkpoint accepted an inconsistent proof or ordinal", corruption)
			}
		}
		work.close()
		if !failed {
			test.Fatal("inconsistent registry was not rejected", corruption)
		}
	}
}

func TestLegacyCanonicalPreparedNumbersBudgetCancellationAndCleanup(test *testing.T) {
	ctx := context.Background()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent([]byte("prepared numeric ownership"))
	var numbers legacyCanonicalNumberValidation
	if err := numbers.prepare(strings.Repeat("9", 4*LegacyCanonicalEncodingBytes)); err != nil {
		test.Fatal(err)
	}
	if _, err := newLegacyCanonicalPreparedNumbersCheckpoint(ctx, &numbers, binding, store, 1); !errors.Is(err, ErrDocumentResourceLimit) {
		test.Fatal("writer allocated before its metadata admission", err)
	}
	work, err := newLegacyCanonicalPreparedNumbersCheckpoint(ctx, &numbers, binding, store, legacyCanonicalPreparedNumbersTestBudget)
	if err != nil {
		test.Fatal(err)
	}
	retained, err := work.retainedBytes()
	if err != nil || retained >= int64(len(*numbers.pending.at(0))) {
		test.Fatal("writer charged its borrowed text body", retained, err)
	}
	work.budget = retained
	if _, err := work.step(ctx); !errors.Is(err, ErrDocumentResourceLimit) || work.numbers != nil || work.rows != nil {
		test.Fatal("writer budget failure retained owners", err)
	}
	if _, err := work.take(ctx); !errors.Is(err, ErrDocumentResourceLimit) {
		test.Fatal("writer budget failure changed on take", err)
	}
	state := legacyCanonicalPreparedNumbersSave(test, &numbers, binding, store)
	if _, err := newLegacyCanonicalPreparedNumbersRestore(ctx, state, binding, store, 1); !errors.Is(err, ErrDocumentResourceLimit) {
		test.Fatal("restore allocated before metadata admission", err)
	}
	for _, cancelWork := range []bool{false, true} {
		restore, err := newLegacyCanonicalPreparedNumbersRestore(ctx, state, binding, store, legacyCanonicalPreparedNumbersTestBudget)
		if err != nil {
			test.Fatal(err)
		}
		for step := 0; step < 100 && restore.scalar == nil; step++ {
			if ready, err := restore.step(ctx); err != nil || ready {
				test.Fatal("restore did not reach owned text workspace", ready, err)
			}
		}
		if restore.scalar == nil {
			test.Fatal("restore did not start text restoration")
		}
		current := ctx
		want := ErrDocumentResourceLimit
		if cancelWork {
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			current, want = canceled, context.Canceled
		} else {
			retained, err := restore.retainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			restore.budget = retained
		}
		if _, err := restore.step(current); !errors.Is(err, want) {
			test.Fatal("restore did not preserve the terminal failure", err, want)
		}
		if retained, err := restore.retainedBytes(); err != nil || retained != 0 || restore.scalar != nil || restore.rows != nil || restore.result != (legacyCanonicalNumberValidation{}) {
			test.Fatal("restore failure retained owned workspace", retained, err)
		}
		if _, _, err := restore.take(ctx); !errors.Is(err, want) {
			test.Fatal("restore take did not preserve failure", err, want)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	writeIO := store
	put := store.Put
	writeIO.Put = func(current context.Context, hash ContentHash, body []byte) error {
		if err := put(current, hash, body); err != nil {
			return err
		}
		cancel()
		return nil
	}
	writer, err := newLegacyCanonicalPreparedNumbersCheckpoint(canceled, &numbers, binding, writeIO, legacyCanonicalPreparedNumbersTestBudget)
	if err != nil {
		test.Fatal(err)
	}
	for step := 0; step < 100; step++ {
		ready, err := writer.step(canceled)
		if err != nil {
			if !errors.Is(err, context.Canceled) || writer.numbers != nil || writer.text != nil || writer.rows != nil {
				test.Fatal("cancellation inside Put retained checkpoint ownership", err)
			}
			return
		}
		if ready {
			test.Fatal("writer ignored cancellation inside current IO")
		}
	}
	test.Fatal("writer did not observe cancellation")
}

func TestLegacyCanonicalPreparedNumbersRegistryEntryWorkAndHeap(test *testing.T) {
	var numbers legacyCanonicalNumberValidation
	text := strings.Clone("123456")
	if err := numbers.prepare(text); err != nil {
		test.Fatal(err)
	}
	var expected, actual docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(legacyCanonicalNumberRegistryNode{})))
	numbers.proven.retained(&actual)
	if expected.err != nil || actual.err != nil || expected.bytes != actual.bytes {
		test.Fatal("registry did not charge the ordinal's enlarged entry", actual, expected)
	}
	if legacyCanonicalNumberProofBytes < legacyCanonicalNumberBucketValues*int(unsafe.Sizeof(legacyCanonicalNumberEntry{})) {
		test.Fatal("proof work omitted the enlarged registry bucket")
	}
	index, err := numbers.index(text)
	if err != nil || index != 0 {
		test.Fatal("bounded ordinal lookup failed", index, err)
	}
	entry, err := numbers.proven.lookup(legacyCanonicalNumberKey(text))
	if err != nil {
		test.Fatal(err)
	}
	entry.ordinal = 1
	if _, err := numbers.index(text); !errors.Is(err, ErrIntegrity) {
		test.Fatal("ordinal lookup accepted a nonexistent pending slot", err)
	}
}
