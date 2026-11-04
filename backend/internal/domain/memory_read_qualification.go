package domain

import (
	"context"
	"encoding/json"
)

const MemoryReadQualificationVersion = 1
const MaxMemoryReadQualificationBytes = 2 << 20

type MemoryReadQualification struct {
	Version   int                              `json:"version"`
	ReadID    ContentHash                      `json:"read_id"`
	CaptureID ContentHash                      `json:"capture_id"`
	Source    RepositoryMemorySourceGeneration `json:"source"`
	Witnesses []MemoryObjectWitness            `json:"witnesses"`
}

func MemoryReadQualificationBytes(ctx context.Context, qualification MemoryReadQualification) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if qualification.Version != MemoryReadQualificationVersion || qualification.Source.Validate() != nil ||
		ValidateContentHash(qualification.ReadID) != nil || ValidateContentHash(qualification.CaptureID) != nil {
		return nil, ErrIntegrity
	}
	if len(qualification.Witnesses) > MaxMemoryCaptureObjects {
		return nil, ErrMemoryProjectionLimit
	}
	base := qualification
	base.Witnesses = []MemoryObjectWitness{}
	header, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	size := len(header)
	set := NewMemoryObjectWitnessSet(qualification.Source.Graph.RepoID)
	for ordinal, witness := range qualification.Witnesses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if ordinal > 0 && qualification.Witnesses[ordinal-1].Hash >= witness.Hash {
			return nil, ErrIntegrity
		}
		if err := set.Add(witness); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(witness)
		if err != nil {
			return nil, err
		}
		size += len(encoded)
		if ordinal > 0 {
			size++
		}
		if size > MaxMemoryReadQualificationBytes {
			return nil, ErrMemoryProjectionLimit
		}
	}
	raw, err := json.Marshal(qualification)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxMemoryReadQualificationBytes {
		return nil, ErrMemoryProjectionLimit
	}
	return raw, ctx.Err()
}

func ValidateMemoryReadQualification(ctx context.Context, qualification MemoryReadQualification, header MemoryReadHeader) error {
	if _, err := MemoryReadQualificationBytes(ctx, qualification); err != nil {
		return err
	}
	raw, err := MemoryReadHeaderBytes(ctx, header)
	if err != nil {
		return err
	}
	if qualification.ReadID != HashContent(raw) || qualification.CaptureID != header.CaptureID ||
		qualification.Source != header.Source || len(qualification.Witnesses) != len(header.Dependencies) {
		return ErrIntegrity
	}
	for ordinal, witness := range qualification.Witnesses {
		dependency := header.Dependencies[ordinal]
		if witness.Hash != dependency.Hash || witness.Available != dependency.Available {
			return ErrIntegrity
		}
	}
	return ctx.Err()
}
