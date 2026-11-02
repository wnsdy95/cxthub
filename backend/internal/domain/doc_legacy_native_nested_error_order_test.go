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

	_, actualErr := legacyNativeMetadataDecode(test, raw, &actual, false)
	if actualErr == nil || reflect.TypeOf(actualErr) != reflect.TypeOf(expectedErr) || !strings.Contains(actualErr.Error(), "invalid number literal") {
		test.Errorf("native child error precedence differs from encoding/json: got=%T %v want=%T %v", actualErr, actualErr, expectedErr, expectedErr)
	}
	if actual.ProviderMetadata != actualMetadata || actualMetadata.CreateTime != &actualTime {
		test.Error("native child decode replaced a seeded metadata pointer")
	}
	if !reflect.DeepEqual(actualMetadata, expectedMetadata) {
		test.Errorf("native metadata child differs after immediate error: got=%#v want=%#v", actualMetadata, expectedMetadata)
	}
}

func TestLegacyNativeNestedErrorOrderLockedRecoverableKeepsPointeeWrites(test *testing.T) {
	raw := []byte(`{"locked":{"blob":7,"scheme":"changed"}}`)
	expectedLocked := &LockedBlob{Provider: "codex", Scheme: "kept", Blob: "kept"}
	actualLocked := &LockedBlob{Provider: "codex", Scheme: "kept", Blob: "kept"}
	expected := wire{Locked: expectedLocked}
	actual := wire{Locked: actualLocked}

	expectedErr := json.Unmarshal(raw, &expected)
	var expectedTypeError *json.UnmarshalTypeError
	if !errors.As(expectedErr, &expectedTypeError) || expectedTypeError.Value != "number" || expectedTypeError.Type != reflect.TypeFor[string]() {
		test.Fatal("encoding/json no longer returns the recoverable blob type error", expectedErr)
	}
	if expected.Locked != expectedLocked || expectedLocked.Scheme != "changed" || expectedLocked.Blob != "kept" {
		test.Fatal("encoding/json no longer writes through the original locked pointer after the recoverable error")
	}

	_, actualErr := legacyNativeMetadataDecode(test, raw, &actual, false)
	var actualTypeError *json.UnmarshalTypeError
	if !errors.As(actualErr, &actualTypeError) || actualTypeError.Value != expectedTypeError.Value || actualTypeError.Type != expectedTypeError.Type {
		test.Errorf("native locked child lost the first recoverable type error: got=%v want=%v", actualErr, expectedErr)
	}
	if actual.Locked != actualLocked {
		test.Error("native child decode replaced the original locked pointer")
	}
	if *actualLocked != *expectedLocked {
		test.Errorf("native child stopped writes through the original locked pointer: got=%#v want=%#v", actualLocked, expectedLocked)
	}
}

func TestLegacyNativeNestedErrorOrderProviderRecoverableKeepsPointeeWrites(test *testing.T) {
	raw := []byte(`{"provider_metadata":{"create_time":false,"turn_id":"changed"}}`)
	expectedTime, actualTime := json.Number("123.5"), json.Number("123.5")
	expectedMetadata := &ProviderMetadata{TurnID: "kept", CreateTime: &expectedTime}
	actualMetadata := &ProviderMetadata{TurnID: "kept", CreateTime: &actualTime}
	expected := wire{ProviderMetadata: expectedMetadata}
	actual := wire{ProviderMetadata: actualMetadata}

	expectedErr := json.Unmarshal(raw, &expected)
	var expectedTypeError *json.UnmarshalTypeError
	if !errors.As(expectedErr, &expectedTypeError) || expectedTypeError.Value != "bool" || expectedTypeError.Type != reflect.TypeFor[json.Number]() {
		test.Fatal("encoding/json no longer returns the recoverable create_time type error", expectedErr)
	}
	if expected.ProviderMetadata != expectedMetadata || expectedMetadata.CreateTime != &expectedTime || expectedTime != "123.5" || expectedMetadata.TurnID != "changed" {
		test.Fatal("encoding/json no longer writes turn_id through the original metadata pointer after the recoverable error")
	}

	_, actualErr := legacyNativeMetadataDecode(test, raw, &actual, false)
	var actualTypeError *json.UnmarshalTypeError
	if !errors.As(actualErr, &actualTypeError) || actualTypeError.Value != expectedTypeError.Value || actualTypeError.Type != expectedTypeError.Type {
		test.Errorf("native metadata child lost the first recoverable type error: got=%v want=%v", actualErr, expectedErr)
	}
	if actual.ProviderMetadata != actualMetadata || actualMetadata.CreateTime != &actualTime {
		test.Error("native child decode replaced an original metadata pointer")
	}
	if !reflect.DeepEqual(actualMetadata, expectedMetadata) {
		test.Errorf("native child stopped writes through the original metadata pointer: got=%#v want=%#v", actualMetadata, expectedMetadata)
	}
}
