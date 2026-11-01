package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"sort"
	"strings"
	"unicode/utf8"
)

func SealMemoryCapture(ctx context.Context, header MemoryCaptureHeader, parts []MemoryCapturePart) (MemoryCaptureBundle, error) {
	rawHeader, err := memoryCapturePreflight(ctx, header, parts)
	if err != nil {
		return MemoryCaptureBundle{}, err
	}
	owned := append([]MemoryCapturePart(nil), parts...)
	comparisons := 0
	var canceled error
	sort.Slice(owned, func(left, right int) bool {
		comparisons++
		if comparisons%1024 == 0 {
			canceled = ctx.Err()
		}
		return memoryCapturePartLess(owned[left], owned[right])
	})
	if canceled != nil {
		return MemoryCaptureBundle{}, canceled
	}
	if err := memoryCaptureValidateParts(ctx, owned); err != nil {
		return MemoryCaptureBundle{}, err
	}
	for index := range owned {
		if err := ctx.Err(); err != nil {
			return MemoryCaptureBundle{}, err
		}
		owned[index].Value = bytes.Clone(owned[index].Value)
	}
	identifier, err := memoryCaptureRoot(ctx, header.Version, rawHeader, owned)
	if err != nil {
		return MemoryCaptureBundle{}, err
	}
	return MemoryCaptureBundle{ID: identifier, Header: header, Parts: owned}, nil
}

func ValidateMemoryCapture(ctx context.Context, bundle MemoryCaptureBundle) error {
	rawHeader, err := memoryCapturePreflight(ctx, bundle.Header, bundle.Parts)
	if err != nil {
		return err
	}
	if len(bundle.ID) != len(contentHashPrefix)+64 || ValidateContentHash(bundle.ID) != nil {
		return ErrIntegrity
	}
	if err := memoryCaptureValidateParts(ctx, bundle.Parts); err != nil {
		return err
	}
	identifier, err := memoryCaptureRoot(ctx, bundle.Header.Version, rawHeader, bundle.Parts)
	if err != nil {
		return err
	}
	if identifier != bundle.ID {
		return ErrIntegrity
	}
	return nil
}

func memoryCapturePreflight(ctx context.Context, header MemoryCaptureHeader, parts []MemoryCapturePart) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(parts) > MaxMemoryCaptureParts {
		return nil, ErrRepositoryViewSourceLimit
	}
	if len(parts) == 0 {
		return nil, ErrIntegrity
	}
	for _, value := range []string{
		header.Generation.Graph.Scope, string(header.Generation.Graph.RepoID), header.Generation.Graph.Epoch,
		header.Generation.Epoch, header.Selection.Branch, string(header.Selection.SnapshotID),
		header.Selection.CodeCommit, string(header.Selection.MemoryHash), header.Content,
	} {
		if len(value) > MaxRepositoryViewSourceKeyBytes {
			return nil, ErrRepositoryViewSourceLimit
		}
		if !utf8.ValidString(value) {
			return nil, ErrIntegrity
		}
	}
	if (header.Version != MemoryCaptureVersion && header.Version != MemoryCaptureDeltaVersion) || header.Rules != MemoryCaptureRules || header.Generation.Validate() != nil ||
		header.Selection.Validate() != nil || (header.Content != "" && header.Content != "claims" && header.Content != "prompt") ||
		header.PinsCaptured != (header.Selection.Branch != "") {
		return nil, ErrIntegrity
	}
	rawHeader, err := canonicalJSON(header)
	if err != nil {
		return nil, ErrIntegrity
	}
	if len(rawHeader) > 16<<10 {
		return nil, ErrRepositoryViewSourceLimit
	}
	totalBytes := len(rawHeader)
	base, err := MemoryCaptureGraphBase(MemoryCaptureBundle{Header: header, Parts: parts})
	if err != nil {
		return nil, err
	}
	deltaBytes, deltaRows := 0, 0
	graphBytes, graphRows, memoryBytes, pinBytes, pinRows := 0, 0, 0, 0, 0
	dependencies := make(map[ContentHash]string)
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(part.Kind) > 64 || len(part.Key) > MaxRepositoryViewSourceKeyBytes || len(part.Value) > MaxMemoryCapturePartBytes {
			return nil, ErrRepositoryViewSourceLimit
		}
		cost := len(part.Kind) + len(part.Key) + len(part.Value) + 128
		if cost > MaxMemoryCaptureBytes-totalBytes {
			return nil, ErrRepositoryViewSourceLimit
		}
		totalBytes += cost
		if part.Key == "" || !utf8.ValidString(part.Key) || strings.IndexByte(part.Key, 0) >= 0 || part.Sequence < 0 {
			return nil, ErrIntegrity
		}
		var dependency ContentHash
		switch {
		case part.Kind == "graph_ref":
		case strings.HasPrefix(part.Kind, "graph_delta:"):
			kind := strings.TrimPrefix(part.Kind, "graph_delta:")
			if header.Version != MemoryCaptureDeltaVersion || !RepositoryGraphSourceKindValid(kind) ||
				part.Sequence <= base.Checkpoint.Sequence || part.Sequence > header.Generation.Graph.Sequence {
				return nil, ErrIntegrity
			}
			deltaRows++
			deltaBytes += cost
			if deltaRows > MaxMemoryCaptureDeltaParts || deltaBytes > MaxMemoryCaptureDeltaBytes {
				return nil, ErrRepositoryViewSourceLimit
			}
		case strings.HasPrefix(part.Kind, "graph:"):
			kind := strings.TrimPrefix(part.Kind, "graph:")
			if header.Version != MemoryCaptureVersion || !RepositoryGraphSourceKindValid(kind) || part.Sequence > header.Generation.Graph.Sequence {
				return nil, ErrIntegrity
			}
			graphRows++
			graphBytes += cost - len("graph:")
			if graphRows > MaxPendingProjectionInputRows || graphBytes > MaxPendingProjectionInputBytes {
				return nil, ErrRepositoryViewSourceLimit
			}
		case part.Kind == "memory":
			if part.Sequence != 0 || len(part.Value) == 0 {
				return nil, ErrIntegrity
			}
			dependency, _, err = memoryCaptureChunkKey(part.Key)
			if err != nil {
				return nil, err
			}
			memoryBytes += len(part.Value)
			if len(part.Value) > MemoryCaptureChunkBytes || memoryBytes > MaxMemoryCaptureObjectBytes {
				return nil, ErrMemoryProjectionLimit
			}
		case part.Kind == "absent":
			dependency = ContentHash(part.Key)
			if part.Sequence != 0 || len(part.Value) != 0 || ValidateContentHash(dependency) != nil {
				return nil, ErrIntegrity
			}
		case part.Kind == "pr_pin" || part.Kind == "git_pin":
			if part.Sequence != 0 || !header.PinsCaptured {
				return nil, ErrIntegrity
			}
			pinRows++
			pinBytes += len(part.Value)
			if pinRows > MaxMemoryCaptureObjects || pinBytes > MaxMemoryCapturePinBytes {
				return nil, ErrMemoryProjectionLimit
			}
		default:
			return nil, ErrIntegrity
		}
		if dependency != "" {
			if previous, exists := dependencies[dependency]; exists {
				if previous != part.Kind {
					return nil, ErrIntegrity
				}
			} else {
				if len(dependencies) == MaxMemoryCaptureObjects {
					return nil, ErrMemoryProjectionLimit
				}
				dependencies[dependency] = part.Kind
			}
		}
	}
	return rawHeader, ctx.Err()
}

