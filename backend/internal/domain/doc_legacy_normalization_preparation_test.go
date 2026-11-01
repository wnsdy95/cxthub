package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func FuzzLegacyNormalizationPreparationTypedOracle(fuzzer *testing.F) {
	for _, raw := range []string{
		`null`,
		`{"envelope":{"cir_version":"2"},"events":null}`,
		`{"envelope":{"cir_version":"2"},"events":[{"kind":"turn","seq":7}],"events":[{"kind":"turn"}]}`,
		`{"events":[{"kind":"turn","seq":2},{"kind":"turn","seq":1},{"kind":"turn","seq":1}],"envelope":{"cir_version":"2"}}`,
		`{"events":[{"kind":"message","seq":"bad","seq":2}],"events":[],"envelope":{"cir_version":"2"}}`,
		`{"unknown":1e999999,"envelope":{"cir_version":"2"}}`,
	} {
		fuzzer.Add([]byte(raw))
	}
	fuzzer.Fuzz(func(test *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			return
		}
		canonical, _, wantErr := legacyNormalizationPreparationOracle(raw)
		work, storage := legacyNormalizationPreparationStart(test, raw, true)
		_, err := legacyNormalizationPreparationDrain(test, context.Background(), work)
		if (err == nil) != (wantErr == nil) {
			test.Fatal("external preparation changed typed acceptance", err, wantErr, string(raw))
		}
		if err != nil {
			if !errors.Is(err, ErrIntegrity) || errors.Is(err, ErrPreparedDocRangeObject) {
				test.Fatal("source rejection changed error category", err)
			}
			return
		}
		if !bytes.Equal(storage.slots[3], canonical) {
			test.Fatal("external preparation changed independent canonical bytes", string(raw))
		}
	})
}

type legacyNormalizationPreparationRead struct {
	slot   int
	offset int
	length int
}

type legacyNormalizationPreparationStorage struct {
	test       *testing.T
	slots      [LegacyNormalizationSlots][]byte
	resets     [LegacyNormalizationSlots]int
	reads      []legacyNormalizationPreparationRead
	borrowed   []byte
	resetHook  func(int) error
	appendHook func(int, []byte) error
	readHook   func(int, int, int) ([]byte, error, bool)
}

func (storage *legacyNormalizationPreparationStorage) Reset(ctx context.Context, slot int) error {
	storage.test.Helper()
	if slot < 0 || slot >= 4 {
		storage.test.Fatal("normalization used an invalid scratch slot", slot)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if storage.resetHook != nil {
		if err := storage.resetHook(slot); err != nil {
			return err
		}
	}
	storage.slots[slot] = nil
	storage.resets[slot]++
	return nil
}

func (storage *legacyNormalizationPreparationStorage) Append(ctx context.Context, slot int, body []byte) error {
	storage.test.Helper()
	if slot < 0 || slot >= 4 || len(body) > 512<<10 {
		storage.test.Fatal("unbounded or invalid scratch append", slot, len(body))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if storage.appendHook != nil {
		if err := storage.appendHook(slot, body); err != nil {
			return err
		}
	}
	storage.slots[slot] = append(storage.slots[slot], body...)
	return nil
}

func (storage *legacyNormalizationPreparationStorage) Read(ctx context.Context, slot, offset, length int) ([]byte, error) {
	storage.test.Helper()
	if slot < 0 || slot >= 4 || length < 0 || length > 512<<10 {
		storage.test.Fatal("unbounded or invalid scratch read", slot, offset, length)
	}
	storage.reads = append(storage.reads, legacyNormalizationPreparationRead{slot: slot, offset: offset, length: length})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if storage.readHook != nil {
		if body, err, handled := storage.readHook(slot, offset, length); handled {
			return body, err
		}
	}
	if offset < 0 || offset > len(storage.slots[slot]) || length > len(storage.slots[slot])-offset {
		return nil, ErrIntegrity
	}
	body := storage.slots[slot][offset : offset+length]
	if storage.borrowed != nil {
		copy(storage.borrowed, body)
		return storage.borrowed[:length], nil
	}
	return body, nil
}

func legacyNormalizationPreparationOracle(raw []byte) ([]byte, VerifiedSessionDoc, error) {
	var document CIRDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, VerifiedSessionDoc{}, ErrIntegrity
	}
	canonical, err := CanonicalBytes(document)
	if err != nil || !json.Valid(canonical) {
		return nil, VerifiedSessionDoc{}, ErrIntegrity
	}
	proof, err := VerifySessionDoc(SessionDoc{Hash: HashContent(canonical), CIR: document})
	if err != nil {
		return nil, VerifiedSessionDoc{}, ErrIntegrity
	}
	return canonical, proof, nil
}

func legacyNormalizationPreparationInput(test *testing.T, input *legacySourceProofBuffer, reuse bool) LegacyByteSource {
	test.Helper()
	source := input.input()
	var borrowed []byte
	if reuse {
		borrowed = make([]byte, 512<<10)
	}
	source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		if length < 0 || length > 512<<10 {
			test.Fatal("normalization requested an unbounded input read", offset, length)
		}
		body, err := input.Read(ctx, offset, length)
		if err != nil || borrowed == nil {
			return body, err
		}
		copy(borrowed, body)
		return borrowed[:len(body)], nil
	}
	return source
}

