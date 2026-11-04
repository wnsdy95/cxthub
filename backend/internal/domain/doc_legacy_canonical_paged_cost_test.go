package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"unsafe"
)

func legacyCanonicalPagedCostDepthSource(test testing.TB, depth int) *legacyCanonicalNativeCostFixture {
	test.Helper()
	var output any = "page-boundary <&> \ud55c🙂"
	for level := 0; level < depth; level++ {
		output = []any{output}
	}
	event := CIREvent{Kind: EventToolResult, Seq: 17, CallID: "paged-depth-cost", Output: output}
	value, err := canonicalEventJSONObject(event)
	if err != nil {
		test.Fatal("paged depth fixture projection failed", err)
	}
	want, err := json.Marshal(value)
	if err != nil {
		test.Fatal("paged depth fixture original canonical oracle failed", err)
	}
	header, err := canonicalJSON(CIREnvelope{CIRVersion: CIRVersionV2, SourceProvider: ProviderCodex, Fidelity: FidelityFull})
	if err != nil {
		test.Fatal("paged depth fixture envelope failed", err)
	}
	raw := AssembleCanonicalDoc(header, [][]byte{want})
	buffer := &legacySourceProofBuffer{body: raw}
	fixture := &legacyCanonicalNativeCostFixture{
		buffer: buffer, source: buffer.input(), want: want, wantDigest: sha256.Sum256(want),
		eventStart: len(canonicalDocPrefix) + len(header) + len(canonicalDocMiddle),
	}
	target := &legacySourceProofBuffer{}
	var verifier CanonicalDocVerifier
	ctx := context.Background()
	proof, err := NewLegacyByteSourcePreparation(ctx, HashContent(raw), fixture.source, target, &verifier)
	if err != nil {
		test.Fatal("paged depth fixture source proof failed", err)
	}
	for allowance := 64 + 32*len(fixture.source.Parts) + len(raw)/LegacyNormalizationStepEvents; fixture.proofSteps < allowance; {
		progress, err := proof.Step(ctx)
		fixture.proofSteps++
		if err != nil {
			test.Fatal("paged depth fixture source proof rejected canonical input", err)
		}
		if progress == LegacyByteSourcePending {
			continue
		}
		verified, err := proof.Result()
		if progress != LegacyByteSourceVerified || err != nil || !verified.Valid() || !bytes.Equal(target.body, want) {
			test.Fatal("paged depth fixture did not obtain the original canonical source proof", progress, err)
		}
		for _, read := range buffer.reads {
			if read[0] == len(raw) && (read[1] == 0 || read[1] == 1) {
				fixture.proofEOF++
				continue
			}
			if read[0] < 0 || read[0] >= len(raw) || read[0]%ConversationManifestChunkBytes != 0 ||
				read[1] != min(ConversationManifestChunkBytes, len(raw)-read[0]) {
				test.Fatal("paged depth source proof bypassed authenticated full-part reads", read)
			}
			fixture.proofLoads++
			fixture.proofReadBytes += int64(read[1])
		}
		if fixture.proofEOF != 2 || fixture.proofReadBytes < int64(len(raw)) {
			test.Fatal("paged depth source proof omitted source bytes or either EOF probe", fixture.proofReadBytes, fixture.proofEOF)
		}
		buffer.reads = nil
		return fixture
	}
	test.Fatal("paged depth fixture source proof exhausted its finite allowance")
	return nil
}

type legacyCanonicalPagedCostStorage struct {
	allocated int64
	copied    int64
}

func legacyCanonicalPagedCostBacking[Value any](test testing.TB, values *legacyCanonicalPages[Value]) legacyCanonicalPagedCostStorage {
	test.Helper()
	_, err := values.ownedBytes()
	if err != nil {
		test.Fatal("paged cost encountered invalid collection storage", err)
	}
	valueBytes := int64(unsafe.Sizeof(*new(Value)))
	inlineBytes := int64(unsafe.Sizeof([legacyCanonicalInlineValues]Value{}))
	storage := legacyCanonicalPagedCostStorage{
		allocated: int64(values.nodes)*int64(unsafe.Sizeof(legacyCanonicalPageNode[Value]{})) +
			int64(values.pages)*int64(unsafe.Sizeof([legacyCanonicalPageValues]Value{})),
	}
	if values.single != nil {
		storage.allocated += valueBytes
	}
	if values.inline != nil || values.root != nil {
		storage.allocated += inlineBytes
		if inlineBytes <= LegacyCanonicalEncodingBytes-valueBytes {
			storage.allocated += valueBytes
			storage.copied += valueBytes
		}
	}
	if values.root != nil {
		storage.copied += inlineBytes
	}
	return storage
}

