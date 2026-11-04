package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"unsafe"
)

func legacyNativeSingletonOwnershipPages(test *testing.T, ledger *legacyNativeBlockPagedOwnershipLedger, expected []CIREvent) {
	test.Helper()
	replacement := ledger.probe.replacement
	if replacement == nil || replacement.length() != len(expected) || replacement.records.length() != len(expected) ||
		replacement.records.inline != nil || replacement.records.root == nil || replacement.records.pages <= 1 ||
		len(ledger.blocks) != len(expected) || len(ledger.childEdges) != len(expected) || len(ledger.paged.values) != 1 {
		test.Fatal("singleton fixture lost its actual replacement tree or a distinct child block owner")
	}
	legacyCanonicalIndependentPageHeap(test, &replacement.records)
	allocation := legacyCanonicalReplacementOwnershipAllocation
	seen := make(map[*legacyNativeBlockValue]bool, len(expected))
	var actualPages, actualBacking, expectedBacking, oldInlinePages int64
	for ordinal, event := range expected {
		child := replacement.at(ordinal)
		if child == nil || child.blocks == nil || child.replacement != nil || child.event.Blocks != nil || child.event.Replacement != nil ||
			len(event.Blocks) != 1 || ledger.childEdges[child] != child.blocks {
			test.Fatal("replacement child lost its sole native block or materialized an old typed array", ordinal)
		}
		values := child.blocks
		if values.length() != 1 || values.records.length() != 1 || values.records.inline != nil || values.records.root != nil ||
			values.records.nodes != 0 || values.records.pages != 0 || values.records.depth != 0 || !values.records.valid() {
			test.Fatal("one-block child still reserves inline-eight or fixed-page storage", ordinal)
		}
		record := values.at(0)
		single := reflect.ValueOf(&values.records).Elem().FieldByName("single")
		if record == nil || seen[record] || !single.IsValid() || single.Kind() != reflect.Pointer || single.IsNil() ||
			single.Type() != reflect.TypeFor[*legacyNativeBlockValue]() || single.UnsafePointer() != unsafe.Pointer(record) ||
			single.Type().Elem().Size() != unsafe.Sizeof(*record) || values.records.at(0) != record ||
			values.at(-1) != nil || values.at(1) != nil || values.records.at(1) != nil || record.block != event.Blocks[0] {
			test.Fatal("singleton is not the distinct actual one-record allocation returned by native access", ordinal)
		}
		seen[record] = true
		page := legacyCanonicalIndependentPageHeap(test, &values.records)
		wantPage := allocation(1, int64(unsafe.Sizeof(*record)))
		wantBacking := allocation(1, int64(unsafe.Sizeof(*values))) + wantPage
		backing, heap := values.backing(), values.heap()
		if page.err != nil || page.bytes != wantPage || backing.err != nil || backing.bytes != wantBacking ||
			values.payload.err != nil || heap.err != nil || heap.bytes != wantBacking+values.payload.bytes {
			test.Fatal("singleton child ledger charges a pointer, eight slots, or duplicate payload instead of actual sizeof", ordinal, page, backing, heap, wantBacking)
		}
		snapshot := ledger.blocks[values]
		if len(snapshot) != 1 || snapshot[0].record != record || snapshot[0].value != *record {
			test.Fatal("canonical handoff or preparation replaced the original singleton address or payload", ordinal)
		}
		actualPages += page.bytes
		actualBacking += backing.bytes
		expectedBacking += wantBacking
		oldInlinePages += allocation(1, int64(unsafe.Sizeof([legacyCanonicalInlineValues]legacyNativeBlockValue{})))
	}
	wantPages := int64(len(expected)) * allocation(1, int64(unsafe.Sizeof(legacyNativeBlockValue{})))
	wantSaved := 2 * int64(len(expected)) * int64(legacyCanonicalInlineValues-1) * int64(unsafe.Sizeof(legacyNativeBlockValue{}))
	if actualPages != wantPages || actualBacking != expectedBacking || oldInlinePages-actualPages != wantSaved || wantSaved <= 0 {
		test.Fatal("many one-block owners did not eliminate exactly the seven unused slots per child", actualPages, wantPages, actualBacking, expectedBacking, oldInlinePages, wantSaved)
	}
}

func legacyNativeSingletonOwnershipEncode(test *testing.T, encoding *LegacyCanonicalEncoding, probe *legacyNativeBlockPagedOwnershipProbe, raw []byte, expected CIREvent) []byte {
	test.Helper()
	owner := encoding.nativeOwner
	if owner == nil || owner.blocks != nil || owner.replacement != probe.replacement || owner.seal != nil ||
		encoding.preparation == nil || encoding.inspectWork != 0 {
		test.Fatal("public singleton handoff bypassed the one-use owner or pending native preparation")
	}
	projected := make(map[*legacyNativeBlockValues]bool)
	legacyNativeBlockPagedParityValue(test, owner.event, owner.blocks, owner.replacement, expected, true, projected)
	ledger := legacyNativeBlockPagedOwnershipNewLedger(probe, string(raw))
	legacyNativeSingletonOwnershipPages(test, ledger, expected.Replacement)
	ledger.check(encoding)
	if _, _, err := owner.claim(context.Background()); !errors.Is(err, ErrIntegrity) {
		test.Fatal("public singleton owner admitted a second claim", err)
	}
	for quantum := 0; encoding.preparation != nil && quantum < 1<<16; quantum++ {
		chunk, complete := LegacyReplacementOrderBoundedNext(test, encoding)
		if complete || len(chunk) != 0 {
			test.Fatal("singleton preparation exposed output before the full native owner was prepared")
		}
	}
	if encoding.preparation != nil {
		test.Fatal("singleton preparation exhausted its finite action allowance")
	}
	legacyNativeBlockPagedParityProjection(test, encoding, projected)
	legacyNativeSingletonOwnershipPages(test, ledger, expected.Replacement)
	ledger.check(encoding)
	first, complete := LegacyReplacementOrderBoundedNext(test, encoding)
	if complete || len(first) == 0 {
		test.Fatal("8192-child singleton fixture did not retain its owner across partial canonical output")
	}
	ledger.check(encoding)
	rest, _ := collectLegacyCanonicalEncoding(test, encoding)
	legacyNativeBlockPagedOwnershipReleased(test, encoding, owner, nil)
	encoding.release()
	encoding.release()
	owner.release()
	legacyNativeBlockPagedOwnershipReleased(test, encoding, owner, nil)
	return append(first, rest...)
}

