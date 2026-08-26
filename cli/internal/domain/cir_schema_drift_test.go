package domain

import (
	"encoding/json"
	"os"
	"testing"
)

func TestCIRSchemaDocumentsCompactionWireFields(t *testing.T) {
	raw, err := os.ReadFile("../../../schemas/cir.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("invalid CIR schema JSON: %v", err)
	}
	defs, ok := schema["$defs"].(map[string]interface{})
	if !ok {
		t.Fatal("CIR schema has no $defs")
	}
	envelope := defs["envelope"].(map[string]interface{})
	properties := envelope["properties"].(map[string]interface{})
	if _, ok := properties["compaction_count"]; !ok {
		t.Fatal("CIR schema omits envelope.compaction_count emitted by Go domains")
	}
	versions := properties["cir_version"].(map[string]interface{})["enum"].([]interface{})
	if len(versions) != 2 || versions[0] != CIRVersionV1 || versions[1] != CIRVersionV2 {
		t.Fatalf("CIR schema versions = %v, want [1 2]", versions)
	}
	if _, ok := defs["compactionEvent"]; !ok {
		t.Fatal("CIR schema omits compaction event definition")
	}
	event := defs["event"].(map[string]interface{})
	if allowed, ok := event["unevaluatedProperties"].(bool); !ok || allowed {
		t.Fatal("CIR event union permits fields from the wrong event kind")
	}
	rootConditions, conditionsOK := schema["allOf"].([]interface{})
	if _, ok := defs["v1Event"]; !ok || !conditionsOK || len(rootConditions) == 0 {
		t.Fatal("CIR schema omits the v1 envelope/event compatibility condition")
	}
	compaction := defs["compactionEvent"].(map[string]interface{})["properties"].(map[string]interface{})
	if _, ok := compaction["replacement_complete"]; !ok {
		t.Fatal("CIR schema omits compaction.replacement_complete")
	}
	message := defs["messageEvent"].(map[string]interface{})["properties"].(map[string]interface{})
	for _, field := range []string{"agent_message", "agent_author", "agent_recipient", "locked"} {
		if _, ok := message[field]; !ok {
			t.Fatalf("CIR schema omits message.%s", field)
		}
	}
	eventBase := defs["eventBase"].(map[string]interface{})
	if _, ok := eventBase["properties"].(map[string]interface{})["provider_metadata"]; !ok {
		t.Fatal("CIR schema omits event.provider_metadata")
	}
	kind := eventBase["properties"].(map[string]interface{})["kind"].(map[string]interface{})
	found := false
	for _, value := range kind["enum"].([]interface{}) {
		if value == EventCompaction {
			found = true
		}
	}
	if !found {
		t.Fatal("CIR event kind enum omits compaction")
	}
}