func legacyNormalizationPreparationPending(test *testing.T, work *LegacyNormalizationPreparation) {
	test.Helper()
	source, err := work.Source()
	if err == nil || source.Bytes != 0 || source.Parts != nil || source.Read != nil {
		test.Fatal("incomplete or failed normalization exposed a nonzero source", source.Bytes, len(source.Parts), err)
	}
}

func legacyNormalizationPreparationStart(test *testing.T, raw []byte, reuse bool) (*LegacyNormalizationPreparation, *legacyNormalizationPreparationStorage) {
	test.Helper()
	if LegacyNormalizationSlots != 4 {
		test.Fatal("normalization changed the four-slot scratch contract", LegacyNormalizationSlots)
	}
	storage := &legacyNormalizationPreparationStorage{test: test}
	for slot := range storage.slots {
		storage.slots[slot] = []byte("stale private scratch")
	}
	if reuse {
		storage.borrowed = make([]byte, 512<<10)
	}
	input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
	source := legacyNormalizationPreparationInput(test, input, reuse)
	work, err := NewLegacyNormalizationPreparation(context.Background(), source, storage)
	if err != nil {
		test.Fatal(err)
	}
	for slot := range storage.slots {
		if len(storage.slots[slot]) != 0 || storage.resets[slot] != 1 {
			test.Fatal("constructor did not reset every independent scratch slot", slot)
		}
	}
	for ordinal := range source.Parts {
		source.Parts[ordinal].Hash = HashContent([]byte("caller replaced source descriptor"))
	}
	legacyNormalizationPreparationPending(test, work)
	return work, storage
}

func legacyNormalizationPreparationDrain(test *testing.T, ctx context.Context, work *LegacyNormalizationPreparation) (map[int]int, error) {
	test.Helper()
	quanta := make(map[int]int)
	for stepIndex := 0; stepIndex < 8192; stepIndex++ {
		legacyNormalizationPreparationPending(test, work)
		phase, position, count, target, width := work.phase, work.position, work.count, work.targetSlot, work.width
		previous := work.files[target].size
		complete, err := work.Step(ctx)
		if err != nil {
			if complete {
				test.Fatal("failed step reported completion", err)
			}
			legacyNormalizationPreparationPending(test, work)
			return quanta, err
		}
		retained, err := work.RetainedBytes()
		if err != nil || retained <= 0 || retained >= 2<<20 {
			test.Fatal("normalization retained cumulative events, indexes, or root bytes", retained, err)
		}
		if phase == legacyNormalizationRuns || phase == legacyNormalizationMerge {
			appended := work.files[target].size - previous
			if appended != min(1024, count-position)*64 {
				test.Fatal("sort quantum did not process exactly its bounded record batch", phase, position, count, appended)
			}
			if phase == legacyNormalizationRuns {
				quanta[0]++
			} else {
				quanta[width]++
			}
		}
		if complete {
			return quanta, nil
		}
	}
	test.Fatal("normalization did not terminate within the explicit step limit")
	return nil, ErrIntegrity
}

