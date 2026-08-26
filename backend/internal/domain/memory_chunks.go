package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const MemoryChunkTarget = 64 << 10

const (
	MemoryChunkFormatV1 = "cxt-memory-chunks-v1"
	MemoryChunkFormatV2 = "cxt-memory-chunks-v2"
)

// MemoryChunkManifest is an at-rest representation. MemoryDigestHash remains
// the hash of complete wire JSON; storage chunking never changes identity.
type MemoryChunkManifest struct {
	Format         string               `json:"format"`
	SnapshotID     ContentHash          `json:"snapshot_id"`
	SummaryChunks  []ContentHash        `json:"summary_chunks,omitempty"`
	KeyFacts       []string             `json:"key_facts"`
	OpenTasks      []string             `json:"open_tasks"`
	Provider       ProviderKind         `json:"provider"`
	FragmentChunks []ContentHash        `json:"fragment_chunks,omitempty"`
	GraftCoverage  *MemoryGraftCoverage `json:"graft_coverage,omitempty"`
}

type MemoryChunkPlan struct {
	Manifest MemoryChunkManifest
	Bodies   map[ContentHash][]byte
	Order    []ContentHash
}

func PlanMemoryChunks(d MemoryDigest) (plan MemoryChunkPlan, ok bool, err error) {
	var fragments []byte
	if len(d.Fragments) > 0 {
		fragments, err = json.Marshal(d.Fragments)
		if err != nil {
			return MemoryChunkPlan{}, false, err
		}
	}
	if len([]byte(d.Summary))+len(fragments) <= MemoryChunkTarget {
		return MemoryChunkPlan{}, false, nil
	}
	plan = MemoryChunkPlan{
		Manifest: MemoryChunkManifest{
			Format:        memoryChunkFormatFor(d),
			SnapshotID:    d.SnapshotID,
			KeyFacts:      d.KeyFacts,
			OpenTasks:     d.OpenTasks,
			Provider:      d.Provider,
			GraftCoverage: d.GraftCoverage,
		},
		Bodies: map[ContentHash][]byte{},
	}
	plan.Manifest.SummaryChunks = plan.addComponent([]byte(d.Summary))
	plan.Manifest.FragmentChunks = plan.addComponent(fragments)
	reassembled, err := AssembleMemoryChunks(plan.Manifest, plan.Bodies)
	if err != nil {
		return MemoryChunkPlan{}, false, err
	}
	want, err := MemoryDigestHash(d)
	if err != nil {
		return MemoryChunkPlan{}, false, err
	}
	got, err := MemoryDigestHash(reassembled)
	if err != nil || got != want {
		return MemoryChunkPlan{}, false, ErrIntegrity
	}
	return plan, true, nil
}

func memoryChunkFormatFor(d MemoryDigest) string {
	if d.GraftCoverage != nil {
		return MemoryChunkFormatV2
	}
	return MemoryChunkFormatV1
}

func SupportedMemoryChunkFormat(format string) bool {
	return format == MemoryChunkFormatV1 || format == MemoryChunkFormatV2
}

func (p *MemoryChunkPlan) addComponent(data []byte) []ContentHash {
	if len(data) == 0 {
		return nil
	}
	hashes := make([]ContentHash, 0, (len(data)+MemoryChunkTarget-1)/MemoryChunkTarget)
	for len(data) > 0 {
		n := MemoryChunkTarget
		if len(data) < n {
			n = len(data)
		}
		body := append([]byte(nil), data[:n]...)
		hash := HashContent(body)
		p.Bodies[hash] = body
		p.Order = append(p.Order, hash)
		hashes = append(hashes, hash)
		data = data[n:]
	}
	return hashes
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
		SnapshotID:    man.SnapshotID,
		Summary:       string(summary),
		KeyFacts:      man.KeyFacts,
		OpenTasks:     man.OpenTasks,
		Provider:      man.Provider,
		Fragments:     fragments,
		GraftCoverage: man.GraftCoverage,
	}, nil
}