func legacyCanonicalPagedCostSnapshot(test testing.TB, encoding *LegacyCanonicalEncoding, preparation *legacyCanonicalPreparation) legacyCanonicalPagedCostStorage {
	test.Helper()
	storage := legacyCanonicalPagedCostBacking(test, &encoding.mapKeys)
	add := func(child legacyCanonicalPagedCostStorage) {
		storage.allocated += child.allocated
		storage.copied += child.copied
	}
	add(legacyCanonicalPagedCostBacking(test, &preparation.frames))
	add(legacyCanonicalPagedCostBacking(test, &encoding.frames))
	for position := 0; position < encoding.mapKeys.length(); position++ {
		keys := encoding.mapKeys.at(position)
		if keys == nil {
			test.Fatal("paged cost lost a live key-directory entry", position)
		}
		add(legacyCanonicalPagedCostBacking(test, &keys.fields))
	}
	return storage
}

type legacyCanonicalPagedCostDrain struct {
	preparation      int
	nextCalls        int
	maxAllocated     int64
	maxCopied        int64
	maxGrowth        int64
	maxAccounted     int
	maxPrepStorage   int
	maxStorage       int
	maxActions       int
	preparationPages int
	preparationDepth int
	inspectWork      uint64
	keyWork          uint64
	length           int64
	digest           [sha256.Size]byte
}

func legacyCanonicalPagedCostMeasure(benchmark *testing.B, fixture *legacyCanonicalNativeCostFixture, encoding *LegacyCanonicalEncoding) legacyCanonicalPagedCostDrain {
	benchmark.Helper()
	var cost legacyCanonicalPagedCostDrain
	owner := encoding.nativeOwner
	if owner == nil || encoding.preparation == nil || encoding.inspectWork != 0 || encoding.mapKeys.length() != 0 || encoding.frames.length() != 0 {
		benchmark.Fatal("paged cost requires an actual fresh, unvisited native owner")
	}
	digest := sha256.New()
	position := 0
	for stepIndex := 0; stepIndex < 1<<16; stepIndex++ {
		preparation, beforeInspect, beforeKeys := encoding.preparation, encoding.inspectWork, encoding.keyWork
		var before legacyCanonicalPagedCostStorage
		if preparation != nil {
			before = legacyCanonicalPagedCostSnapshot(benchmark, encoding, preparation)
		}
		benchmark.StartTimer()
		chunk, complete, err := encoding.Next(context.Background())
		benchmark.StopTimer()
		cost.nextCalls++
		if err != nil || len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes {
			benchmark.Fatal("paged cost canonical Next failed or escaped the original chunk cap", err, len(chunk), cap(chunk))
		}
		if encoding.inspectWork < beforeInspect || encoding.keyWork < beforeKeys {
			benchmark.Fatal("paged cost inspect/key work decreased", stepIndex)
		}
		if encoding.lastStorageBytes < 0 || encoding.lastStorageBytes > LegacyCanonicalEncodingBytes {
			benchmark.Fatal("paged Next exceeded its reserved structural storage-work cap", stepIndex, encoding.lastStorageBytes)
		}
		cost.maxStorage = max(cost.maxStorage, encoding.lastStorageBytes)
		if preparation != nil {
			after := legacyCanonicalPagedCostSnapshot(benchmark, encoding, preparation)
			allocated, copied := after.allocated-before.allocated, after.copied-before.copied
			if allocated < 0 || copied < 0 || allocated+copied > LegacyCanonicalEncodingBytes {
				benchmark.Fatal("paged preparation changed its storage history or exceeded its structural growth quantum", stepIndex, allocated, copied)
			}
			accounted := encoding.inspectWork - beforeInspect
			if len(chunk) != 0 || complete || accounted == 0 || accounted > legacyCanonicalEncodingActions ||
				preparation.lastActions <= 0 || preparation.lastActions > legacyCanonicalEncodingActions ||
				preparation.lastBytes < 0 || preparation.lastBytes > LegacyCanonicalEncodingBytes || uint64(preparation.lastActions) != accounted {
				benchmark.Fatal("paged preparation exposed output or exceeded the original accounted quantum", stepIndex, len(chunk), complete, accounted, preparation.lastActions, preparation.lastBytes)
			}
			if preparation.lastStorageBytes < 0 || preparation.lastStorageBytes > preparation.lastBytes ||
				encoding.lastStorageBytes != preparation.lastStorageBytes || allocated+copied > int64(preparation.lastStorageBytes) {
				benchmark.Fatal("paged preparation storage-work lost its bounded page/copy reservation", stepIndex, allocated, copied, preparation.lastStorageBytes, preparation.lastBytes, encoding.lastStorageBytes)
			}
			cost.preparation++
			cost.maxAllocated = max(cost.maxAllocated, allocated)
			cost.maxCopied = max(cost.maxCopied, copied)
			cost.maxGrowth = max(cost.maxGrowth, allocated+copied)
			cost.maxAccounted = max(cost.maxAccounted, preparation.lastBytes)
			cost.maxPrepStorage = max(cost.maxPrepStorage, preparation.lastStorageBytes)
			cost.maxActions = max(cost.maxActions, preparation.lastActions)
			cost.preparationPages = max(cost.preparationPages, preparation.frames.pages)
			cost.preparationDepth = max(cost.preparationDepth, preparation.frames.depth)
		} else if encoding.inspectWork != beforeInspect {
			benchmark.Fatal("paged canonical output restarted completed inspection", stepIndex)
		}
		if len(chunk) > len(fixture.want)-position || !bytes.Equal(chunk, fixture.want[position:position+len(chunk)]) {
			benchmark.Fatal("paged cost changed original canonical bytes", stepIndex, position)
		}
		if _, err := digest.Write(chunk); err != nil {
			benchmark.Fatal(err)
		}
		position += len(chunk)
		if !complete {
			if encoding.nativeOwner != owner {
				benchmark.Fatal("paged cost released the native owner before completion")
			}
			continue
		}
		cost.length, cost.inspectWork, cost.keyWork = int64(position), encoding.inspectWork, encoding.keyWork
		copy(cost.digest[:], digest.Sum(nil))
		if position != len(fixture.want) || cost.digest != fixture.wantDigest || encoding.nativeOwner != nil || encoding.preparation != nil ||
			encoding.mapKeys.length() != 0 || encoding.frames.length() != 0 {
			benchmark.Fatal("paged cost changed the eventual canonical SHA, length, or terminal ownership", position, len(fixture.want))
		}
		return cost
	}
	benchmark.Fatal("paged cost exhausted its finite canonical Next allowance")
	return cost
}

