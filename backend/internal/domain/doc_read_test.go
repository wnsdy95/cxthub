package domain

import (
	"testing"
)

func TestDocReadIndexUsesCanonicalEventOrder(t *testing.T) {
	doc := SessionDoc{CIR: CIRDocument{Events: []CIREvent{
		{Kind: EventMessage, Seq: 9, Role: RoleAssistant, Blocks: []ContentBlock{{Type: "text", Text: "later"}}},
		{Kind: EventMessage, Seq: 1, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "earlier"}}},
	}}}
	raw, err := CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = HashContent(raw)
	idx, err := BuildDocReadIndex(doc)
	if err != nil {
		t.Fatal(err)
	}
	_, bodies, err := splitCanonicalDocBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i, item := range idx.Events {
		ev, err := DecodeIndexedEvent(bodies[i], item)
		if err != nil {
			t.Fatal(err)
		}
		if item.Seq != ev.Seq || item.Role != string(ev.Role) || item.Text != SearchableEventText(ev) {
			t.Fatalf("index metadata disagrees with its own canonical event: index=%+v event=%+v", item, ev)
		}
	}
	if doc.CIR.Events[0].Seq != 9 {
		t.Fatal("index creation reordered caller input")
	}
}
