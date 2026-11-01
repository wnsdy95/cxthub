package domain

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func assertDocDisplayPageJSONParity(test testing.TB, page DocDisplayPage) []byte {
	test.Helper()
	before, beforeErr := json.Marshal(page)
	want, wantErr := canonicalJSON(page)
	got, gotErr := marshalDocDisplayPage(page)
	if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || reflect.TypeOf(gotErr) != reflect.TypeOf(wantErr) {
		test.Fatalf("error mismatch: got %v, want %v", gotErr, wantErr)
	}
	if !bytes.Equal(got, want) || (got == nil) != (want == nil) {
		test.Fatalf("canonical bytes mismatch: got %q, want %q", got, want)
	}
	after, afterErr := json.Marshal(page)
	if !bytes.Equal(before, after) || fmt.Sprint(beforeErr) != fmt.Sprint(afterErr) {
		test.Fatal("serializer mutated source page or runs")
	}
	return got
}

func TestMarshalDocDisplayPageCanonicalJSON(test *testing.T) {
	assertDocDisplayPageJSONParity(test, DocDisplayPage{})
	controls := make([]byte, 32)
	for index := range controls {
		controls[index] = byte(index)
	}
	manyRuns := make([]DocDisplayRun, DocDisplayPageRuns)
	for index := range manyRuns {
		manyRuns[index] = DocDisplayRun{To: index + 1, SourceFrom: 2*index + 11, SourceTo: 2*index + 13, Linear: index%2 == 0}
	}
	source := ContentHash("sha256:" + strings.Repeat("a", 64))
	cases := []struct {
		name string
		page DocDisplayPage
	}{
		{"nil-runs", DocDisplayPage{}},
		{"empty-runs", DocDisplayPage{Runs: []DocDisplayRun{}}},
		{"one-run", DocDisplayPage{Source: source, Index: 2, From: 17, Text: "mapped", Runs: []DocDisplayRun{{To: 6, SourceFrom: 11, SourceTo: 29, Linear: true}}}},
		{"mixed-runs", DocDisplayPage{Source: source, Index: 3, From: 31, Text: "x\ny", Runs: []DocDisplayRun{{To: 1, SourceFrom: 7, SourceTo: 8, Linear: true}, {To: 3, SourceFrom: 23, SourceTo: 41}}}},
		{"zero-run", DocDisplayPage{Runs: []DocDisplayRun{{}}}},
		{"many-runs", DocDisplayPage{Source: source, Index: 11, From: 29, Text: strings.Repeat("x", len(manyRuns)), Runs: manyRuns}},
		{"over-byte-limit", DocDisplayPage{Text: strings.Repeat("<", MaxDocDisplayPageBytes/6+1)}},
	}
	for _, textCase := range []struct {
		name string
		text string
	}{
		{"unicode", "\ud55c\uae00 e\u0301 🙂 \ufeff \U0010ffff"},
		{"json-escapes", string(controls) + "\"\\/\x7f"},
		{"html", "</script><>&"},
		{"separators", "before\u2028middle\u2029after"},
		{"rune-error", "before\ufffdafter"},
		{"literal-escapes", `\u003c\u2028\u0000\n\"\\\/`},
	} {
		cases = append(cases, struct {
			name string
			page DocDisplayPage
		}{textCase.name, DocDisplayPage{Source: ContentHash(textCase.text), Text: textCase.text, Runs: []DocDisplayRun{{To: len(textCase.text), SourceFrom: 13, SourceTo: 71, Linear: true}}}})
	}
	for _, candidate := range cases {
		for version := 1; version <= 4; version++ {
			test.Run(fmt.Sprintf("%s/v%d", candidate.name, version), func(test *testing.T) {
				page := candidate.page
				page.Version = version
				assertDocDisplayPageJSONParity(test, page)
			})
		}
	}
}

func TestMarshalDocDisplayPageExactIntegers(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	minimum := -maximum - 1
	boundaries := []int{minimum, minimum + 1, -1, 0, 1, maximum - 1, maximum}
	if maximum > 1<<31-1 {
		precisionBoundary := int64(1) << 53
		for _, sign := range []int64{-1, 1} {
			for delta := int64(-2); delta <= 2; delta++ {
				boundaries = append(boundaries, int(sign*precisionBoundary+delta))
			}
		}
	}
	for boundaryIndex, boundary := range boundaries {
		test.Run(fmt.Sprintf("%d", boundary), func(test *testing.T) {
			valueAt := func(offset int) int {
				return boundaries[(boundaryIndex+offset)%len(boundaries)]
			}
			page := DocDisplayPage{
				Version: valueAt(0), Source: "not-a-hash", Index: valueAt(1), From: valueAt(2), Text: "unvalidated",
				Runs: []DocDisplayRun{
					{To: valueAt(3), SourceFrom: valueAt(4), SourceTo: valueAt(5), Linear: true},
					{To: valueAt(6), SourceFrom: valueAt(7), SourceTo: valueAt(8)},
				},
			}
			assertDocDisplayPageJSONParity(test, page)
		})
	}
}

