package domain

import (
	"bytes"
	"testing"
)

func TestDocRangeJSONFieldPositionSharesLastValueSemantics(testContext *testing.T) {
	const value = `"duplicate"`
	const prefix = `{"unknown":{"redacted_summary":"duplicate"},"redacted_summary":"old","locked":[{"blob":"duplicate"}],"redacted\u005fsummary" : `
	raw := []byte(prefix + value + `,"other":"duplicate"}`)
	got, from, to := docRangeJSONFieldPosition(raw, "redacted_summary")
	if from != len(prefix) || to != len(prefix)+len(value) || string(got) != value || !bytes.Equal(got, docRangeJSONField(raw, "redacted_summary")) {
		testContext.Fatal("field accessors disagree or returned nested/earlier bytes", string(got), from, to)
	}
	if &got[0] != &raw[from] {
		testContext.Fatal("field locator copied its value")
	}
	for _, body := range [][]byte{nil, []byte(`[]`), []byte(`{}`), []byte(`{"nested":{"redacted_summary":"duplicate"}}`)} {
		got, from, to := docRangeJSONFieldPosition(body, "redacted_summary")
		if got != nil || from != 0 || to != 0 || docRangeJSONField(body, "redacted_summary") != nil {
			testContext.Fatal("missing field yielded a value", string(body), string(got), from, to)
		}
	}
}
