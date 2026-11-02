package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func docDisplayPageDecodeFixture(test testing.TB, text string) (DocDisplayStringSource, []byte) {
	test.Helper()
	raw, err := json.Marshal(struct {
		Kind   string `json:"kind"`
		Output string `json:"output"`
	}{Kind: "tool_result", Output: text})
	if err != nil {
		test.Fatal(err)
	}
	source, err := BuildDocDisplayStringSourceReader(context.Background(), HashContent(raw), len(raw), bytes.NewReader(raw))
	if err != nil {
		test.Fatal(err)
	}
	prior, err := NewDocDisplayStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	step, err := StepDocDisplayString(context.Background(), source, prior, func(_ context.Context, offset, length int) ([]byte, error) {
		return raw[offset : offset+length], nil
	})
	if err != nil || step.Page == nil {
		test.Fatal("missing actual string page", err)
	}
	return source, step.Objects[len(step.Objects)-1].Bytes
}

func docDisplayPageDecodeOracle(source DocDisplayStringSource, expected ContentHash, raw []byte) (DocDisplayPage, error) {
	var page DocDisplayPage
	if len(raw) == 0 || len(raw) > MaxDocDisplayPageBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &page) != nil {
		return DocDisplayPage{}, ErrIntegrity
	}
	canonical, err := page.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDisplayPage{}, ErrIntegrity
	}
	return page, nil
}

func assertDocDisplayPageDecodeParity(test testing.TB, source DocDisplayStringSource, expected ContentHash, raw []byte) {
	test.Helper()
	before := bytes.Clone(raw)
	want, wantErr := docDisplayPageDecodeOracle(source, expected, raw)
	got, gotErr := DecodeDocDisplayPage(source, expected, raw)
	if !errors.Is(gotErr, wantErr) || !reflect.DeepEqual(got, want) || !bytes.Equal(before, raw) {
		test.Fatalf("page decoder changed: got %v (%v), want %v (%v), input %q", got, gotErr, want, wantErr, raw)
	}
}

func TestDocDisplayPageBoundedDecoderCanonicalParity(test *testing.T) {
	for _, text := range []string{"ab", strings.Repeat("a🙂<&>\n", 200), "a\x00\t\r\n\"\\<>&\u2028\u2029\ufffd\ud55c\uae00🙂", strings.Repeat("a", DocDisplayPageTextBytes)} {
		source, raw := docDisplayPageDecodeFixture(test, text)
		assertDocDisplayPageDecodeParity(test, source, HashContent(raw), raw)
		page, err := DecodeDocDisplayPage(source, HashContent(raw), raw)
		if err != nil {
			test.Fatal("valid canonical page was rejected", err)
		}
		encoded, err := page.Encode(source)
		if err != nil || !bytes.Equal(raw, encoded) {
			test.Fatal("bounded decoder changed canonical bytes", err)
		}
	}
}

