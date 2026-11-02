package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeWrongNumberAlias string

type legacyNativeWrongNumberOwner struct {
	Before string                       `json:"before"`
	Plain  string                       `json:"plain"`
	Alias  legacyNativeWrongNumberAlias `json:"alias"`
	After  string                       `json:"after"`
}

func legacyNativeWrongNumberStart(test *testing.T, destination *legacyNativeWrongNumberOwner, name, token string) (*legacyNativeObject, *legacyNormalizationInput) {
	test.Helper()
	metadata := newLegacyNativeObjectFields(reflect.TypeFor[legacyNativeWrongNumberOwner]())
	native, err := newLegacyNativeObject(reflect.ValueOf(destination), &metadata, 1, false)
	if err != nil {
		test.Fatal("wrong-number fixture could not construct its native owner", err)
	}
	native.field = metadata.match(name)
	if native.field < 0 {
		test.Fatal("wrong-number fixture selected an unknown field", name)
	}
	field := metadata.fields[native.field]
	const before = `{"before":"earlier",`
	raw := []byte(before + field.prefix[1:] + token + `,"after":"later"}`)
	buffer := &legacySourceProofBuffer{body: raw}
	source := buffer.input()
	source.Read = func(context.Context, int, int) ([]byte, error) {
		test.Fatal("already-framed wrong-number token was unnecessarily reread")
		return nil, ErrIntegrity
	}
	native.valueStart = len(before) + len(field.prefix) - 1
	native.valueEnd = native.valueStart + len(token)
	native.position, native.end = native.valueEnd, len(raw)
	native.phase, native.valueFirst = legacyNativeObjectApply, token[0]
	return native, &legacyNormalizationInput{ctx: context.Background(), source: source}
}

func TestLegacyNativeWrongNumberFieldSelection(test *testing.T) {
	type selection struct {
		Plain       string                        `json:"plain"`
		Alias       legacyNativeWrongNumberAlias  `json:"alias"`
		JSON        legacyNativeMetadataJSONAlias `json:"json"`
		Text        legacyNativeMetadataTextAlias `json:"text"`
		Number      json.Number                   `json:"number"`
		Quoted      string                        `json:"quoted,string"`
		QuotedAlias legacyNativeWrongNumberAlias  `json:"quoted_alias,string"`
		Interface   any                           `json:"interface"`
		Pointer     *string                       `json:"pointer"`
	}
	metadata := newLegacyNativeObjectFields(reflect.TypeFor[selection]())
	if metadata.err != nil {
		test.Fatal(metadata.err)
	}
	native := &legacyNativeObject{metadata: &metadata, valueStart: 37}
	for _, first := range []byte{'-', '0', '1', '9', '"', 'n', 't', '[', '{'} {
		for _, length := range []int{LegacyCanonicalEncodingBytes - 1, LegacyCanonicalEncodingBytes, LegacyCanonicalEncodingBytes + 1} {
			native.valueFirst, native.valueEnd = first, native.valueStart+length
			for _, field := range metadata.fields {
				expected := (field.name == "plain" || field.name == "alias") && length > LegacyCanonicalEncodingBytes &&
					(first == '-' || first >= '0' && first <= '9')
				if native.wrongNumberField(field) != expected {
					test.Fatalf("wrong-number selection changed: field=%s first=%q length=%d want=%t", field.name, first, length, expected)
				}
			}
		}
	}
}

func TestLegacyNativeWrongNumberNativeOracleAndBoundedState(test *testing.T) {
	for _, token := range []string{
		"1" + strings.Repeat("0", LegacyCanonicalEncodingBytes),
		"-0." + strings.Repeat("0", LegacyCanonicalEncodingBytes+17),
		"1e+" + strings.Repeat("9", 2*ConversationManifestChunkBytes+17),
	} {
		for _, name := range []string{"plain", "alias"} {
			for _, prior := range []string{"", "kept"} {
				expected := legacyNativeWrongNumberOwner{Before: "earlier", Plain: prior, Alias: legacyNativeWrongNumberAlias(prior), After: "later"}
				actual := expected
				native, input := legacyNativeWrongNumberStart(test, &actual, name, token)
				field := native.metadata.fields[native.field]
				expectedErr := json.Unmarshal([]byte(field.prefix+token+"}"), &expected)
				var expectedTypeError *json.UnmarshalTypeError
				if !errors.As(expectedErr, &expectedTypeError) || expectedTypeError.Value != "number" ||
					expectedTypeError.Offset != int64(len(field.prefix)+len(token)) || expectedTypeError.Offset == int64(native.valueEnd) {
					test.Fatal("native wrong-number oracle changed its bounded value or exact token-end offset", expectedErr)
				}
				input.workBytes, input.loads, input.fields = ConversationManifestChunkBytes, 2, 7
				input.semanticBytes = 11
				before := *native
				complete, actualErr := native.decodeWrongNumber(context.Background(), input)
				if !complete || !reflect.DeepEqual(actualErr, expectedErr) || actual != expected {
					test.Fatalf("wrong-number helper differs from native field-fragment semantics: field=%s length=%d complete=%t got=%#v want=%#v", name, len(token), complete, actualErr, expectedErr)
				}
				if !reflect.DeepEqual(*native, before) || input.unbounded || input.workBytes != ConversationManifestChunkBytes ||
					input.loads != 2 || input.fields != 7 || input.semanticBytes != 11 {
					test.Fatal("wrong-number rejection changed the owner, allocated token state, or reset shared budgets")
				}
			}
		}
	}
}

