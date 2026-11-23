package domain

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestLegacyEnvelopeEncodingCanonicalTypedParity(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	envelopes := []CIREnvelope{
		{},
		{SourceModels: []string{}},
		{SourceModels: []string{"", "same", "same", "last"}},
		{ContextTokens: maximum, OutputTokens: -maximum - 1, CompactionCount: -1},
		{ContextTokens: -maximum - 1, OutputTokens: maximum, CompactionCount: -maximum - 1},
		{ContextTokens: -1, OutputTokens: 1, CompactionCount: maximum},
	}
	for _, text := range []string{
		"v2", "\"\\/\b\f\n\r\t\x00\x01\x1f<&>\u2028\u2029\ud55c\uad6d🙂\ufffd",
		"\xff\x80\xc0\xaf\xed\xa0\x80\xf4\x90\x80\x80", `\ufffd\\ufffd`,
	} {
		var envelope CIREnvelope
		value := reflect.ValueOf(&envelope).Elem()
		for index := 0; index < value.NumField(); index++ {
			member := value.Field(index)
			switch member.Kind() {
			case reflect.String:
				member.SetString(text)
			case reflect.Int:
				member.SetInt(int64(index + 1))
			case reflect.Slice:
				member.Set(reflect.ValueOf([]string{text, "", text, "last"}))
			default:
				test.Fatalf("update the parity fixture for native envelope type %s", member.Type())
			}
		}
		envelopes = append(envelopes, envelope)
	}
}

func TestLegacyEnvelopeEncodingNativeDuplicateAndNullParity(test *testing.T) {
	for index, raw := range []string{
		`null`, `{}`, `{"source_models":null}`, `{"source_models":[]}`,
		`{"source_models":["first","same","same"],"source_models":["last","last"]}`,
		`{"source_models":["old","tail"],"source_models":[null,"new","new"]}`,
		`{"source_models":["old","tail"],"SOURCE_MODELS":null,"Source_Models":["again",null]}`,
		`{"source_model":"old","SOURCE_MODEL":null,"source_models":["kept"],"source_models":null}`,
		`{"context_tokens":7,"CONTEXT_TOKENS":null,"output_tokens":0,"compaction_count":-7}`,
		`{"source_models":["\ud800","\udfff","\ud83d\ude42"],"ignored":{"source_models":["wrong"]}}`,
	} {
		test.Run(fmt.Sprint(index), func(test *testing.T) {
			var envelope CIREnvelope
			if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
				test.Fatal(err)
			}
		})
	}
}
