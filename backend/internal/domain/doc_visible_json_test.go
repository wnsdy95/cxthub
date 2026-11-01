package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

var errDocVisibleJSONTest = errors.New("visible JSON marshaler failure")
var docVisibleJSONBenchmarkOutput []byte

type docVisibleJSONTestMarshaler struct {
	raw json.RawMessage
	err error
}

func (value docVisibleJSONTestMarshaler) MarshalJSON() ([]byte, error) {
	return value.raw, value.err
}

type docVisibleJSONPointerPage docVisiblePage

func (*docVisibleJSONPointerPage) MarshalJSON() ([]byte, error) {
	return []byte(`{"z":9007199254740993,"a":"<&>"}`), nil
}

func assertDocVisibleJSONParity(testingTarget testing.TB, value any) []byte {
	testingTarget.Helper()
	before, beforeErr := json.Marshal(value)
	want, wantErr := canonicalJSON(value)
	got, gotErr := canonicalDocVisibleObjectJSON(value)
	if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || reflect.TypeOf(gotErr) != reflect.TypeOf(wantErr) || errors.Is(gotErr, errDocVisibleJSONTest) != errors.Is(wantErr, errDocVisibleJSONTest) {
		testingTarget.Fatalf("%T error mismatch: got %v, want %v", value, gotErr, wantErr)
	}
	if !bytes.Equal(got, want) || wantErr == nil && HashContent(got) != HashContent(want) {
		testingTarget.Fatalf("%T canonical mismatch: got %q, want %q", value, got, want)
	}
	after, afterErr := json.Marshal(value)
	if !bytes.Equal(before, after) || fmt.Sprint(beforeErr) != fmt.Sprint(afterErr) {
		testingTarget.Fatalf("%T source mutated", value)
	}
	return got
}

func TestCanonicalDocVisibleObjectJSONShapes(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	minimum := -maximum - 1
	boundaries := []int{minimum, minimum + 1, -129, -1, 0, 1, 127, 128, 129, maximum - 1, maximum}
	if maximum > 1<<31-1 {
		precisionBoundary := int64(1) << 53
		boundaries = append(boundaries, int(precisionBoundary-1), int(precisionBoundary), int(precisionBoundary+1), int(-precisionBoundary-1))
	}
	for _, value := range []any{docVisiblePage{}, docVisiblePage{Units: []DocVisibleUnit{}}, docVisibleDirectory{}, docVisibleDirectory{Children: []docVisibleSpan{}}} {
		assertDocVisibleJSONParity(test, value)
	}
	hashes := []string{"", "sha256:" + strings.Repeat("a", 64), "<>&", "한글🙂", "\"\\/\b\f\n\r\t\x00\x1f", "\u2028\u2029", "\\u003c", "\xff", "\xfe", "\ufffd", "prefix\xc0\xafsuffix", "\xed\xa0\x80", "\xf0\x9f"}
	for boundaryIndex, boundary := range boundaries {
		for _, count := range []int{1, 2, 127, 128, 129} {
			page := docVisiblePage{Start: boundary, Units: make([]DocVisibleUnit, count)}
			directory := docVisibleDirectory{Level: boundary, Children: make([]docVisibleSpan, count)}
			for entryIndex := range page.Units {
				values := [6]int{}
				for fieldIndex := range values {
					values[fieldIndex] = boundaries[(boundaryIndex+entryIndex+fieldIndex)%len(boundaries)]
				}
				page.Units[entryIndex] = DocVisibleUnit{From: values[0], To: values[1], EventIndex: values[2], TurnIndex: values[3], RenderedBefore: values[4], FoldRows: values[5]}
				directory.Children[entryIndex] = docVisibleSpan{Hash: ContentHash(hashes[(boundaryIndex+entryIndex)%len(hashes)]), First: values[0], Limit: values[1], From: values[2], To: values[3], Rank: values[4], Rendered: values[5]}
			}
			assertDocVisibleJSONParity(test, page)
			assertDocVisibleJSONParity(test, directory)
		}
	}
	assertDocVisibleJSONParity(test, docVisibleDirectory{Level: -1, Children: []docVisibleSpan{{Hash: ContentHash(strings.Repeat("<", MaxDocVisibleObjectBytes/6+1))}}})
}

func TestCanonicalDocVisibleObjectJSONHashCollisions(test *testing.T) {
	for _, hashes := range [][]string{{"\xff", "\xfe", "\ufffd"}, {"prefix\xffsuffix", "prefix\ufffdsuffix"}, {"\xc0\xaf", "\ufffd\ufffd"}} {
		var previous []byte
		for _, hash := range hashes {
			got := assertDocVisibleJSONParity(test, docVisibleDirectory{Children: []docVisibleSpan{{Hash: "valid"}, {Hash: ContentHash(hash)}}})
			if previous != nil && !bytes.Equal(previous, got) {
				test.Fatalf("legacy UTF-8 collision changed for %q", hash)
			}
			previous = got
		}
	}
}