func TestLegacyNativeSingletonOwnershipPublic8192OneBlockChildren(test *testing.T) {
	const children = 8192
	if LegacyCanonicalEncodingBytes != 64<<10 || legacyCanonicalEncodingActions != 1024 ||
		LegacyNormalizationPartBytes != 64<<10 || ConversationManifestChunkBytes != 512<<10 ||
		legacyCanonicalInlineValues != 8 || legacyCanonicalPageValues != 64 {
		test.Fatal("singleton regression requires the unchanged canonical, source, and page contracts")
	}
	fixture := legacyNativeEventsL06Source(test, legacyNativeEventsL06Case{children: children, text: 64})
	typed, want, err := legacyNativePagedParityOracle(test, fixture.raw)
	if err != nil || len(typed.Events) != 1 || !reflect.DeepEqual(typed.Events[0], fixture.expected) ||
		HashContent(want) != fixture.reference || len(fixture.raw) <= ConversationManifestChunkBytes || len(fixture.raw) >= 2<<20 {
		test.Fatal("singleton regression lost the original multipart 8192-child typed/canonical source", err)
	}
	wantEvent := legacyCanonicalNativePreparationOracle(test, fixture.expected)
	buffer, source := legacyInputGeometrySource(bytes.Clone(fixture.raw), LegacyNormalizationPartBytes)
	normalizer, err := NewLegacyNormalizationDecoder(context.Background(), source)
	if err != nil || normalizer == nil || len(buffer.reads) != 0 {
		test.Fatal("public singleton decoder failed or read source during construction", err)
	}
	probe := &legacyNativeBlockPagedOwnershipProbe{
		Context: context.Background(), test: test, normalizer: normalizer, borrowed: make(map[*legacyNativeBlockList]int64),
	}
	var eventBody []byte
	emitted := 0
	for quantum := 0; quantum < 4096; quantum++ {
		before := len(buffer.reads)
		step, err := normalizer.Step(probe)
		if err != nil || len(buffer.reads)-before > 2 || len(step.Events) > 1 || step.Reset && emitted != 0 {
			test.Fatal("public singleton decoder lost its existing source-load or event handoff contract", err)
		}
		for _, pending := range step.Events {
			if emitted != 0 || pending.Encoding == nil || pending.Sequence != fixture.expected.Seq || !probe.captured {
				test.Fatal("public singleton source lost its sole observed native event")
			}
			test.Cleanup(pending.Encoding.release)
			legacyNativeEventsOwnershipCleared(test, probe.native)
			reads := len(buffer.reads)
			eventBody = legacyNativeSingletonOwnershipEncode(test, pending.Encoding, probe, fixture.event, fixture.expected)
			if len(buffer.reads) != reads || !bytes.Equal(eventBody, wantEvent) ||
				sha256.Sum256(eventBody) != sha256.Sum256(wantEvent) || HashContent(eventBody) != HashContent(wantEvent) {
				test.Fatal("singleton ownership changed canonical event bytes/SHA or reread the original source")
			}
			emitted++
		}
		if !step.Complete {
			continue
		}
		actual := AssembleCanonicalDoc(step.Envelope, [][]byte{eventBody})
		if step.Count != 1 || emitted != 1 || !bytes.Equal(actual, want) || sha256.Sum256(actual) != sha256.Sum256(want) ||
			HashContent(actual) != fixture.reference || !bytes.Equal(buffer.body, fixture.raw) || sha256.Sum256(buffer.body) != fixture.bytesSHA {
			test.Fatal("singleton ownership changed the original source or completed canonical document bytes/SHA")
		}
		typedAfter, err := json.Marshal(fixture.expected)
		if err != nil || !bytes.Equal(typedAfter, fixture.typedJSON) || sha256.Sum256(typedAfter) != fixture.typedSHA {
			test.Fatal("singleton observation or encoding mutated the independent original typed oracle", err)
		}
		legacyNativePagedParitySourceReads(test, buffer, LegacyNormalizationPartBytes, fixture.raw)
		reads := len(buffer.reads)
		again, err := normalizer.Step(context.Background())
		if err != nil || !again.Complete || again.Count != 1 || len(again.Events) != 0 || !bytes.Equal(again.Envelope, step.Envelope) || len(buffer.reads) != reads {
			test.Fatal("completed singleton normalization revived ownership or reread source", err)
		}
		return
	}
	test.Fatal("public singleton ownership exhausted its finite normalization allowance")
}
