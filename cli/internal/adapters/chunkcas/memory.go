package chunkcas

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// MemoryChunkTarget is deliberately smaller than the transcript chunk target.
// Memory summaries are copied between generations and commonly share long
// prefixes, so 64 KiB keeps the rewritten tail bounded without creating a
// large number of tiny files.
const MemoryChunkTarget = 64 << 10

const MemoryFormatV1 = "cxt-memory-chunks-v1"

// MemoryManifest is an at-rest representation only. The wire protocol keeps
// sending a complete MemoryDigest, and MemoryDigestHash remains the identity.
// Potentially large, prefix-sharing components are chunked independently;
// small structured fields stay inline.
type MemoryManifest struct {
	Format         string               `json:"format"`
	SnapshotID     domain.ContentHash   `json:"snapshot_id"`
	SummaryChunks  []domain.ContentHash `json:"summary_chunks,omitempty"`
	KeyFacts       []string             `json:"key_facts"`
	OpenTasks      []string             `json:"open_tasks"`
	Provider       domain.ProviderKind  `json:"provider"`
	FragmentChunks []domain.ContentHash `json:"fragment_chunks,omitempty"`
}

type MemoryPlan struct {
	Manifest MemoryManifest
	Bodies   map[domain.ContentHash][]byte
	Order    []domain.ContentHash
}

// PlanMemory returns ok=false for small digests, which stay as one legacy JSON
// object. For larger digests it creates fixed-offset component chunks and
// verifies that reconstruction preserves the original content identity.
func PlanMemory(d domain.MemoryDigest) (plan MemoryPlan, ok bool, err error) {
	var fragments []byte
	if len(d.Fragments) > 0 {
		fragments, err = json.Marshal(d.Fragments)
		if err != nil {
			return MemoryPlan{}, false, err
		}
	}
	if len([]byte(d.Summary))+len(fragments) <= MemoryChunkTarget {
		return MemoryPlan{}, false, nil
	}
	plan = MemoryPlan{
		Manifest: MemoryManifest{
			Format:     MemoryFormatV1,
			SnapshotID: d.SnapshotID,
			KeyFacts:   d.KeyFacts,
			OpenTasks:  d.OpenTasks,
			Provider:   d.Provider,
		},
		Bodies: map[domain.ContentHash][]byte{},
	}
	plan.Manifest.SummaryChunks = plan.addComponent([]byte(d.Summary))
	plan.Manifest.FragmentChunks = plan.addComponent(fragments)

	reassembled, err := AssembleMemory(plan.Manifest, plan.Bodies)
	if err != nil {
		return MemoryPlan{}, false, err
	}
	want, err := domain.MemoryDigestHash(d)
	if err != nil {
		return MemoryPlan{}, false, err
	}
	got, err := domain.MemoryDigestHash(reassembled)
	if err != nil || got != want {
		return MemoryPlan{}, false, domain.ErrHashMismatch
	}
	return plan, true, nil
}

func (p *MemoryPlan) addComponent(data []byte) []domain.ContentHash {
	if len(data) == 0 {
		return nil
	}
	hashes := make([]domain.ContentHash, 0, (len(data)+MemoryChunkTarget-1)/MemoryChunkTarget)
	for len(data) > 0 {
		n := MemoryChunkTarget
		if len(data) < n {
			n = len(data)
		}
		body := append([]byte(nil), data[:n]...)
		hash := domain.HashContent(body)
		p.Bodies[hash] = body
		p.Order = append(p.Order, hash)
		hashes = append(hashes, hash)
		data = data[n:]
	}
	return hashes
}

// ParseMemoryManifest distinguishes legacy full JSON from a chunk manifest.
// A recognized/future manifest with malformed or unsupported contents is
// reported as a manifest error instead of being silently parsed as a digest.
func ParseMemoryManifest(data []byte) (MemoryManifest, bool, error) {
	var probe struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		limit := len(data)
		if limit > 128 {
			limit = 128
		}
		if bytes.Contains(data[:limit], []byte("cxt-memory-chunks-")) {
			return MemoryManifest{}, true, err
		}
		return MemoryManifest{}, false, nil
	}
	if !strings.HasPrefix(probe.Format, "cxt-memory-chunks-") {
		return MemoryManifest{}, false, nil
	}
	if probe.Format != MemoryFormatV1 {
		return MemoryManifest{}, true, fmt.Errorf("unsupported memory manifest format %q", probe.Format)
	}
	var man MemoryManifest
	if err := json.Unmarshal(data, &man); err != nil {
		return MemoryManifest{}, true, err
	}
	if len(man.SummaryChunks) == 0 && len(man.FragmentChunks) == 0 {
		return MemoryManifest{}, true, fmt.Errorf("empty memory manifest")
	}
	return man, true, nil
}

// AssembleMemory rebuilds a digest and validates every component's own CAS
// identity. The caller additionally compares the digest hash with its object
// path, which protects the inline manifest fields.
func AssembleMemory(man MemoryManifest, bodies map[domain.ContentHash][]byte) (domain.MemoryDigest, error) {
	join := func(hashes []domain.ContentHash) ([]byte, error) {
		var out bytes.Buffer
		for _, hash := range hashes {
			if err := domain.ValidateContentHash(hash); err != nil {
				return nil, err
			}
			body, exists := bodies[hash]
			if !exists || domain.HashContent(body) != hash {
				return nil, domain.ErrHashMismatch
			}
			out.Write(body)
		}
		return out.Bytes(), nil
	}
	summary, err := join(man.SummaryChunks)
	if err != nil {
		return domain.MemoryDigest{}, err
	}
	fragmentBytes, err := join(man.FragmentChunks)
	if err != nil {
		return domain.MemoryDigest{}, err
	}
	var fragments []domain.MemoryFragment
	if len(fragmentBytes) > 0 {
		if err := json.Unmarshal(fragmentBytes, &fragments); err != nil {
			return domain.MemoryDigest{}, err
		}
	}
	return domain.MemoryDigest{
		SnapshotID: man.SnapshotID,
		Summary:    string(summary),
		KeyFacts:   man.KeyFacts,
		OpenTasks:  man.OpenTasks,
		Provider:   man.Provider,
		Fragments:  fragments,
	}, nil
}
