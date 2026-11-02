package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeTypedNumberCompatState struct {
	provider         bool
	direct           ProviderMetadata
	event            wire
	originalMetadata *ProviderMetadata
	originalTime     *json.Number
}

func newLegacyNativeTypedNumberCompatState(provider bool, seed string) *legacyNativeTypedNumberCompatState {
	state := &legacyNativeTypedNumberCompatState{provider: provider}
	metadata := &state.direct
	if provider {
		metadata = nil
		if seed != "nil-metadata" {
			metadata = &ProviderMetadata{}
		}
		state.event.ProviderMetadata = metadata
	}
	if metadata != nil && seed != "nil-metadata" {
		metadata.TurnID = "kept-turn"
		if seed == "seeded" {
			number := json.Number("123.450e-8")
			metadata.CreateTime = &number
		}
	}
	state.originalMetadata = metadata
	if metadata != nil {
		state.originalTime = metadata.CreateTime
	}
	return state
}

func (state *legacyNativeTypedNumberCompatState) destination() any {
	if state.provider {
		return &state.event
	}
	return &state.direct
}

func (state *legacyNativeTypedNumberCompatState) metadata() *ProviderMetadata {
	if state.provider {
		return state.event.ProviderMetadata
	}
	return &state.direct
}

func legacyNativeTypedNumberCompatStateEqual(test *testing.T, actual, expected *legacyNativeTypedNumberCompatState) {
	test.Helper()
	if !reflect.DeepEqual(actual.destination(), expected.destination()) ||
		!reflect.DeepEqual(actual.originalMetadata, expected.originalMetadata) ||
		!reflect.DeepEqual(actual.originalTime, expected.originalTime) {
		test.Error("typed Number changed native values, partial writes, or detached original pointees")
	}
	actualMetadata, expectedMetadata := actual.metadata(), expected.metadata()
	if (actualMetadata == actual.originalMetadata) != (expectedMetadata == expected.originalMetadata) {
		test.Error("typed Number changed the native metadata pointer identity")
	}
	if actualMetadata != nil && expectedMetadata != nil &&
		(actualMetadata.CreateTime == actual.originalTime) != (expectedMetadata.CreateTime == expected.originalTime) {
		test.Error("typed Number replaced or retained the CreateTime pointer differently from encoding/json")
	}
}

type legacyNativeTypedNumberCompatDecoder struct {
	native *legacyNativeObject
	frame  legacyJSONValueScanner
	buffer *legacySourceProofBuffer
	source LegacyByteSource
}

func newLegacyNativeTypedNumberCompatDecoder(test *testing.T, raw []byte, destination any) *legacyNativeTypedNumberCompatDecoder {
	test.Helper()
	metadata := newLegacyNativeObjectFields(reflect.TypeOf(destination).Elem())
	native, err := newLegacyNativeObject(reflect.ValueOf(destination), &metadata, 1, false)
	if err != nil {
		test.Fatal("typed Number object construction failed", err)
	}
	if metadata.model == reflect.TypeFor[ProviderMetadata]() {
		native.deferTypeErrors = true
	}
	buffer := &legacySourceProofBuffer{body: raw}
	decoder := &legacyNativeTypedNumberCompatDecoder{native: native, buffer: buffer, source: buffer.input()}
	native.start(0, len(raw))
	if len(buffer.reads) != 0 {
		test.Fatal("typed Number construction eagerly read the source")
	}
	return decoder
}