func legacyNormalizationPreparationAdvance(test *testing.T, work *LegacyNormalizationPreparation, ready func() bool) {
	test.Helper()
	for stepIndex := 0; stepIndex < 8192; stepIndex++ {
		if ready() {
			return
		}
		legacyNormalizationPreparationPending(test, work)
		if complete, err := work.Step(context.Background()); err != nil || complete {
			test.Fatal("normalization terminated before the fault injection point", err, complete)
		}
	}
	test.Fatal("fault injection phase was never reached")
}

func legacyNormalizationPreparationFailed(test *testing.T, work *LegacyNormalizationPreparation, err, want error) {
	test.Helper()
	if !errors.Is(err, want) {
		test.Fatal("normalization failure lost its error category", err, want)
	}
	legacyNormalizationPreparationPending(test, work)
	if complete, repeated := work.Step(context.Background()); complete || !errors.Is(repeated, want) {
		test.Fatal("terminal failure was not sticky and zero-valued", complete, repeated)
	}
	legacyNormalizationPreparationPending(test, work)
}

func legacyNormalizationPreparationMany(count int) []byte {
	maximum := int(^uint(0) >> 1)
	var raw strings.Builder
	raw.WriteString(`{"envelope":{"cir_version":"2","source_provider":"codex","fidelity":"full"},"events":[`)
	for ordinal := 0; ordinal < count; ordinal++ {
		if ordinal != 0 {
			raw.WriteByte(',')
		}
		sequence := count - ordinal
		switch {
		case ordinal%1024 == 0:
			sequence = maximum
		case ordinal%1024 == 1:
			sequence = -maximum - 1
		case ordinal%257 == 0:
			sequence = -7
		}
		fmt.Fprintf(&raw, `{"kind":"turn","id":"ordinal-%06d","seq":%d,"role":"user"}`, ordinal, sequence)
	}
	raw.WriteString(`]}`)
	return []byte(raw.String())
}

