package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBranchContextHistoricalDeliveryWireIsExplicitAndFailClosed(t *testing.T) {
	var legacy BranchContextMerge
	if err := json.Unmarshal([]byte(`{"state":"included","source":"old","merge_sha":"old"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.DeliveryAcknowledged {
		t.Fatal("legacy inclusion invented an acknowledgement")
	}
	for _, acknowledged := range []bool{false, true} {
		merge := BranchContextMerge{DeliveryAcknowledged: acknowledged, State: "not_selected"}
		raw, err := json.Marshal(merge)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"delivery_acknowledged":`) {
			t.Fatalf("missing explicit acknowledgement: %s", raw)
		}
		var decoded BranchContextMerge
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded.DeliveryAcknowledged != acknowledged || decoded.State != "not_selected" {
			t.Fatalf("wire changed acknowledgement or inclusion: %+v %v", decoded, err)
		}
	}
}
