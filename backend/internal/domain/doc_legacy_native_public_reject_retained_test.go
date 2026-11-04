package domain

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type legacyNativePublicRejectSource struct {
	prefix  string
	suffix  string
	digits  int
	page    []byte
	loads   int
	eofZero int
	eofOne  int
}

func (fixture *legacyNativePublicRejectSource) fill(offset, length int) []byte {
	body := fixture.page[:length]
	for index := range body {
		body[index] = '9'
	}
	if offset < len(fixture.prefix) {
		copy(body, fixture.prefix[offset:])
	}
	suffixStart := len(fixture.prefix) + fixture.digits
	if offset+length > suffixStart {
		copy(body[max(0, suffixStart-offset):], fixture.suffix[max(0, offset-suffixStart):])
	}
	return body
}

func (fixture *legacyNativePublicRejectSource) read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	extent := len(fixture.prefix) + fixture.digits + len(fixture.suffix)
	if offset == extent && length == 0 {
		fixture.eofZero++
		return nil, nil
	}
	if offset == extent && length == 1 {
		fixture.eofOne++
		return nil, io.EOF
	}
	if offset < 0 || offset >= extent || offset%ConversationManifestChunkBytes != 0 ||
		length != min(ConversationManifestChunkBytes, extent-offset) {
		return nil, ErrIntegrity
	}
	fixture.loads++
	return fixture.fill(offset, length), nil
}

func legacyNativePublicRejectInput(prefix, suffix string, digits int) (*legacyNativePublicRejectSource, LegacyByteSource) {
	fixture := &legacyNativePublicRejectSource{
		prefix: prefix, suffix: suffix, digits: digits, page: make([]byte, ConversationManifestChunkBytes),
	}
	source := LegacyByteSource{Bytes: len(prefix) + digits + len(suffix), Read: fixture.read}
	for offset := 0; offset < source.Bytes; offset += ConversationManifestChunkBytes {
		body := fixture.fill(offset, min(ConversationManifestChunkBytes, source.Bytes-offset))
		source.Parts = append(source.Parts, ConversationManifestChunk{Hash: HashContent(body), Bytes: int64(len(body))})
	}
	return fixture, source
}

func legacyNativePublicRejectNumber(test *testing.T, number reflect.Value) {
	test.Helper()
	if number.FieldByName("diagnostics").Uint() != uint64(legacyNativeNumberDiagnosticsRejectOnly) {
		test.Fatal("public giant numeric wrapper did not capture reject-only diagnostics")
	}
	if !number.FieldByName("errorText").IsNil() || !number.FieldByName("semantic").IsNil() {
		test.Fatal("public giant numeric wrapper allocated a diagnostic builder or raw type error")
	}
}

func legacyNativePublicRejectInspect(test *testing.T, normalizer *LegacyNormalizationDecoder, numbers map[uintptr]reflect.Value) {
	test.Helper()
	seen := make(map[uintptr]bool)
	var storage int64
	var visit func(reflect.Value)
	visit = func(value reflect.Value) {
		if !value.IsValid() || value.Type() == reflect.TypeFor[reflect.Value]() || value.Type() == reflect.TypeFor[reflect.Type]() {
			return
		}
		switch value.Kind() {
		case reflect.Interface:
			if !value.IsNil() {
				visit(value.Elem())
			}
		case reflect.Pointer:
			if value.IsNil() || seen[value.Pointer()] {
				return
			}
			seen[value.Pointer()] = true
			storage += int64(value.Type().Elem().Size())
			visit(value.Elem())
		case reflect.Struct:
			if value.Type() == reflect.TypeFor[legacyNativeNumberValue]() {
				legacyNativePublicRejectNumber(test, value)
				numbers[value.Addr().Pointer()] = value
				if len(numbers) > 1 {
					test.Fatal("one-token public fixture retained multiple numeric wrappers")
				}
			}
			for fieldIndex := 0; fieldIndex < value.NumField(); fieldIndex++ {
				visit(value.Field(fieldIndex))
			}
		case reflect.Array:
			if value.Type().Elem().Kind() != reflect.Uint8 {
				for index := 0; index < value.Len(); index++ {
					visit(value.Index(index))
				}
			}
		case reflect.Slice:
			backingBytes := int64(value.Cap()) * int64(value.Type().Elem().Size())
			if backingBytes > 16<<20 {
				test.Fatal("public rejection retained oversized slice backing", value.Type(), backingBytes)
			}
			storage += backingBytes
			if value.Type().Elem().Kind() != reflect.Uint8 {
				backing := value.Slice(0, value.Cap())
				for index := 0; index < backing.Len(); index++ {
					visit(backing.Index(index))
				}
			}
		case reflect.Map:
			entries := value.MapRange()
			for entries.Next() {
				visit(entries.Key())
				visit(entries.Value())
			}
		case reflect.String:
			text := value.String()
			if len(text) > canonicalCheckBytes || strings.HasPrefix(text, "number ") {
				test.Fatal("public rejection retained a raw numeric diagnostic string", len(text))
			}
			storage += int64(len(text))
		}
		if storage > 16<<20 {
			test.Fatal("directly reachable public rejection storage exceeded the parked bound", storage)
		}
	}
	visit(reflect.ValueOf(normalizer))
	for _, number := range numbers {
		legacyNativePublicRejectNumber(test, number)
	}
}

