package domain

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestDocFindFieldsBlocksValidationAndCanonicalMetadata(testContext *testing.T) {
	eventHash := HashContent([]byte("source"))
	for _, span := range []*DocFindSpan{
		{From: -1, To: 2}, {From: 40, To: 39}, {From: 40, To: 40}, {From: 40, To: 41},
		{From: 40, To: 101}, {From: 101, To: 103}, {From: math.MaxInt, To: math.MinInt},
	} {
		fields := DocFindFields{Version: 1, EventHash: eventHash, EventBytes: 100, Blocks: span}
		if !errors.Is(fields.Validate(eventHash, 100), ErrIntegrity) {
			testContext.Fatal("invalid blocks span validated", span)
		}
		if raw, err := fields.Encode(); raw != nil || !errors.Is(err, ErrIntegrity) {
			testContext.Fatal("invalid blocks span encoded", raw, err)
		}
	}
	fields := DocFindFields{Version: 1, EventHash: eventHash, EventBytes: 100, Blocks: &DocFindSpan{From: 40, To: 60}}
	valid, err := fields.Encode()
	if err != nil {
		testContext.Fatal(err)
	}
	for _, projection := range []string{
		strings.Replace(string(valid), `"blocks":`, `"blocks":null,"blocks":`, 1),
		strings.Replace(string(valid), `"blocks":{`, `"blocks":{"unknown":"body",`, 1),
		strings.Replace(string(valid), `"summary":null`, `"summary":{"from":40,"to":60}`, 1),
	} {
		if decoded, err := DecodeDocFindFields(HashContent([]byte(projection)), eventHash, 100, []byte(projection)); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocFindFields{}) {
			testContext.Fatal("invalid blocks metadata decoded", projection, decoded, err)
		}
	}
	fields.Summary = &DocFindSpan{From: 10, To: 20}
	if encoded, err := fields.Encode(); encoded != nil || !errors.Is(err, ErrIntegrity) {
		testContext.Fatal("both public field types encoded", string(encoded), err)
	}
	fields.Summary, fields.Blocks = nil, nil
	valid, err = fields.Encode()
	if err != nil {
		testContext.Fatal(err)
	}
	projection := append([]byte(`{"blocks":null,`), valid[1:]...)
	if _, err := DecodeDocFindFields(HashContent(projection), eventHash, 100, projection); !errors.Is(err, ErrIntegrity) {
		testContext.Fatal("noncanonical explicit nil blocks accepted", err)
	}
}

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

func TestDocFindFieldsValidateAndEncodeRejectInvalidMetadata(testContext *testing.T) {
	eventHash := HashContent([]byte("source"))
	valid := DocFindFields{Version: 1, EventHash: eventHash, EventBytes: 100, Summary: &DocFindSpan{From: 40, To: 60}}
	for _, mutate := range []func(*DocFindFields){
		func(fields *DocFindFields) { fields.Version = 0 },
		func(fields *DocFindFields) { fields.Version = 2 },
		func(fields *DocFindFields) { fields.EventHash = "" },
		func(fields *DocFindFields) { fields.EventHash = "sha256:invalid" },
		func(fields *DocFindFields) { fields.EventBytes = 0 },
		func(fields *DocFindFields) { fields.EventBytes = -1 },
		func(fields *DocFindFields) { fields.Summary = &DocFindSpan{From: -1, To: 2} },
		func(fields *DocFindFields) { fields.Summary = &DocFindSpan{From: 40, To: 39} },
		func(fields *DocFindFields) { fields.Summary = &DocFindSpan{From: 40, To: 40} },
		func(fields *DocFindFields) { fields.Summary = &DocFindSpan{From: 40, To: 41} },
		func(fields *DocFindFields) { fields.Summary = &DocFindSpan{From: 40, To: 101} },
		func(fields *DocFindFields) { fields.Summary = &DocFindSpan{From: 101, To: 103} },
		func(fields *DocFindFields) { fields.Summary = &DocFindSpan{From: math.MaxInt, To: math.MinInt} },
	} {
		fields := valid
		mutate(&fields)
		if !errors.Is(fields.Validate(fields.EventHash, fields.EventBytes), ErrIntegrity) {
			testContext.Fatal("invalid metadata validated", fields)
		}
		if raw, err := fields.Encode(); !errors.Is(err, ErrIntegrity) || raw != nil {
			testContext.Fatal("invalid metadata encoded", string(raw), err)
		}
	}
	for _, binding := range []struct {
		hash  ContentHash
		bytes int
	}{
		{hash: "", bytes: valid.EventBytes},
		{hash: "sha256:invalid", bytes: valid.EventBytes},
		{hash: HashContent([]byte("other source")), bytes: valid.EventBytes},
		{hash: eventHash, bytes: valid.EventBytes - 1},
		{hash: eventHash, bytes: valid.EventBytes + 1},
		{hash: eventHash, bytes: 0},
		{hash: eventHash, bytes: -1},
	} {
		if !errors.Is(valid.Validate(binding.hash, binding.bytes), ErrIntegrity) {
			testContext.Fatal("incorrect source binding validated", binding)
		}
	}
}