func (decoder *legacyNativeTypedNumberCompatDecoder) step(test *testing.T, ctx context.Context, remaining, fields int) (bool, error) {
	test.Helper()
	initialWork, reads := ConversationManifestChunkBytes-remaining, len(decoder.buffer.reads)
	input := &legacyNormalizationInput{ctx: ctx, source: decoder.source, workBytes: initialWork, fields: fields}
	complete, err := decoder.native.step(ctx, input, &decoder.frame)
	if input.workBytes < initialWork || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded ||
		input.fields < fields || input.fields > decoder.native.fieldActionLimit() {
		test.Fatal("typed Number escaped or reset the owner's shared byte, load, or field quantum", initialWork, input.workBytes, input.loads, fields, input.fields)
	}
	if len(decoder.buffer.reads)-reads != input.loads || input.source.authority != decoder.source.authority ||
		input.source.Bytes != decoder.source.Bytes || !reflect.DeepEqual(input.source.Parts, decoder.source.Parts) ||
		reflect.ValueOf(input.source.Read).Pointer() != reflect.ValueOf(decoder.source.Read).Pointer() {
		test.Fatal("typed Number replaced its source identity or bypassed the shared source loads")
	}
	for _, read := range decoder.buffer.reads[reads:] {
		if read[0] < 0 || read[0] >= decoder.source.Bytes || read[0]%ConversationManifestChunkBytes != 0 ||
			read[1] != min(ConversationManifestChunkBytes, decoder.source.Bytes-read[0]) {
			test.Fatal("typed Number bypassed authenticated source-part reads", read)
		}
	}
	objects := append([]*legacyNativeObject{decoder.native}, decoder.native.nested...)
	for _, object := range objects {
		if object != nil && (len(object.raw) > object.metadata.rawBytes || cap(object.raw) > object.metadata.rawBytes) {
			test.Fatal("typed Number expanded bounded object-field scratch beyond its metadata allowance")
		}
	}
	for _, page := range input.pages {
		clear(page)
	}
	if input.failure != nil {
		return complete, input.failure
	}
	return complete, err
}

func (decoder *legacyNativeTypedNumberCompatDecoder) drain(test *testing.T) error {
	test.Helper()
	for range 512 {
		complete, err := decoder.step(test, context.Background(), ConversationManifestChunkBytes, 0)
		if err != nil || complete {
			if complete && (decoder.native.position != decoder.native.end || decoder.native.presence != 0) {
				test.Fatal("typed Number completed outside its selected span or enabled presence tracking")
			}
			return err
		}
	}
	test.Fatal("typed Number exhausted its local finite scan, decode, and diagnostic drain")
	return ErrIntegrity
}

func legacyNativeTypedNumberCompatErrors(test *testing.T, actualErr, fullErr, fragmentErr error, provider bool, invalidToken string) {
	test.Helper()
	if reflect.TypeOf(actualErr) != reflect.TypeOf(fullErr) || reflect.TypeOf(fragmentErr) != reflect.TypeOf(fullErr) {
		test.Errorf("typed Number changed native error precedence or type: got=%T full=%T fragment=%T", actualErr, fullErr, fragmentErr)
		return
	}
	if fullErr == nil {
		return
	}
	var fullType, fragmentType *json.UnmarshalTypeError
	if errors.As(fullErr, &fullType) {
		if !errors.As(fragmentErr, &fragmentType) {
			test.Fatal("recoverable full and field-fragment oracles disagree")
		}
		field := fragmentType.Field
		if provider {
			field = "provider_metadata." + field
		}
		if fullType.Value != fragmentType.Value || fullType.Type != fragmentType.Type || fullType.Struct != fragmentType.Struct ||
			fullType.Field != field || fullType.Offset < fragmentType.Offset {
			test.Fatal("full oracle lost its nested field context or absolute error offset")
		}
		if !reflect.DeepEqual(actualErr, fragmentErr) {
			test.Errorf("typed Number changed field-fragment diagnostics: got=%v want=%v", actualErr, fragmentErr)
		}
		return
	}
	if invalidToken == "" {
		test.Fatal("immediate invalid Number fixture has no original raw token")
	}
	want := fmt.Sprintf("json: invalid number literal, trying to unmarshal %q into Number", []byte(invalidToken))
	if fullErr.Error() != want || fragmentErr.Error() != want || actualErr.Error() != want {
		test.Error("immediate invalid Number error must quote the original raw token, not decoded or normalized text")
	}
}

