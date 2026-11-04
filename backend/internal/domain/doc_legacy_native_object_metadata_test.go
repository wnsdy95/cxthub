package domain

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeMetadataStringAlias string

type legacyNativeMetadataJSONAlias string

func (value *legacyNativeMetadataJSONAlias) UnmarshalJSON(raw []byte) error {
	var decoded string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	*value = legacyNativeMetadataJSONAlias("json:" + decoded)
	return nil
}

type legacyNativeMetadataTextAlias string

func (value *legacyNativeMetadataTextAlias) UnmarshalText(raw []byte) error {
	*value = legacyNativeMetadataTextAlias("text:" + string(raw))
	return nil
}

type legacyNativeMetadataFixture struct {
	Plain       string `json:"plain"`
	Ignored     string `json:"-"`
	hidden      string
	Alias       legacyNativeMetadataStringAlias `json:"alias,omitempty"`
	Quoted      string                          `json:",string"`
	JSON        legacyNativeMetadataJSONAlias   `json:"json"`
	Text        legacyNativeMetadataTextAlias   `json:"text"`
	QuotedAlias legacyNativeMetadataStringAlias `json:"quoted_alias,omitempty,string"`
}

func legacyNativeMetadataDecode(test *testing.T, raw []byte, destination any, trackPresence bool) (eventFieldPresence, error) {
	test.Helper()
	metadata := newLegacyNativeObjectFields(reflect.TypeOf(destination).Elem())
	native, err := newLegacyNativeObject(reflect.ValueOf(destination), &metadata, 1, trackPresence)
	if err != nil {
		test.Fatal("native metadata constructor failed", err)
	}
	buffer := &legacySourceProofBuffer{body: raw}
	source := buffer.input()
	ctx := context.Background()
	var frame legacyJSONValueScanner
	native.start(0, len(raw))
	for range 16 {
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		complete, err := native.step(ctx, input, &frame)
		if input.failure != nil {
			return native.presence, input.failure
		}
		if err != nil || complete {
			return native.presence, err
		}
	}
	test.Fatal("small native metadata fixture exhausted its step limit")
	return 0, ErrIntegrity
}

func TestLegacyNativeObjectMetadataScalarSelection(test *testing.T) {
	metadata := newLegacyNativeObjectFields(reflect.TypeOf(legacyNativeMetadataFixture{}))
	expected := []struct {
		name         string
		index        int
		scalarString bool
	}{
		{name: "plain", index: 0, scalarString: true},
		{name: "alias", index: 3, scalarString: true},
		{name: "Quoted", index: 4},
		{name: "json", index: 5},
		{name: "text", index: 6},
		{name: "quoted_alias", index: 7},
	}
	if metadata.err != nil || len(metadata.fields) != len(expected) {
		test.Fatal("metadata did not exclude ignored and unexported fields", len(metadata.fields), metadata.err)
	}
	for position, field := range expected {
		actual := metadata.fields[position]
		encoded, err := json.Marshal(field.name)
		if err != nil {
			test.Fatal(err)
		}
		if actual.name != field.name || actual.index != field.index || actual.scalarString != field.scalarString ||
			actual.prefix != "{"+string(encoded)+":" || metadata.match(field.name) != position {
			test.Fatalf("metadata field mismatch: got=%+v want=%+v", actual, field)
		}
	}
	for _, ignored := range []string{"Ignored", "-", "hidden", "HIDDEN"} {
		if metadata.match(ignored) != -1 {
			test.Fatal("ignored or unexported field is selectable", ignored)
		}
	}
}

func TestLegacyNativeObjectMetadataIgnoresTaggedUnexportedField(test *testing.T) {
	model := reflect.StructOf([]reflect.StructField{
		{Name: "Visible", Type: reflect.TypeFor[string](), Tag: `json:"visible"`},
		{Name: "hidden", PkgPath: reflect.TypeFor[legacyNativeMetadataFixture]().PkgPath(), Type: reflect.TypeFor[string](), Tag: `json:"hidden"`},
	})
	metadata := newLegacyNativeObjectFields(model)
	if metadata.err != nil || len(metadata.fields) != 1 || metadata.fields[0].name != "visible" {
		test.Fatal("a JSON tag made an unexported field selectable", metadata.err)
	}
	actual, expected := reflect.New(model), reflect.New(model)
	raw := []byte(`{"visible":"kept","hidden":"ignored","HIDDEN":"also ignored"}`)
	if err := json.Unmarshal(raw, expected.Interface()); err != nil {
		test.Fatal(err)
	}
	if _, err := legacyNativeMetadataDecode(test, raw, actual.Interface(), false); err != nil || !reflect.DeepEqual(actual.Interface(), expected.Interface()) {
		test.Fatal("tagged unexported field changed native JSON visibility", err)
	}
}

