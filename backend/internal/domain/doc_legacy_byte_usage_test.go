package domain

import (
	"errors"
	"testing"
)

func TestLegacyDerivedByteStorageUsageValidate(test *testing.T) {
	for _, fixture := range []struct {
		name    string
		usage   LegacyDerivedByteStorageUsage
		wantErr bool
	}{
		{
			name: "zero",
		},
		{
			name: "nonempty",
			usage: LegacyDerivedByteStorageUsage{
				Sources: 3, CompleteSources: 2, Objects: 7, Parts: 11,
				PayloadBytes: 1024, SpecBytes: 128, TableBytes: 4096, IndexBytes: 8192,
			},
		},
		{
			name:  "all sources complete",
			usage: LegacyDerivedByteStorageUsage{Sources: 3, CompleteSources: 3},
		},
		{
			name:  "stored objects exceed parts",
			usage: LegacyDerivedByteStorageUsage{Sources: 1, Objects: 5, Parts: 1},
		},
		{
			name: "byte metrics are independent",
			usage: LegacyDerivedByteStorageUsage{
				PayloadBytes: 4096, SpecBytes: 1024, TableBytes: 1, IndexBytes: 2,
			},
		},
		{
			name:    "negative sources",
			usage:   LegacyDerivedByteStorageUsage{Sources: -1},
			wantErr: true,
		},
		{
			name:    "negative complete sources",
			usage:   LegacyDerivedByteStorageUsage{Sources: 1, CompleteSources: -1},
			wantErr: true,
		},
		{
			name:    "negative objects",
			usage:   LegacyDerivedByteStorageUsage{Objects: -1},
			wantErr: true,
		},
		{
			name:    "negative parts",
			usage:   LegacyDerivedByteStorageUsage{Parts: -1},
			wantErr: true,
		},
		{
			name:    "negative payload bytes",
			usage:   LegacyDerivedByteStorageUsage{PayloadBytes: -1},
			wantErr: true,
		},
		{
			name:    "negative spec bytes",
			usage:   LegacyDerivedByteStorageUsage{SpecBytes: -1},
			wantErr: true,
		},
		{
			name:    "negative table bytes",
			usage:   LegacyDerivedByteStorageUsage{TableBytes: -1},
			wantErr: true,
		},
		{
			name:    "negative index bytes",
			usage:   LegacyDerivedByteStorageUsage{IndexBytes: -1},
			wantErr: true,
		},
		{
			name:    "complete sources exceed sources",
			usage:   LegacyDerivedByteStorageUsage{Sources: 2, CompleteSources: 3},
			wantErr: true,
		},
		{
			name:    "complete sources without sources",
			usage:   LegacyDerivedByteStorageUsage{CompleteSources: 1},
			wantErr: true,
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			err := fixture.usage.Validate()
			if fixture.wantErr {
				if !errors.Is(err, ErrValidation) {
					test.Fatalf("Validate() = %v, want ErrValidation", err)
				}
			} else if err != nil {
				test.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}