func TestLegacyNativePublicRejectRetainedGiantOverflow(test *testing.T) {
	const digits = (16 << 20) + 17
	for _, fixture := range []struct {
		name   string
		prefix string
		suffix string
	}{
		{name: "scalar-output", prefix: `{"kind":"tool_result","output":1e+`, suffix: `,"output":null}`},
		{name: "input-map-array", prefix: `{"kind":"tool_call","input":{"number":[1e+`, suffix: `]},"input":null}`},
		{name: "output-array-map", prefix: `{"kind":"tool_result","output":[{"number":1e+`, suffix: `}],"output":[]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			prefix := `{"envelope":{"cir_version":"2"},"events":[` + fixture.prefix
			suffix := fixture.suffix + `],"events":null}`
			buffer, source := legacyNativePublicRejectInput(prefix, suffix, digits)
			normalizer, err := NewLegacyNormalizationDecoder(context.Background(), source)
			if err != nil || normalizer == nil || buffer.loads != 0 || source.Bytes <= 16<<20 {
				test.Fatal("virtual giant public fixture failed or read eagerly", source.Bytes, buffer.loads, err)
			}
			numbers := make(map[uintptr]reflect.Value)
			legacyNativePublicRejectInspect(test, normalizer, numbers)
			limit := 64 + 32*len(source.Parts)
			for stepIndex := 0; stepIndex < limit; stepIndex++ {
				before := buffer.loads
				step, stepErr := normalizer.Step(context.Background())
				if buffer.loads-before > 2 || step.Complete || len(step.Events) != 0 || len(step.Envelope) != 0 || step.Count != 0 {
					test.Fatal("public overflow exceeded source loads or exposed usable output", stepIndex, buffer.loads-before)
				}
				legacyNativePublicRejectInspect(test, normalizer, numbers)
				if retained, retainedErr := normalizer.RetainedBytes(); retainedErr != nil || retained <= 0 || retained > 16<<20 {
					test.Fatal("public giant rejection exceeded its reported parked bound", retained, retainedErr)
				}
				if stepErr == nil {
					continue
				}
				var diagnostic *json.UnmarshalTypeError
				if !errors.Is(stepErr, ErrIntegrity) || errors.Is(stepErr, ErrPreparedDocRangeObject) || errors.As(stepErr, &diagnostic) {
					test.Fatal("public giant overflow changed its rejection category", stepErr)
				}
				if buffer.eofZero != 1 || buffer.eofOne != 1 || normalizer.rejectPart != len(source.Parts) || len(numbers) != 1 {
					test.Fatal("public overflow skipped original source authentication, EOF, or the native wrapper", buffer.eofZero, buffer.eofOne, normalizer.rejectPart, len(numbers))
				}
				for _, number := range numbers {
					parser := number.FieldByName("parser").Elem()
					if !number.FieldByName("converted").Bool() || !number.FieldByName("rejected").Bool() || !number.FieldByName("complete").Bool() ||
						parser.FieldByName("position").Int() != parser.FieldByName("end").Int() ||
						parser.FieldByName("end").Int()-parser.FieldByName("from").Int() != int64(digits+len("1e+")) {
						test.Fatal("public rejection did not finish the real giant numeric span")
					}
				}
				before = buffer.loads
				sticky, stickyErr := normalizer.Step(context.Background())
				if !errors.Is(stickyErr, ErrIntegrity) || !reflect.DeepEqual(sticky, LegacyNormalizationStep{}) || buffer.loads != before || buffer.eofZero != 1 || buffer.eofOne != 1 {
					test.Fatal("public giant rejection was not terminal and atomic")
				}
				legacyNativePublicRejectInspect(test, normalizer, numbers)
				return
			}
			test.Fatal("public giant rejection exhausted its finite per-part step allowance")
		})
	}
}

func TestLegacyNativePrivateNumberFullDiagnosticsRetained(test *testing.T) {
	if legacyNativeNumberDiagnosticsFull != 0 {
		test.Fatal("private zero-value numeric diagnostics no longer select full text")
	}
	for _, length := range []int{LegacyCanonicalEncodingBytes + 17, 2*ConversationManifestChunkBytes + 17} {
		for _, fixture := range []struct {
			name   string
			kind   string
			field  string
			prefix string
			suffix string
		}{
			{name: "scalar-output", kind: "tool_result", field: "output", prefix: `{"output":`, suffix: `}`},
			{name: "input-map-array", kind: "tool_call", field: "input", prefix: `{"input":{"number":[false,`, suffix: `],"tail":true}}`},
			{name: "output-array-map", kind: "tool_result", field: "output", prefix: `{"output":[false,{"number":`, suffix: `},true]}`},
		} {
			test.Run(fixture.name+"-"+strconv.Itoa(length), func(test *testing.T) {
				token := "-1e+" + strings.Repeat("9", length-len("-1e+"))
				fragment := []byte(fixture.prefix + token + fixture.suffix)
				var expected wire
				expectedErr := json.Unmarshal(fragment, &expected)
				var expectedDiagnostic *json.UnmarshalTypeError
				if !errors.As(expectedErr, &expectedDiagnostic) || expectedDiagnostic.Type != reflect.TypeFor[float64]() ||
					expectedDiagnostic.Struct != "wire" || expectedDiagnostic.Field != fixture.field ||
					len(expectedDiagnostic.Value) != len("number ")+length || !strings.HasPrefix(expectedDiagnostic.Value, "number ") ||
					expectedDiagnostic.Value[len("number "):] != token {
					test.Fatal("bounded private oracle lost its complete numeric diagnostic")
				}
				rawPrefix := `{"kind":"` + fixture.kind + `","seq":7,"call_id":"preceding-fields",` + fixture.prefix[1:]
				if expectedDiagnostic.Offset >= int64(len(rawPrefix)+length) || expectedDiagnostic.Offset <= int64(length) {
					test.Fatal("private fixture no longer distinguishes field-fragment and physical source offsets", expectedDiagnostic.Offset)
				}
				raw := []byte(rawPrefix + token + fixture.suffix)
				_, _, actualErr := Float64IntegrationNative(test, raw)
				var actualDiagnostic *json.UnmarshalTypeError
				if !errors.As(actualErr, &actualDiagnostic) {
					test.Fatalf("private native overflow lost its full diagnostic: %T", actualErr)
				}
				if !reflect.DeepEqual(actualDiagnostic, expectedDiagnostic) {
					test.Fatal("private native overflow changed Value, Type, Struct, Field, or Offset", len(actualDiagnostic.Value), len(expectedDiagnostic.Value), actualDiagnostic.Offset, expectedDiagnostic.Offset)
				}
			})
		}
	}
}

func TestLegacyNativePublicRejectDiagnosticAllocationRegression(test *testing.T) {
	buffer, source := legacyNativePublicRejectInput(`{"envelope":{"cir_version":"2"},"events":[{"kind":"tool_result","output":1e+`, `}]}`, 2*ConversationManifestChunkBytes+17)
	normalizer, err := NewLegacyNormalizationDecoder(context.Background(), source)
	if err != nil {
		test.Fatal(err)
	}
	observedNumber := false
	for range 128 {
		_, err = normalizer.Step(context.Background())
		if normalizer.nativeCache != nil && normalizer.nativeCache.object.number != nil {
			observedNumber = true
			if normalizer.nativeCache.object.number.errorText != nil {
				test.Fatal("public normalization allocated an unused giant native diagnostic builder")
			}
		}
		if err != nil {
			if !observedNumber || !errors.Is(err, ErrIntegrity) || buffer.eofZero != 1 || buffer.eofOne != 1 {
				test.Fatal("diagnostic allocation regression did not complete actual authenticated rejection", observedNumber, err)
			}
			return
		}
	}
	test.Fatal("diagnostic allocation regression exhausted bounded progress")
}