func legacyNormalizationPreparationCheck(test *testing.T, work *LegacyNormalizationPreparation, storage *legacyNormalizationPreparationStorage, canonical []byte, original VerifiedSessionDoc) {
	test.Helper()
	if work.phase != legacyNormalizationReady || !bytes.Equal(storage.slots[3], canonical) {
		test.Fatal("prepared output differs from the independent typed canonical oracle")
	}
	covered, zeroEOF, oneEOF := 0, false, false
	for _, read := range storage.reads {
		if read.slot != 3 {
			continue
		}
		if read.offset == len(canonical) {
			zeroEOF = zeroEOF || read.length == 0
			oneEOF = oneEOF || read.length == 1
		} else if read.length > 0 {
			if read.offset != covered {
				test.Fatal("output verification skipped or revisited bytes", covered, read)
			}
			covered += read.length
		}
	}
	if covered != len(canonical) || !zeroEOF || !oneEOF {
		test.Fatal("Source became available before every output byte and both EOF probes", covered, len(canonical), zeroEOF, oneEOF)
	}
	source, err := work.Source()
	if err != nil || source.Bytes != len(canonical) || source.Read == nil || len(source.Parts) != (len(canonical)+(512<<10)-1)/(512<<10) {
		test.Fatal("ready source has invalid metadata", source.Bytes, len(source.Parts), err)
	}
	var actual []byte
	for ordinal, part := range source.Parts {
		offset := ordinal * (512 << 10)
		want := canonical[offset:min(offset+(512<<10), len(canonical))]
		if part.Bytes != int64(len(want)) || part.Hash != HashContent(want) {
			test.Fatal("output descriptor disagrees with independent canonical bytes", ordinal)
		}
		body, err := source.Read(context.Background(), offset, len(want))
		if err != nil || len(body) != len(want) {
			test.Fatal("ready source cannot read its own exact descriptor", ordinal, err)
		}
		actual = append(actual, body...)
	}
	if !bytes.Equal(actual, canonical) || HashContent(actual) != original.Hash() {
		test.Fatal("normalization changed canonical bytes or the original content hash")
	}
	source.Parts[0].Hash = HashContent([]byte("caller changed output descriptors"))
	source, err = work.Source()
	if err != nil || source.Parts[0].Hash != HashContent(canonical[:min(512<<10, len(canonical))]) {
		test.Fatal("ready source descriptors alias caller-owned metadata", err)
	}
	var verifier CanonicalDocVerifier
	output := &legacySourceProofBuffer{}
	bridge, err := NewLegacyByteSourcePreparation(context.Background(), original.Hash(), source, output, &verifier)
	if err != nil {
		test.Fatal(err)
	}
	for stepIndex := 0; stepIndex < 6000; stepIndex++ {
		if proof, err := bridge.Result(); proof.Valid() || !errors.Is(err, ErrDocRangeIndexPending) {
			test.Fatal("provisional normalized source bypassed original proof preparation", err)
		}
		progress, err := bridge.Step(context.Background())
		if err != nil {
			test.Fatal("original producer rejected normalized output", err)
		}
		if progress == LegacyByteSourcePending {
			continue
		}
		proof, err := bridge.Result()
		if progress != LegacyByteSourceVerified || err != nil || !proof.Valid() {
			test.Fatal("normalized source never earned original publication authority", progress, err)
		}
		plan, err := proof.Plan()
		if err != nil {
			test.Fatal(err)
		}
		wantPlan, err := NewLegacyByteBackfill(context.Background(), original)
		if err != nil {
			test.Fatal(err)
		}
		gotSpec, _ := plan.Spec()
		wantSpec, _ := wantPlan.Spec()
		gotKey, _ := plan.Key()
		wantKey, _ := wantPlan.Key()
		if gotSpec != wantSpec || gotKey != wantKey {
			test.Fatal("normalization bridge changed the original byte plan identity")
		}
		for ordinal := 0; ordinal < gotSpec.Blocks; ordinal++ {
			got, err := plan.Block(context.Background(), ordinal)
			want, wantErr := wantPlan.Block(context.Background(), ordinal)
			if err != nil || wantErr != nil || got.Descriptor() != want.Descriptor() || !bytes.Equal(got.Bytes(), want.Bytes()) {
				test.Fatal("normalization bridge changed the original event stream", ordinal, err, wantErr)
			}
		}
		seed, err := proof.Seed()
		wantSeed, wantErr := NewLegacyRangeMetadataSeed(context.Background(), original, wantPlan)
		if err != nil || wantErr != nil || !reflect.DeepEqual(seed, wantSeed) {
			test.Fatal("normalization bridge changed original header authority", err, wantErr)
		}
		if complete, err := work.Step(context.Background()); !complete || err != nil {
			test.Fatal("ready normalization is not idempotent", complete, err)
		}
		return
	}
	test.Fatal("original producer bridge did not finish within its explicit step limit")
}

