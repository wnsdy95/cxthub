package domain

import (
	"context"
	"testing"
)

func TestLegacyNativeGoGraphW88NilBackingReservation(test *testing.T) {
	for _, length := range []int{0, 1} {
		array := make([]any, length, 16384)
		if length != 0 {
			array[0] = "visible"
		}
		roots := []any{array, array}
		binding := HashContent([]byte("W88 nil backing keeps ordinary native ownership"))
		storage := legacyNativeGoGraphSetW85Storage(test)
		saved := legacyNativeGoGraphSetW85Capture(test, roots, binding, storage)
		state := legacyNativeGoGraphW86State(test, storage, saved)
		owner := legacyNativeGoGraphW86Owner(test, storage, state.Roots[0])
		if owner.FullBacking || owner.Visible != 0 || owner.Length != uint64(length) || owner.Capacity != uint64(cap(array)) || owner.Members.Count != uint64(length) {
			test.Fatal("nil backing did not reuse the original native owner format", owner)
		}
		actual := legacyNativeGoGraphSetW85Restore(test, saved, binding, storage, 0)
		legacyNativeGoGraphSetW85Values(test, roots, actual)
		if actual[0].owner.array != actual[1].owner.array || actual[0].owner.array.tail != nil {
			test.Fatal("nil backing created duplicate owners or per-hidden-slot metadata")
		}
		decoded := actual[0].value.([]any)
		for _, value := range decoded[len(decoded):cap(decoded)] {
			if value != nil {
				test.Fatal("nil backing acquired a hidden value")
			}
		}
		if err := actual[0].validateOwnership(); err != nil {
			test.Fatal(err)
		}
	}
	test.Run("typed-nil-is-not-an-empty-slot", func(test *testing.T) {
		backing := make([]any, 33)
		backing[32] = map[string]any(nil)
		array := backing[:0]
		storage := legacyNativeGoGraphSetW85Storage(test)
		binding := HashContent([]byte("W88 typed nil remains a populated backing slot"))
		saved := legacyNativeGoGraphSetW85Capture(test, []any{array}, binding, storage)
		state := legacyNativeGoGraphW86State(test, storage, saved)
		owner := legacyNativeGoGraphW86Owner(test, storage, state.Roots[0])
		if !owner.FullBacking || owner.Members.Count != 33 {
			test.Fatal("typed nil was dropped as an empty backing slot", owner)
		}
		actual := legacyNativeGoGraphSetW85Restore(test, saved, binding, storage, 0)
		legacyNativeGoGraphW86Values(test, []any{array}, actual)
		if _, err := actual[0].owner.array.tailValue(context.Background(), 32); err != nil {
			test.Fatal("typed nil lost its native tail metadata", err)
		}
	})
}
