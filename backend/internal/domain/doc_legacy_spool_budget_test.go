package domain

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

type legacyNormalizationBudgetStorage struct {
	LegacyNormalizationStorage
	failure error
	writes  int
}

func (storage *legacyNormalizationBudgetStorage) Append(_ context.Context, _ int, _ []byte) error {
	storage.writes++
	return storage.failure
}

func TestLegacyNormalizationSpoolBudgetPreservesRetryableOverload(test *testing.T) {
	for _, failure := range []error{ErrResourceOverloaded, errors.Join(ErrResourceOverloaded, errors.New("capacity unavailable"))} {
		storage := &legacyNormalizationBudgetStorage{failure: failure}
		original := []byte("staged output")
		file := &legacyNormalizationFile{storage: storage, buffer: bytes.Clone(original), size: len(original)}
		for range 2 {
			if err := file.drain(context.Background()); !errors.Is(err, ErrResourceOverloaded) || errors.Is(err, ErrIntegrity) || errors.Is(err, ErrPreparedDocRangeObject) {
				test.Fatal("capacity exhaustion changed to corrupt prepared data", err)
			}
			if !bytes.Equal(file.buffer, original) || file.pageBytes != 0 || len(file.hashState) != 0 || len(file.parts) != 0 || file.size != len(original) {
				test.Fatal("failed reservation advanced normalization state")
			}
		}
		if storage.writes != 2 {
			test.Fatal("failed drain did not retry the same private output", storage.writes)
		}
		storage.failure = nil
		if err := file.flush(context.Background()); err != nil || len(file.buffer) != 0 || len(file.parts) != 1 || file.parts[0].Hash != HashContent(original) || file.parts[0].Bytes != int64(len(original)) {
			test.Fatal("retry changed normalized output proof", file.parts, err)
		}
	}
}

func TestLegacyNormalizationSpoolBudgetRetainsActualStorageFailure(test *testing.T) {
	failure := errors.New("private storage write failed")
	storage := &legacyNormalizationBudgetStorage{failure: failure}
	file := &legacyNormalizationFile{storage: storage, buffer: []byte("staged output")}
	if err := file.drain(context.Background()); !errors.Is(err, failure) || !errors.Is(err, ErrPreparedDocRangeObject) || !errors.Is(err, ErrIntegrity) || errors.Is(err, ErrResourceOverloaded) {
		test.Fatal("actual prepared-file error lost its original classification", err)
	}
}