func TestLegacyNormalizationPreparationTypedOracle(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	for _, fixture := range []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: `{}`},
		{name: "root-null", raw: `null`},
		{name: "null-fields", raw: `{"envelope":null,"events":null}`},
		{name: "duplicate-envelope-null", raw: `{"envelope":{"cir_version":"2","source_model":"first"},"envelope":null,"envelope":{"cwd":"second"},"events":[]}`},
		{name: "envelope-hidden-backing", raw: `{"envelope":{"cir_version":"2","source_models":["first","hidden-tail"]},"envelope":{"source_models":["changed"]},"envelope":{"source_models":[null,null]}}`},
		{name: "block-hidden-backing", raw: `{"envelope":{"cir_version":"2"},"events":[{"kind":"message","blocks":[{"type":"text","text":"old"},{"type":"text","text":"tail"}],"blocks":[{"text":"new"}],"blocks":[null,{}]}]}`},
		{name: "duplicate-payload-maps", raw: `{"events":[{"kind":"tool_call","input":{"keep":1,"replace":{"old":1}},"input":{"replace":{"new":2}}},{"kind":"tool_result","output":{"old":1},"output":{"new":2}}]}`},
		{name: "discarded-invalid-union", raw: `{"events":[{"kind":"unknown"},{"kind":"message","agent_author":"invalid"}],"events":[{"kind":"turn","role":"user"}]}`},
		{name: "discarded-v2-null-reset", raw: `{"envelope":{"cir_version":"1"},"events":[{"kind":"message","agent_message":false}],"events":null}`},
		{name: "folded-root-fields", raw: `{"EnVeLoPe":{"CIR_VERSION":"2"},"event\u017F":[{"kind":"turn","seq":1,"role":"system"}]}`},
		{name: "identical-duplicates-kept", raw: `{"events":[{"kind":"turn","seq":0,"id":"same"},{"kind":"turn","seq":0,"id":"same"},{"kind":"turn","seq":0,"id":"last"}]}`},
		{name: "signed-extreme-stable-ties", raw: fmt.Sprintf(`{"events":[{"kind":"turn","id":"max-first","seq":%d},{"kind":"turn","id":"min-first","seq":%d},{"kind":"turn","id":"max-second","seq":%d},{"kind":"turn","id":"min-second","seq":%d}]}`, maximum, -maximum-1, maximum, -maximum-1)},
		{name: "nested-compaction", raw: `{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","seq":5,"replacement_complete":false,"replacement":[{"kind":"turn","seq":7,"id":"late"},{"kind":"compaction","seq":-1,"replacement_complete":true,"replacement":[{"kind":"turn","seq":8,"id":"tie-first"},{"kind":"turn","seq":-2},{"kind":"turn","seq":8,"id":"tie-second"}]},{"kind":"compaction","seq":1,"replacement":[]}]},{"kind":"turn","seq":-3}]}`},
		{name: "invalid-utf8-normalization", raw: "{\"envelope\":{\"source_model\":\"\xff\"},\"ignored\":\"\xed\xa0\x80\"}"},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			canonical, original, err := legacyNormalizationPreparationOracle([]byte(fixture.raw))
			if err != nil {
				test.Fatal("accepted fixture is invalid under the independent oracle", err)
			}
			work, storage := legacyNormalizationPreparationStart(test, []byte(fixture.raw), true)
			if _, err := legacyNormalizationPreparationDrain(test, context.Background(), work); err != nil {
				test.Fatal(err)
			}
			legacyNormalizationPreparationCheck(test, work, storage, canonical, original)
		})
	}
}

func TestLegacyNormalizationPreparationMultipleMergePasses(test *testing.T) {
	for _, count := range []int{2049, 4097, 32769} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			raw := legacyNormalizationPreparationMany(count)
			canonical, original, err := legacyNormalizationPreparationOracle(raw)
			if err != nil {
				test.Fatal(err)
			}
			work, storage := legacyNormalizationPreparationStart(test, raw, true)
			quanta, err := legacyNormalizationPreparationDrain(test, context.Background(), work)
			if err != nil {
				test.Fatal(err)
			}
			passes, batches := 1, (count+1023)/1024
			if quanta[0] != batches {
				test.Fatal("initial runs did not use exactly 1024 records per quantum", quanta)
			}
			for width := 1024; width < count; width *= 2 {
				passes++
				if quanta[width] != batches {
					test.Fatal("two-way merge pass did not yield at its 1024-record boundary", width, quanta)
				}
			}
			if len(quanta) != passes {
				test.Fatal("merge passes were skipped or unexpectedly repeated", quanta)
			}
			bodyReadBytes := 0
			for _, read := range storage.reads {
				if read.slot == 0 && read.offset < len(storage.slots[0]) {
					bodyReadBytes += read.length
				}
			}
			if bodyReadBytes != len(storage.slots[0]) {
				test.Fatal("event body copying amplified tiny events into whole-page reads", bodyReadBytes, len(storage.slots[0]))
			}
			legacyNormalizationPreparationCheck(test, work, storage, canonical, original)
		})
	}
}

func TestLegacyNormalizationPreparationNativeMetadataBatchRemainsBounded(test *testing.T) {
	work, _ := legacyNormalizationPreparationStart(test, legacyNormalizationPreparationMany(129), false)
	legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationStage })
	encodings := make([]*LegacyCanonicalEncoding, len(work.pending))
	for index, event := range work.pending {
		if event.Encoding == nil || event.Encoding.preparation == nil {
			test.Fatal("fixture did not retain actual native preparation before staging", index)
		}
		encodings[index] = event.Encoding
	}
	before, count := work.files[0].size, work.count
	if complete, err := work.Step(context.Background()); complete || err != nil {
		test.Fatal("bounded native batch failed before publication", complete, err)
	}
	prepared := 0
	for _, encoding := range encodings {
		if encoding.preparation == nil {
			prepared++
		}
	}
	if work.count-count < 2 || work.count-count > ConversationManifestChunkBytes/LegacyCanonicalEncodingBytes || prepared < work.count-count ||
		work.files[0].size-before+prepared*LegacyCanonicalEncodingBytes > ConversationManifestChunkBytes {
		test.Fatal("native batching stalled per event or exceeded the shared preparation/output budget", work.count-count, prepared, work.files[0].size-before)
	}
	legacyNormalizationPreparationPending(test, work)
}

