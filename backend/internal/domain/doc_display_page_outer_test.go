package domain

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDocDisplayPageOuterDecoderPreservesCanonicalStrings(test *testing.T) {
	for _, text := range []string{
		strings.Repeat("a\U0001f642<&>\n", 200),
		"\x00\b\f\n\r\t\"\\\ud55c\uae00\u2028\u2029\ufffd",
		`],"source":"wrong","text":"\\\"","version":1}`,
	} {
		source, raw := docDisplayPageDecodeFixture(test, text)
		assertDocDisplayPageDecodeParity(test, source, HashContent(raw), raw)
	}
}

func TestDocDisplayPageOuterDecoderRejectsNoncanonicalFields(test *testing.T) {
	source, raw := docDisplayPageDecodeFixture(test, "ab")
	for name, mutation := range map[string]struct{ before, after string }{
		"leading-whitespace":  {`{"from":0`, ` {"from":0`},
		"field-whitespace":    {`"index":0`, `"index": 0`},
		"duplicate-field":     {`"from":0`, `"from":1,"from":0`},
		"unknown-field":       {`"from":0`, `"unknown":false,"from":0`},
		"reordered-field":     {`"from":0,"index":0`, `"index":0,"from":0`},
		"escaped-field":       {`"from":0`, `"fr\u006fm":0`},
		"missing-field":       {`"from":0,`, ``},
		"null-integer":        {`"index":0`, `"index":null`},
		"negative-integer":    {`"index":0`, `"index":-1`},
		"leading-zero":        {`"index":0`, `"index":00`},
		"integer-overflow":    {`"index":0`, `"index":999999999999999999999999999`},
		"fraction-integer":    {`"index":0`, `"index":0.0`},
		"exponent-integer":    {`"index":0`, `"index":0e0`},
		"escaped-source":      {`"source":"sha256:`, `"source":"\u0073ha256:`},
		"escaped-text":        {`"text":"ab"`, `"text":"\u0061b"`},
		"null-text":           {`"text":"ab"`, `"text":null`},
		"unclosed-text":       {`"text":"ab"`, `"text":"ab\"`},
		"invalid-text-escape": {`"text":"ab"`, `"text":"\x61b"`},
		"trailing-field":      {`"version":1}`, `"version":1,"extra":0}`},
		"trailing-data":       {`"version":1}`, `"version":1}null`},
	} {
		test.Run(name, func(test *testing.T) {
			changed := bytes.Replace(raw, []byte(mutation.before), []byte(mutation.after), 1)
			if bytes.Equal(changed, raw) {
				test.Fatal("fixture did not mutate original bytes")
			}
			assertDocDisplayPageDecodeParity(test, source, HashContent(changed), changed)
			if page, err := DecodeDocDisplayPage(source, HashContent(changed), changed); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) {
				test.Fatal("self-hashed noncanonical page escaped the original fence", page, err)
			}
		})
	}
}