func legacyNativeTypedNumberCompatParity(test *testing.T, raw, fragment, invalidToken string, provider bool, seed string) {
	test.Helper()
	actual := newLegacyNativeTypedNumberCompatState(provider, seed)
	expected := newLegacyNativeTypedNumberCompatState(provider, seed)
	if actual.originalMetadata != nil && actual.originalMetadata == expected.originalMetadata ||
		actual.originalTime != nil && actual.originalTime == expected.originalTime {
		test.Fatal("typed Number destination shares mutable pointees with its independent oracle")
	}
	expectedErr := json.Unmarshal([]byte(raw), expected.destination())
	if (expectedErr != nil) != (fragment != "") {
		test.Fatal("typed Number fixture changed its Go encoding/json acceptance", reflect.TypeOf(expectedErr))
	}
	if invalidToken != "" && (expected.metadata() == nil || expected.metadata().CreateTime == nil) {
		test.Fatal("invalid Number oracle no longer allocates a missing metadata or CreateTime pointer before rejecting")
	}
	var fragmentErr error
	if fragment != "" {
		fragmentState := newLegacyNativeTypedNumberCompatState(false, seed)
		fragmentErr = json.Unmarshal([]byte(fragment), fragmentState.destination())
	}
	decoder := newLegacyNativeTypedNumberCompatDecoder(test, []byte(raw), actual.destination())
	actualErr := decoder.drain(test)
	legacyNativeTypedNumberCompatErrors(test, actualErr, expectedErr, fragmentErr, provider, invalidToken)
	legacyNativeTypedNumberCompatStateEqual(test, actual, expected)
}

func legacyNativeTypedNumberCompatRoutes(test *testing.T, object, fragment, invalidToken string) {
	test.Helper()
	for _, route := range []struct {
		name     string
		provider bool
	}{
		{name: "native-object"},
		{name: "provider-child", provider: true},
	} {
		test.Run(route.name, func(test *testing.T) {
			raw := object
			if route.provider {
				raw = `{"provider_metadata":` + object + `}`
			}
			for _, seed := range []string{"seeded", "nil-number", "nil-metadata"} {
				test.Run(seed, func(test *testing.T) {
					legacyNativeTypedNumberCompatParity(test, raw, fragment, invalidToken, route.provider, seed)
				})
			}
		})
	}
}

func TestLegacyNativeTypedNumberCompatLargeSpelling(test *testing.T) {
	zeroes := strings.Repeat("0", LegacyCanonicalEncodingBytes+17)
	for _, fixture := range []struct {
		name  string
		token string
	}{
		{name: "raw-negative-zero-fraction", token: "-0." + zeroes + "E+0003"},
		{name: "raw-integer-beyond-float64", token: "1" + zeroes},
		{name: "raw-exponent-beyond-float64", token: "1E+" + strings.Repeat("9", LegacyCanonicalEncodingBytes+17)},
		{name: "raw-with-external-whitespace", token: " \t-0." + zeroes + "e-0003\r\n"},
		{name: "quoted-fraction", token: `"-0.` + zeroes + `E+0003"`},
		{name: "quoted-escaped-fraction", token: `"\u002d0\u002e` + strings.Repeat(`\u0030`, LegacyCanonicalEncodingBytes+17) + `\u0045\u002b0003"`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			if len(fixture.token) <= LegacyCanonicalEncodingBytes {
				test.Fatal("typed Number fixture did not cross the helper threshold")
			}
			legacyNativeTypedNumberCompatRoutes(test, `{"create_time":`+fixture.token+`,"turn_id":"written"}`, "", "")
		})
	}
}

func TestLegacyNativeTypedNumberCompatInvalidGrammarAndRawErrors(test *testing.T) {
	digits := strings.Repeat("1", LegacyCanonicalEncodingBytes+17)
	for _, fixture := range []struct {
		name  string
		token string
	}{
		{name: "empty", token: `""`},
		{name: "whitespace-only", token: `"` + strings.Repeat(" ", LegacyCanonicalEncodingBytes+17) + `"`},
		{name: "leading-whitespace", token: `" ` + digits + `"`},
		{name: "trailing-whitespace", token: `"` + digits + ` "`},
		{name: "escaped-whitespace", token: `"\u0020` + digits + `"`},
		{name: "leading-plus", token: `"+` + digits + `"`},
		{name: "leading-zero", token: `"0` + digits + `"`},
		{name: "negative-leading-zero", token: `"-0` + digits + `"`},
		{name: "missing-integer", token: `".` + digits + `"`},
		{name: "missing-fraction", token: `"` + digits + `."`},
		{name: "missing-exponent", token: `"` + digits + `e"`},
		{name: "exponent-sign-only", token: `"` + digits + `E+"`},
		{name: "minus-only", token: `"-"`},
		{name: "nan", token: `"NaN"`},
		{name: "infinity", token: `"Infinity"`},
		{name: "escaped-leading-zero", token: `"\u0030` + strings.Repeat(`\u0031`, LegacyCanonicalEncodingBytes+17) + `"`},
		{name: "escaped-invalid-suffix", token: `"` + digits + `\u0078"`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			fragment := `{"create_time":` + fixture.token + `}`
			object := `{"turn_id":"written-before","create_time":` + fixture.token + `,"turn_id":"not-written","create_time":17}`
			legacyNativeTypedNumberCompatRoutes(test, object, fragment, fixture.token)
		})
	}
}

