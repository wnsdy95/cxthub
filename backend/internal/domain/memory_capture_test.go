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

func memoryCaptureTestObject(test *testing.T, digest MemoryDigest) []byte {
	test.Helper()
	raw, err := json.Marshal(digest)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func memoryCaptureTestDigest() MemoryDigest {
	snapshot := HashContent([]byte("memory owner"))
	return MemoryDigest{
		SnapshotID: snapshot, PreviousMemoryHash: HashContent([]byte("previous memory")),
		Summary: "bounded <memory> & \"summary\"\n", KeyFacts: []string{"fact"}, OpenTasks: []string{}, Provider: ProviderCodex,
		ClaimsVersion: MemoryClaimsVersion,
		Fragments:     []MemoryFragment{{SourceSnapshot: snapshot, Claims: []MemoryClaim{{Kind: "decision", Text: "keep captured inputs bounded"}}}},
		GraftCoverage: &MemoryGraftCoverage{
			ProjectionVersion: MemoryProjectionVersion, GraftSeq: 1,
			ProjectionComplete: true, LineageFingerprint: snapshot, GraftParents: []ContentHash{snapshot}, PinnedSources: []ContentHash{snapshot},
		},
	}
}

func TestDecodeMemoryCaptureObjectRoundTrip(test *testing.T) {
	for _, digest := range []MemoryDigest{{}, memoryCaptureTestDigest(), {KeyFacts: make([]string, 4090)}, {KeyFacts: make([]string, MaxMemoryCaptureObjects)}} {
		raw := memoryCaptureTestObject(test, digest)
		original := bytes.Clone(raw)
		decoded, err := DecodeMemoryCaptureObject(context.Background(), raw)
		if err != nil || !reflect.DeepEqual(decoded, digest) || !bytes.Equal(raw, original) {
			test.Fatal("logical memory round trip changed content", err)
		}
	}
	digest, err := DecodeMemoryCaptureObject(context.Background(), []byte(`{"summary":"legacy","key_facts":null,"open_tasks":null}`))
	if err != nil || digest.SnapshotID != "" || digest.PreviousMemoryHash != "" || digest.Summary != "legacy" {
		test.Fatal("optional legacy references rejected", err)
	}
}

func TestDecodeMemoryCaptureObjectRejectsInvalidContent(test *testing.T) {
	invalid := [][]byte{
		nil, []byte(`[]`), []byte(`null`), []byte(`{} {}`), []byte(`{"summary":`),
		[]byte(`{"unknown":"value"}`), []byte(`{"summary":"first","summary":"second"}`),
		[]byte(`{"key_facts":[{}]}`), []byte("{\"summary\":\"\xff\"}"),
	}
	mutations := []func(*MemoryDigest){
		func(digest *MemoryDigest) { digest.SnapshotID = "invalid" },
		func(digest *MemoryDigest) { digest.PreviousMemoryHash = "invalid" },
		func(digest *MemoryDigest) { digest.Fragments[0].SourceSnapshot = "" },
		func(digest *MemoryDigest) { digest.Fragments[0].SourceSnapshot = "invalid" },
		func(digest *MemoryDigest) { digest.ClaimsVersion++ },
		func(digest *MemoryDigest) { digest.ClaimsVersion = 0 },
		func(digest *MemoryDigest) { digest.Fragments[0].Claims[0].Text = "" },
		func(digest *MemoryDigest) { digest.Fragments[0].Claims[0].Kind = "unknown" },
		func(digest *MemoryDigest) { digest.GraftCoverage.ProjectionVersion = 0 },
		func(digest *MemoryDigest) { digest.GraftCoverage.GraftSeq = MaxGraftSeq + 1 },
		func(digest *MemoryDigest) { digest.GraftCoverage.LineageFingerprint = "" },
		func(digest *MemoryDigest) { digest.GraftCoverage.LineageFingerprint = "invalid" },
		func(digest *MemoryDigest) { digest.GraftCoverage.GraftParents[0] = "invalid" },
		func(digest *MemoryDigest) { digest.GraftCoverage.PinnedSources[0] = "invalid" },
	}
	for _, mutate := range mutations {
		digest := memoryCaptureTestDigest()
		mutate(&digest)
		invalid = append(invalid, memoryCaptureTestObject(test, digest))
	}
	for index, raw := range invalid {
		test.Run(fmt.Sprint(index), func(test *testing.T) {
			if digest, err := DecodeMemoryCaptureObject(context.Background(), raw); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(digest, MemoryDigest{}) {
				test.Fatal("invalid memory object decoded", err)
			}
		})
	}
}

func TestCountMemoryCaptureRecordsArrayBudget(test *testing.T) {
	for _, count := range []int{MaxMemoryCaptureObjects, MaxMemoryCaptureObjects + 1} {
		raw, err := json.Marshal(make([]string, count))
		if err != nil {
			test.Fatal(err)
		}
		records := 0
		err = CountMemoryCaptureRecords(context.Background(), bytes.NewReader(raw), &records)
		if count == MaxMemoryCaptureObjects && (err != nil || records != count) {
			test.Fatal("exact root-array budget rejected", records, err)
		}
		if count > MaxMemoryCaptureObjects && !errors.Is(err, ErrMemoryProjectionLimit) {
			test.Fatal("oversized array accepted", records, err)
		}
	}
	records := 0
	if err := CountMemoryCaptureRecords(context.Background(), strings.NewReader(`{"key_facts":["fact"],"open_tasks":[],"summary":"body","fragments":[{"key_facts":["nested"]}]}`), &records); err != nil || records != 3 {
		test.Fatal("object fields consumed array-entry budget", records, err)
	}
	remaining, err := json.Marshal(make([]string, MaxMemoryCaptureObjects-records))
	if err != nil {
		test.Fatal(err)
	}
	if err := CountMemoryCaptureRecords(context.Background(), bytes.NewReader(remaining), &records); err != nil || records != MaxMemoryCaptureObjects {
		test.Fatal("root/fragment streams did not share array budget", records, err)
	}
	if err := CountMemoryCaptureRecords(context.Background(), strings.NewReader(`[0]`), &records); !errors.Is(err, ErrMemoryProjectionLimit) {
		test.Fatal("shared array-entry budget exceeded", err)
	}
	records = 0
	if err := CountMemoryCaptureRecords(context.Background(), strings.NewReader(`[1e10000]`), &records); err != nil || records != 1 {
		test.Fatal("number tokens were converted to float64", records, err)
	}
}

func TestCountMemoryCaptureRecordsMalformed(test *testing.T) {
	for _, raw := range []string{``, `{"fragments":[`, `[{]`, `{"key_facts":["unfinished"`, `]`, `{1:2}`, `{} {}`, `true false`} {
		records := 0
		if err := CountMemoryCaptureRecords(context.Background(), strings.NewReader(raw), &records); !errors.Is(err, ErrIntegrity) {
			test.Fatal("malformed stream accepted", raw, err)
		}
	}
	for _, records := range []int{-1, MaxMemoryCaptureObjects + 1} {
		if err := CountMemoryCaptureRecords(context.Background(), strings.NewReader(`[]`), &records); err == nil {
			test.Fatal("invalid initial record count accepted", records)
		}
	}
	if err := CountMemoryCaptureRecords(context.Background(), strings.NewReader(`[]`), nil); !errors.Is(err, ErrValidation) {
		test.Fatal("nil counter accepted", err)
	}
}

func TestMemoryCaptureObjectFieldAndDepthBudgets(test *testing.T) {
	for _, count := range []int{64, 65} {
		fields := make([]string, count)
		for index := range fields {
			fields[index] = fmt.Sprintf(`"field%d":{}`, index)
		}
		raw := "{" + strings.Join(fields, ",") + "}"
		records := 0
		err := CountMemoryCaptureRecords(context.Background(), strings.NewReader(raw), &records)
		if count == 64 && (err != nil || records != 0) {
			test.Fatal("object field budget charged as array entries", records, err)
		}
		if count > 64 {
			if !errors.Is(err, ErrMemoryProjectionLimit) {
				test.Fatal("oversized object accepted", err)
			}
			if digest, err := DecodeMemoryCaptureObject(context.Background(), []byte(raw)); !errors.Is(err, ErrMemoryProjectionLimit) || !reflect.DeepEqual(digest, MemoryDigest{}) {
				test.Fatal("unknown-field expansion reached strict decoder", err)
			}
		}
	}
	for _, depth := range []int{64, 65} {
		for _, raw := range []string{
			strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth),
			strings.Repeat(`{"nested":`, depth) + "0" + strings.Repeat("}", depth),
		} {
			records := 0
			err := CountMemoryCaptureRecords(context.Background(), strings.NewReader(raw), &records)
			if depth == 64 && err != nil {
				test.Fatal("exact depth rejected", err)
			}
			if depth > 64 && !errors.Is(err, ErrMemoryProjectionLimit) {
				test.Fatal("excessive depth accepted", err)
			}
			if depth > 64 {
				if _, err := DecodeMemoryCaptureObject(context.Background(), []byte(raw)); !errors.Is(err, ErrMemoryProjectionLimit) {
					test.Fatal("deep object reached strict decoder", err)
				}
			}
		}
	}
}