func TestLegacyNativeObjectMetadataNativeStringParity(test *testing.T) {
	payload := strings.Repeat("x", LegacyCanonicalEncodingBytes+1)
	encoded, err := json.Marshal(payload)
	if err != nil {
		test.Fatal(err)
	}
	quoted, err := json.Marshal(string(encoded))
	if err != nil {
		test.Fatal(err)
	}
	for _, fixture := range []struct {
		name     string
		raw      string
		accepted bool
	}{
		{name: "plain-and-alias-scalar", raw: `{"plain":` + string(encoded) + `,"alias":` + string(encoded) + `}`, accepted: true},
		{name: "default-name-string-tag", raw: `{"Quoted":` + string(quoted) + `}`, accepted: true},
		{name: "alias-string-tag-after-omitempty", raw: `{"quoted_alias":` + string(quoted) + `}`, accepted: true},
		{name: "json-unmarshaler-string-alias", raw: `{"json":` + string(encoded) + `}`, accepted: true},
		{name: "text-unmarshaler-string-alias", raw: `{"text":` + string(encoded) + `}`, accepted: true},
		{name: "json-null-still-calls-unmarshaler", raw: `{"json":null}`, accepted: true},
		{name: "native-null-retains-strings", raw: `{"plain":null,"alias":null,"Quoted":null,"quoted_alias":null,"text":null}`, accepted: true},
		{name: "ignored-fields-do-not-decode", raw: `{"Ignored":{},"-":false,"hidden":7,"HIDDEN":[]}`, accepted: true},
		{name: "string-tag-requires-inner-quotes", raw: `{"Quoted":` + string(encoded) + `}`},
		{name: "string-tag-rejects-unquoted-number", raw: `{"Quoted":7}`},
		{name: "json-unmarshaler-native-error", raw: `{"json":7}`},
		{name: "text-unmarshaler-native-error", raw: `{"text":7}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			expected := legacyNativeMetadataFixture{
				Plain: "keep-plain", Ignored: "keep-ignored", hidden: "keep-hidden", Alias: "keep-alias",
				Quoted: "keep-quoted", JSON: "keep-json", Text: "keep-text", QuotedAlias: "keep-quoted-alias",
			}
			actual := expected
			raw := []byte(fixture.raw)
			expectedErr := json.Unmarshal(raw, &expected)
			if (expectedErr == nil) != fixture.accepted {
				test.Fatal("fixture lost its native acceptance", fixture.accepted, expectedErr)
			}
			presence, actualErr := legacyNativeMetadataDecode(test, raw, &actual, false)
			if reflect.TypeOf(actualErr) != reflect.TypeOf(expectedErr) || presence != 0 {
				test.Fatal("native metadata changed acceptance, error type, or disabled presence", actualErr, expectedErr, presence)
			}
			if actualErr == nil && actual != expected {
				test.Fatal("scalar selection bypassed native tags, custom decoding, or ignored fields", fixture.name)
			}
		})
	}
}

func TestLegacyNativeObjectMetadataFoldedMatchingAndExactPresence(test *testing.T) {
	test.Run("exact-name-before-folded-fallback", func(test *testing.T) {
		type matchingFixture struct {
			Folded string `json:"name"`
			Exact  string `json:"NAME"`
		}
		metadata := newLegacyNativeObjectFields(reflect.TypeOf(matchingFixture{}))
		if metadata.err != nil || metadata.match("NAME") != 1 || metadata.match("NaMe") != 0 {
			test.Fatal("matching lost exact-name priority or first folded match", metadata.err)
		}
		raw := []byte(`{"NAME":"exact","NaMe":"folded"}`)
		var actual, expected matchingFixture
		if err := json.Unmarshal(raw, &expected); err != nil {
			test.Fatal(err)
		}
		if _, err := legacyNativeMetadataDecode(test, raw, &actual, false); err != nil || actual != expected {
			test.Fatal("matching differs from the native struct decoder", actual, expected, err)
		}
	})
	for _, fixture := range []struct {
		name     string
		raw      string
		presence eventFieldPresence
	}{
		{name: "folded-only", raw: `{"kind":"message","AGENT_AUTHOR":"upper","agent_meſſage":false,"locKed":null}`},
		{name: "exact-zero-and-null", raw: `{"kind":"message","agent_message":false,"agent_author":null,"locked":null}`, presence: eventFieldAgentMessage | eventFieldAgentAuthor | eventFieldLocked},
		{name: "escaped-exact", raw: `{"kind":"message","\u0061gent_message":false,"agent_\u0061uthor":"kept"}`, presence: eventFieldAgentMessage | eventFieldAgentAuthor},
		{name: "exact-survives-folded-null", raw: `{"kind":"message","agent_author":"kept","AGENT_AUTHOR":null,"AGENT_MESSAGE":false,"agent_\u006dessage":null}`, presence: eventFieldAgentMessage | eventFieldAgentAuthor},
		{name: "unknown-lookalikes", raw: `{"kind":"message","AgentAuthor":"ignored","agent_author\u0000":"ignored","agent_meſſage":false}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(fixture.raw)
			var expected CIREvent
			if err := json.Unmarshal(raw, &expected); err != nil || expected.fieldPresence != fixture.presence {
				test.Fatal("fixture lost its native exact presence", expected.fieldPresence, fixture.presence, err)
			}
			for _, tracked := range []bool{true, false} {
				var decoded wire
				presence, err := legacyNativeMetadataDecode(test, raw, &decoded, tracked)
				actual := CIREvent(decoded)
				actual.fieldPresence = presence
				comparison := expected
				if !tracked {
					comparison.fieldPresence = 0
				}
				if err != nil || !reflect.DeepEqual(actual, comparison) {
					test.Fatal("folded decoding changed native values or exact presence", tracked, actual, comparison, err)
				}
			}
		})
	}
}
