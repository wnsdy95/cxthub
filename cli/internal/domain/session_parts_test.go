package domain

import (
	"reflect"
	"testing"
)

func TestSessionPartsAreCutWhereTheirRecordsReachTheLimit(t *testing.T) {
	for name, test := range map[string]struct {
		sizes   []int
		records []int
		sealed  bool
	}{
		"no records":                               {nil, nil, false},
		"one record under the limit":               {[]int{5}, []int{1}, false},
		"one record at the limit":                  {[]int{6}, []int{1}, true},
		"one record over the limit":                {[]int{10}, []int{1}, true},
		"records that reach the limit together":    {[]int{3, 3}, []int{2}, true},
		"a part is sealed and one record is left":  {[]int{3, 3, 3, 3, 1}, []int{2, 2, 1}, false},
		"a large record seals the part it is in":   {[]int{1, 10, 2, 2, 2}, []int{2, 3}, true},
		"records of no size never reach the limit": {[]int{0, 0, 0}, []int{3}, false},
	} {
		if records, sealed := CutSessionParts(test.sizes, 6); !reflect.DeepEqual(records, test.records) || sealed != test.sealed {
			t.Errorf("%s: %v sealed=%v", name, records, sealed)
		}
	}
}

// Cutting what follows a sealed part gives the cuts that cutting the whole
// session gives: a run that goes on from what was remembered builds the parts
// a run from the start builds.
func TestCuttingAfterASealedPartGivesTheSameCuts(t *testing.T) {
	// Sizes from a fixed sequence, some of them larger than the limit.
	sizes, value := make([]int, 400), uint32(7)
	for index := range sizes {
		value = value*1664525 + 1013904223
		sizes[index] = int(value>>8) % 90
	}
	const limit = 200
	whole, sealed := CutSessionParts(sizes, limit)
	if len(whole) < 20 {
		t.Fatalf("the sequence gives %d parts", len(whole))
	}
	at := 0
	for border := 0; border < len(whole); border++ {
		rest, restSealed := CutSessionParts(sizes[at:], limit)
		if !reflect.DeepEqual(rest, whole[border:]) || restSealed != sealed {
			t.Fatalf("after %d sealed parts: %v sealed=%v, the whole gives %v sealed=%v", border, rest, restSealed, whole[border:], sealed)
		}
		at += whole[border]
	}
	// A session that goes on: the parts sealed before stay as they were cut.
	for shorter := 1; shorter < len(sizes); shorter += 37 {
		earlier, earlierSealed := CutSessionParts(sizes[:shorter], limit)
		kept := len(earlier)
		if !earlierSealed {
			kept--
		}
		if !reflect.DeepEqual(earlier[:kept], whole[:kept]) {
			t.Fatalf("with %d records the sealed parts are %v, with all of them %v", shorter, earlier[:kept], whole[:kept])
		}
	}
}

func TestSessionRecordChainNamesTheRecordsAndTheirOrder(t *testing.T) {
	chain := SessionRecordChain("", []string{"a", "b"})
	for name, other := range map[string]string{
		"another order":           SessionRecordChain("", []string{"b", "a"}),
		"one record fewer":        SessionRecordChain("", []string{"a"}),
		"one record more":         SessionRecordChain("", []string{"a", "b", "c"}),
		"the same letters":        SessionRecordChain("", []string{"ab"}),
		"records moved over":      SessionRecordChain("", []string{"a", "", "b"}),
		"another chain before it": SessionRecordChain(SessionRecordChain("", []string{"z"}), []string{"a", "b"}),
		"no records":              SessionRecordChain("", nil),
	} {
		if other == chain {
			t.Errorf("%s has the chain of the records a, b", name)
		}
	}
	if again := SessionRecordChain("", []string{"a", "b"}); again != chain || len(chain) != 64 {
		t.Fatalf("the same records give %q and %q", chain, again)
	}
}

func TestKeptSessionPartsAreTheLeadingPartsWhoseRecordsAreTheSame(t *testing.T) {
	ids := []string{"r1", "r2", "r3", "r4", "r5", "r6", "r7"}
	// Three sealed parts of two records each; the seventh record is open.
	var remembered []SessionPartState
	chain := ""
	for part := 0; part < 3; part++ {
		chain = SessionRecordChain(chain, ids[2*part:2*part+2])
		remembered = append(remembered, SessionPartState{Chain: chain, Counts: []int{1, 1}})
	}
	change := func(edit func([]SessionPartState)) []SessionPartState {
		copied := append([]SessionPartState(nil), remembered...)
		edit(copied)
		return copied
	}
	for name, test := range map[string]struct {
		remembered []SessionPartState
		ids        []string
		kept       int
	}{
		"the same records":                       {remembered, ids, 3},
		"the session went on":                    {remembered, append(append([]string(nil), ids...), "r8", "r9"), 3},
		"exactly the sealed records":             {remembered, ids[:6], 3},
		"nothing remembered":                     {nil, ids, 0},
		"no records":                             {remembered, nil, 0},
		"a record of the second part is another": {remembered, []string{"r1", "r2", "r3", "x4", "r5", "r6", "r7"}, 1},
		"a record of the first part is another":  {remembered, []string{"x1", "r2", "r3", "r4", "r5", "r6", "r7"}, 0},
		"two records changed places":             {remembered, []string{"r1", "r2", "r4", "r3", "r5", "r6", "r7"}, 1},
		"fewer records than the parts hold":      {remembered, ids[:5], 2},
		"a part that holds no record":            {change(func(parts []SessionPartState) { parts[1].Counts = nil }), ids, 1},
		"a part that holds a record fewer":       {change(func(parts []SessionPartState) { parts[2].Counts = []int{1} }), ids, 2},
		"a chain that is another":                {change(func(parts []SessionPartState) { parts[0].Chain = parts[1].Chain }), ids, 0},
	} {
		if kept := KeptSessionParts(test.remembered, test.ids); kept != test.kept {
			t.Errorf("%s: %d parts kept, want %d", name, kept, test.kept)
		}
	}
}

func TestSessionRangeEventsPlacesEveryRangeInTheConversation(t *testing.T) {
	// Five records with 1, 0, 2, 3 and 1 events.
	counts := []int{1, 0, 2, 3, 1}
	first, count, ok := SessionRangeEvents([][]string{{"a", "b"}, {"c"}, {}, {"d", "e"}}, counts)
	if !ok || !reflect.DeepEqual(first, []int{0, 1, 3, 3}) || !reflect.DeepEqual(count, []int{1, 2, 0, 4}) {
		t.Fatalf("first=%v count=%v ok=%v", first, count, ok)
	}
	if first, count, ok := SessionRangeEvents(nil, nil); !ok || len(first) != 0 || len(count) != 0 {
		t.Fatalf("no ranges and no records: %v %v %v", first, count, ok)
	}
	for name, ranges := range map[string][][]string{
		"ranges that hold fewer records": {{"a", "b"}, {"c"}},
		"ranges that hold more records":  {{"a", "b"}, {"c", "d", "e", "f"}},
		"no ranges":                      nil,
	} {
		if _, _, ok := SessionRangeEvents(ranges, counts); ok {
			t.Errorf("%s were placed", name)
		}
	}
}
