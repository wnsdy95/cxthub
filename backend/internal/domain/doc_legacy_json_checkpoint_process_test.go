package domain

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const legacyJSONCheckpointProcessMarker = "CXTHUB_W71_SCANNER_CHECKPOINT_CHILD"
const legacyJSONCheckpointProcessInput = "CXTHUB_W71_SCANNER_CHECKPOINT_INPUT"

type legacyJSONCheckpointProcessRequest struct {
	Checkpoint []byte `json:"checkpoint"`
	Cursor     int    `json:"cursor"`
	Suffix     []byte `json:"suffix"`
}

type legacyJSONCheckpointProcessStep struct {
	Operation string `json:"operation"`
	Cursor    int    `json:"cursor"`
	Consumed  bool   `json:"consumed"`
	Complete  bool   `json:"complete"`
	Integrity bool   `json:"integrity"`
	Error     string `json:"error"`
}

type legacyJSONCheckpointProcessReport struct {
	ProcessID int                               `json:"process_id"`
	Trace     []legacyJSONCheckpointProcessStep `json:"trace"`
}

func legacyJSONCheckpointProcessPrefix(test *testing.T, prefix string, parents int) legacyJSONValueScanner {
	test.Helper()
	var scanner legacyJSONValueScanner
	if err := scanner.reset(parents); err != nil {
		test.Fatal(err)
	}
	for offset := 0; offset < len(prefix); offset++ {
		consumed, complete, err := scanner.add(prefix[offset])
		if err != nil || !consumed || complete {
			test.Fatalf("prefix byte %d: consumed=%v complete=%v error=%v", offset, consumed, complete, err)
		}
	}
	return scanner
}

func legacyJSONCheckpointProcessRecord(operation string, cursor int, consumed, complete bool, err error) legacyJSONCheckpointProcessStep {
	step := legacyJSONCheckpointProcessStep{
		Operation: operation,
		Cursor:    cursor,
		Consumed:  consumed,
		Complete:  complete,
		Integrity: errors.Is(err, ErrIntegrity),
	}
	if err != nil {
		step.Error = err.Error()
	}
	return step
}

func legacyJSONCheckpointProcessTrace(scanner *legacyJSONValueScanner, cursor int, suffix []byte) []legacyJSONCheckpointProcessStep {
	trace := make([]legacyJSONCheckpointProcessStep, 0, len(suffix)+4)
	for _, current := range suffix {
		consumed, complete, err := scanner.add(current)
		if consumed {
			cursor++
		}
		trace = append(trace, legacyJSONCheckpointProcessRecord("add", cursor, consumed, complete, err))
	}
	for attempt := 0; attempt < 2; attempt++ {
		complete, err := scanner.finish()
		trace = append(trace, legacyJSONCheckpointProcessRecord("finish", cursor, false, complete, err))
		consumed, complete, err := scanner.add('!')
		if consumed {
			cursor++
		}
		trace = append(trace, legacyJSONCheckpointProcessRecord("after_finish", cursor, consumed, complete, err))
	}
	return trace
}

func legacyJSONCheckpointProcessWire(mode legacyJSONValueMode, parents int, frames ...legacyJSONValueFrame) []byte {
	raw := make([]byte, 10+len(frames))
	raw[0] = 1
	raw[2] = byte(mode)
	binary.BigEndian.PutUint16(raw[6:8], uint16(parents))
	binary.BigEndian.PutUint16(raw[8:10], uint16(len(frames)))
	for index, frame := range frames {
		raw[10+index] = byte(frame)
	}
	return raw
}

