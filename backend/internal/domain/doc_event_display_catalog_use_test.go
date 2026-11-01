package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
)

func eventDisplayCatalogStep(test *testing.T) (DocEventDisplayStep, DocDisplayCatalogUse) {
	test.Helper()
	fixture := newEventDisplayTestFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"catalog test"}`), false)
	step, err := StepDocEventDisplay(context.Background(), fixture.state, fixture.loadRaw, fixture.loadObject)
	if err != nil || len(step.Objects()) == 0 {
		test.Fatal("could not calculate a real sealed step", err)
	}
	source, err := fixture.state.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	return step, DocDisplayCatalogUse{Source: source, Generation: "11111111-1111-4111-8111-111111111111", Epoch: "33333333-3333-4333-8333-333333333333"}
}

func TestDocEventDisplayCatalogUseDoesNotChangeContent(test *testing.T) {
	step, use := eventDisplayCatalogStep(test)
	before, err := step.Preparation().Encode()
	if err != nil {
		test.Fatal(err)
	}
	bound, err := step.WithCatalogUse(use)
	if err != nil {
		test.Fatal(err)
	}
	after, err := bound.Preparation().Encode()
	if err != nil || !bytes.Equal(after, before) || HashContent(after) != HashContent(before) ||
		bound.Prior() != step.Prior() || !reflect.DeepEqual(bound.Objects(), step.Objects()) {
		test.Fatal("catalog witness changed content, predecessor or objects", err)
	}
	if actual, exists := step.CatalogUse(); exists || actual != (DocDisplayCatalogUse{}) {
		test.Fatal("binding mutated the unbound input", actual)
	}
	actual, exists := bound.CatalogUse()
	if !exists || actual != use {
		test.Fatal("binding lost the captured use", actual, exists)
	}
	actual.Generation = "22222222-2222-4222-8222-222222222222"
	actual.Epoch = "44444444-4444-4444-8444-444444444444"
	if copied, _ := bound.CatalogUse(); copied != use {
		test.Fatal("getter exposed mutable catalog authority")
	}
	if again, err := bound.WithCatalogUse(use); err != nil || !reflect.DeepEqual(again, bound) {
		test.Fatal("identical binding must be idempotent", err)
	}
	changed := use
	changed.Generation = actual.Generation
	if rebound, err := bound.WithCatalogUse(changed); !errors.Is(err, ErrConflict) || !reflect.DeepEqual(rebound, DocEventDisplayStep{}) {
		test.Fatal("an old calculation was rebound to another generation", err)
	}
	changed = use
	changed.Epoch = actual.Epoch
	if rebound, err := bound.WithCatalogUse(changed); !errors.Is(err, ErrConflict) || !reflect.DeepEqual(rebound, DocEventDisplayStep{}) {
		test.Fatal("an old calculation was rebound to another catalog epoch", err)
	}
}

func TestDocEventDisplayCatalogUseRejectsInvalidBinding(test *testing.T) {
	step, use := eventDisplayCatalogStep(test)
	for _, sample := range []struct {
		name string
		use  DocDisplayCatalogUse
	}{
		{"missing", DocDisplayCatalogUse{}},
		{"missing_source", DocDisplayCatalogUse{Generation: use.Generation, Epoch: use.Epoch}},
		{"foreign_source", DocDisplayCatalogUse{Source: HashContent([]byte("foreign")), Generation: use.Generation, Epoch: use.Epoch}},
		{"missing_generation", DocDisplayCatalogUse{Source: use.Source, Epoch: use.Epoch}},
		{"invalid_generation", DocDisplayCatalogUse{Source: use.Source, Generation: "invalid", Epoch: use.Epoch}},
		{"zero_generation", DocDisplayCatalogUse{Source: use.Source, Generation: "00000000-0000-0000-0000-000000000000", Epoch: use.Epoch}},
		{"missing_epoch", DocDisplayCatalogUse{Source: use.Source, Generation: use.Generation}},
		{"invalid_epoch", DocDisplayCatalogUse{Source: use.Source, Generation: use.Generation, Epoch: "invalid"}},
		{"zero_epoch", DocDisplayCatalogUse{Source: use.Source, Generation: use.Generation, Epoch: "00000000-0000-0000-0000-000000000000"}},
	} {
		test.Run(sample.name, func(test *testing.T) {
			if bound, err := step.WithCatalogUse(sample.use); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(bound, DocEventDisplayStep{}) {
				test.Fatal("invalid catalog use produced a bound step", err)
			}
		})
	}
	if bound, err := (DocEventDisplayStep{}).WithCatalogUse(use); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(bound, DocEventDisplayStep{}) {
		test.Fatal("catalog use legalized an empty step", err)
	}
}