func TestLegacyNormalizationPreparationResetDiscardsStoredHistory(test *testing.T) {
	old := legacyNormalizationPreparationMany(9217)
	raw := append(bytes.Clone(old[:len(old)-1]), []byte(`,"events":[{"kind":"turn","seq":-1,"id":"selected"},{"kind":"turn","seq":-1,"id":"selected"}],"events":null,"events":[{"kind":"turn","seq":0,"id":"final"}]}`)...)
	canonical, original, err := legacyNormalizationPreparationOracle(raw)
	if err != nil {
		test.Fatal(err)
	}
	work, storage := legacyNormalizationPreparationStart(test, raw, true)
	if _, err := legacyNormalizationPreparationDrain(test, context.Background(), work); err != nil {
		test.Fatal(err)
	}
	if storage.resets[0] != 4 || storage.resets[1] != 5 || work.count != 1 || bytes.Contains(storage.slots[0], []byte("ordinal-")) {
		test.Fatal("duplicate events selection retained discarded body or index history", storage.resets, work.count)
	}
	legacyNormalizationPreparationCheck(test, work, storage, canonical, original)
}

func TestLegacyNormalizationPreparationLargeEventBorrowedBuffers(test *testing.T) {
	payload := strings.Repeat("x", (512<<10)+73) + "한🙂<&>\"\\\n"
	raw := []byte(fmt.Sprintf(`{"envelope":{"cir_version":"2"},"events":[{"kind":"message","seq":9,"role":"user","blocks":[{"type":"text","text":%q}]},{"kind":"turn","seq":-1}]}`, payload))
	canonical, original, err := legacyNormalizationPreparationOracle(raw)
	if err != nil {
		test.Fatal(err)
	}
	work, storage := legacyNormalizationPreparationStart(test, raw, true)
	if _, err := legacyNormalizationPreparationDrain(test, context.Background(), work); err != nil {
		test.Fatal(err)
	}
	fragments := 0
	for _, read := range storage.reads {
		if read.slot == 0 && read.offset < len(storage.slots[0]) && read.length > 0 {
			fragments++
		}
	}
	if fragments < 3 || len(work.files[3].parts) < 2 {
		test.Fatal("large event did not cross body-copy and output-verification boundaries", fragments)
	}
	legacyNormalizationPreparationCheck(test, work, storage, canonical, original)
}

func TestLegacyNormalizationPreparationRejectsTypedAndMalformedInputs(test *testing.T) {
	for ordinal, raw := range []string{
		``, `[]`, `true`, `1`, `{"events":{}}`, `{"events":[null]}`, `{"envelope":[]}`,
		`{"events":[{"kind":"turn","seq":"bad"}],"events":[]}`,
		`{"envelope":{"cir_version":7},"envelope":{"cir_version":"2"}}`,
		`{"events":[{"kind":"message","blocks":[{"text":12}]}],"events":null}`,
		`{"envelope":{"cir_version":"1"},"events":[{"kind":"message","agent_message":false}]}`,
		`{"envelope":{"cir_version":"99"}}`, `{"events":[{"kind":"unknown"}]}`,
		`{"events":[{"kind":"turn","replacement":null}]}`, `{"events":[],}`, `{"events":[,]}`,
		`{"events":[{} {}]}`, `{"events":[]`, `{}null`, `null[]`, `{"events":[]} garbage`,
	} {
		test.Run(fmt.Sprint(ordinal), func(test *testing.T) {
			if _, _, err := legacyNormalizationPreparationOracle([]byte(raw)); err == nil {
				test.Fatal("rejection fixture was accepted by the independent oracle")
			}
			work, _ := legacyNormalizationPreparationStart(test, []byte(raw), false)
			_, err := legacyNormalizationPreparationDrain(test, context.Background(), work)
			legacyNormalizationPreparationFailed(test, work, err, ErrIntegrity)
		})
	}
}