func BenchmarkLegacyCanonicalPagedNativeNext(benchmark *testing.B) {
	benchmark.StopTimer()
	benchmark.Log("Only native canonical Next calls are timed. Source generation, original canonical oracle, authenticated source proof, fresh native decode/one-use owner transfer, constructor, page snapshots, exact byte comparison, and SHA-256 are untimed. Use finite serial benchtime such as 1x; this is not a whole-lifecycle speed, RSS, wall-clock bound, or durable-restore proof.")
	benchmark.Log("Structural allocation bytes cover single values, inline arrays, value pages, and radix nodes in preparation frames, map directory, key fields, and the encoding stack created by the final preparation Next. Cumulative snapshots include discarded single and inline allocations and their promotion copies, not allocator overhead, source/native storage, map iteration, number validation, or canonical output buffers. Counters must remain monotone until preparation finishes.")
	benchmark.Log("Reserved storage-work is reported separately: it includes bounded numeric registry work and is not total allocator/runtime bytes or RSS. The real root-frame initialization transition is included once in inspectWork and must preserve equality with preparation.lastActions; initial native constructor inspection remains zero.")
	if LegacyCanonicalEncodingBytes != 64<<10 || legacyCanonicalEncodingActions != 1024 || ConversationManifestChunkBytes != 512<<10 {
		benchmark.Fatal("paged cost requires the original encoding, action, and source quanta")
	}
	cases := []struct {
		name  string
		count int
		depth int
	}{
		{name: "map=1024", count: 1 << 10},
		{name: "map=16384", count: 16 << 10},
		{name: "map=65536", count: 64 << 10},
		{name: "prep-frames=8", depth: 6},
		{name: "prep-frames=9", depth: 7},
		{name: "prep-frames=64", depth: 62},
		{name: "prep-frames=65", depth: 63},
		{name: "prep-frames=4096", depth: 4094},
		{name: "prep-frames=4097", depth: 4095},
	}
	ctx := context.Background()
	for _, fixtureCase := range cases {
		benchmark.Run(fixtureCase.name, func(benchmark *testing.B) {
			benchmark.StopTimer()
			var fixture *legacyCanonicalNativeCostFixture
			if fixtureCase.count != 0 {
				fixture = legacyCanonicalNativeCostSource(benchmark, "map", fixtureCase.count)
			} else {
				fixture = legacyCanonicalPagedCostDepthSource(benchmark, fixtureCase.depth)
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			var preparation, nextCalls, inspectWork, keyWork, canonicalBytes, decodeSteps float64
			var maximum legacyCanonicalPagedCostDrain
			for operation := 0; operation < benchmark.N; operation++ {
				native, _, steps := legacyCanonicalNativeCostDecode(benchmark, fixture)
				decodeSteps += float64(steps)
				owner, err := native.takeCanonicalOwner(ctx)
				if err != nil || owner == nil {
					benchmark.Fatal("paged cost did not transfer an actual fresh native owner", err)
				}
				encoding, err := newLegacyCanonicalEncodingNative(ctx, owner)
				if err != nil || encoding == nil || encoding.nativeOwner != owner {
					benchmark.Fatal("paged cost native constructor lost its owner", err)
				}
				cost := legacyCanonicalPagedCostMeasure(benchmark, fixture, encoding)
				if fixtureCase.depth != 0 {
					frames := fixtureCase.depth + 2
					pages, depth := 0, 0
					if frames > legacyCanonicalInlineValues {
						pages = (frames + legacyCanonicalPageValues - 1) / legacyCanonicalPageValues
						for capacity := legacyCanonicalPageValues; frames > capacity; capacity *= legacyCanonicalPageValues {
							depth++
						}
					}
					if cost.preparationPages != pages || cost.preparationDepth != depth {
						benchmark.Fatal("paged depth fixture missed its actual preparation growth boundary", frames, cost.preparationPages, pages, cost.preparationDepth, depth)
					}
				}
				preparation += float64(cost.preparation)
				nextCalls += float64(cost.nextCalls)
				inspectWork += float64(cost.inspectWork)
				keyWork += float64(cost.keyWork)
				canonicalBytes += float64(cost.length)
				maximum.maxAllocated = max(maximum.maxAllocated, cost.maxAllocated)
				maximum.maxCopied = max(maximum.maxCopied, cost.maxCopied)
				maximum.maxGrowth = max(maximum.maxGrowth, cost.maxGrowth)
				maximum.maxAccounted = max(maximum.maxAccounted, cost.maxAccounted)
				maximum.maxPrepStorage = max(maximum.maxPrepStorage, cost.maxPrepStorage)
				maximum.maxStorage = max(maximum.maxStorage, cost.maxStorage)
				maximum.maxActions = max(maximum.maxActions, cost.maxActions)
				maximum.preparationPages = max(maximum.preparationPages, cost.preparationPages)
				maximum.preparationDepth = max(maximum.preparationDepth, cost.preparationDepth)
			}
			operations := float64(benchmark.N)
			benchmark.ReportMetric(preparation/operations, "prep-Next/op")
			benchmark.ReportMetric(nextCalls/operations, "Next/op")
			benchmark.ReportMetric(inspectWork/operations, "inspect-actions/op")
			benchmark.ReportMetric(keyWork/operations, "key-work/op")
			benchmark.ReportMetric(canonicalBytes/operations, "canonical-B/op")
			benchmark.ReportMetric(float64(maximum.maxAllocated), "max-prep-page-alloc-B/Next")
			benchmark.ReportMetric(float64(maximum.maxCopied), "max-prep-page-copy-B/Next")
			benchmark.ReportMetric(float64(maximum.maxGrowth), "max-prep-page-alloc+copy-B/Next")
			benchmark.ReportMetric(float64(maximum.maxAccounted), "max-prep-accounted-B/Next")
			benchmark.ReportMetric(float64(maximum.maxPrepStorage), "max-prep-storage-work-B/Next")
			benchmark.ReportMetric(float64(maximum.maxStorage), "max-storage-work-B/Next")
			benchmark.ReportMetric(float64(maximum.maxActions), "max-prep-actions/Next")
			benchmark.ReportMetric(float64(maximum.preparationPages), "prep-frame-pages")
			benchmark.ReportMetric(float64(maximum.preparationDepth), "prep-frame-radix-depth")
			benchmark.ReportMetric(decodeSteps/operations, "untimed-native-decode-steps/op")
			benchmark.Logf("Untimed source proof: %d source bytes, %d parts, %d steps, %d authenticated loads, %d read bytes, %d EOF probes. Each operation emitted %d canonical bytes and exactly matched the original oracle/SHA-256 %x. Structural growth is not RSS or whole-lifecycle evidence.", len(fixture.buffer.body), len(fixture.source.Parts), fixture.proofSteps, fixture.proofLoads, fixture.proofReadBytes, fixture.proofEOF, len(fixture.want), fixture.wantDigest)
		})
	}
}
