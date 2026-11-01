package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func legacyNativeBlocksOracleCheck(test *testing.T, raw string, backing []ContentBlock, length int) error {
	test.Helper()
	var actual, expected struct {
		Blocks []ContentBlock `json:"blocks"`
	}
	if backing != nil {
		actual.Blocks = append([]ContentBlock(nil), backing...)[:length]
		expected.Blocks = append([]ContentBlock(nil), backing...)[:length]
	}
	expectedErr := json.Unmarshal([]byte(raw), &expected)
	_, actualErr := legacyNativeMetadataDecode(test, []byte(raw), &actual, false)
	if (actualErr == nil) != (expectedErr == nil) {
		test.Fatalf("native blocks acceptance differs from encoding/json: raw=%s got=%v want=%v", raw, actualErr, expectedErr)
	}
	if expectedErr != nil {
		var actualTypeError, expectedTypeError *json.UnmarshalTypeError
		if !errors.As(expectedErr, &expectedTypeError) || !errors.As(actualErr, &actualTypeError) ||
			actualTypeError.Value != expectedTypeError.Value || actualTypeError.Type != expectedTypeError.Type {
			test.Fatalf("native blocks rejection differs from encoding/json: raw=%s got=%v want=%v", raw, actualErr, expectedErr)
		}
		return expectedErr
	}
	if !reflect.DeepEqual(actual.Blocks, expected.Blocks) {
		test.Fatalf("native blocks values differ from encoding/json: raw=%s got=%#v want=%#v", raw, actual.Blocks, expected.Blocks)
	}
	return nil
}

func TestLegacyNativeBlocksArraySequencesMatchJSONUnmarshal(test *testing.T) {
	for _, fixture := range []struct {
		name    string
		backing []ContentBlock
		length  int
		arrays  []string
	}{
		{
			name:   "null-empty-and-null-elements",
			arrays: []string{`null`, `[]`, `[null]`, `[{},null,{"type":null,"text":null}]`, `[]`, `null`, `[]`},
		},
		{
			name: "short-duplicates-reexpose-type-and-text",
			arrays: []string{
				`[{"type":"first-type","text":"first-text"},{"type":"second-type","text":"second-text"},{"type":"third-type","text":"third-text"}]`,
				`[{"text":"changed-head"}]`,
				`[null,{},null]`,
				`[{}]`,
				`[{"type":null,"text":null},{"type":"changed-tail"},{"text":"changed-third"}]`,
			},
		},
		{
			name: "explicit-empty-strings-clear-reused-fields",
			arrays: []string{
				`[{"type":"head-type","text":"head-text"},{"type":"tail-type","text":"tail-text"}]`,
				`[{"type":"","text":""}]`,
				`[null,{"type":""}]`,
				`[{},{}]`,
				`[null,{"text":""}]`,
				`[null,null]`,
			},
		},
		{
			name: "empty-array-discards-hidden-history",
			arrays: []string{
				`[{"type":"head-type","text":"head-text"},{"type":"tail-type","text":"tail-text"}]`,
				`[{}]`,
				`[]`,
				`[{},null]`,
			},
		},
		{
			name: "whole-null-discards-hidden-history",
			arrays: []string{
				`[{"type":"head-type","text":"head-text"},{"type":"tail-type","text":"tail-text"}]`,
				`[null]`,
				`null`,
				`[null,{}]`,
			},
		},
		{
			name: "regrowth-after-short-empty-and-null",
			arrays: []string{
				`[{"type":"text","text":"old"},{"type":"text","text":"old-tail"}]`,
				`[{"text":"short"}]`,
				`[]`,
				`[{"type":"fresh-type","text":"fresh-text"},{"type":"fresh-tail","text":"fresh-tail-text"}]`,
				`[null]`,
				`[null,null,{}]`,
				`null`,
				`[{},null,{}]`,
			},
		},
		{
			name: "seeded-hidden-type-and-text",
			backing: []ContentBlock{
				{Type: "head-type", Text: "head-text"},
				{Type: "hidden-type", Text: "hidden-text"},
				{Type: "hidden-third-type", Text: "hidden-third-text"},
			},
			length: 1,
			arrays: []string{`[{}]`, `[null,{"text":null},{}]`, `[{"type":"new-head"}]`, `[{},null,null]`},
		},
		{
			name: "zero-length-seed-with-hidden-values",
			backing: []ContentBlock{
				{Type: "hidden-first-type", Text: "hidden-first-text"},
				{Type: "hidden-second-type", Text: "hidden-second-text"},
			},
			arrays: []string{`[null,{}]`, `[{"text":"changed"}]`, `[{},null]`, `[]`, `[null,{}]`},
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			for last := range fixture.arrays {
				raw := `{"blocks":` + strings.Join(fixture.arrays[:last+1], `,"blocks":`) + `}`
				if err := legacyNativeBlocksOracleCheck(test, raw, fixture.backing, fixture.length); err != nil {
					test.Fatal("array sequence was rejected by encoding/json", err)
				}
			}
		})
	}
}

