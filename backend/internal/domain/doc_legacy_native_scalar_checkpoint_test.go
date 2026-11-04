package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const legacyNativeScalarCheckpointOracleHeapBudget int64 = 64 << 20

func legacyNativeScalarCheckpointOracleSource(body []byte, partBytes int) (*legacySourceProofBuffer, LegacyByteSource) {
	buffer := &legacySourceProofBuffer{body: bytes.Clone(body)}
	source := buffer.input()
	if partBytes == LegacyCanonicalEncodingBytes {
		source.PartBytes, source.Parts = partBytes, nil
		for offset := 0; offset < len(body); offset += partBytes {
			part := buffer.body[offset:min(offset+partBytes, len(body))]
			source.Parts = append(source.Parts, ConversationManifestChunk{Hash: HashContent(part), Bytes: int64(len(part))})
		}
	}
	return buffer, source
}

func legacyNativeScalarCheckpointOracleRawSource(test *testing.T, body []byte) (*legacySourceProofBuffer, LegacyByteSource) {
	test.Helper()
	buffer, source := legacyNativeScalarCheckpointOracleSource(body, 0)
	read := source.Read
	source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		if offset < 0 || offset >= source.Bytes || offset%ConversationManifestChunkBytes != 0 ||
			length != min(ConversationManifestChunkBytes, source.Bytes-offset) {
			test.Fatal("scalar continuation bypassed known authenticated RAW parts", offset, length)
		}
		return read(ctx, offset, length)
	}
	return buffer, source
}

func legacyNativeScalarCheckpointOracleStep(test *testing.T, scalar *legacyNativeScalar, source LegacyByteSource, workBytes int) (bool, error) {
	test.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	input := &legacyNormalizationInput{ctx: ctx, source: source, workBytes: workBytes}
	complete, err := scalar.step(ctx, input)
	cancel()
	if input.workBytes < workBytes || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded {
		test.Fatal("scalar continuation escaped its fresh shared work/load budget", input.workBytes, input.loads)
	}
	return complete, err
}

func legacyNativeScalarCheckpointOracleFinish(test *testing.T, scalar *legacyNativeScalar, source LegacyByteSource, expected string) {
	test.Helper()
	for range 256 {
		complete, err := legacyNativeScalarCheckpointOracleStep(test, scalar, source, 0)
		if err != nil {
			test.Fatal("scalar continuation rejected known native text", err)
		}
		if !complete {
			continue
		}
		if scalar.text == nil || scalar.text.String() != expected || scalar.position != scalar.end ||
			scalar.buffered != 0 || scalar.previous != nil {
			test.Fatal("scalar continuation changed exact native text or selected-source EOF", scalar.position, scalar.end)
		}
		terminal := *scalar
		reads := 0
		source.Read = func(context.Context, int, int) ([]byte, error) {
			reads++
			return nil, ErrIntegrity
		}
		complete, err = legacyNativeScalarCheckpointOracleStep(test, scalar, source, 0)
		if err != nil || !complete || reads != 0 || *scalar != terminal {
			test.Fatal("completed scalar reread RAW bytes or changed terminal state", complete, err, reads)
		}
		return
	}
	test.Fatal("scalar continuation exhausted its finite schedule")
}