func TestLegacyJSONValueScannerCheckpointProcessContinuation(test *testing.T) {
	fixtures := []struct {
		name    string
		prefix  string
		suffix  string
		parents int
		mode    legacyJSONValueMode
		key     bool
		valid   bool
	}{
		{"string", `"do-not-transfer-prefix`, `-suffix"`, 0, legacyJSONValueString, false, true},
		{"string_utf8_mid_byte", "\"\xed\x95", "\x9c\"", 0, legacyJSONValueString, false, true},
		{"string_key", `{"key`, `-end":null}`, 1, legacyJSONValueString, true, true},
		{"escape", `"before\`, `nend"`, 0, legacyJSONValueEscape, false, true},
		{"escape_key", `{"before\`, `"after":true}`, 0, legacyJSONValueEscape, true, true},
		{"unicode_four_left", `"\u`, `0041"`, 0, legacyJSONValueUnicode, false, true},
		{"unicode_three_left", `"\uD`, `800"`, 0, legacyJSONValueUnicode, false, true},
		{"unicode_two_left_key", `{"key\u00`, `41":[null]}`, 2, legacyJSONValueUnicode, true, true},
		{"unicode_one_left", `"before\u123`, `4"`, 0, legacyJSONValueUnicode, false, true},
		{"unicode_bad_digit", `"\u12`, `x4"`, 0, legacyJSONValueUnicode, false, false},
		{"true", `tr`, `ue`, 0, legacyJSONValueLiteral, false, true},
		{"false", `fals`, `e`, 0, legacyJSONValueLiteral, false, true},
		{"null_max_parents", `nu`, `ll`, 10000, legacyJSONValueLiteral, false, true},
		{"literal_mismatch", `fa`, `Lse`, 0, legacyJSONValueLiteral, false, false},
		{"number_minus", `-`, `12`, 0, legacyJSONValueMinus, false, true},
		{"number_zero", `-0`, `.125E+2`, 0, legacyJSONValueZero, false, true},
		{"number_integer", `-12`, `34`, 0, legacyJSONValueInteger, false, true},
		{"number_point", `-1.`, `25`, 0, legacyJSONValuePoint, false, true},
		{"number_fraction", `-1.25`, `e-9`, 0, legacyJSONValueFraction, false, true},
		{"number_exponent", `-1e`, `+7`, 0, legacyJSONValueExponent, false, true},
		{"number_exponent_sign", `-1e+`, `7`, 0, legacyJSONValueExponentSign, false, true},
		{"number_exponent_digits", `-1e+2`, `3`, 0, legacyJSONValueExponentDigits, false, true},
		{"number_delimiter", `-12.5e+67`, `,unread`, 2, legacyJSONValueExponentDigits, false, true},
		{"number_bad_suffix", `12`, `x`, 0, legacyJSONValueInteger, false, false},
		{"object_first", `{`, `}`, 0, legacyJSONValueIdle, false, true},
		{"object_key", `{"first":null,`, `"second":true}`, 0, legacyJSONValueIdle, false, true},
		{"object_colon", `{"name"`, `:[false,null]}`, 0, legacyJSONValueIdle, false, true},
		{"object_value", `{"name":`, `{"nested":[1,2]}}`, 0, legacyJSONValueIdle, false, true},
		{"object_after", `{"first":[]`, `,"second":{}}`, 0, legacyJSONValueIdle, false, true},
		{"array_first", `[`, `null,{}]`, 0, legacyJSONValueIdle, false, true},
		{"array_value", `[true,`, `[false],null]`, 0, legacyJSONValueIdle, false, true},
		{"array_after", `[true`, `,false]`, 0, legacyJSONValueIdle, false, true},
		{"nested_ancestors", `{"outer":[{"inner":[`, `"\u0041",-1e2,null]}]}`, 3, legacyJSONValueIdle, false, true},
		{"depth_boundary", `[`, `]`, 9999, legacyJSONValueIdle, false, true},
		{"depth_overflow_suffix", `[`, `[]]`, 9999, legacyJSONValueIdle, false, false},
		{"eof_string", `"unfinished`, ``, 0, legacyJSONValueString, false, false},
		{"eof_escape", `"unfinished\`, ``, 0, legacyJSONValueEscape, false, false},
		{"eof_unicode", `"\u12`, ``, 0, legacyJSONValueUnicode, false, false},
		{"eof_literal", `tru`, ``, 0, legacyJSONValueLiteral, false, false},
		{"eof_number_point", `1.`, ``, 0, legacyJSONValuePoint, false, false},
		{"eof_number_exponent", `1e`, ``, 0, legacyJSONValueExponent, false, false},
		{"eof_number_sign", `1e+`, ``, 0, legacyJSONValueExponentSign, false, false},
		{"eof_container", `{"name":[1,`, `null`, 0, legacyJSONValueIdle, false, false},
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			original := legacyJSONCheckpointProcessPrefix(test, fixture.prefix, fixture.parents)
			if original.mode != fixture.mode || original.key != fixture.key {
				test.Fatalf("fixture state: mode=%d key=%v, want mode=%d key=%v", original.mode, original.key, fixture.mode, fixture.key)
			}
			before := original
			checkpoint, err := original.checkpoint(context.Background())
			if err != nil || original != before {
				test.Fatalf("checkpoint changed original or failed: %v", err)
			}
			if len(checkpoint) != 10+original.depth || len(checkpoint) > 10010 {
				test.Fatalf("checkpoint length=%d depth=%d", len(checkpoint), original.depth)
			}
			request := legacyJSONCheckpointProcessRequest{
				Checkpoint: checkpoint,
				Cursor:     len(fixture.prefix),
				Suffix:     []byte(fixture.suffix),
			}
			expected := legacyJSONCheckpointProcessTrace(&original, request.Cursor, request.Suffix)
			finish := expected[len(request.Suffix)]
			if finish.Complete != fixture.valid || finish.Integrity != !fixture.valid ||
				fixture.valid && finish.Error != "" || !fixture.valid && finish.Error != ErrIntegrity.Error() {
				test.Fatalf("uninterrupted finish=%+v, want valid=%v", finish, fixture.valid)
			}
			if fixture.name == "number_delimiter" {
				first := expected[0]
				if first.Consumed || !first.Complete || first.Error != "" || first.Cursor != request.Cursor {
					test.Fatalf("number consumed its root delimiter: %+v", first)
				}
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				test.Fatal(err)
			}
			inputPath := filepath.Join(test.TempDir(), "continuation.json")
			if err := os.WriteFile(inputPath, encoded, 0600); err != nil {
				test.Fatal(err)
			}
			command := exec.Command(os.Args[0], "-test.run=^TestLegacyJSONValueScannerCheckpointProcessChild$", "-test.count=1")
			for _, variable := range os.Environ() {
				if !strings.HasPrefix(variable, legacyJSONCheckpointProcessMarker+"=") &&
					!strings.HasPrefix(variable, legacyJSONCheckpointProcessInput+"=") {
					command.Env = append(command.Env, variable)
				}
			}
			command.Env = append(command.Env, legacyJSONCheckpointProcessMarker+"=1", legacyJSONCheckpointProcessInput+"="+inputPath)
			if output, err := command.CombinedOutput(); err != nil {
				test.Fatalf("checkpoint child failed: %v\n%s", err, output)
			}
			encodedReport, err := os.ReadFile(inputPath + ".trace")
			if err != nil {
				test.Fatal(err)
			}
			var report legacyJSONCheckpointProcessReport
			if err := json.Unmarshal(encodedReport, &report); err != nil {
				test.Fatal(err)
			}
			if report.ProcessID <= 0 || report.ProcessID == os.Getpid() {
				test.Fatalf("continuation did not run in a child process: %d", report.ProcessID)
			}
			if !reflect.DeepEqual(report.Trace, expected) {
				test.Fatalf("child trace=%+v\nuninterrupted trace=%+v", report.Trace, expected)
			}
		})
	}
}

