package app

import (
	"encoding/json"
	"fmt"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// memoryReadAnchor preserves every observed page across authorized rereads.
// Revision-derived hashes and cursors may move; source lineage and content may not.
type memoryReadAnchor struct{ pages map[int]memoryPageAnchor }

type memoryPageAnchor struct {
	body, state     domain.ContentHash
	cursor          string
	graph, evidence uint64
}

func (a *memoryReadAnchor) check(index int, p domain.EffectiveMemoryPage) error {
	next := memoryPageAnchor{state: p.StateHash, cursor: p.NextCursor, graph: p.Revision.Graph, evidence: p.Revision.Evidence}
	// Effective-memory StateHash includes repository read revisions. It is not a
	// content-only hash. Keep lineage and every item, but compare this hash only
	// while Graph/Evidence are equal.
	p.StateHash = ""
	p.Revision.Graph, p.Revision.Evidence, p.Revision.Pending = 0, 0, 0
	if p.NextCursor != "" {
		p.NextCursor = "present"
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	hash := domain.HashContent(raw)
	if a.pages == nil {
		a.pages = map[int]memoryPageAnchor{}
	}
	next.body = hash
	if old, ok := a.pages[index]; ok {
		if old.body != next.body || (old.graph == next.graph && old.evidence == next.evidence && (old.state != next.state || old.cursor != next.cursor)) {
			return fmt.Errorf("%w: selected memory content changed", domain.ErrSelectionChanged)
		}
	}
	a.pages[index] = next
	return nil
}
