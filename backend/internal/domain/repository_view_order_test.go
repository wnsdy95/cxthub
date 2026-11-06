package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestRepositoryViewMetadataOrderDoesNotDependOnStorageOrder(test *testing.T) {
	stamp := time.Unix(100, 0).UTC()
	input := RepositoryView{
		Refs:    []Ref{{Kind: RefTag, Name: "z"}, {Kind: RefBranch, Name: "alpha"}, {Kind: RefBranch, Name: "Zeta"}},
		History: []HistoryEvent{{ID: "child", BindingParent: "parent", CreatedAt: stamp}, {ID: "parent", CreatedAt: stamp.Add(time.Second)}},
		Pending: []Pending{{SessionID: "alpha"}, {SessionID: "Zeta"}, {SessionID: "alpha-1"}},
		Unsync:  []Unsync{{User: "alpha", Branch: "main", UpdatedAt: stamp}, {User: "zeta", Branch: "main", UpdatedAt: stamp.Add(time.Second)}},
	}
	original, _ := json.Marshal(input)
	got, err := OrderRepositoryViewMetadata(context.Background(), input)
	if err != nil || got.Refs[0].Name != "Zeta" || got.History[0].ID != "parent" || got.Unsync[0].User != "zeta" ||
		got.Pending[0].SessionID != "Zeta" || got.Pending[1].SessionID != "alpha" || got.Pending[2].SessionID != "alpha-1" {
		test.Fatalf("storage-independent order: %+v %v", got, err)
	}
	after, _ := json.Marshal(input)
	if string(after) != string(original) {
		test.Fatal("ordering mutated source slices")
	}
	slices.Reverse(input.Refs)
	slices.Reverse(input.History)
	slices.Reverse(input.Pending)
	slices.Reverse(input.Unsync)
	again, err := OrderRepositoryViewMetadata(context.Background(), input)
	if err != nil || !reflect.DeepEqual(again, got) {
		test.Fatal("input order changed the query contract", err)
	}
	input.History[0].BindingParent = input.History[1].ID
	if _, err := OrderRepositoryViewMetadata(context.Background(), input); !errors.Is(err, ErrIntegrity) {
		test.Fatal("cyclic history was accepted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OrderRepositoryViewMetadata(ctx, input); !errors.Is(err, context.Canceled) {
		test.Fatal("cancellation ignored", err)
	}
}

func TestRepositoryViewMetadataUTCDoesNotMutateSlicesOrActivityPointers(test *testing.T) {
	stamp := time.Date(2026, time.November, 7, 6, 10, 20, 123456789, time.FixedZone("source", 9*60*60))
	activity := stamp.Add(time.Second)
	input := RepositoryView{
		History: []HistoryEvent{{ID: "event", CreatedAt: stamp}},
		Pending: []Pending{{SessionID: "alpha", UpdatedAt: stamp, ActivityAt: &activity}, {SessionID: "Zeta", UpdatedAt: stamp, ActivityAt: &activity}, {SessionID: "idle"}},
		Unsync:  []Unsync{{User: "user", Branch: "main", UpdatedAt: stamp}},
		Reflog:  []RefLogEntry{{Name: "newest", CreatedAt: stamp}, {Name: "oldest", CreatedAt: stamp.Add(-time.Second)}},
	}
	original := pendingProjectionTestJSON(test, input)
	got, err := OrderRepositoryViewMetadata(context.Background(), input)
	if err != nil {
		test.Fatal(err)
	}
	for _, timestamp := range []time.Time{got.History[0].CreatedAt, got.Pending[0].UpdatedAt, got.Pending[1].UpdatedAt, got.Unsync[0].UpdatedAt, got.Reflog[0].CreatedAt} {
		if timestamp != stamp.UTC() {
			test.Fatal("metadata normalization changed its instant or precision", timestamp)
		}
	}
	if got.Reflog[1].CreatedAt != stamp.Add(-time.Second).UTC() || got.Reflog[0].Name != "newest" || got.Reflog[1].Name != "oldest" {
		test.Fatal("timestamp normalization reordered reflog history")
	}
	if got.Pending[0].ActivityAt == &activity || got.Pending[1].ActivityAt == &activity || got.Pending[0].ActivityAt == got.Pending[1].ActivityAt ||
		*got.Pending[0].ActivityAt != activity.UTC() || *got.Pending[1].ActivityAt != activity.UTC() || got.Pending[2].ActivityAt != nil {
		test.Fatal("activity normalization retained aliases or changed absent activity")
	}
	if string(pendingProjectionTestJSON(test, input)) != string(original) {
		test.Fatal("normalization mutated the input view")
	}
	got.History[0].CreatedAt = time.Time{}
	got.Pending[0].SessionID = "changed"
	*got.Pending[0].ActivityAt = time.Time{}
	got.Unsync[0].UpdatedAt = time.Time{}
	got.Reflog[0].CreatedAt = time.Time{}
	if string(pendingProjectionTestJSON(test, input)) != string(original) || input.Pending[0].ActivityAt != &activity {
		test.Fatal("normalized results alias raw slices or activity pointers")
	}
}

func TestRepositorySourceMetadataTimezoneAndOrderParity(test *testing.T) {
	for _, offset := range []int{9 * 60 * 60, -7 * 60 * 60} {
		test.Run(time.FixedZone("source", offset).String()+time.Unix(int64(offset), 0).UTC().Format("150405"), func(test *testing.T) {
			repo, entries := repositoryDecoderFixture(test)
			metadata, want, err := DecodeRepositoryViewSource(context.Background(), repo, entries)
			if err != nil {
				test.Fatal(err)
			}
			for index := range entries {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(entries[index].Value, &fields); err != nil {
					test.Fatal(err)
				}
				for _, name := range []string{"created_at", "updated_at", "activity_at", "archived_at"} {
					raw := fields[name]
					if len(raw) == 0 || string(raw) == "null" {
						continue
					}
					var stamp time.Time
					if err := json.Unmarshal(raw, &stamp); err != nil {
						test.Fatal(err)
					}
					fields[name] = pendingProjectionTestJSON(test, stamp.In(time.FixedZone("source", offset)))
				}
				entries[index].Value = pendingProjectionTestJSON(test, fields)
			}
			slices.Reverse(entries)
			original := pendingProjectionTestJSON(test, entries)
			gotMetadata, got, err := DecodeRepositoryViewSource(context.Background(), repo, entries)
			if err != nil || !reflect.DeepEqual(gotMetadata, metadata) || !reflect.DeepEqual(got, want) {
				test.Fatal("source timezone or input order changed the canonical view", err)
			}
			var decoder *RepositoryViewSourceDecoder
			for range 2 {
				gotMetadata, got, decoder, err = DecodeRepositoryViewSourceReusing(context.Background(), repo, entries, decoder)
				if err != nil || !reflect.DeepEqual(gotMetadata, metadata) || !reflect.DeepEqual(got, want) {
					test.Fatal("decoded cache changed timezone or order normalization", err)
				}
			}
			if string(pendingProjectionTestJSON(test, entries)) != string(original) {
				test.Fatal("timezone normalization mutated captured source bytes")
			}
		})
	}
}

func TestRepositorySourceSnapshotUsesUTCWithoutChangingInstant(test *testing.T) {
	stamp := time.Date(2026, time.November, 7, 6, 10, 20, 123456789, time.FixedZone("source", 9*60*60))
	raw, err := json.Marshal(Snapshot{CreatedAt: stamp})
	if err != nil {
		test.Fatal(err)
	}
	snapshot, err := decodeRepositorySnapshotSource(context.Background(), raw)
	if err != nil || !snapshot.CreatedAt.Equal(stamp) || snapshot.CreatedAt.Location() != time.UTC || snapshot.CreatedAt.Nanosecond() != stamp.Nanosecond() {
		test.Fatal("source timestamp changed instant or retained storage timezone", err)
	}
}