func legacyNativeScalarCheckpointOracleCapture(test *testing.T, scalar *legacyNativeScalar, expected string, readFrom int) *legacyNativeScalarSnapshot {
	test.Helper()
	before := *scalar
	ctx, cancel := context.WithCancel(context.Background())
	snapshot, err := scalar.checkpoint(ctx)
	cancel()
	if err != nil || snapshot == nil || *scalar != before {
		test.Fatal("snapshot changed the live scalar or rejected its accepted prefix", err)
	}
	state := snapshot.state
	if state.Version != 1 || state.Position != scalar.position || state.End != scalar.end || state.Maximum != scalar.maximum ||
		state.Bytes != len(expected) || snapshot.prefix != expected || state.Present != (scalar.text != nil || scalar.previous != nil) ||
		state.Expanded != scalar.expanded || state.Capacity < state.Bytes || state.Capacity > 2*state.Maximum+64 {
		test.Fatal("snapshot confused accepted text with RAW position or migration progress", state, len(expected))
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		test.Fatal("checkpoint metadata is not JSON encodable", err)
	}
	var decoded legacyNativeScalarCheckpoint
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != state {
		test.Fatal("checkpoint JSON lost a logical continuation field", err)
	}
	if retained, err := snapshot.retainedBytes(); err != nil || retained < int64(len(expected)) {
		test.Fatal("snapshot omitted retained immutable prefix bytes", retained, err)
	}
	for _, bounds := range [][2]int{{-1, 0}, {0, -1}, {state.Bytes + 1, 0}, {state.Bytes, 1}, {0, LegacyCanonicalEncodingBytes + 1}} {
		body, err := snapshot.read(context.Background(), bounds[0], bounds[1])
		if !errors.Is(err, ErrIntegrity) || len(body) != 0 {
			test.Fatal("snapshot admitted invalid or over-64KiB reads", bounds, len(body), err)
		}
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if body, err := snapshot.read(ctx, -1, LegacyCanonicalEncodingBytes+1); !errors.Is(err, context.Canceled) || len(body) != 0 {
		test.Fatal("snapshot read did not prioritize context over bounds", len(body), err)
	}
	if body, err := snapshot.read(context.Background(), state.Bytes, 0); err != nil || len(body) != 0 {
		test.Fatal("snapshot rejected its exact empty end range", len(body), err)
	}
	if state.Bytes > readFrom {
		length := min(state.Bytes-readFrom, LegacyCanonicalEncodingBytes)
		body, err := snapshot.read(context.Background(), readFrom, length)
		if err != nil || len(body) != length || cap(body) > LegacyCanonicalEncodingBytes || string(body) != expected[readFrom:readFrom+length] {
			test.Fatal("snapshot changed bounded accepted-prefix bytes", len(body), cap(body), err)
		}
		body[0] ^= 0xff
		fresh, err := snapshot.read(context.Background(), readFrom, 1)
		if err != nil || string(fresh) != expected[readFrom:readFrom+1] || snapshot.prefix != expected || *scalar != before {
			test.Fatal("snapshot read exposed mutable live prefix storage", err)
		}
	}
	return snapshot
}

func legacyNativeScalarCheckpointOracleRead(test *testing.T, snapshot *legacyNativeScalarSnapshot, from, to int) []byte {
	test.Helper()
	result := make([]byte, 0, to-from)
	for offset := from; offset < to; {
		length := min(LegacyCanonicalEncodingBytes, to-offset)
		body, err := snapshot.read(context.Background(), offset, length)
		if err != nil || len(body) != length || cap(body) > LegacyCanonicalEncodingBytes {
			test.Fatal("snapshot delta exceeded bounded page export", offset, length, len(body), cap(body), err)
		}
		result = append(result, body...)
		offset += length
	}
	return result
}

func legacyNativeScalarCheckpointOracleRestore(test *testing.T, snapshot *legacyNativeScalarSnapshot, persisted []byte, partBytes, sourceBytes int, raw *legacySourceProofBuffer) *legacyNativeScalar {
	test.Helper()
	prefix, source := legacyNativeScalarCheckpointOracleSource(persisted, partBytes)
	rawReads := len(raw.reads)
	ctx, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeScalarRestore(ctx, snapshot.state, source, sourceBytes, legacyNativeScalarCheckpointOracleHeapBudget)
	cancel()
	if err != nil || work == nil || work.copied != 0 || len(prefix.reads) != 0 || len(raw.reads) != rawReads {
		test.Fatal("restore constructor read prefix/RAW bytes or rejected admitted state", err)
	}
	defer work.close()
	for stepIndex := 0; stepIndex < 256; stepIndex++ {
		beforeCopied, beforeReads := work.copied, len(prefix.reads)
		ctx, cancel := context.WithCancel(context.Background())
		ready, err := work.step(ctx)
		cancel()
		if err != nil || work.copied < beforeCopied || work.copied-beforeCopied > LegacyCanonicalEncodingBytes ||
			work.copied > snapshot.state.Bytes || len(prefix.reads)-beforeReads > 1 || len(raw.reads) != rawReads {
			test.Fatal("restore exceeded accepted-prefix copy/read bounds", beforeCopied, work.copied, len(prefix.reads)-beforeReads, err)
		}
		if stepIndex == 0 && (work.copied != 0 || len(prefix.reads) != 0) {
			test.Fatal("restore combined workspace allocation with prefix IO or copying")
		}
		for _, read := range prefix.reads[beforeReads:] {
			if read[0] < 0 || read[0] >= len(persisted) || read[0]%source.partBytes() != 0 ||
				read[1] != min(source.partBytes(), len(persisted)-read[0]) || read[1] > ConversationManifestChunkBytes {
				test.Fatal("restore bypassed exact hash-checked prefix parts", read)
			}
		}
		if retained, err := work.retainedBytes(); err != nil || retained <= 0 || retained > legacyNativeScalarCheckpointOracleHeapBudget {
			test.Fatal("restore exceeded its explicit admitted heap budget", retained, err)
		}
		if !ready {
			continue
		}
		if work.copied != len(persisted) {
			test.Fatal("restore completed before reconstructing the accepted prefix", work.copied, len(persisted))
		}
		ctx, cancel = context.WithCancel(context.Background())
		scalar, err := work.take(ctx)
		cancel()
		if err != nil || scalar == nil || scalar.position != snapshot.state.Position || scalar.end != snapshot.state.End ||
			scalar.maximum != snapshot.state.Maximum || scalar.expanded != snapshot.state.Expanded ||
			scalar.previous != nil || scalar.copied != 0 || scalar.copyDeferred || scalar.buffered != 0 || scalar.pending != [12]byte{} ||
			(scalar.text != nil) != snapshot.state.Present {
			test.Fatal("restore retained inactive prefetch/copy state or lost logical scalar fields", err)
		}
		if scalar.text != nil && (scalar.text.String() != string(persisted) || scalar.text.Cap() < snapshot.state.Capacity) {
			test.Fatal("restore changed accepted text or ignored reconstruction capacity")
		}
		transferred := *scalar
		work.close()
		if len(prefix.body) > 0 {
			prefix.body[0] ^= 0xff
		}
		if *scalar != transferred || scalar.text != nil && scalar.text.String() != string(persisted) {
			test.Fatal("closed restore or borrowed prefix storage changed transferred scalar ownership")
		}
		return scalar
	}
	test.Fatal("restore exhausted its finite prefix reconstruction schedule")
	return nil
}

func TestLegacyNativeScalarCheckpointOracleNativeContinuation(test *testing.T) {
	for _, fixture := range []struct {
		name      string
		body      string
		expected  string
		accepted  string
		advance   int
		workBytes int
		buffered  int
		started   bool
		complete  bool
		expanded  bool
	}{
		{name: "absent-builder", body: "fresh", expected: "fresh"},
		{name: "completed-empty", started: true, complete: true},
		{name: "escaped-surrogate-prefetch", body: `head\"\\\/\b\f\n\r\t\u0000\u2028\u2029\uD83D\uDE42\uD800x\uDC00`,
			expected: "head\"\\/\b\f\n\r\t\x00\u2028\u2029🙂�x�", accepted: "head", advance: 4,
			workBytes: ConversationManifestChunkBytes - 8 - 5, buffered: 5, started: true},
		{name: "unicode-prefetch", body: "seen🙂\ud55c\\u0061tail", expected: "seen🙂\ud55catail", accepted: "seen", advance: 4,
			workBytes: ConversationManifestChunkBytes - 8 - 2, buffered: 2, started: true},
		{name: "invalid-utf8-prefetch", body: "bad\xff\xc0\x80\xed\xa0\x80\xf0\x9f" + `\uD800tail`,
			expected: "bad" + strings.Repeat("�", 8) + "�tail", accepted: "bad", advance: 3,
			workBytes: ConversationManifestChunkBytes - 6 - 3, buffered: 3, started: true},
		{name: "completed-expanded", body: "\xff" + `\uDC00` + "\ud55c", expected: "��\ud55c", accepted: "��\ud55c",
			advance: len("\xff" + `\uDC00` + "\ud55c"), started: true, complete: true, expanded: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			var native string
			if err := json.Unmarshal([]byte(`"`+fixture.body+`"`), &native); err != nil || native != fixture.expected {
				test.Fatal("fixture lost its explicit native JSON string oracle", err)
			}
			raw, source := legacyNativeScalarCheckpointOracleRawSource(test, []byte(`["`+fixture.body+`",true]`))
			scalar := &legacyNativeScalar{position: 2, end: 2 + len(fixture.body), maximum: 3 * len(fixture.body)}
			if fixture.started {
				complete, err := legacyNativeScalarCheckpointOracleStep(test, scalar, source, fixture.workBytes)
				if err != nil || complete != fixture.complete || scalar.text == nil || scalar.text.String() != fixture.accepted {
					test.Fatal("actual scalar step missed the selected known prefix", complete, err)
				}
			}
			if scalar.position != 2+fixture.advance || scalar.buffered != fixture.buffered || scalar.expanded != fixture.expanded ||
				fixture.buffered > 0 && string(scalar.pending[:fixture.buffered]) != fixture.body[fixture.advance:fixture.advance+fixture.buffered] {
				test.Fatal("fixture did not reach the intended RAW position and partial prefetch", scalar.position, scalar.buffered)
			}
			snapshot := legacyNativeScalarCheckpointOracleCapture(test, scalar, fixture.accepted, 0)
			defer snapshot.close()
			persisted := legacyNativeScalarCheckpointOracleRead(test, snapshot, 0, snapshot.state.Bytes)
			legacyNativeScalarCheckpointOracleFinish(test, scalar, source, fixture.expected)
			if snapshot.prefix != fixture.accepted || !bytes.Equal(legacyNativeScalarCheckpointOracleRead(test, snapshot, 0, snapshot.state.Bytes), persisted) {
				test.Fatal("continued original scalar changed its immutable captured prefix")
			}
			restored := legacyNativeScalarCheckpointOracleRestore(test, snapshot, persisted, 0, source.Bytes, raw)
			if fixture.name == "escaped-surrogate-prefetch" {
				rejected := legacyNativeScalarCheckpointOracleRestore(test, snapshot, persisted, LegacyCanonicalEncodingBytes, source.Bytes, raw)
				corrupt := source
				corrupt.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
					body, err := source.Read(ctx, offset, length)
					body = bytes.Clone(body)
					if len(body) > 0 {
						body[0] ^= 0xff
					}
					return body, err
				}
				if complete, err := legacyNativeScalarCheckpointOracleStep(test, rejected, corrupt, 0); complete || !errors.Is(err, ErrPreparedDocRangeObject) {
					test.Fatal("discarded prefetch bypassed current RAW source hash authentication", complete, err)
				}
			}
			snapshot.close()
			legacyNativeScalarCheckpointOracleFinish(test, restored, source, fixture.expected)
		})
	}
}