func TestMarshalDocDisplayPageInvalidUTF8(test *testing.T) {
	for caseIndex, invalid := range []string{
		"\xff", "\xfe", "\x80", "\xc0\xaf", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\xe2\x82", "\xf0\x9f", "prefix\xffsuffix", "\ufffd\xff<&>\u2028",
	} {
		for _, field := range []string{"source", "text", "both"} {
			test.Run(fmt.Sprintf("%d/%s", caseIndex, field), func(test *testing.T) {
				page := DocDisplayPage{Version: 4, Source: "valid", Index: 2, From: 17, Text: "valid", Runs: []DocDisplayRun{{To: 5, SourceFrom: 11, SourceTo: 29, Linear: true}}}
				if field != "text" {
					page.Source = ContentHash(invalid)
				}
				if field != "source" {
					page.Text = invalid
				}
				assertDocDisplayPageJSONParity(test, page)
			})
		}
	}
}

func TestPreparedDisplayJSONFieldCompleteness(test *testing.T) {
	type field struct {
		name string
		typ  reflect.Type
		tag  string
	}
	integerType := reflect.TypeOf(int(0))
	for shape, fields := range map[reflect.Type][]field{
		reflect.TypeOf(DocDisplayPage{}): {
			{"Version", integerType, "version"},
			{"Source", reflect.TypeOf(ContentHash("")), "source"},
			{"Index", integerType, "index"},
			{"From", integerType, "from"},
			{"Text", reflect.TypeOf(""), "text"},
			{"Runs", reflect.TypeOf([]DocDisplayRun(nil)), "runs"},
		},
		reflect.TypeOf(DocDisplayRun{}): {
			{"To", integerType, "to"},
			{"SourceFrom", integerType, "source_from"},
			{"SourceTo", integerType, "source_to"},
			{"Linear", reflect.TypeOf(false), "linear"},
		},
		reflect.TypeOf(DocDisplayProjectionState{}): {
			{"Version", integerType, "version"},
			{"Source", reflect.TypeOf(ContentHash("")), "source"},
			{"TextBytes", integerType, "text_bytes"},
			{"Directory", reflect.TypeOf(DocRangeDirectoryAccumulator{}), "directory"},
			{"Done", reflect.TypeOf(false), "done"},
		},
		reflect.TypeOf(DocRangeDirectoryAccumulator{}): {
			{"Count", integerType, "count"},
			{"Levels", reflect.TypeOf([][]ContentHash(nil)), "levels"},
		},
		reflect.TypeOf(DocFindSpan{}): {
			{"From", integerType, "from"},
			{"To", integerType, "to"},
		},
		reflect.TypeOf(DocMessageBlockInput{}): {
			{"Index", integerType, "index"},
			{"Anchor", reflect.TypeOf(DocFindSpan{}), "anchor"},
			{"Type", reflect.TypeOf((*DocFindSpan)(nil)), "type,omitempty"},
			{"Name", reflect.TypeOf((*DocFindSpan)(nil)), "name,omitempty"},
			{"Text", reflect.TypeOf((*DocFindSpan)(nil)), "text,omitempty"},
			{"IsText", reflect.TypeOf(false), "is_text"},
		},
		reflect.TypeOf(DocMessageInputPage{}): {
			{"Version", integerType, "version"},
			{"Source", reflect.TypeOf(ContentHash("")), "source"},
			{"Index", integerType, "index"},
			{"Entries", reflect.TypeOf([]DocMessageBlockInput(nil)), "entries"},
		},
		reflect.TypeOf(DocMessageDisplayPart{}): {
			{"Index", integerType, "index"},
			{"Block", integerType, "block"},
			{"From", integerType, "from"},
			{"Separator", integerType, "separator"},
			{"TextBytes", integerType, "text_bytes"},
			{"Kind", reflect.TypeOf(DocMessageDisplayKind("")), "kind"},
			{"Plan", reflect.TypeOf(ContentHash("")), "plan"},
			{"First", reflect.TypeOf(DocFindSpan{}), "first"},
		},
		reflect.TypeOf(DocMessageDisplayPage{}): {
			{"Version", integerType, "version"},
			{"Source", reflect.TypeOf(ContentHash("")), "source"},
			{"Index", integerType, "index"},
			{"Parts", reflect.TypeOf([]DocMessageDisplayPart(nil)), "parts"},
		},
		reflect.TypeOf(DocMessageDisplayState{}): {
			{"Version", integerType, "version"},
			{"Source", reflect.TypeOf(ContentHash("")), "source"},
			{"Next", integerType, "next"},
			{"Count", integerType, "count"},
			{"TextBytes", integerType, "text_bytes"},
			{"Breaks", integerType, "breaks"},
			{"Entries", reflect.TypeOf([]DocMessageDisplayPart(nil)), "entries,omitempty"},
			{"Directory", reflect.TypeOf(DocRangeDirectoryAccumulator{}), "directory"},
		},
	} {
		test.Run(shape.Name(), func(test *testing.T) {
			if shape.NumField() != len(fields) {
				test.Fatalf("%s fields changed: update display page wire conversion and guards", shape)
			}
			for fieldIndex, expected := range fields {
				actual := shape.Field(fieldIndex)
				if actual.Name != expected.name || actual.Type != expected.typ || actual.Tag.Get("json") != expected.tag || actual.Anonymous || actual.PkgPath != "" {
					test.Fatalf("%s field %d changed: update display page wire conversion and guards", shape, fieldIndex)
				}
			}
		})
	}
}

