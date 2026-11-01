package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestLegacyCanonicalNumberReviewEagerErrorOrdering(test *testing.T) {
	number := json.Number("01")
	unsupported := make(chan int)
	_, numberErr := json.Marshal(number)
	if numberErr == nil {
		test.Fatal("invalid Number fixture lost its native error")
	}
	for _, fixture := range []struct {
		name        string
		values      []any
		expectedErr error
	}{
		{
			name:        "number-before-unsupported",
			values:      []any{number, unsupported},
			expectedErr: numberErr,
		},
		{
			name:        "unsupported-before-number",
			values:      []any{unsupported, number},
			expectedErr: fmt.Errorf("%w: unsupported legacy canonical value %T", ErrValidation, unsupported),
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			encoding, actualErr := newLegacyCanonicalEncoding(context.Background(), fixture.values)
			if encoding != nil || actualErr == nil {
				test.Fatal("eager constructor admitted invalid values", actualErr)
			}
			if reflect.TypeOf(actualErr) != reflect.TypeOf(fixture.expectedErr) || actualErr.Error() != fixture.expectedErr.Error() ||
				errors.Is(actualErr, ErrValidation) != errors.Is(fixture.expectedErr, ErrValidation) {
				test.Fatalf("eager error ordering changed: got=%T %v want=%T %v", actualErr, actualErr, fixture.expectedErr, fixture.expectedErr)
			}
		})
	}
}
