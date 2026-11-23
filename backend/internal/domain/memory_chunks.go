package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	MemoryChunkFormatV1 = "cxt-memory-chunks-v1"
	MemoryChunkFormatV2 = "cxt-memory-chunks-v2"
	MemoryChunkFormatV3 = "cxt-memory-chunks-v3"
	MemoryChunkFormatV4 = "cxt-memory-chunks-v4"
)

// MemoryChunkManifest is an at-rest representation. MemoryDigestHash remains
// the hash of complete wire JSON; storage chunking never changes identity.
type MemoryChunkManifest struct {
	ClaimsVersion      uint32               `json:"claims_version,omitempty"`
	Format             string               `json:"format"`
	SnapshotID         ContentHash          `json:"snapshot_id"`
	PreviousMemoryHash ContentHash          `json:"previous_memory_hash,omitempty"`
	SummaryChunks      []ContentHash        `json:"summary_chunks,omitempty"`
	KeyFacts           []string             `json:"key_facts"`
	OpenTasks          []string             `json:"open_tasks"`
	Provider           ProviderKind         `json:"provider"`
	FragmentChunks     []ContentHash        `json:"fragment_chunks,omitempty"`
	GraftCoverage      *MemoryGraftCoverage `json:"graft_coverage,omitempty"`
}

func SupportedMemoryChunkFormat(format string) bool {
	return format == MemoryChunkFormatV1 || format == MemoryChunkFormatV2 || format == MemoryChunkFormatV3 || format == MemoryChunkFormatV4
}

func ParseMemoryChunkManifest(data []byte) (MemoryChunkManifest, bool, error) {
	var probe struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		limit := len(data)
		if limit > 128 {
			limit = 128
		}
		if bytes.Contains(data[:limit], []byte("cxt-memory-chunks-")) {
			return MemoryChunkManifest{}, true, err
		}
		return MemoryChunkManifest{}, false, nil
	}
	if !strings.HasPrefix(probe.Format, "cxt-memory-chunks-") {
		return MemoryChunkManifest{}, false, nil
	}
	if !SupportedMemoryChunkFormat(probe.Format) {
		return MemoryChunkManifest{}, true, fmt.Errorf("unsupported memory manifest format %q", probe.Format)
	}
	var man MemoryChunkManifest
	if err := json.Unmarshal(data, &man); err != nil {
		return MemoryChunkManifest{}, true, err
	}
	if (man.Format == MemoryChunkFormatV4) != (man.ClaimsVersion == MemoryClaimsVersion) || man.ClaimsVersion > MemoryClaimsVersion {
		return MemoryChunkManifest{}, true, fmt.Errorf("invalid claims version for memory manifest")
	}
	if len(man.SummaryChunks) == 0 && len(man.FragmentChunks) == 0 {
		return MemoryChunkManifest{}, true, fmt.Errorf("empty memory manifest")
	}
	return man, true, nil
}

func AssembleMemoryChunks(man MemoryChunkManifest, bodies map[ContentHash][]byte) (MemoryDigest, error) {
	join := func(hashes []ContentHash) ([]byte, error) {
		var out bytes.Buffer
		for _, hash := range hashes {
			if err := ValidateContentHash(hash); err != nil {
				return nil, err
			}
			body, exists := bodies[hash]
			if !exists || HashContent(body) != hash {
				return nil, ErrIntegrity
			}
			out.Write(body)
		}
		return out.Bytes(), nil
	}
	summary, err := join(man.SummaryChunks)
	if err != nil {
		return MemoryDigest{}, err
	}
	fragmentBytes, err := join(man.FragmentChunks)
	if err != nil {
		return MemoryDigest{}, err
	}
	var fragments []MemoryFragment
	if len(fragmentBytes) > 0 {
		if err := json.Unmarshal(fragmentBytes, &fragments); err != nil {
			return MemoryDigest{}, err
		}
	}
	return MemoryDigest{
		ClaimsVersion:      man.ClaimsVersion,
		SnapshotID:         man.SnapshotID,
		PreviousMemoryHash: man.PreviousMemoryHash,
		Summary:            string(summary),
		KeyFacts:           man.KeyFacts,
		OpenTasks:          man.OpenTasks,
		Provider:           man.Provider,
		Fragments:          fragments,
		GraftCoverage:      man.GraftCoverage,
	}, nil
}
