package domain

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestReadBlocksPreserveExactCoordinatesAndStablePrefixes(t *testing.T) {
	texts := make([]string, ReadIndexBlockEvents*2+1)
	for i := range texts {
		texts[i] = fmt.Sprintf("event %d %s", i, strings.Repeat("x", i%17))
	}
	_, base := prefixFixture(t, texts...)
	p, err := base.PlanReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	want, err := p.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	blocks := p.Blocks()
	var got []DocEventIndex
	for _, b := range blocks {
		idx, err := b.Build(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(idx.Events) != b.Count || b.Count > ReadIndexBlockEvents {
			t.Fatal("unbounded block")
		}
		for _, e := range idx.Events {
			e.Index += b.FirstEvent
			e.Offset += b.Offset
			got = append(got, e)
		}
	}
	if !reflect.DeepEqual(got, want.Events) {
		t.Fatal("coordinates/text changed")
	}
	_, next := prefixFixture(t, append(texts, "appended")...)
	np, _ := next.PlanReadIndex()
	nb := np.Blocks()
	if blocks[0].Hash != nb[0].Hash || blocks[1].Hash != nb[1].Hash || blocks[2].Hash == nb[2].Hash {
		t.Fatal("append did not reuse full prefix blocks")
	}
	// A longer first event moves later block byte offsets, but their local
	// contents/sequence remain unchanged and are safe to share.
	texts[0] += "different length"
	_, changed := prefixFixture(t, texts...)
	cp, _ := changed.PlanReadIndex()
	cb := cp.Blocks()
	if cb[0].Hash == blocks[0].Hash || cb[1].Hash != blocks[1].Hash || cb[1].Offset == blocks[1].Offset {
		t.Fatal("document and block coordinate spaces conflated")
	}
	_, empty := prefixFixture(t)
	ep, _ := empty.PlanReadIndex()
	if len(ep.Blocks()) != 0 || ep.EventCount() != 0 {
		t.Fatal("empty document has locations")
	}
}