func TestDocDisplayPageBoundedDecoderRejectsCorruptAndNoncanonicalRuns(test *testing.T) {
	source, raw := docDisplayPageDecodeFixture(test, strings.Repeat("a🙂<&>\n", 200))
	var page DocDisplayPage
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Runs) != DocDisplayPageRuns {
		test.Fatal("fixture did not reach the original run limit", err)
	}
	from := []byte(fmt.Sprintf(`"source_from":%d`, page.Runs[0].SourceFrom))
	for name, mutation := range map[string]struct{ before, after string }{
		"run-whitespace":   {`"runs":[`, `"runs":[ `},
		"field-whitespace": {`"linear":true`, `"linear" :true`},
		"null-boolean":     {`"linear":true`, `"linear":null`},
		"number-boolean":   {`"linear":true`, `"linear":1`},
		"duplicate-field":  {`"linear":true`, `"linear":false,"linear":true`},
		"unknown-field":    {`"linear":true`, `"linear":true,"extra":null`},
		"escaped-field":    {`"source_from":`, `"source_\u0066rom":`},
		"negative-offset":  {string(from), `"source_from":-1`},
		"overflow-offset":  {string(from), `"source_from":999999999999999999999999999`},
		"fraction-offset":  {string(from), string(from) + `.0`},
		"exponent-offset":  {string(from), string(from) + `e0`},
		"null-offset":      {string(from), `"source_from":null`},
		"leading-zero":     {string(from), `"source_from":0` + strings.TrimPrefix(string(from), `"source_from":`)},
	} {
		test.Run(name, func(test *testing.T) {
			changed := bytes.Replace(raw, []byte(mutation.before), []byte(mutation.after), 1)
			if bytes.Equal(raw, changed) {
				test.Fatal("corruption fixture did not change the page")
			}
			assertDocDisplayPageDecodeParity(test, source, HashContent(changed), changed)
			if got, err := DecodeDocDisplayPage(source, HashContent(changed), changed); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, DocDisplayPage{}) {
				test.Fatal("self-hashed noncanonical/corrupt run was accepted", got, err)
			}
		})
	}
	var wire struct {
		Runs json.RawMessage `json:"runs"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		test.Fatal(err)
	}
	tooMany := page
	tooMany.Runs = append(append([]DocDisplayRun(nil), page.Runs...), page.Runs[0])
	overflow, err := marshalDocDisplayPage(tooMany)
	if err != nil {
		test.Fatal(err)
	}
	for _, changed := range [][]byte{
		bytes.Replace(raw, wire.Runs, []byte("null"), 1),
		bytes.Replace(raw, wire.Runs, []byte("[]"), 1),
		bytes.Replace(raw, wire.Runs, []byte("[{}]"), 1),
		overflow, append(bytes.Clone(raw), '\n'), raw[:len(raw)-1],
	} {
		assertDocDisplayPageDecodeParity(test, source, HashContent(changed), changed)
		if got, err := DecodeDocDisplayPage(source, HashContent(changed), changed); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, DocDisplayPage{}) {
			test.Fatal("invalid page escaped its original bounds", got, err)
		}
	}
	assertDocDisplayPageDecodeParity(test, source, HashContent([]byte("different bytes")), raw)
	changed := bytes.Clone(raw)
	changed[len(changed)/2] ^= 1
	assertDocDisplayPageDecodeParity(test, source, HashContent(raw), changed)
	foreign := source
	foreign.EventHash = HashContent([]byte("different source"))
	assertDocDisplayPageDecodeParity(test, foreign, HashContent(raw), raw)
}

func FuzzDocDisplayPageBoundedDecoderOracle(fuzz *testing.F) {
	source, raw := docDisplayPageDecodeFixture(fuzz, "ab")
	for _, seed := range [][]byte{raw, {}, []byte("null"), []byte(`{"runs":[]}`), append(bytes.Clone(raw), '\n'), raw[:len(raw)/2]} {
		fuzz.Add(seed)
	}
	fuzz.Fuzz(func(test *testing.T, raw []byte) {
		assertDocDisplayPageDecodeParity(test, source, HashContent(raw), raw)
	})
}

func BenchmarkDocDisplayPageBoundedDecode(benchmark *testing.B) {
	source, raw := docDisplayPageDecodeFixture(benchmark, strings.Repeat("a🙂<&>\n", 200))
	hash := HashContent(raw)
	for _, candidate := range []struct {
		name   string
		decode func(DocDisplayStringSource, ContentHash, []byte) (DocDisplayPage, error)
	}{
		{"json-oracle", docDisplayPageDecodeOracle},
		{"bounded-runs", DecodeDocDisplayPage},
	} {
		benchmark.Run(candidate.name, func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			for benchmark.Loop() {
				if page, err := candidate.decode(source, hash, raw); err != nil || len(page.Runs) != DocDisplayPageRuns {
					benchmark.Fatal(err)
				}
			}
		})
	}
}
