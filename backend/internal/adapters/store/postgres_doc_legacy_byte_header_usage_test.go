//go:build postgres

package store

import (
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestLegacyByteHeaderUsageValidation(test *testing.T) {
	for _, usage := range []domain.LegacyDerivedByteStorageUsage{
		{HeaderBytes: -1}, {HeaderParts: -1}, {CheckpointBytes: -1}, {CheckpointObjects: -1},
	} {
		if err := usage.Validate(); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("negative private byte usage accepted", usage, err)
		}
	}
	for _, usage := range []domain.LegacyDerivedByteStorageUsage{
		{}, {HeaderBytes: 17, HeaderParts: 2}, {CheckpointBytes: 17, CheckpointObjects: 2},
		{Sources: 1, CompleteSources: 1, Objects: 2, Parts: 5, PayloadBytes: 99, HeaderBytes: 131073, HeaderParts: 3},
	} {
		if err := usage.Validate(); err != nil {
			test.Fatal("independent header usage rejected", usage, err)
		}
	}
}
