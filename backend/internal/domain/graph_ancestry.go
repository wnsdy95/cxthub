package domain

import "math/bits"

// A closure's identifiers are indexed once per coherent read. The compact
// bitset is immutable after construction and never escapes the query model.
type graphClosureIDs struct {
	index *graphStateIndex
	words []uint64
}

func (s graphClosureIDs) has(id ContentHash) bool {
	n, ok := s.index.ordinal[id]
	return ok && n/64 < len(s.words) && s.words[n/64]&(uint64(1)<<uint(n%64)) != 0
}
func (s graphClosureIDs) each(yield func(ContentHash) bool) {
	for block, word := range s.words {
		for word != 0 {
			bit := bits.TrailingZeros64(word)
			if !yield(s.index.nodeIDs[block*64+bit]) {
				return
			}
			word &= word - 1
		}
	}
}
func (s graphClosureIDs) list() []ContentHash {
	ids := []ContentHash{}
	for id := range s.each {
		ids = append(ids, id)
	}
	return ids
}
func (s graphClosureIDs) subset(keep func(ContentHash) bool) graphSet {
	out := graphSet{}
	for id := range s.each {
		if keep(id) {
			out[id] = true
		}
	}
	return out
}