func TestPreparedDisplayJSONPrimitiveMethodSets(test *testing.T) {
	marshalers := []reflect.Type{
		reflect.TypeOf((*json.Marshaler)(nil)).Elem(),
		reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem(),
	}
	for _, shape := range []reflect.Type{
		reflect.TypeOf(ContentHash("")), reflect.TypeOf(DocDisplayPage{}), reflect.TypeOf(DocDisplayRun{}),
		reflect.TypeOf(DocDisplayProjectionState{}), reflect.TypeOf(DocRangeDirectoryAccumulator{}),
		reflect.TypeOf(DocFindSpan{}), reflect.TypeOf(DocMessageDisplayKind("")), reflect.TypeOf(DocMessageBlockInput{}),
		reflect.TypeOf(DocMessageInputPage{}), reflect.TypeOf(DocMessageDisplayPart{}), reflect.TypeOf(DocMessageDisplayPage{}), reflect.TypeOf(DocMessageDisplayState{}),
	} {
		for _, receiver := range []reflect.Type{shape, reflect.PointerTo(shape)} {
			for _, marshaler := range marshalers {
				if receiver.Implements(marshaler) {
					test.Fatalf("%s gained %s behavior: review display page wire conversion", receiver, marshaler)
				}
			}
		}
	}
}

func FuzzMarshalDocDisplayPageCanonicalJSON(fuzz *testing.F) {
	for _, seed := range []struct {
		source  string
		text    string
		integer int64
		shape   uint16
	}{
		{"", "", 0, 0},
		{"hash", "text", 1, DocDisplayPageRuns + 2},
		{"<>&\u2028\u2029", "\ud55c\uae00🙂\"\\/\b\f\n\r\t\x00\x1f", 1<<53 + 1, 1},
		{"\xff", "\ufffd\xed\xa0\x80", -1<<53 - 1, DocDisplayPageRuns},
		{"valid", "\xff", 1<<63 - 1, DocDisplayPageRuns + 1},
		{"\ufffd", "\ufffd", -1 << 63, 2},
	} {
		fuzz.Add(seed.source, seed.text, seed.integer, seed.shape)
	}
	fuzz.Fuzz(func(test *testing.T, source, text string, integer int64, shape uint16) {
		page := DocDisplayPage{Version: int(shape%4) + 1, Source: ContentHash(source), Index: int(integer), From: ^int(integer), Text: text}
		if shape != 0 {
			page.Runs = make([]DocDisplayRun, int(shape)%(DocDisplayPageRuns+2))
			for index := range page.Runs {
				page.Runs[index] = DocDisplayRun{To: int(integer) ^ index, SourceFrom: int(integer >> 1), SourceTo: ^int(integer >> 1), Linear: index%2 == 0}
			}
		}
		assertDocDisplayPageJSONParity(test, page)
	})
}
