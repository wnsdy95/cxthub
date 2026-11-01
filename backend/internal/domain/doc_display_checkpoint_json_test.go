package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func assertDocDisplayCheckpointJSONParity(test testing.TB, state DocDisplayProjectionState) {
	test.Helper()
	before, beforeErr := json.Marshal(state)
	want, wantErr := canonicalJSON(state)
	got, gotErr := marshalDocDisplayProjectionState(state)
	if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || reflect.TypeOf(gotErr) != reflect.TypeOf(wantErr) || !bytes.Equal(got, want) || (got == nil) != (want == nil) {
		test.Fatalf("checkpoint canonical encoding mismatch: got %q (%v), want %q (%v)", got, gotErr, want, wantErr)
	}
	after, afterErr := json.Marshal(state)
	if !bytes.Equal(before, after) || fmt.Sprint(beforeErr) != fmt.Sprint(afterErr) {
		test.Fatal("checkpoint serializer mutated the source frontier")
	}
}

func TestMarshalDocDisplayProjectionStateCanonicalJSON(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	for caseIndex, state := range []DocDisplayProjectionState{
		{},
		{Directory: DocRangeDirectoryAccumulator{Levels: [][]ContentHash{}}},
		{Version: 1, Source: HashContent([]byte("source")), TextBytes: 512, Directory: DocRangeDirectoryAccumulator{Count: 1, Levels: [][]ContentHash{{HashContent([]byte("page"))}}}},
		{Version: 1, Source: HashContent([]byte("source")), Done: true, Directory: DocRangeDirectoryAccumulator{Count: 128, Levels: [][]ContentHash{nil, {HashContent([]byte("directory"))}}}},
		{Directory: DocRangeDirectoryAccumulator{Levels: [][]ContentHash{nil, {}, {"<>&\u2028\u2029", "한글🙂\x00\"\\/", "\ufffd"}}}},
		{Version: maximum, Source: "not-a-hash", TextBytes: -maximum - 1, Directory: DocRangeDirectoryAccumulator{Count: maximum, Levels: [][]ContentHash{{"unvalidated"}}}},
		{Source: "\xff", Directory: DocRangeDirectoryAccumulator{Levels: [][]ContentHash{{"valid"}}}},
		{Directory: DocRangeDirectoryAccumulator{Levels: [][]ContentHash{{"valid", "\xff\xed\xa0\x80\ufffd"}}}},
	} {
		test.Run(fmt.Sprint(caseIndex), func(test *testing.T) {
			assertDocDisplayCheckpointJSONParity(test, state)
		})
	}
}

func TestDocDisplayProjectionCheckpointRejectsNoncanonicalWire(test *testing.T) {
	source := projectionTestSource(512)
	state, err := NewDocDisplayProjectionState(source)
	if err != nil {
		test.Fatal(err)
	}
	raw, err := state.Encode(source)
	if err != nil {
		test.Fatal(err)
	}
	for _, malformed := range [][]byte{
		append(bytes.Clone(raw), '\n'),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"extra":null`), 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":0,"version":1`), 1),
		bytes.Replace(raw, []byte(`"count":0`), []byte(`"count":-1`), 1),
	} {
		got, err := DecodeDocDisplayProjectionState(source, HashContent(malformed), malformed)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, DocDisplayProjectionState{}) {
			test.Fatalf("noncanonical checkpoint accepted: %s (%v)", malformed, err)
		}
	}
}

func FuzzMarshalDocDisplayProjectionStateCanonicalJSON(fuzz *testing.F) {
	fuzz.Add("source", "page", int64(1), uint8(1), false)
	fuzz.Add("\xff", "\ufffd\xed\xa0\x80", int64(1<<53+1), uint8(2), true)
	fuzz.Add("<>&", "한글🙂\x00\"\\/", int64(-1<<63), uint8(0), false)
	fuzz.Fuzz(func(test *testing.T, source, hash string, number int64, shape uint8, done bool) {
		var levels [][]ContentHash
		if shape != 0 {
			levels = make([][]ContentHash, int(shape)%4)
			for level := range levels {
				if level%2 == 0 {
					levels[level] = []ContentHash{ContentHash(hash), ""}
				}
			}
		}
		assertDocDisplayCheckpointJSONParity(test, DocDisplayProjectionState{Version: int(number), Source: ContentHash(source), TextBytes: ^int(number), Directory: DocRangeDirectoryAccumulator{Count: int(shape), Levels: levels}, Done: done})
	})
}

func BenchmarkDocDisplayProjectionCheckpointJSON(benchmark *testing.B) {
	hash := ContentHash("sha256:" + strings.Repeat("a", 64))
	level := make([]ContentHash, DocRangeDirectoryFanout-1)
	for index := range level {
		level[index] = hash
	}
	state := DocDisplayProjectionState{Version: 1, Source: hash, TextBytes: (DocRangeDirectoryFanout*DocRangeDirectoryFanout - 1) * DocDisplayPageRuns,
		Directory: DocRangeDirectoryAccumulator{Count: DocRangeDirectoryFanout*DocRangeDirectoryFanout - 1, Levels: [][]ContentHash{level, level}}}
	for _, candidate := range []struct {
		name   string
		encode func(DocDisplayProjectionState) ([]byte, error)
	}{
		{"oracle", func(state DocDisplayProjectionState) ([]byte, error) { return canonicalJSON(state) }},
		{"typed", marshalDocDisplayProjectionState},
	} {
		benchmark.Run(candidate.name, func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			for benchmark.Loop() {
				if raw, err := candidate.encode(state); err != nil || len(raw) == 0 {
					benchmark.Fatal(err)
				}
			}
		})
	}
}
