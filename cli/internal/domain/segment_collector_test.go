package domain

import (
	"testing"
	"time"
)

func TestAutomaticScopeIsWhatWasRecordedAfterItBegan(t *testing.T) {
	began := time.Date(2031, 1, 2, 3, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		session SegmentedSession
		want    bool
	}{
		"ended before":                  {SegmentedSession{StartedAt: "2031-01-01T00:00:00.000Z", EndedAt: "2031-01-02T02:59:59.999Z"}, false},
		"ended at the moment":           {SegmentedSession{EndedAt: "2031-01-02T03:00:00.000Z"}, true},
		"started before, still going":   {SegmentedSession{StartedAt: "2031-01-01T00:00:00.000Z", EndedAt: "2031-01-02T04:00:00.000Z"}, true},
		"no own record, started after":  {SegmentedSession{StartedAt: "2031-01-02T05:00:00.000Z"}, true},
		"no own record, started before": {SegmentedSession{StartedAt: "2031-01-01T05:00:00.000Z"}, false},
		"no time at all":                {SegmentedSession{}, false},
		"a time that cannot be read":    {SegmentedSession{EndedAt: "yesterday"}, false},
	} {
		if got := InAutomaticScope(test.session, began); got != test.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestSessionContentKeyFollowsRecordsRangesAndEpoch(t *testing.T) {
	records := SessionRecordRanges{Provider: ProviderClaude, ID: "s1", Source: "/home/dev/s1.jsonl", Ranges: [][]string{{"m1", "m2"}, {"m3"}}}
	key := SessionContentKey(records, "epoch-1")
	if key == "" || key != SessionContentKey(records, "epoch-1") {
		t.Fatal("the same records in the same epoch must give the same key")
	}
	// Where the tool keeps the records is not what they are.
	moved := records
	moved.Source = "/elsewhere/s1.jsonl"
	if SessionContentKey(moved, "epoch-1") != key {
		t.Fatal("the key follows the source path")
	}
	for name, other := range map[string]SessionRecordRanges{
		"an appended record": {Provider: ProviderClaude, ID: "s1", Ranges: [][]string{{"m1", "m2"}, {"m3", "m4"}}},
		"a moved boundary":   {Provider: ProviderClaude, ID: "s1", Ranges: [][]string{{"m1"}, {"m2", "m3"}}},
		"another session":    {Provider: ProviderClaude, ID: "s2", Ranges: [][]string{{"m1", "m2"}, {"m3"}}},
		"another tool":       {Provider: ProviderCodex, ID: "s1", Ranges: [][]string{{"m1", "m2"}, {"m3"}}},
		"one range, not two": {Provider: ProviderClaude, ID: "s1", Ranges: [][]string{{"m1", "m2", "m3"}}},
		"joined identifiers": {Provider: ProviderClaude, ID: "s1", Ranges: [][]string{{"m1m2"}, {"m3"}}},
	} {
		if SessionContentKey(other, "epoch-1") == key {
			t.Errorf("%s has the same key", name)
		}
	}
	if SessionContentKey(records, "epoch-2") == key {
		t.Fatal("another masking policy or document identity must give another key")
	}
	if Fingerprint(map[string]int{"a": 1}) != Fingerprint(map[string]int{"a": 1}) || Fingerprint(1) == Fingerprint(2) || Fingerprint(make(chan int)) != "" {
		t.Fatal("fingerprints")
	}
	if CollectorSessionKey(ProviderCodex, "t1") != "codex/t1" {
		t.Fatal("session key")
	}
}