func TestLegacyNormalizationPreparationPrivateCorruptionAtEveryPhase(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		phase uint8
		slot  int
		width int
	}{
		{name: "body", phase: legacyNormalizationCompose, slot: 0},
		{name: "initial-index", phase: legacyNormalizationRuns, slot: 1},
		{name: "sorted-index", phase: legacyNormalizationMerge, slot: 2, width: 1024},
		{name: "second-merge-index", phase: legacyNormalizationMerge, slot: 1, width: 2048},
		{name: "output", phase: legacyNormalizationVerify, slot: 3},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			work, storage := legacyNormalizationPreparationStart(test, legacyNormalizationPreparationMany(4097), false)
			legacyNormalizationPreparationAdvance(test, work, func() bool {
				return work.phase == fixture.phase && (fixture.width == 0 || work.width == fixture.width)
			})
			if len(storage.slots[fixture.slot]) == 0 {
				test.Fatal("fault target has no authenticated private bytes", fixture.slot)
			}
			storage.slots[fixture.slot][len(storage.slots[fixture.slot])-1] ^= 1
			_, err := legacyNormalizationPreparationDrain(test, context.Background(), work)
			legacyNormalizationPreparationFailed(test, work, err, ErrPreparedDocRangeObject)
		})
	}
}

func TestLegacyNormalizationPreparationAppendMutationCannotRedefineReceipts(test *testing.T) {
	for slot := 0; slot < 4; slot++ {
		test.Run(fmt.Sprint(slot), func(test *testing.T) {
			work, storage := legacyNormalizationPreparationStart(test, legacyNormalizationPreparationMany(2049), false)
			mutated := false
			storage.appendHook = func(target int, body []byte) error {
				if target == slot && !mutated {
					if len(body) == 0 {
						test.Fatal("empty append cannot exercise mutation")
					}
					body[len(body)-1] ^= 1
					mutated = true
				}
				return nil
			}
			_, err := legacyNormalizationPreparationDrain(test, context.Background(), work)
			if !mutated {
				test.Fatal("append mutation hook never ran", slot)
			}
			legacyNormalizationPreparationFailed(test, work, err, ErrPreparedDocRangeObject)
		})
	}
}

func TestLegacyNormalizationPreparationOutputEOFMustPrecedeSource(test *testing.T) {
	for _, fault := range []string{"extra-byte", "private-eof", "short-read", "borrowed-corruption"} {
		test.Run(fault, func(test *testing.T) {
			work, storage := legacyNormalizationPreparationStart(test, legacyNormalizationPreparationMany(4097), true)
			legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationVerify })
			extent, injected := len(storage.slots[3]), false
			if fault == "extra-byte" {
				storage.slots[3] = append(storage.slots[3], '!')
			}
			storage.readHook = func(slot, offset, length int) ([]byte, error, bool) {
				if slot != 3 {
					return nil, nil, false
				}
				legacyNormalizationPreparationPending(test, work)
				if offset == extent && length == 1 {
					if fault == "extra-byte" {
						injected = true
					}
					if fault == "private-eof" {
						injected = true
						return nil, errors.Join(ErrPreparedDocRangeObject, io.EOF), true
					}
				}
				if offset < extent && length > 0 && (fault == "short-read" || fault == "borrowed-corruption") {
					injected = true
					copy(storage.borrowed, storage.slots[slot][offset:offset+length])
					if fault == "short-read" {
						return storage.borrowed[:length-1], nil, true
					}
					storage.borrowed[0] ^= 1
					return storage.borrowed[:length], nil, true
				}
				return nil, nil, false
			}
			_, err := legacyNormalizationPreparationDrain(test, context.Background(), work)
			if !injected {
				test.Fatal("output fault was never exercised", fault)
			}
			legacyNormalizationPreparationFailed(test, work, err, ErrPreparedDocRangeObject)
		})
	}
}