func TestCanonicalDocVisibleObjectJSONFallback(test *testing.T) {
	type namedPage docVisiblePage
	type namedDirectory docVisibleDirectory
	type unknown struct {
		Z int `json:"z"`
		A int `json:"a"`
	}
	page := docVisiblePage{Start: -3, Units: []DocVisibleUnit{{From: -1, To: 7}}}
	directory := docVisibleDirectory{Level: -2, Children: []docVisibleSpan{{Hash: "\xff", First: -1}}}
	var nilPage *docVisiblePage
	var nilDirectory *docVisibleDirectory
	var nilCustom *docVisibleJSONPointerPage
	customPage := docVisibleJSONPointerPage(page)
	cycle := map[string]any{}
	cycle["self"] = cycle
	values := []any{
		nil, nilPage, nilDirectory, nilCustom, &page, &directory, &customPage,
		namedPage(page), namedDirectory(directory), unknown{Z: 2, A: 1}, &unknown{},
		[]docVisiblePage{page}, map[string]any{"z": page, "a": directory},
		json.RawMessage(`{"z":1,"a":9007199254740993,"\u0061":-0}`),
		docVisibleJSONTestMarshaler{raw: json.RawMessage(`{"z":1,"a":{"z":2,"a":"<&>"}}`)},
		&docVisibleJSONTestMarshaler{raw: json.RawMessage(`null`)},
		docVisibleJSONTestMarshaler{err: errDocVisibleJSONTest},
		docVisibleJSONTestMarshaler{raw: json.RawMessage(`{"broken":`)},
		json.RawMessage(`{} {}`), json.Number("bad"), math.NaN(), math.Inf(1), math.Inf(-1),
		make(chan int), func() {}, cycle,
	}
	for caseIndex, value := range values {
		test.Run(fmt.Sprintf("%02d-%T", caseIndex, value), func(test *testing.T) {
			assertDocVisibleJSONParity(test, value)
		})
	}
}

func TestCanonicalDocVisibleObjectJSONOwnsOutput(test *testing.T) {
	page := docVisiblePage{Units: []DocVisibleUnit{{From: 17, To: 29, EventIndex: 23}}}
	directory := docVisibleDirectory{Children: []docVisibleSpan{{Hash: "original", First: 11, Limit: 31}}}
	for _, value := range []any{page, directory} {
		retained := assertDocVisibleJSONParity(test, value)
		snapshot := bytes.Clone(retained)
		for repeat := 0; repeat < 4; repeat++ {
			assertDocVisibleJSONParity(test, page)
			assertDocVisibleJSONParity(test, directory)
		}
		page.Units[0].From--
		directory.Children[0].Hash += "changed"
		if !bytes.Equal(retained, snapshot) {
			test.Fatal("retained output changed after serialization or source mutation")
		}
		for byteIndex := range retained {
			retained[byteIndex] ^= 0xff
		}
		assertDocVisibleJSONParity(test, value)
	}
}

func TestCanonicalDocVisibleObjectJSONFieldCompleteness(test *testing.T) {
	shapes := []struct {
		source, wire any
		fields       int
	}{
		{DocVisibleUnit{}, docVisibleUnitJSON{}, 6},
		{docVisibleSpan{}, docVisibleSpanJSON{}, 7},
		{docVisiblePage{}, struct {
			Start int              `json:"start"`
			Units []DocVisibleUnit `json:"units"`
		}{}, 2},
		{docVisibleDirectory{}, struct {
			Children []docVisibleSpan `json:"children"`
			Level    int              `json:"level"`
		}{}, 2},
	}
	for _, shape := range shapes {
		sourceType, wireType := reflect.TypeOf(shape.source), reflect.TypeOf(shape.wire)
		if sourceType.NumField() != shape.fields || wireType.NumField() != shape.fields {
			test.Fatalf("%s fields changed: update canonical wire conversion", sourceType)
		}
		previousTag := ""
		for fieldIndex := 0; fieldIndex < wireType.NumField(); fieldIndex++ {
			wireField := wireType.Field(fieldIndex)
			sourceField, exists := sourceType.FieldByName(wireField.Name)
			jsonTag := wireField.Tag.Get("json")
			if !exists || sourceField.Type != wireField.Type || sourceField.Tag != wireField.Tag || sourceField.Anonymous != wireField.Anonymous || wireField.PkgPath != "" || jsonTag == "" || strings.Contains(jsonTag, ",") || jsonTag <= previousTag {
				test.Fatalf("%s.%s field/tag or lexical wire order drift", sourceType, wireField.Name)
			}
			previousTag = jsonTag
		}
	}
}