func TestLegacyNativeTypedNumberCompatDuplicateNullAndErrorOrder(test *testing.T) {
	token := `"-0.` + strings.Repeat("0", LegacyCanonicalEncodingBytes+17) + `E+0003"`
	invalid := `"\u0030` + strings.Repeat(`\u0031`, LegacyCanonicalEncodingBytes+17) + `"`
	for _, fixture := range []struct {
		name         string
		object       string
		fragment     string
		invalidToken string
	}{
		{name: "folded-and-escaped-duplicates", object: `{"create_time":` + token + `,"CREATE_TIME":"1.2300","create_\u0074ime":"-0E+00"}`},
		{name: "null-clears-number-only", object: `{"create_time":` + token + `,"create_time":null,"turn_id":null}`},
		{name: "null-then-large-number", object: `{"create_time":null,"CREATE_TIME":` + token + `}`},
		{name: "large-null-small-keeps-detached-old-value", object: `{"create_time":` + token + `,"create_time":null,"create_time":"1.2500"}`},
		{name: "recoverable-error-allows-large-number-and-sibling-writes", object: `{"create_time":false,"create_time":` + token + `,"turn_id":"written"}`, fragment: `{"create_time":false}`},
		{name: "recoverable-error-after-large-number-keeps-value", object: `{"create_time":` + token + `,"create_time":false,"turn_id":"written"}`, fragment: `{"create_time":false}`},
		{name: "immediate-number-error-outranks-earlier-sibling-type-error", object: `{"turn_id":7,"create_time":` + invalid + `,"turn_id":"not-written"}`, fragment: `{"create_time":` + invalid + `}`, invalidToken: invalid},
		{name: "immediate-number-error-outranks-earlier-number-type-error", object: `{"create_time":false,"turn_id":"written-before","create_time":` + invalid + `,"turn_id":"not-written"}`, fragment: `{"create_time":` + invalid + `}`, invalidToken: invalid},
		{name: "large-value-before-invalid-is-retained", object: `{"create_time":` + token + `,"create_time":` + invalid + `,"turn_id":"not-written"}`, fragment: `{"create_time":` + invalid + `}`, invalidToken: invalid},
		{name: "null-before-invalid-still-allocates-number", object: `{"create_time":null,"create_time":` + invalid + `,"create_time":null}`, fragment: `{"create_time":` + invalid + `}`, invalidToken: invalid},
		{name: "invalid-is-not-repaired-by-null", object: `{"create_time":` + invalid + `,"create_time":null}`, fragment: `{"create_time":` + invalid + `}`, invalidToken: invalid},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			legacyNativeTypedNumberCompatRoutes(test, fixture.object, fixture.fragment, fixture.invalidToken)
		})
	}
	for _, fixture := range []struct {
		name         string
		raw          string
		fragment     string
		invalidToken string
	}{
		{name: "outer-duplicates-retain-metadata-and-time-pointers", raw: `{"provider_metadata":{"create_time":` + token + `},"PROVIDER_METADATA":{},"provider_\u006detadata":{"turn_id":"written"}}`},
		{name: "outer-null-reattaches-new-metadata", raw: `{"provider_metadata":{"create_time":` + token + `},"provider_metadata":null,"provider_metadata":{"create_time":"1.2500"}}`},
		{name: "outer-null-leaves-old-number-detached", raw: `{"provider_metadata":{"create_time":` + token + `},"provider_metadata":null,"provider_metadata":{}}`},
		{name: "outer-null-cannot-repair-immediate-invalid-number", raw: `{"provider_metadata":{"turn_id":"written-before","create_time":` + invalid + `},"provider_metadata":null,"provider_metadata":{"create_time":17}}`, fragment: `{"create_time":` + invalid + `}`, invalidToken: invalid},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			legacyNativeTypedNumberCompatParity(test, fixture.raw, fixture.fragment, fixture.invalidToken, true, "seeded")
		})
	}
}