func TestMemoryCaptureObjectHostileExpansion(test *testing.T) {
	for _, raw := range []string{
		`{"fragments":[` + strings.Repeat(`{},`, 1<<20) + `{}]}`,
		`{` + strings.Repeat(`"unknown":{},`, 1<<20) + `"last":{}}`,
	} {
		reader := strings.NewReader(raw)
		records := 0
		if err := CountMemoryCaptureRecords(context.Background(), reader, &records); !errors.Is(err, ErrMemoryProjectionLimit) || reader.Len() < len(raw)/2 {
			test.Fatal("record preflight scanned entire hostile expansion", reader.Len(), err)
		}
		if digest, err := DecodeMemoryCaptureObject(context.Background(), []byte(raw)); !errors.Is(err, ErrMemoryProjectionLimit) || !reflect.DeepEqual(digest, MemoryDigest{}) {
			test.Fatal("hostile expansion reached typed decoder", err)
		}
	}
	if digest, err := DecodeMemoryCaptureObject(context.Background(), make([]byte, MaxMemoryCaptureObjectBytes+1)); !errors.Is(err, ErrMemoryProjectionLimit) || !reflect.DeepEqual(digest, MemoryDigest{}) {
		test.Fatal("oversized object reached record scan", err)
	}
}

func TestMemoryCaptureObjectCancellation(test *testing.T) {
	raw := memoryCaptureTestObject(test, memoryCaptureTestDigest())
	for _, operation := range []string{"count", "decode"} {
		test.Run(operation, func(test *testing.T) {
			run := func(ctx context.Context) error {
				if operation == "count" {
					records := 0
					return CountMemoryCaptureRecords(ctx, bytes.NewReader(raw), &records)
				}
				digest, err := DecodeMemoryCaptureObject(ctx, raw)
				if err != nil && !reflect.DeepEqual(digest, MemoryDigest{}) {
					test.Fatal("canceled decoder returned partial state")
				}
				return err
			}
			counter := &memoryCaptureTestContext{Context: context.Background()}
			if err := run(counter); err != nil {
				test.Fatal(err)
			}
			for cancelAfter := 1; cancelAfter <= counter.checks; cancelAfter++ {
				ctx := &memoryCaptureTestContext{Context: context.Background(), cancelAfter: cancelAfter}
				if err := run(ctx); !errors.Is(err, context.Canceled) {
					test.Fatal("missed object cancellation checkpoint", cancelAfter, err)
				}
			}
		})
	}
}