func TestLegacyNativeScalarCheckpointOracleMigrationAndAppendOnlyDelta(test *testing.T) {
	invalidBytes := 2*ConversationManifestChunkBytes + 37
	body := strings.Repeat("\xff", invalidBytes) + `\uD83D\uDE42` + "\ud55c"
	expected := strings.Repeat("�", invalidBytes) + "🙂\ud55c"
	var native string
	if err := json.Unmarshal([]byte(`"`+body+`"`), &native); err != nil || native != expected {
		test.Fatal("migration fixture lost its explicit invalid-UTF8/native oracle", err)
	}
	raw, source := legacyNativeScalarCheckpointOracleRawSource(test, []byte(`["`+body+`",false]`))
	scalar := &legacyNativeScalar{position: 2, end: 2 + len(body), maximum: 3 * len(body)}
	for range 128 {
		complete, err := legacyNativeScalarCheckpointOracleStep(test, scalar, source, 0)
		if err != nil || complete {
			test.Fatal("migration fixture completed before an actual partial builder copy", complete, err)
		}
		if scalar.previous != nil && scalar.copied > 0 && scalar.copied < scalar.previous.Len() {
			break
		}
	}
	if scalar.previous == nil || scalar.copied <= 0 || scalar.copied >= scalar.previous.Len() ||
		scalar.previous.Len()%3 != 0 || scalar.previous.Len()-scalar.copied <= LegacyCanonicalEncodingBytes ||
		scalar.text.Len() != scalar.copied || scalar.text.String() != scalar.previous.String()[:scalar.copied] {
		test.Fatal("fixture did not retain a full accepted builder beside a genuinely partial new builder")
	}
	accepted := strings.Repeat("�", scalar.previous.Len()/3)
	if scalar.position != 2+len(accepted)/3 || scalar.previous.String() != accepted || !scalar.expanded {
		test.Fatal("migration changed the independently known accepted replacement text")
	}
	older := legacyNativeScalarCheckpointOracleCapture(test, scalar, accepted, 0)
	defer older.close()
	persisted := legacyNativeScalarCheckpointOracleRead(test, older, 0, older.state.Bytes)
	beforeCopied := scalar.copied
	if complete, err := legacyNativeScalarCheckpointOracleStep(test, scalar, source, ConversationManifestChunkBytes-LegacyCanonicalEncodingBytes); err != nil || complete || scalar.previous == nil || scalar.copied-beforeCopied != LegacyCanonicalEncodingBytes || scalar.text.Len() != scalar.copied {
		test.Fatal("second actual step did not advance only the partial migration cursor", complete, err)
	}
	migrating := legacyNativeScalarCheckpointOracleCapture(test, scalar, accepted, older.state.Bytes)
	defer migrating.close()
	if migrating.state.Bytes != older.state.Bytes || migrating.prefix != older.prefix || migrating.state.Bytes == scalar.text.Len() {
		test.Fatal("advanced partial copy replaced the old full accepted prefix with new partial text")
	}
	if delta := legacyNativeScalarCheckpointOracleRead(test, migrating, older.state.Bytes, migrating.state.Bytes); len(delta) != 0 {
		test.Fatal("copy-only migration required re-exporting an unchanged accepted prefix")
	}
	for range 128 {
		complete, err := legacyNativeScalarCheckpointOracleStep(test, scalar, source, 0)
		if err != nil || complete {
			test.Fatal("migration fixture failed before its next append-only checkpoint", complete, err)
		}
		if scalar.previous == nil && scalar.text.Len() > older.state.Bytes {
			break
		}
	}
	if scalar.previous != nil || scalar.text.Len() <= older.state.Bytes || scalar.text.Len()%3 != 0 || scalar.position >= 2+invalidBytes {
		test.Fatal("fixture did not append a new known logical prefix after builder growth")
	}
	newAccepted := strings.Repeat("�", scalar.text.Len()/3)
	newer := legacyNativeScalarCheckpointOracleCapture(test, scalar, newAccepted, older.state.Bytes)
	defer newer.close()
	delta := legacyNativeScalarCheckpointOracleRead(test, newer, older.state.Bytes, newer.state.Bytes)
	composed := append(bytes.Clone(persisted), delta...)
	if string(delta) != newAccepted[len(accepted):] || string(composed) != newAccepted || older.prefix != accepted || migrating.prefix != accepted {
		test.Fatal("bounded new-byte delta failed to compose with the retained old accepted prefix")
	}
	_, oldParts := legacyNativeScalarCheckpointOracleSource(persisted, 0)
	_, newParts := legacyNativeScalarCheckpointOracleSource(composed, 0)
	for ordinal := 0; ordinal < len(persisted)/ConversationManifestChunkBytes; ordinal++ {
		if oldParts.Parts[ordinal] != newParts.Parts[ordinal] {
			test.Fatal("append-only composition changed a previously persisted complete prefix part", ordinal)
		}
	}
	legacyNativeScalarCheckpointOracleFinish(test, scalar, source, expected)
	for _, snapshot := range []*legacyNativeScalarSnapshot{older, migrating} {
		from := max(0, snapshot.state.Bytes-LegacyCanonicalEncodingBytes)
		if snapshot.prefix != accepted || string(legacyNativeScalarCheckpointOracleRead(test, snapshot, from, snapshot.state.Bytes)) != accepted[from:] {
			test.Fatal("original append/growth changed an older snapshot's immutable accepted bytes")
		}
	}
	for _, fixture := range []struct {
		name      string
		snapshot  *legacyNativeScalarSnapshot
		persisted []byte
		partBytes int
	}{
		{name: "old-full-accepted-prefix", snapshot: older, persisted: persisted},
		{name: "advanced-partial-copy", snapshot: migrating, persisted: persisted, partBytes: LegacyCanonicalEncodingBytes},
		{name: "retained-prefix-plus-delta", snapshot: newer, persisted: composed},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			restored := legacyNativeScalarCheckpointOracleRestore(test, fixture.snapshot, fixture.persisted, fixture.partBytes, source.Bytes, raw)
			legacyNativeScalarCheckpointOracleFinish(test, restored, source, expected)
		})
	}
	test.Run("known-prefix-hash-guard", func(test *testing.T) {
		prefix, prefixSource := legacyNativeScalarCheckpointOracleSource(composed, LegacyCanonicalEncodingBytes)
		prefix.body[0] ^= 0xff
		rawReads := len(raw.reads)
		work, err := newLegacyNativeScalarRestore(context.Background(), newer.state, prefixSource, source.Bytes, legacyNativeScalarCheckpointOracleHeapBudget)
		if err != nil || work == nil || len(prefix.reads) != 0 {
			test.Fatal("prefix guard fixture did not defer hash IO", err)
		}
		defer work.close()
		for range 4 {
			beforeCopied, beforeReads := work.copied, len(prefix.reads)
			ctx, cancel := context.WithCancel(context.Background())
			ready, err := work.step(ctx)
			cancel()
			if work.copied-beforeCopied > LegacyCanonicalEncodingBytes || len(prefix.reads)-beforeReads > 1 || len(raw.reads) != rawReads {
				test.Fatal("hash rejection escaped restore copy/read isolation")
			}
			if err != nil {
				if !errors.Is(err, ErrPreparedDocRangeObject) || ready || work.copied != 0 {
					test.Fatal("restore accepted unauthenticated prefix bytes", ready, work.copied, err)
				}
				return
			}
			if ready {
				test.Fatal("corrupt prefix became ready without matching its known hash")
			}
		}
		test.Fatal("corrupt first prefix part escaped the finite hash guard schedule")
	})
}