func TestLegacyJSONValueScannerCheckpointProcessChild(test *testing.T) {
	if os.Getenv(legacyJSONCheckpointProcessMarker) != "1" {
		return
	}
	inputPath := os.Getenv(legacyJSONCheckpointProcessInput)
	encoded, err := os.ReadFile(inputPath)
	if err != nil {
		test.Fatal(err)
	}
	var request legacyJSONCheckpointProcessRequest
	if err := json.Unmarshal(encoded, &request); err != nil {
		test.Fatal(err)
	}
	if request.Cursor < 0 || len(request.Checkpoint) > 10010 || len(request.Suffix) > 10000 {
		test.Fatal("child request exceeds finite fixture bounds")
	}
	scanner, err := restoreLegacyJSONValueScanner(context.Background(), request.Checkpoint)
	if err != nil {
		test.Fatal(err)
	}
	report := legacyJSONCheckpointProcessReport{
		ProcessID: os.Getpid(),
		Trace:     legacyJSONCheckpointProcessTrace(&scanner, request.Cursor, request.Suffix),
	}
	encodedReport, err := json.Marshal(report)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(inputPath+".trace", encodedReport, 0600); err != nil {
		test.Fatal(err)
	}
}

func TestLegacyJSONValueScannerCheckpointBinaryShapes(test *testing.T) {
	test.Run("malformed_wire", func(test *testing.T) {
		type malformedCheckpoint struct {
			name string
			raw  []byte
		}
		base := legacyJSONCheckpointProcessWire(legacyJSONValueIdle, 0)
		literal := legacyJSONCheckpointProcessWire(legacyJSONValueLiteral, 0)
		literal[3], literal[4] = 1, 1
		unicode := legacyJSONCheckpointProcessWire(legacyJSONValueUnicode, 0)
		unicode[5] = 4
		key := legacyJSONCheckpointProcessWire(legacyJSONValueString, 0, legacyJSONValueObjectFirst)
		key[1] = 4
		fixtures := []malformedCheckpoint{{"nil", nil}}
		appendMutation := func(name string, source []byte, offset int, value byte) {
			raw := bytes.Clone(source)
			raw[offset] = value
			fixtures = append(fixtures, malformedCheckpoint{name, raw})
		}
		for length := 0; length < 10; length++ {
			fixtures = append(fixtures, malformedCheckpoint{fmt.Sprintf("short_header_%d", length), base[:length]})
		}
		for _, version := range []byte{0, 2, 255} {
			appendMutation(fmt.Sprintf("version_%d", version), base, 0, version)
		}
		for _, status := range []byte{3, 8, 12, 16, 32, 64, 128, 255} {
			appendMutation(fmt.Sprintf("status_%d", status), base, 1, status)
		}
		for _, mode := range []byte{13, 255} {
			appendMutation(fmt.Sprintf("mode_%d", mode), base, 2, mode)
		}
		appendMutation("inactive_literal", base, 3, 1)
		appendMutation("inactive_literal_next", base, 4, 1)
		appendMutation("inactive_unicode", base, 5, 1)
		appendMutation("literal_with_unicode", literal, 5, 1)
		appendMutation("unicode_with_literal", unicode, 3, 1)
		appendMutation("unicode_with_literal_next", unicode, 4, 1)
		for _, code := range []byte{0, 4, 255} {
			appendMutation(fmt.Sprintf("literal_code_%d", code), literal, 3, code)
		}
		for _, next := range []byte{0, 4, 255} {
			appendMutation(fmt.Sprintf("true_next_%d", next), literal, 4, next)
		}
		for _, selection := range []struct{ code, next byte }{{2, 5}, {3, 4}} {
			raw := bytes.Clone(literal)
			raw[3], raw[4] = selection.code, selection.next
			fixtures = append(fixtures, malformedCheckpoint{fmt.Sprintf("finished_literal_%d", selection.code), raw})
		}
		for _, remaining := range []byte{0, 5, 255} {
			appendMutation(fmt.Sprintf("unicode_left_%d", remaining), unicode, 5, remaining)
		}
		appendMutation("key_idle", base, 1, 4)
		appendMutation("key_root", legacyJSONCheckpointProcessWire(legacyJSONValueString, 0), 1, 4)
		appendMutation("key_literal", literal, 1, 4)
		appendMutation("key_number", legacyJSONCheckpointProcessWire(legacyJSONValueInteger, 0), 1, 4)
		for _, frame := range []legacyJSONValueFrame{
			legacyJSONValueObjectColon, legacyJSONValueObjectValue, legacyJSONValueObjectAfter,
			legacyJSONValueArrayFirst, legacyJSONValueArrayValue, legacyJSONValueArrayAfter,
		} {
			appendMutation(fmt.Sprintf("key_frame_%d", frame), key, 10, byte(frame))
		}
		for _, frame := range []legacyJSONValueFrame{
			legacyJSONValueObjectFirst, legacyJSONValueObjectKey, legacyJSONValueObjectColon,
			legacyJSONValueObjectAfter, legacyJSONValueArrayAfter,
		} {
			fixtures = append(fixtures,
				malformedCheckpoint{fmt.Sprintf("ancestor_frame_%d", frame), legacyJSONCheckpointProcessWire(legacyJSONValueIdle, 0, frame, legacyJSONValueArrayFirst)},
				malformedCheckpoint{fmt.Sprintf("value_frame_%d", frame), legacyJSONCheckpointProcessWire(legacyJSONValueString, 0, frame)},
			)
		}
		for _, frame := range []legacyJSONValueFrame{8, 255} {
			fixtures = append(fixtures, malformedCheckpoint{fmt.Sprintf("unknown_frame_%d", frame), legacyJSONCheckpointProcessWire(legacyJSONValueIdle, 0, frame)})
		}
		for _, parents := range []int{10001, 65535} {
			fixtures = append(fixtures, malformedCheckpoint{fmt.Sprintf("parents_%d", parents), legacyJSONCheckpointProcessWire(legacyJSONValueIdle, parents)})
		}
		fixtures = append(fixtures,
			malformedCheckpoint{"parents_plus_depth", legacyJSONCheckpointProcessWire(legacyJSONValueIdle, 10000, legacyJSONValueArrayFirst)},
			malformedCheckpoint{"parents_plus_depth_no_wrap", legacyJSONCheckpointProcessWire(legacyJSONValueIdle, 65535, legacyJSONValueArrayFirst)},
			malformedCheckpoint{"trailing_header", append(bytes.Clone(base), 0)},
			malformedCheckpoint{"truncated_frames", legacyJSONCheckpointProcessWire(legacyJSONValueIdle, 0, legacyJSONValueArrayFirst)[:10]},
			malformedCheckpoint{"trailing_frames", append(legacyJSONCheckpointProcessWire(legacyJSONValueIdle, 0, legacyJSONValueArrayFirst), 0)},
		)
		for _, depth := range []int{10000, 10001, 65535} {
			raw := bytes.Clone(base)
			binary.BigEndian.PutUint16(raw[8:10], uint16(depth))
			fixtures = append(fixtures, malformedCheckpoint{fmt.Sprintf("depth_header_%d", depth), raw})
		}
		tooDeep := make([]legacyJSONValueFrame, 10001)
		for index := range tooDeep {
			tooDeep[index] = legacyJSONValueArrayFirst
		}
		fixtures = append(fixtures, malformedCheckpoint{"oversized_exact_frames", legacyJSONCheckpointProcessWire(legacyJSONValueIdle, 0, tooDeep...)})
		for _, status := range []byte{1, 2} {
			terminal := bytes.Clone(base)
			terminal[1] = status
			for offset := 2; offset < 10; offset++ {
				appendMutation(fmt.Sprintf("terminal_%d_field_%d", status, offset), terminal, offset, 1)
			}
			appendMutation(fmt.Sprintf("terminal_%d_key", status), terminal, 1, status|4)
			fixtures = append(fixtures, malformedCheckpoint{fmt.Sprintf("terminal_%d_trailing", status), append(terminal, 0)})
		}
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		for _, fixture := range fixtures {
			test.Run(fixture.name, func(test *testing.T) {
				if _, err := restoreLegacyJSONValueScanner(context.Background(), fixture.raw); !errors.Is(err, ErrIntegrity) {
					test.Fatalf("malformed wire accepted or wrong error: %v", err)
				}
				if _, err := restoreLegacyJSONValueScanner(canceled, fixture.raw); !errors.Is(err, context.Canceled) {
					test.Fatalf("malformed wire beat pre-cancellation: %v", err)
				}
			})
		}
	})

	test.Run("encoder_failure_and_context_priority", func(test *testing.T) {
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		var absent *legacyJSONValueScanner
		if raw, err := absent.checkpoint(context.Background()); raw != nil || !errors.Is(err, ErrIntegrity) {
			test.Fatalf("nil encoder: bytes=%x error=%v", raw, err)
		}
		if raw, err := absent.checkpoint(canceled); raw != nil || !errors.Is(err, context.Canceled) {
			test.Fatalf("nil encoder beat pre-cancellation: bytes=%x error=%v", raw, err)
		}
		seed := legacyJSONCheckpointProcessPrefix(test, `{"name":"`, 1)
		before := seed
		if raw, err := seed.checkpoint(canceled); raw != nil || !errors.Is(err, context.Canceled) || seed != before {
			test.Fatalf("canceled encoder: bytes=%x error=%v changed=%v", raw, err, seed != before)
		}
		if _, err := restoreLegacyJSONValueScanner(canceled, legacyJSONCheckpointProcessWire(legacyJSONValueIdle, 0)); !errors.Is(err, context.Canceled) {
			test.Fatalf("valid wire beat pre-cancellation: %v", err)
		}
		fixtures := []struct {
			name   string
			mutate func(*legacyJSONValueScanner)
		}{
			{"negative_parents", func(scanner *legacyJSONValueScanner) { scanner.parents = -1 }},
			{"parents_overflow", func(scanner *legacyJSONValueScanner) { scanner.parents = 10001 }},
			{"negative_depth", func(scanner *legacyJSONValueScanner) { scanner.depth = -1 }},
			{"depth_overflow", func(scanner *legacyJSONValueScanner) { scanner.depth = 10001 }},
			{"parents_plus_depth", func(scanner *legacyJSONValueScanner) { scanner.parents = 10000 }},
			{"unknown_mode", func(scanner *legacyJSONValueScanner) { scanner.mode = 255 }},
			{"unicode_zero", func(scanner *legacyJSONValueScanner) { scanner.mode, scanner.unicodeLeft = legacyJSONValueUnicode, 0 }},
			{"unicode_overflow", func(scanner *legacyJSONValueScanner) { scanner.mode, scanner.unicodeLeft = legacyJSONValueUnicode, 5 }},
			{"unknown_literal", func(scanner *legacyJSONValueScanner) {
				scanner.mode, scanner.literal, scanner.literalNext = legacyJSONValueLiteral, "TRUE", 1
			}},
			{"literal_zero", func(scanner *legacyJSONValueScanner) {
				scanner.mode, scanner.literal, scanner.literalNext = legacyJSONValueLiteral, "true", 0
			}},
			{"literal_finished", func(scanner *legacyJSONValueScanner) {
				scanner.mode, scanner.literal, scanner.literalNext = legacyJSONValueLiteral, "true", 4
			}},
			{"key_at_root", func(scanner *legacyJSONValueScanner) { scanner.depth, scanner.key = 0, true }},
			{"key_number", func(scanner *legacyJSONValueScanner) { scanner.mode, scanner.key = legacyJSONValueInteger, true }},
			{"unknown_frame", func(scanner *legacyJSONValueScanner) { scanner.stack[0] = 255 }},
			{"blocked_value_frame", func(scanner *legacyJSONValueScanner) { scanner.stack[0] = legacyJSONValueObjectColon }},
			{"blocked_ancestor", func(scanner *legacyJSONValueScanner) {
				scanner.depth, scanner.mode = 2, legacyJSONValueIdle
				scanner.stack[0], scanner.stack[1] = legacyJSONValueObjectAfter, legacyJSONValueArrayFirst
			}},
		}
		for _, fixture := range fixtures {
			test.Run(fixture.name, func(test *testing.T) {
				scanner := seed
				fixture.mutate(&scanner)
				before := scanner
				if raw, err := scanner.checkpoint(context.Background()); raw != nil || !errors.Is(err, ErrIntegrity) || scanner != before {
					test.Fatalf("failed encode: bytes=%x error=%v changed=%v", raw, err, scanner != before)
				}
				if raw, err := scanner.checkpoint(canceled); raw != nil || !errors.Is(err, context.Canceled) || scanner != before {
					test.Fatalf("invalid encoder beat pre-cancellation: bytes=%x error=%v changed=%v", raw, err, scanner != before)
				}
			})
		}
	})

	test.Run("inactive_runtime_and_owned_decode", func(test *testing.T) {
		fixtures := []struct{ name, prefix, suffix string }{
			{"idle", `[`, `null]`},
			{"string", `["prefix`, `tail"]`},
			{"escape", `["before\`, `n"]`},
			{"unicode", `["\u12`, `34"]`},
			{"literal", `[tr`, `ue]`},
			{"number", `[-1.2e+3`, `,null]`},
			{"unicode_key", `{"outer":[{"key\u00`, `41":true}]}`},
		}
		for _, fixture := range fixtures {
			test.Run(fixture.name, func(test *testing.T) {
				original := legacyJSONCheckpointProcessPrefix(test, fixture.prefix, 2)
				canonical, err := original.checkpoint(context.Background())
				if err != nil {
					test.Fatal(err)
				}
				dirty := original
				if dirty.mode != legacyJSONValueLiteral {
					dirty.literal, dirty.literalNext = "inactive-runtime-literal", 255
				}
				if dirty.mode != legacyJSONValueUnicode {
					dirty.unicodeLeft = 255
				}
				dirty.stack[dirty.depth], dirty.stack[len(dirty.stack)-1] = 255, 255
				before := dirty
				raw, err := dirty.checkpoint(context.Background())
				if err != nil || dirty != before || !bytes.Equal(raw, canonical) {
					test.Fatalf("inactive runtime state was serialized or mutated: bytes=%x want=%x error=%v changed=%v", raw, canonical, err, dirty != before)
				}
				if len(raw) != 10+dirty.depth || dirty.mode != legacyJSONValueLiteral && (raw[3] != 0 || raw[4] != 0) ||
					dirty.mode != legacyJSONValueUnicode && raw[5] != 0 {
					test.Fatalf("inactive fields or unused stack tail survived: %x", raw)
				}
				restored, err := restoreLegacyJSONValueScanner(context.Background(), raw)
				if err != nil {
					test.Fatal(err)
				}
				for index := range raw {
					raw[index] ^= 255
				}
				owned, err := restored.checkpoint(context.Background())
				if err != nil || !bytes.Equal(owned, canonical) {
					test.Fatalf("decoded scanner aliases input: bytes=%x want=%x error=%v", owned, canonical, err)
				}
				expected := legacyJSONCheckpointProcessTrace(&original, len(fixture.prefix), []byte(fixture.suffix))
				actual := legacyJSONCheckpointProcessTrace(&restored, len(fixture.prefix), []byte(fixture.suffix))
				if !reflect.DeepEqual(actual, expected) {
					test.Fatalf("owned decode trace=%+v\nuninterrupted trace=%+v", actual, expected)
				}
			})
		}
	})

	test.Run("inclusive_bounds", func(test *testing.T) {
		frames := make([]legacyJSONValueFrame, 10000)
		for index := range frames {
			frames[index] = legacyJSONValueArrayFirst
		}
		fixtures := []struct {
			name   string
			raw    []byte
			suffix []byte
		}{
			{"parents_10000", legacyJSONCheckpointProcessWire(legacyJSONValueIdle, 10000), []byte("null")},
			{"parents_plus_depth_10000", legacyJSONCheckpointProcessWire(legacyJSONValueIdle, 9999, legacyJSONValueArrayFirst), []byte("]")},
			{"depth_10000", legacyJSONCheckpointProcessWire(legacyJSONValueIdle, 0, frames...), bytes.Repeat([]byte{']'}, 10000)},
		}
		for _, fixture := range fixtures {
			test.Run(fixture.name, func(test *testing.T) {
				scanner, err := restoreLegacyJSONValueScanner(context.Background(), fixture.raw)
				if err != nil {
					test.Fatal(err)
				}
				encoded, err := scanner.checkpoint(context.Background())
				if err != nil || !bytes.Equal(encoded, fixture.raw) {
					test.Fatalf("boundary checkpoint changed: bytes=%x error=%v", encoded, err)
				}
				trace := legacyJSONCheckpointProcessTrace(&scanner, 0, fixture.suffix)
				if finish := trace[len(fixture.suffix)]; !finish.Complete || finish.Error != "" {
					test.Fatalf("boundary continuation finish=%+v", finish)
				}
			})
		}
	})

	test.Run("absorbing_canonical_and_invalid_precedence", func(test *testing.T) {
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		for _, fixture := range []struct {
			name     string
			complete bool
			invalid  bool
			status   byte
		}{
			{"complete", true, false, 1},
			{"invalid", false, true, 2},
			{"invalid_precedence", true, true, 2},
		} {
			test.Run(fixture.name, func(test *testing.T) {
				original := legacyJSONValueScanner{
					parents:     -1,
					depth:       10001,
					mode:        255,
					literal:     "discard-terminal-state",
					literalNext: 255,
					unicodeLeft: 255,
					key:         true,
					complete:    fixture.complete,
					invalid:     fixture.invalid,
				}
				original.stack[0], original.stack[len(original.stack)-1] = 255, 255
				before := original
				expectedRaw := legacyJSONCheckpointProcessWire(legacyJSONValueIdle, 0)
				expectedRaw[1] = fixture.status
				raw, err := original.checkpoint(context.Background())
				if err != nil || original != before || !bytes.Equal(raw, expectedRaw) {
					test.Fatalf("noncanonical terminal: bytes=%x want=%x error=%v changed=%v", raw, expectedRaw, err, original != before)
				}
				if canceledRaw, err := original.checkpoint(canceled); canceledRaw != nil || !errors.Is(err, context.Canceled) || original != before {
					test.Fatalf("terminal beat pre-cancellation: bytes=%x error=%v changed=%v", canceledRaw, err, original != before)
				}
				restored, err := restoreLegacyJSONValueScanner(context.Background(), raw)
				if err != nil {
					test.Fatal(err)
				}
				canonical := legacyJSONValueScanner{complete: fixture.status == 1, invalid: fixture.status == 2}
				if restored != canonical {
					test.Fatal("terminal decode retained noncanonical fields")
				}
				encoded, err := restored.checkpoint(context.Background())
				if err != nil || !bytes.Equal(encoded, expectedRaw) {
					test.Fatalf("terminal round trip: bytes=%x want=%x error=%v", encoded, expectedRaw, err)
				}
				suffix := []byte("x0[]\"")
				expected := legacyJSONCheckpointProcessTrace(&original, 19, suffix)
				actual := legacyJSONCheckpointProcessTrace(&restored, 19, suffix)
				if !reflect.DeepEqual(actual, expected) {
					test.Fatalf("terminal trace=%+v\noriginal trace=%+v", actual, expected)
				}
				for _, step := range actual {
					if step.Consumed || step.Cursor != 19 || step.Complete != (fixture.status == 1) || step.Integrity != (fixture.status == 2) ||
						fixture.status == 1 && step.Error != "" || fixture.status == 2 && step.Error != ErrIntegrity.Error() {
						test.Fatalf("terminal was not absorbing: %+v", step)
					}
				}
			})
		}
	})
}