func TestLegacyNativeWrongNumberCancellationAndFraming(test *testing.T) {
	token := "1" + strings.Repeat("0", LegacyCanonicalEncodingBytes)
	actual := legacyNativeWrongNumberOwner{Plain: "kept"}
	native, input := legacyNativeWrongNumberStart(test, &actual, "plain", token)
	before := *native
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input.failure = ErrPreparedDocRangeObject
	complete, err := native.decodeWrongNumber(ctx, input)
	if complete || !errors.Is(err, context.Canceled) || !reflect.DeepEqual(*native, before) || actual.Plain != "kept" {
		test.Fatal("cancellation did not precede wrong-number rejection and source failure", complete, err)
	}
	complete, err = native.decodeWrongNumber(context.Background(), input)
	if complete || err != input.failure || native.typeError != nil || actual.Plain != "kept" {
		test.Fatal("wrong-number rejection hid an existing authenticated-source failure", complete, err)
	}
	input.failure = nil
	native.position--
	complete, err = native.decodeWrongNumber(context.Background(), input)
	if complete || !errors.Is(err, ErrIntegrity) || actual.Plain != "kept" {
		test.Fatal("wrong-number helper accepted a token before framing completed", complete, err)
	}
	native.position++
	checks := 0
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	stepContext := canonicalAdmissionContext{Context: ctx, check: func() {
		checks++
		if checks == 2 {
			cancel()
		}
	}}
	complete, err = native.decodeWrongNumber(stepContext, input)
	if complete || !errors.Is(err, context.Canceled) || !reflect.DeepEqual(*native, before) || actual.Plain != "kept" {
		test.Fatal("final cancellation check exposed a wrong-number semantic result", complete, err)
	}
}

func TestLegacyNativeWrongNumberPreservesRememberedError(test *testing.T) {
	token := "1" + strings.Repeat("0", LegacyCanonicalEncodingBytes)
	actual := legacyNativeWrongNumberOwner{Before: "kept-before", Plain: "kept-plain"}
	native, input := legacyNativeWrongNumberStart(test, &actual, "plain", token)
	native.deferTypeErrors = true
	expected := actual
	earliest := json.Unmarshal([]byte(`{"before":false}`), &expected)
	if !native.rememberTypeError(earliest) || native.typeError == nil {
		test.Fatal("earlier native type error was not remembered", earliest)
	}
	before := *native
	complete, err := native.decodeWrongNumber(context.Background(), input)
	var failure *json.UnmarshalTypeError
	if !complete || !errors.As(err, &failure) || failure.Field != "plain" || !reflect.DeepEqual(*native, before) || actual != expected {
		test.Fatal("wrong-number helper replaced an earlier error or mutated the rejected destination", complete, err)
	}
	if !native.rememberTypeError(err) || native.typeError != earliest {
		test.Fatal("later giant wrong-number error displaced the first recoverable error", native.typeError, earliest)
	}
}

func TestLegacyNativeWrongNumberSiblingDuplicateAndFirstError(test *testing.T) {
	token := "-1e+" + strings.Repeat("9", LegacyCanonicalEncodingBytes+17)
	for _, fixture := range []struct {
		name      string
		fragments []string
	}{
		{name: "plain-preserves-old-and-writes-sibling", fragments: []string{`{"plain":` + token + `}`, `{"after":"written"}`}},
		{name: "alias-preserves-old-and-writes-sibling", fragments: []string{`{"alias":` + token + `}`, `{"after":"written"}`}},
		{name: "plain-duplicate-repairs-value-not-error", fragments: []string{`{"plain":` + token + `}`, `{"plain":"replacement"}`, `{"after":"written"}`}},
		{name: "alias-duplicate-retains-null-and-first-error", fragments: []string{`{"alias":"replacement"}`, `{"alias":` + token + `}`, `{"alias":null}`, `{"after":"written"}`}},
		{name: "earlier-error-wins", fragments: []string{`{"before":false}`, `{"plain":` + token + `}`, `{"after":"written"}`}},
		{name: "first-giant-error-wins", fragments: []string{`{"alias":` + token + `}`, `{"plain":` + token + `}`, `{"after":"written"}`}},
		{name: "giant-error-before-later-small-error", fragments: []string{`{"plain":` + token + `}`, `{"alias":false}`, `{"after":"written"}`}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			initial := legacyNativeWrongNumberOwner{Before: "kept-before", Plain: "kept-plain", Alias: "kept-alias", After: "kept-after"}
			expected := initial
			var expectedErr error
			members := make([]string, 0, len(fixture.fragments))
			for _, fragment := range fixture.fragments {
				members = append(members, fragment[1:len(fragment)-1])
				if err := json.Unmarshal([]byte(fragment), &expected); err != nil && expectedErr == nil {
					expectedErr = err
				}
			}
			raw := []byte("{" + strings.Join(members, ",") + "}")
			whole := initial
			wholeErr := json.Unmarshal(raw, &whole)
			var wholeTypeError, expectedTypeError *json.UnmarshalTypeError
			if whole != expected || !errors.As(wholeErr, &wholeTypeError) || !errors.As(expectedErr, &expectedTypeError) ||
				wholeTypeError.Value != expectedTypeError.Value || wholeTypeError.Type != expectedTypeError.Type ||
				wholeTypeError.Struct != expectedTypeError.Struct || wholeTypeError.Field != expectedTypeError.Field {
				test.Fatal("field-fragment oracle changed native sibling, duplicate, or first-error behavior", wholeErr, expectedErr)
			}
			actual := initial
			actualErr := legacyNativeAnyReviewDecode(test, raw, &actual, true)
			if actual != expected || !reflect.DeepEqual(actualErr, expectedErr) {
				test.Fatalf("wrong-number owner lost native writes or the first field-fragment error: got=%#v err=%#v want=%#v err=%#v", actual, actualErr, expected, expectedErr)
			}
		})
	}
}
