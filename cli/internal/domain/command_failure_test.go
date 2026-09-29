package domain

import (
	"errors"
	"fmt"
	"testing"
)

func TestPreciseCommandErrorsKeepCategoryCompatibility(t *testing.T) {
	for _, pair := range [][2]error{{ErrIndexChanged, ErrSyncConflict}, {ErrCodePositionMismatch, ErrSelectionChanged}} {
		wrapped := fmt.Errorf("operation: %w", pair[0])
		if !errors.Is(wrapped, pair[0]) || !errors.Is(wrapped, pair[1]) {
			t.Fatalf("lost compatibility: %v", wrapped)
		}
		if errors.Is(pair[1], pair[0]) {
			t.Fatal("generic category became a precise cause")
		}
	}
	if errors.Is(errors.New(ErrDeliveryFailed.Error()), ErrDeliveryFailed) {
		t.Fatal("message equality became typed identity")
	}
}
