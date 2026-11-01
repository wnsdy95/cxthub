package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

const (
	MemoryCaptureVersion        = 1
	MemoryCaptureDeltaVersion   = 2
	MemoryCaptureRules          = 1
	MemoryCaptureChunkBytes     = 64 << 10
	MaxMemoryCapturePartBytes   = 1 << 20
	MaxMemoryCaptureBytes       = 112 << 20
	MaxMemoryCaptureParts       = 110000
	MaxMemoryCaptureObjects     = 4096
	MaxMemoryCaptureObjectBytes = 64 << 20
	MaxMemoryCapturePinBytes    = 4 << 20
	MaxMemoryCaptureDeltaParts  = 4096
	MaxMemoryCaptureDeltaBytes  = 4 << 20
)

var ErrMemoryCaptureExpired = errors.New("memory capture retention expired")

type MemoryCaptureHeader struct {
	Version      int                              `json:"version"`
	Rules        int                              `json:"rules"`
	Generation   RepositoryMemorySourceGeneration `json:"generation"`
	Selection    EffectiveMemorySelection         `json:"selection"`
	Content      string                           `json:"content"`
	PinsCaptured bool                             `json:"pins_captured"`
}

type MemoryCapturePart struct {
	Kind     string
	Key      string
	Sequence int64
	Value    []byte
}

type MemoryCaptureBundle struct {
	ID     ContentHash
	Header MemoryCaptureHeader
	Parts  []MemoryCapturePart
}

type MemoryGraphBase struct {
	ID         ContentHash
	Checkpoint RepositoryViewSourceCheckpoint
}

func MemoryCaptureGraphBase(bundle MemoryCaptureBundle) (MemoryGraphBase, error) {
	var base MemoryGraphBase
	for _, part := range bundle.Parts {
		if part.Kind != "graph_ref" {
			continue
		}
		if bundle.Header.Version != MemoryCaptureDeltaVersion || base.ID != "" || len(part.Value) > 16<<10 ||
			len(part.Key) != len(contentHashPrefix)+64 || ValidateContentHash(ContentHash(part.Key)) != nil {
			return MemoryGraphBase{}, ErrIntegrity
		}
		if !utf8.Valid(part.Value) || DecodeRepositorySourceJSON(context.Background(), part.Value, &base.Checkpoint) != nil {
			return MemoryGraphBase{}, ErrIntegrity
		}
		current := bundle.Header.Generation.Graph
		if base.Checkpoint.Validate() != nil || base.Checkpoint.Scope != RepositoryGraphSourceScope ||
			base.Checkpoint.Version != current.Version || base.Checkpoint.Scope != current.Scope || base.Checkpoint.RepoID != current.RepoID ||
			base.Checkpoint.Epoch != current.Epoch || base.Checkpoint.Sequence > current.Sequence || part.Sequence != base.Checkpoint.Sequence {
			return MemoryGraphBase{}, ErrIntegrity
		}
		base.ID = ContentHash(part.Key)
	}
	if (bundle.Header.Version == MemoryCaptureDeltaVersion) != (base.ID != "") ||
		(bundle.Header.Version != MemoryCaptureVersion && bundle.Header.Version != MemoryCaptureDeltaVersion) {
		return MemoryGraphBase{}, ErrIntegrity
	}
	return base, nil
}

func CountMemoryCaptureRecords(ctx context.Context, reader io.Reader, records *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reader == nil || records == nil || *records < 0 {
		return ErrValidation
	}
	if *records > MaxMemoryCaptureObjects {
		return ErrMemoryProjectionLimit
	}
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	if err := countMemoryCaptureValue(ctx, decoder, records, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrIntegrity
	}
	return ctx.Err()
}

func countMemoryCaptureValue(ctx context.Context, decoder *json.Decoder, records *int, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 64 {
		return ErrMemoryProjectionLimit
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrIntegrity
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return ErrIntegrity
	}
	fields := 0
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if delimiter == '{' {
			fields++
			if fields > 64 {
				return ErrMemoryProjectionLimit
			}
			key, err := decoder.Token()
			if _, valid := key.(string); err != nil || !valid {
				return ErrIntegrity
			}
		} else {
			if *records == MaxMemoryCaptureObjects {
				return ErrMemoryProjectionLimit
			}
			*records++
		}
		if err := countMemoryCaptureValue(ctx, decoder, records, depth+1); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil || (delimiter == '{' && closing != json.Delim('}')) || (delimiter == '[' && closing != json.Delim(']')) {
		return ErrIntegrity
	}
	return nil
}

func DecodeMemoryCaptureObject(ctx context.Context, raw []byte) (MemoryDigest, error) {
	if err := ctx.Err(); err != nil {
		return MemoryDigest{}, err
	}
	if len(raw) > MaxMemoryCaptureObjectBytes {
		return MemoryDigest{}, ErrMemoryProjectionLimit
	}
	records := 0
	if err := CountMemoryCaptureRecords(ctx, bytes.NewReader(raw), &records); err != nil {
		return MemoryDigest{}, err
	}
	if !utf8.Valid(raw) {
		return MemoryDigest{}, ErrIntegrity
	}
	var digest MemoryDigest
	if err := DecodeRepositorySourceJSON(ctx, raw, &digest); err != nil {
		return MemoryDigest{}, err
	}
	if err := memoryCaptureObjectRefs(ctx, digest); err != nil {
		return MemoryDigest{}, err
	}
	if digest.ValidateMemoryClaims() != nil {
		return MemoryDigest{}, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return MemoryDigest{}, err
	}
	return digest, nil
}

func memoryCaptureObjectRefs(ctx context.Context, digest MemoryDigest) error {
	validHash := func(value ContentHash) bool {
		return len(value) == len(contentHashPrefix)+64 && ValidateContentHash(value) == nil
	}
	for _, reference := range []ContentHash{digest.SnapshotID, digest.PreviousMemoryHash} {
		if reference != "" && !validHash(reference) {
			return ErrIntegrity
		}
	}
	for _, fragment := range digest.Fragments {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validHash(fragment.SourceSnapshot) {
			return ErrIntegrity
		}
	}
	if coverage := digest.GraftCoverage; coverage != nil {
		if coverage.ProjectionVersion == 0 || coverage.GraftSeq > MaxGraftSeq ||
			(coverage.ProjectionComplete && coverage.LineageFingerprint == "") ||
			(coverage.LineageFingerprint != "" && !validHash(coverage.LineageFingerprint)) {
			return ErrIntegrity
		}
		for _, references := range [][]ContentHash{coverage.GraftParents, coverage.PinnedSources} {
			for _, reference := range references {
				if err := ctx.Err(); err != nil {
					return err
				}
				if !validHash(reference) {
					return ErrIntegrity
				}
			}
		}
	}
	return ctx.Err()
}
