package domain

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func marshalDocDisplayPageReflectOracle(page DocDisplayPage) ([]byte, error) {
	type runJSON struct {
		Linear     bool `json:"linear"`
		SourceFrom int  `json:"source_from"`
		SourceTo   int  `json:"source_to"`
		To         int  `json:"to"`
	}
	var runs []runJSON
	if page.Runs != nil {
		runs = make([]runJSON, len(page.Runs))
	}
	for index, run := range page.Runs {
		runs[index] = runJSON{Linear: run.Linear, SourceFrom: run.SourceFrom, SourceTo: run.SourceTo, To: run.To}
	}
	return json.Marshal(struct {
		From    int         `json:"from"`
		Index   int         `json:"index"`
		Runs    []runJSON   `json:"runs"`
		Source  ContentHash `json:"source"`
		Text    string      `json:"text"`
		Version int         `json:"version"`
	}{From: page.From, Index: page.Index, Runs: runs, Source: page.Source, Text: page.Text, Version: page.Version})
}

func TestDocDisplayPageAppendEncoderPreservesFullRunPage(test *testing.T) {
	source, original := docDisplayPageDecodeFixture(test, strings.Repeat("a\U0001f642<&>\n", 200))
	var page DocDisplayPage
	if err := json.Unmarshal(original, &page); err != nil || len(page.Runs) != DocDisplayPageRuns {
		test.Fatal("fixture did not retain the full run count", err)
	}
	want, err := marshalDocDisplayPageReflectOracle(page)
	if err != nil || !bytes.Equal(want, original) {
		test.Fatal("original page differs from the previous encoder", err)
	}
	got := assertDocDisplayPageJSONParity(test, page)
	if !bytes.Equal(got, want) || HashContent(got) != HashContent(original) {
		test.Fatal("append encoder changed canonical bytes or hash")
	}
	if _, err := DecodeDocDisplayPage(source, HashContent(original), got); err != nil {
		test.Fatal("append encoding is not accepted by the existing decoder", err)
	}
}

func BenchmarkDocDisplayPageEncodeRuns(benchmark *testing.B) {
	_, raw := docDisplayPageDecodeFixture(benchmark, strings.Repeat("a\U0001f642<&>\n", 200))
	var page DocDisplayPage
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Runs) != DocDisplayPageRuns {
		benchmark.Fatal("fixture did not retain the full run count", err)
	}
	for _, candidate := range []struct {
		name   string
		encode func(DocDisplayPage) ([]byte, error)
	}{
		{"previous-reflect", marshalDocDisplayPageReflectOracle},
		{"bounded-append", marshalDocDisplayPage},
	} {
		benchmark.Run(candidate.name, func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			benchmark.SetBytes(int64(len(raw)))
			for benchmark.Loop() {
				encoded, err := candidate.encode(page)
				if err != nil || !bytes.Equal(encoded, raw) {
					benchmark.Fatal("encoder changed stored bytes", err)
				}
			}
		})
	}
}