func TestLegacyNativeTypedNumberCompatSourceReuseAndSharedQuantum(test *testing.T) {
	tokens := []string{
		"-0." + strings.Repeat("0", LegacyCanonicalEncodingBytes+17) + "E+0003",
		`"\u0031\u002e` + strings.Repeat(`\u0030`, LegacyCanonicalEncodingBytes+17) + `\u0031"`,
	}
	for _, provider := range []bool{false, true} {
		test.Run(fmt.Sprintf("provider-%t", provider), func(test *testing.T) {
			objects := make([]string, 0, len(tokens))
			for _, token := range tokens {
				object := `{"create_time":` + token + `,"turn_id":"written"}`
				if provider {
					object = `{"provider_metadata":` + object + `}`
				}
				objects = append(objects, object)
			}
			actual := newLegacyNativeTypedNumberCompatState(provider, "seeded")
			expected := newLegacyNativeTypedNumberCompatState(provider, "seeded")
			prefix := strings.Repeat(" ", ConversationManifestChunkBytes-1)
			decoder := newLegacyNativeTypedNumberCompatDecoder(test, []byte(prefix+strings.Join(objects, "")), actual.destination())
			authority, extent := decoder.source.authority, decoder.source.Bytes
			parts := append([]ConversationManifestChunk(nil), decoder.source.Parts...)
			from := len(prefix)
			var retained []string
			for ordinal, object := range objects {
				decoder.native.start(from, from+len(object))
				decoder.frame = legacyJSONValueScanner{}
				metadata, pointer := actual.metadata(), actual.metadata().CreateTime
				prior := *pointer
				for range 16 {
					complete, err := decoder.step(test, context.Background(), 4096, 1)
					if complete || err != nil || actual.metadata() != metadata || metadata.CreateTime != pointer || *pointer != prior {
						test.Fatal("partial shared quanta published a Number before its complete token was available", ordinal, complete, err)
					}
				}
				position, reads := decoder.native.progressPosition(), len(decoder.buffer.reads)
				complete, err := decoder.step(test, context.Background(), 0, 1)
				if complete || err != nil || decoder.native.progressPosition() != position || len(decoder.buffer.reads) != reads || *pointer != prior {
					test.Fatal("exhausted shared quantum performed reads, progress, or pointee writes")
				}
				complete, err = decoder.step(test, context.Background(), ConversationManifestChunkBytes, decoder.native.fieldActionLimit())
				if complete || err != nil || decoder.native.progressPosition() != position || len(decoder.buffer.reads) != reads || *pointer != prior {
					test.Fatal("exhausted shared field quantum performed reads, progress, or pointee writes")
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				complete, err = decoder.step(test, ctx, ConversationManifestChunkBytes, 0)
				if complete || !errors.Is(err, context.Canceled) || decoder.native.progressPosition() != position || len(decoder.buffer.reads) != reads || *pointer != prior {
					test.Fatal("canceled typed Number resume read source or changed the old value", err)
				}
				if err := json.Unmarshal([]byte(object), expected.destination()); err != nil {
					test.Fatal("source reuse oracle rejected a valid Number", err)
				}
				if err := decoder.drain(test); err != nil {
					test.Fatal("typed Number failed its bounded drain after more than sixteen partial steps", err)
				}
				legacyNativeTypedNumberCompatStateEqual(test, actual, expected)
				retained = append(retained, actual.metadata().CreateTime.String())
				for previous, text := range retained {
					var number json.Number
					if err := json.Unmarshal([]byte(tokens[previous]), &number); err != nil || text != number.String() {
						test.Fatal("source-page reuse or replacement mutated an earlier immutable Number spelling", previous, err)
					}
				}
				if decoder.source.authority != authority || decoder.source.Bytes != extent || !reflect.DeepEqual(decoder.source.Parts, parts) {
					test.Fatal("native object reuse minted a new source identity or replaced the authenticated part plan")
				}
				from += len(object)
			}
		})
	}
}
