package domain

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestBranchContextMergeAcknowledgementWire(test *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		merge := BranchContextMerge{DeliveryAcknowledged: acknowledged, EventID: "synthetic-event"}
		raw, err := json.Marshal(merge)
		if err != nil {
			test.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			test.Fatal(err)
		}
		want, _ := json.Marshal(acknowledged)
		if !bytes.Equal(fields["delivery_acknowledged"], want) {
			test.Fatalf("required acknowledgement = %s, want %s", fields["delivery_acknowledged"], want)
		}
		var decoded BranchContextMerge
		if err := json.Unmarshal(raw, &decoded); err != nil || !reflect.DeepEqual(decoded, merge) {
			test.Fatalf("current wire round trip: %+v, %v", decoded, err)
		}
		if decoded.PreserveLegacyReceiptEncoding() {
			test.Fatal("explicit acknowledgement treated as legacy")
		}
		again, err := json.Marshal(decoded)
		if err != nil || !bytes.Equal(again, raw) {
			test.Fatalf("current encoding changed: %s, %v", again, err)
		}
	}
}

func TestBranchContextMergeLegacyReceiptEncoding(test *testing.T) {
	raw := []byte(`{"destination_branch_id":"main","event_id":"synthetic-event","source":"","merge_sha":"","pr_number":1,"state":"included","reason":"synthetic","order":0}`)
	var merge BranchContextMerge
	if err := json.Unmarshal(raw, &merge); err != nil {
		test.Fatal(err)
	}
	current, err := json.Marshal(merge)
	if err != nil || !bytes.Contains(current, []byte(`"delivery_acknowledged":false`)) {
		test.Fatalf("ordinary output lost required false: %s, %v", current, err)
	}
	if !merge.PreserveLegacyReceiptEncoding() {
		test.Fatal("absent legacy acknowledgement was not preserved")
	}
	legacy, err := json.Marshal(merge)
	if err != nil || !bytes.Equal(legacy, raw) {
		test.Fatalf("legacy bytes changed: %s, %v", legacy, err)
	}
	merge.DeliveryAcknowledged = true
	acknowledged, err := json.Marshal(merge)
	if err != nil || !bytes.Contains(acknowledged, []byte(`"delivery_acknowledged":true`)) {
		test.Fatalf("true acknowledgement silently omitted: %s, %v", acknowledged, err)
	}
	if merge.PreserveLegacyReceiptEncoding() {
		test.Fatal("true acknowledgement opted into legacy encoding")
	}
	if err := json.Unmarshal(current, &merge); err != nil || merge.PreserveLegacyReceiptEncoding() {
		test.Fatalf("reused receiver retained legacy presence: %v", err)
	}
}

func TestBranchContextMergeRejectsAmbiguousAcknowledgement(test *testing.T) {
	for _, raw := range []string{
		`null`,
		`{"delivery_acknowledged":null}`,
		`{"delivery_acknowledged":"false"}`,
		`{"delivery_acknowledged":0}`,
		`{"delivery_acknowledged":true,"delivery_acknowledged":false}`,
		`{"delivery_acknowledged":false,"delivery_acknowledged":true}`,
		`{"delivery_acknowledged":true,"DELIVERY_ACKNOWLEDGED":false}`,
		`{"DELIVERY_ACKNOWLEDGED":true,"delivery_acknowledged":false}`,
	} {
		var merge BranchContextMerge
		if err := json.Unmarshal([]byte(raw), &merge); err == nil {
			test.Fatalf("ambiguous acknowledgement accepted: %s", raw)
		}
	}
}