func TestLegacyNativeBlocksMemberMatchingMatchesJSONUnmarshal(test *testing.T) {
	for _, fixture := range []struct {
		name string
		raw  string
	}{
		{
			name: "duplicate-type-and-text-follow-input-order",
			raw:  `{"blocks":[{"type":"old-type","type":"new-type","type":null,"text":"old-text","text":"new-text","text":null}]}`,
		},
		{
			name: "ascii-folded-and-escaped-block-fields",
			raw:  `{"blocks":[{"TYPE":"upper-type","Type":"title-type","ty\u0070e":"escaped-type","TEXT":"upper-text","Text":"title-text","te\u0078t":"한🙂\n\u0000"}]}`,
		},
		{
			name: "folded-and-escaped-array-duplicates",
			raw:  `{"blocks":[{"type":"text","text":"first"},{"type":"hidden-type","text":"hidden-text"}],"BLOCKS":[{"TEXT":"changed"}],"BlOcKs":[null,{}],"blocKſ":[{"TYPE":null},{"te\u0078t":null}],"bloc\u006bs":[{},null]}`,
		},
		{
			name: "null-folded-members-retain-string-values",
			raw:  `{"blocks":[{"type":"kept-type","TYPE":null,"text":"kept-text","TEXT":null,"TYPE":"","TEXT":"","type":null,"text":null}]}`,
		},
		{
			name: "unknown-lookalikes-and-invalid-kinds-are-ignored",
			raw:  `{"blocks":[{"type":"not-a-text-enum","text":"kept","type\u0000":false,"text ":[],"unknown":{"type":7,"text":false},"ignored":1e999}],"blockſ ":false,"blocks\u0000":{},"Blocks_":7}`,
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			if err := legacyNativeBlocksOracleCheck(test, fixture.raw, nil, 0); err != nil {
				test.Fatal("member-matching fixture was rejected by encoding/json", err)
			}
		})
	}
}

func TestLegacyNativeBlocksInvalidKindsMatchJSONUnmarshal(test *testing.T) {
	for _, key := range []string{`"blocks"`, `"BLOCKS"`, `"blocKſ"`, `"bloc\u006bs"`} {
		for _, invalid := range []string{`false`, `7`, `1e999`, `"text"`, `{}`} {
			test.Run("array/"+key+"/"+invalid, func(test *testing.T) {
				raw := `{` + key + `:` + invalid + `,"blocks":[],"BLOCKS":null}`
				if err := legacyNativeBlocksOracleCheck(test, raw, nil, 0); err == nil {
					test.Fatal("invalid array kind was accepted by encoding/json")
				}
			})
		}
	}
	for _, invalid := range []string{`false`, `7`, `1e999`, `"text"`, `[]`} {
		test.Run("element/"+invalid, func(test *testing.T) {
			raw := `{"blocks":[{"type":"text","text":"first"},` + invalid + `,null],"blocks":[{}],"blocks":null}`
			if err := legacyNativeBlocksOracleCheck(test, raw, nil, 0); err == nil {
				test.Fatal("invalid element kind was accepted by encoding/json")
			}
		})
	}
	for _, key := range []string{`"type"`, `"TYPE"`, `"ty\u0070e"`, `"text"`, `"TEXT"`, `"te\u0078t"`} {
		for _, invalid := range []string{`false`, `7`, `1e999`, `{}`, `[]`} {
			test.Run("field/"+key+"/"+invalid, func(test *testing.T) {
				raw := `{"blocks":[{"type":"text","text":"before",` + key + `:` + invalid + `,"type":"text","text":"after"}],"blocks":[],"blocks":null}`
				if err := legacyNativeBlocksOracleCheck(test, raw, nil, 0); err == nil {
					test.Fatal("invalid block field kind was accepted by encoding/json")
				}
			})
		}
	}
}
