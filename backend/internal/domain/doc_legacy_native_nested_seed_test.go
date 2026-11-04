package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

type legacyNativeNestedSeedFixture struct {
	Locked           *LockedBlob       `json:"locked"`
	ProviderMetadata *ProviderMetadata `json:"provider_metadata"`
}

func legacyNativeNestedSeedValue() legacyNativeNestedSeedFixture {
	createTime := json.Number("123.450e-8")
	return legacyNativeNestedSeedFixture{
		Locked: &LockedBlob{
			Provider: "codex",
			Scheme:   "encrypted_content",
			Blob:     "seed-blob",
		},
		ProviderMetadata: &ProviderMetadata{
			TurnID:     "seed-turn",
			CreateTime: &createTime,
		},
	}
}

func legacyNativeNestedSeedPointer[Value any](test *testing.T, name string, actual, seed *Value, identity string) {
	test.Helper()
	switch identity {
	case "":
		if actual != seed {
			test.Fatal(name, "replaced the preseeded pointer")
		}
	case "nil":
		if actual != nil {
			test.Fatal(name, "retained a pointer after null")
		}
	case "new":
		if actual == nil || actual == seed {
			test.Fatal(name, "did not allocate a new pointer after null")
		}
	default:
		test.Fatal("unknown pointer identity fixture", identity)
	}
}

func TestLegacyNativeObjectNestedSeedMatchesJSONUnmarshal(test *testing.T) {
	for _, fixture := range []struct {
		name          string
		raw           string
		outerIdentity string
		timeIdentity  string
		rejected      bool
	}{
		{
			name: "omitted-children",
			raw:  `{}`,
		},
		{
			name: "empty-children",
			raw:  `{"locked":{},"provider_metadata":{}}`,
		},
		{
			name: "null-string-members",
			raw:  `{"locked":{"provider":null,"scheme":null,"blob":null},"provider_metadata":{"turn_id":null}}`,
		},
		{
			name: "partial-duplicate-children",
			raw:  `{"locked":{"blob":"first"},"LOCKED":{"scheme":"signature"},"locked":{"blob":null},"provider_metadata":{"turn_id":"first"},"PROVIDER_METADATA":{},"provider_metadata":{"turn_id":null}}`,
		},
		{
			name: "create-time-merges-through-old-pointer",
			raw:  `{"provider_metadata":{"create_time":2.500},"provider_metadata":{},"PROVIDER_METADATA":{"turn_id":"changed","create_time":3E+4}}`,
		},
		{
			name:          "outer-null-clears-pointers",
			raw:           `{"locked":null,"provider_metadata":null}`,
			outerIdentity: "nil",
		},
		{
			name:          "outer-null-then-empty-allocates-without-seed",
			raw:           `{"locked":{"blob":"before-reset"},"locked":null,"locked":{},"provider_metadata":{"turn_id":"before-reset","create_time":2.500},"provider_metadata":null,"provider_metadata":{}}`,
			outerIdentity: "new",
			timeIdentity:  "nil",
		},
		{
			name:          "outer-null-then-partial-allocates-without-seed",
			raw:           `{"locked":null,"locked":{"blob":"fresh"},"provider_metadata":null,"provider_metadata":{"create_time":3.250}}`,
			outerIdentity: "new",
			timeIdentity:  "new",
		},
		{
			name:         "create-time-null-clears-only-inner-pointer",
			raw:          `{"provider_metadata":{"create_time":null},"provider_metadata":{}}`,
			timeIdentity: "nil",
		},
		{
			name:         "create-time-null-then-number-allocates",
			raw:          `{"provider_metadata":{"create_time":null},"provider_metadata":{"create_time":2.500,"turn_id":null},"provider_metadata":{}}`,
			timeIdentity: "new",
		},
		{
			name:         "old-create-time-updated-before-inner-reset",
			raw:          `{"provider_metadata":{"create_time":2.500},"provider_metadata":{"create_time":null},"provider_metadata":{"create_time":3E+4}}`,
			timeIdentity: "new",
		},
		{
			name:     "preseeded-locked-native-type-error",
			raw:      `{"locked":{"blob":7}}`,
			rejected: true,
		},
		{
			name:     "preseeded-create-time-native-type-error",
			raw:      `{"provider_metadata":{"create_time":false}}`,
			rejected: true,
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			actual, expected := legacyNativeNestedSeedValue(), legacyNativeNestedSeedValue()
			actualLocked, expectedLocked := actual.Locked, expected.Locked
			actualMetadata, expectedMetadata := actual.ProviderMetadata, expected.ProviderMetadata
			actualTime, expectedTime := actualMetadata.CreateTime, expectedMetadata.CreateTime
			if actualLocked == expectedLocked || actualMetadata == expectedMetadata || actualTime == expectedTime {
				test.Fatal("actual and native oracle share preseeded pointees")
			}
			raw := []byte(fixture.raw)
			expectedErr := json.Unmarshal(raw, &expected)
			if (expectedErr != nil) != fixture.rejected {
				test.Fatal("fixture lost its native acceptance", fixture.rejected, expectedErr)
			}
			presence, actualErr := legacyNativeMetadataDecode(test, raw, &actual, false)
			if reflect.TypeOf(actualErr) != reflect.TypeOf(expectedErr) || presence != 0 {
				test.Fatal("preseeded child changed native acceptance, error type, or disabled presence", actualErr, expectedErr, presence)
			}
			if expectedErr != nil {
				var actualTypeError, expectedTypeError *json.UnmarshalTypeError
				if !errors.As(actualErr, &actualTypeError) || !errors.As(expectedErr, &expectedTypeError) ||
					actualTypeError.Value != expectedTypeError.Value || actualTypeError.Type != expectedTypeError.Type {
					test.Fatal("preseeded child changed native error Value or Type", actualErr, expectedErr)
				}
			}
			if !reflect.DeepEqual(actual, expected) {
				test.Fatalf("preseeded child changed native values: got=%+v want=%+v", actual, expected)
			}
			legacyNativeNestedSeedPointer(test, "actual locked", actual.Locked, actualLocked, fixture.outerIdentity)
			legacyNativeNestedSeedPointer(test, "native locked", expected.Locked, expectedLocked, fixture.outerIdentity)
			legacyNativeNestedSeedPointer(test, "actual metadata", actual.ProviderMetadata, actualMetadata, fixture.outerIdentity)
			legacyNativeNestedSeedPointer(test, "native metadata", expected.ProviderMetadata, expectedMetadata, fixture.outerIdentity)
			if fixture.outerIdentity != "nil" {
				legacyNativeNestedSeedPointer(test, "actual create_time", actual.ProviderMetadata.CreateTime, actualTime, fixture.timeIdentity)
				legacyNativeNestedSeedPointer(test, "native create_time", expected.ProviderMetadata.CreateTime, expectedTime, fixture.timeIdentity)
			}
			if !reflect.DeepEqual(actualLocked, expectedLocked) || !reflect.DeepEqual(actualMetadata, expectedMetadata) || *actualTime != *expectedTime {
				test.Fatal("writes through external preseeded pointees differ from native decoding")
			}
			if (actualMetadata.CreateTime == actualTime) != (expectedMetadata.CreateTime == expectedTime) {
				test.Fatal("external metadata changed its original create_time pointer differently from native decoding")
			}
		})
	}
}
