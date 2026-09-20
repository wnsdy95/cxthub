package domain

// MergeDigestSequence is equivalent to the ordered pairwise fold, but renders
// attributed fragments once. A branch with many PRs must not rescan and copy
// the complete accumulated archive for every source. Opaque unattributed input
// keeps the legacy pairwise semantics.
func MergeDigestSequence(digests ...MemoryDigest) MemoryDigest {
	if len(digests) == 0 {
		return MemoryDigest{}
	}
	if len(digests) == 1 {
		return digests[0]
	}
	groups := make([][]MemoryFragment, 0, len(digests))
	out := digests[len(digests)-1]
	for _, d := range digests {
		f := memoryFragments(d)
		if len(f) == 0 {
			legacy := digests[0]
			for _, next := range digests[1:] {
				legacy = MergeDigests(legacy, next)
			}
			return legacy
		}
		groups = append(groups, f)
		if d.ClaimsVersion > out.ClaimsVersion {
			out.ClaimsVersion = d.ClaimsVersion
		}
	}
	out.Fragments = mergeMemoryFragments(groups...)
	if out.HasMemoryClaims() && out.ClaimsVersion == 0 {
		out.ClaimsVersion = MemoryClaimsVersion
	}
	renderMemoryFragments(&out)
	return out
}