func memoryCapturePartLess(left, right MemoryCapturePart) bool {
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	return left.Key < right.Key
}

func memoryCaptureValidateParts(ctx context.Context, parts []MemoryCapturePart) error {
	var previousHash ContentHash
	nextChunk := 0
	for index, part := range parts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if index > 0 && !memoryCapturePartLess(parts[index-1], part) {
			return ErrIntegrity
		}
		switch part.Kind {
		case "memory":
			dependency, chunk, err := memoryCaptureChunkKey(part.Key)
			if err != nil {
				return err
			}
			if dependency != previousHash {
				previousHash, nextChunk = dependency, 0
			}
			if chunk != nextChunk {
				return ErrIntegrity
			}
			nextChunk++
		case "absent":
		default:
			if strings.HasPrefix(part.Kind, "graph_delta:") && len(part.Value) == 0 {
				continue
			}
			if !utf8.Valid(part.Value) || !json.Valid(part.Value) {
				return ErrIntegrity
			}
			if (strings.HasPrefix(part.Kind, "graph:") || strings.HasPrefix(part.Kind, "graph_delta:")) && bytes.TrimSpace(part.Value)[0] != '{' {
				return ErrIntegrity
			}
		}
	}
	return ctx.Err()
}

func memoryCaptureChunkKey(key string) (ContentHash, int, error) {
	const hashBytes = len(contentHashPrefix) + 64
	if len(key) != hashBytes+9 || key[hashBytes] != ':' {
		return "", 0, ErrIntegrity
	}
	dependency := ContentHash(key[:hashBytes])
	if ValidateContentHash(dependency) != nil {
		return "", 0, ErrIntegrity
	}
	chunk := 0
	for _, digit := range key[hashBytes+1:] {
		if digit < '0' || digit > '9' {
			return "", 0, ErrIntegrity
		}
		chunk = chunk*10 + int(digit-'0')
	}
	return dependency, chunk, nil
}

func memoryCaptureRoot(ctx context.Context, version int, rawHeader []byte, parts []MemoryCapturePart) (ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	root := sha256.New()
	if version == MemoryCaptureVersion {
		root.Write([]byte("cxthub:memory-capture:v1\x00"))
	} else {
		root.Write([]byte("cxthub:memory-capture:v2\x00"))
	}
	memoryCaptureHashField(root, rawHeader)
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], uint64(len(parts)))
	root.Write(number[:])
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		root.Write([]byte("part\x00"))
		memoryCaptureHashField(root, []byte(part.Kind))
		memoryCaptureHashField(root, []byte(part.Key))
		binary.BigEndian.PutUint64(number[:], uint64(part.Sequence))
		root.Write(number[:])
		digest := sha256.Sum256(part.Value)
		root.Write(digest[:])
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return ContentHash(contentHashPrefix + hex.EncodeToString(root.Sum(nil))), nil
}

func memoryCaptureHashField(root hash.Hash, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	root.Write(length[:])
	root.Write(value)
}
