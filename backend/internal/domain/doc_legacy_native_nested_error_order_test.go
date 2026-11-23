package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyNativeNestedErrorOrderImmediateNumberOutranksRecoverable(test *testing.T) {
	raw := []byte(`{"provider_metadata":{"turn_id":7,"create_time":"not-a-number"}}`)
	expectedTime, actualTime := json.Number("123.5"), json.Number("123.5")
	expectedMetadata := &ProviderMetadata{TurnID: "kept", CreateTime: &expectedTime}
	actualMetadata := &ProviderMetadata{TurnID: "kept", CreateTime: &actualTime}
	expected := wire{ProviderMetadata: expectedMetadata}
	actual := wire{ProviderMetadata: actualMetadata}

	expectedErr := json.Unmarshal(raw, &expected)
	var expectedTypeError *json.UnmarshalTypeError
	if expectedErr == nil || errors.As(expectedErr, &expectedTypeError) || !strings.Contains(expectedErr.Error(), "invalid number literal") {
		test.Fatal("encoding/json no longer prioritizes the immediate invalid Number error", expectedErr)
	}
	if expected.ProviderMetadata != expectedMetadata || expectedMetadata.CreateTime != &expectedTime {
		test.Fatal("encoding/json no longer preserves the seeded metadata pointers")
	}

	if actual.ProviderMetadata != actualMetadata || actualMetadata.CreateTime != &actualTime {
		test.Error("native child decode replaced a seeded metadata pointer")
	}
	if !reflect.DeepEqual(actualMetadata, expectedMetadata) {
		test.Errorf("native metadata child differs after immediate error: got=%#v want=%#v", actualMetadata, expectedMetadata)
	}
}
