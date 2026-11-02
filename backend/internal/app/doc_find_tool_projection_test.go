package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocPreparedToolResultProjectionFallbackErrors(test *testing.T) {
	source := preparedFindReviewSource(test, strings.Repeat("value ", 40_000))
	metadata, _ := source.entry(0)
	for name, failure := range map[string]error{
		"integrity": domain.ErrIntegrity,
		"forbidden": domain.ErrForbidden,
		"cancelled": context.Canceled,
		"conflict":  domain.ErrConflict,
		"transient": errors.New("temporary storage failure"),
	} {
		test.Run("PRE/"+name, func(test *testing.T) {
			selected := source
			selected.displayGeneration = func(domain.DocEventIndex) (domain.DocDisplayStringGeneration, error) {
				return domain.DocDisplayStringGeneration{}, failure
			}
			selected.eventDisplayGeneration = func(domain.DocEventIndex) (domain.DocDisplayProjectionGeneration, error) {
				test.Fatal("PRE error was hidden by another profile")
				return domain.DocDisplayProjectionGeneration{}, nil
			}
			selected.eventDisplayPage = func(domain.DocEventIndex, int) (domain.DocDisplayPage, error) { return domain.DocDisplayPage{}, nil }
			if _, err := openDocPreparedFindEvent(selected, metadata, ""); !errors.Is(err, failure) {
				test.Fatal("PRE error was replaced", err)
			}
		})
		test.Run("projection/"+name, func(test *testing.T) {
			selected := source
			selected.displayGeneration = func(domain.DocEventIndex) (domain.DocDisplayStringGeneration, error) {
				return domain.DocDisplayStringGeneration{}, &docDisplayPreparationRequired{}
			}
			selected.eventDisplayGeneration = func(domain.DocEventIndex) (domain.DocDisplayProjectionGeneration, error) {
				return domain.DocDisplayProjectionGeneration{}, failure
			}
			selected.eventDisplayPage = func(domain.DocEventIndex, int) (domain.DocDisplayPage, error) { return domain.DocDisplayPage{}, nil }
			if _, err := openDocPreparedFindEvent(selected, metadata, ""); !errors.Is(err, failure) {
				test.Fatal("projection failure was hidden", err)
			}
		})
	}
}

func TestDocPreparedToolResultKeepsPREPreferenceAndMissingDemand(test *testing.T) {
	source := preparedFindReviewSource(test, strings.Repeat("value ", 40_000))
	metadata, _ := source.entry(0)
	source.eventDisplayGeneration = func(domain.DocEventIndex) (domain.DocDisplayProjectionGeneration, error) {
		test.Fatal("existing PRE projection was bypassed")
		return domain.DocDisplayProjectionGeneration{}, nil
	}
	source.eventDisplayPage = func(domain.DocEventIndex, int) (domain.DocDisplayPage, error) { return domain.DocDisplayPage{}, nil }
	if _, err := openDocPreparedFindEvent(source, metadata, ""); err != nil {
		test.Fatal(err)
	}
	missing := &docDisplayPreparationRequired{target: domain.DocDisplayTarget{Profile: domain.DocDisplayStringProfile}}
	source.displayGeneration = func(domain.DocEventIndex) (domain.DocDisplayStringGeneration, error) {
		return domain.DocDisplayStringGeneration{}, missing
	}
	source.eventDisplayGeneration = func(domain.DocEventIndex) (domain.DocDisplayProjectionGeneration, error) {
		return domain.DocDisplayProjectionGeneration{}, &docDisplayPreparationRequired{target: domain.DocDisplayTarget{Profile: domain.DocDisplayProjectionProfile}}
	}
	if _, err := openDocPreparedFindEvent(source, metadata, ""); err != missing || missing.target.DisplayProfile() != domain.DocDisplayStringProfile {
		test.Fatal("absent projections changed the initial demand profile", err)
	}
}