func TestLegacyNormalizationPreparationPrivateSourceOutranksRejection(test *testing.T) {
	for _, rejected := range []bool{false, true} {
		test.Run(fmt.Sprint(rejected), func(test *testing.T) {
			prefix := `{"envelope":{}`
			if rejected {
				prefix = `{"envelope":42`
			}
			raw := []byte(prefix + `,"ignored":"` + strings.Repeat("x", 2*(512<<10)) + `"}`)
			input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
			storage := &legacyNormalizationPreparationStorage{test: test}
			work, err := NewLegacyNormalizationPreparation(context.Background(), legacyNormalizationPreparationInput(test, input, false), storage)
			if err != nil {
				test.Fatal(err)
			}
			input.body[len(input.body)-2] ^= 1
			_, err = legacyNormalizationPreparationDrain(test, context.Background(), work)
			legacyNormalizationPreparationFailed(test, work, err, ErrPreparedDocRangeObject)
		})
	}
}

func TestLegacyNormalizationPreparationCancellationAndConstructorGuards(test *testing.T) {
	for _, phase := range []uint8{legacyNormalizationCompose, legacyNormalizationVerify} {
		test.Run(fmt.Sprintf("cancel-read-%d", phase), func(test *testing.T) {
			work, storage := legacyNormalizationPreparationStart(test, legacyNormalizationPreparationMany(4097), true)
			legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == phase })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := false
			storage.readHook = func(slot, offset, length int) ([]byte, error, bool) {
				if length == 0 {
					return nil, nil, false
				}
				injected = true
				cancel()
				return storage.slots[slot][offset : offset+length], nil, true
			}
			_, err := legacyNormalizationPreparationDrain(test, ctx, work)
			if !injected {
				test.Fatal("cancellation never occurred inside successful I/O")
			}
			legacyNormalizationPreparationFailed(test, work, err, context.Canceled)
		})
	}
	input := &legacySourceProofBuffer{body: []byte(`{}`)}
	for _, invalid := range []LegacyByteSource{
		{}, {Bytes: -1, Read: input.Read}, {Bytes: MaxFinalizedDocBytes + 1, Read: input.Read},
		{Bytes: 2, Read: input.Read}, {Bytes: 2, Parts: input.input().Parts},
		{Bytes: 2, Parts: []ConversationManifestChunk{{Bytes: 1, Hash: HashContent([]byte(`{}`))}}, Read: input.Read},
		{Bytes: 2, Parts: []ConversationManifestChunk{{Bytes: 2, Hash: "bad"}}, Read: input.Read},
	} {
		work, err := NewLegacyNormalizationPreparation(context.Background(), invalid, &legacyNormalizationPreparationStorage{test: test})
		if work != nil || !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid source descriptor created preparation", invalid.Bytes, err)
		}
	}
	if work, err := NewLegacyNormalizationPreparation(context.Background(), input.input(), nil); work != nil || !errors.Is(err, ErrIntegrity) {
		test.Fatal("nil storage created preparation", err)
	}
	for failedSlot := 0; failedSlot < 4; failedSlot++ {
		storage := &legacyNormalizationPreparationStorage{test: test, resetHook: func(slot int) error {
			if slot == failedSlot {
				return io.ErrClosedPipe
			}
			return nil
		}}
		work, err := NewLegacyNormalizationPreparation(context.Background(), input.input(), storage)
		if work != nil || !errors.Is(err, ErrPreparedDocRangeObject) || !errors.Is(err, io.ErrClosedPipe) {
			test.Fatal("scratch reset failure escaped constructor", failedSlot, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if work, err := NewLegacyNormalizationPreparation(ctx, input.input(), &legacyNormalizationPreparationStorage{test: test}); work != nil || !errors.Is(err, context.Canceled) {
		test.Fatal("canceled constructor created preparation", err)
	}
	var absent *LegacyNormalizationPreparation
	legacyNormalizationPreparationPending(test, absent)
	if complete, err := absent.Step(context.Background()); complete || !errors.Is(err, ErrIntegrity) {
		test.Fatal("nil preparation returned a nonzero result", complete, err)
	}
	if retained, err := absent.RetainedBytes(); retained != 0 || !errors.Is(err, ErrIntegrity) {
		test.Fatal("nil preparation returned nonzero retained bytes", retained, err)
	}
}