func TestLegacyNativeScalarCheckpointOracleNumericDiagnosticPrefix(test *testing.T) {
	token := "1" + strings.Repeat("0", ConversationManifestChunkBytes+17)
	var nativeValue any
	var nativeError *json.UnmarshalTypeError
	oracleError := json.Unmarshal([]byte(token), &nativeValue)
	if !errors.As(oracleError, &nativeError) || nativeError.Value != "number "+token {
		test.Fatal("numeric fixture lost its known native overflow diagnostic", oracleError)
	}
	from := 17
	raw, source := legacyNativeScalarCheckpointOracleRawSource(test, []byte(strings.Repeat(" ", from)+token+",true]"))
	number, err := newLegacyNativeNumberValue(from, from+len(token), legacyNativeNumberDiagnosticsFull)
	if err != nil {
		test.Fatal("numeric fixture rejected its selected source span", err)
	}
	stepNumber := func() (bool, error) {
		ctx, cancel := context.WithCancel(context.Background())
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		complete, err := number.step(ctx, input)
		cancel()
		if input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded {
			test.Fatal("numeric diagnostic escaped fresh scalar work/load limits", input.workBytes, input.loads)
		}
		return complete, err
	}
	for range 128 {
		complete, err := stepNumber()
		if err != nil || complete {
			test.Fatal("numeric diagnostic completed before exposing a live accepted-prefix checkpoint", complete, err)
		}
		if number.errorText != nil && number.errorText.previous == nil && number.errorText.text != nil &&
			number.errorText.text.Len() > len("number 1") && number.errorText.position < number.errorText.end {
			break
		}
	}
	if number.errorText == nil || number.errorText.previous != nil || number.errorText.text == nil ||
		number.errorText.text.Len() <= len("number 1") || number.errorText.position >= number.errorText.end || !number.converted || !number.rejected {
		test.Fatal("fixture did not reach the actual numeric diagnostic scalar continuation")
	}
	acceptedBytes := number.errorText.text.Len()
	accepted := "number 1" + strings.Repeat("0", acceptedBytes-len("number 1"))
	snapshot := legacyNativeScalarCheckpointOracleCapture(test, number.errorText, accepted, 0)
	defer snapshot.close()
	if snapshot.state.Bytes != acceptedBytes || snapshot.state.Bytes == snapshot.state.Position-from ||
		snapshot.state.Position != from+acceptedBytes-len("number ") {
		test.Fatal("numeric checkpoint inferred logical diagnostic bytes from RAW advancement", snapshot.state, acceptedBytes)
	}
	persisted := legacyNativeScalarCheckpointOracleRead(test, snapshot, 0, snapshot.state.Bytes)
	finished := false
	for range 128 {
		complete, err := stepNumber()
		if !complete && err == nil {
			continue
		}
		var actualError *json.UnmarshalTypeError
		if !complete || !errors.As(err, &actualError) || *actualError != *nativeError || err.Error() != oracleError.Error() {
			test.Fatal("uninterrupted numeric scalar changed the native diagnostic", complete, err)
		}
		finished = true
		break
	}
	if !finished || number.errorText.text.String() != nativeError.Value || number.errorText.position != from+len(token) || snapshot.prefix != accepted {
		test.Fatal("numeric scalar lost exact final text/RAW position or changed the old snapshot")
	}
	restored := legacyNativeScalarCheckpointOracleRestore(test, snapshot, persisted, LegacyCanonicalEncodingBytes, source.Bytes, raw)
	snapshot.close()
	legacyNativeScalarCheckpointOracleFinish(test, restored, source, nativeError.Value)
}