func docVisibleJSONShapesFromBytes(data []byte) (docVisiblePage, docVisibleDirectory) {
	position := 0
	nextByte := func() byte {
		if position == len(data) {
			return 0
		}
		value := data[position]
		position++
		return value
	}
	nextInteger := func() int {
		var value uint64
		for shift := uint(0); shift < 64; shift += 8 {
			value |= uint64(nextByte()) << shift
		}
		return int(value)
	}
	count := int(nextByte()) % 130
	page := docVisiblePage{Start: nextInteger()}
	directory := docVisibleDirectory{Level: nextInteger()}
	if len(data) == 0 {
		return page, directory
	}
	page.Units = make([]DocVisibleUnit, count)
	directory.Children = make([]docVisibleSpan, count)
	for entryIndex := range page.Units {
		unit := DocVisibleUnit{From: nextInteger(), To: nextInteger(), EventIndex: nextInteger(), TurnIndex: nextInteger(), RenderedBefore: nextInteger(), FoldRows: nextInteger()}
		hash := make([]byte, int(nextByte())%65)
		for byteIndex := range hash {
			hash[byteIndex] = nextByte()
		}
		page.Units[entryIndex] = unit
		directory.Children[entryIndex] = docVisibleSpan{Hash: ContentHash(hash), First: unit.From, Limit: unit.To, From: unit.EventIndex, To: unit.TurnIndex, Rank: unit.RenderedBefore, Rendered: unit.FoldRows}
	}
	return page, directory
}

func TestCanonicalDocVisibleObjectJSONRandomized(test *testing.T) {
	generator := rand.New(rand.NewSource(8842))
	for caseIndex := 0; caseIndex < 96; caseIndex++ {
		data := make([]byte, generator.Intn(4097))
		_, _ = generator.Read(data)
		page, directory := docVisibleJSONShapesFromBytes(data)
		assertDocVisibleJSONParity(test, page)
		assertDocVisibleJSONParity(test, directory)
	}
}

func FuzzCanonicalDocVisibleObjectJSON(fuzzer *testing.F) {
	for _, seed := range [][]byte{nil, {0}, {1}, {127}, {128}, {129}, bytes.Repeat([]byte{0xff}, 256), []byte("<>&\"\\\n한글\u2028\xff")} {
		fuzzer.Add(seed)
	}
	fuzzer.Fuzz(func(test *testing.T, data []byte) {
		if len(data) > 16<<10 {
			return
		}
		page, directory := docVisibleJSONShapesFromBytes(data)
		assertDocVisibleJSONParity(test, page)
		assertDocVisibleJSONParity(test, directory)
	})
}

func BenchmarkCanonicalDocVisibleObjectJSON(benchmark *testing.B) {
	page := docVisiblePage{Start: 256, Units: make([]DocVisibleUnit, 128)}
	directory := docVisibleDirectory{Level: 2, Children: make([]docVisibleSpan, 128)}
	for entryIndex := range page.Units {
		page.Units[entryIndex] = DocVisibleUnit{From: entryIndex, To: entryIndex + 1, EventIndex: entryIndex, TurnIndex: entryIndex / 4, RenderedBefore: entryIndex, FoldRows: 0}
		directory.Children[entryIndex] = docVisibleSpan{Hash: HashContent([]byte(fmt.Sprintf("span-%d", entryIndex))), First: entryIndex * 128, Limit: (entryIndex + 1) * 128, From: entryIndex * 128, To: (entryIndex + 1) * 128, Rank: entryIndex, Rendered: entryIndex + 128}
	}
	for _, shape := range []struct {
		name  string
		value any
	}{{"page-128", page}, {"directory-128", directory}} {
		canonical := assertDocVisibleJSONParity(benchmark, shape.value)
		for _, encoder := range []struct {
			name   string
			encode func(any) ([]byte, error)
		}{{"old", canonicalJSON}, {"new", canonicalDocVisibleObjectJSON}} {
			benchmark.Run(shape.name+"/"+encoder.name, func(benchmark *testing.B) {
				benchmark.ReportAllocs()
				benchmark.SetBytes(int64(len(canonical)))
				benchmark.ResetTimer()
				for iteration := 0; iteration < benchmark.N; iteration++ {
					encoded, err := encoder.encode(shape.value)
					if err != nil {
						benchmark.Fatal(err)
					}
					docVisibleJSONBenchmarkOutput = encoded
				}
			})
		}
	}
}
