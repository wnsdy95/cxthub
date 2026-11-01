package domain

import "strings"

type legacyCanonicalPreparedKeyRef struct {
	key string
	ref legacyNativeCheckpointRef
}

type legacyCanonicalPreparedKeyRefs struct {
	entries [32]legacyCanonicalPreparedKeyRef
	next    int
}

func (refs *legacyCanonicalPreparedKeyRefs) find(key string) legacyNativeCheckpointRef {
	if refs != nil && len(key) <= 64 {
		for _, entry := range refs.entries {
			if entry.ref.Bytes != 0 && entry.key == key {
				return entry.ref
			}
		}
	}
	return legacyNativeCheckpointRef{}
}

func (refs *legacyCanonicalPreparedKeyRefs) remember(key string, ref legacyNativeCheckpointRef) {
	if refs == nil || len(key) > 64 || !ref.valid() {
		return
	}
	refs.entries[refs.next] = legacyCanonicalPreparedKeyRef{key: strings.Clone(key),
		ref: legacyNativeCheckpointRef{Hash: ContentHash(strings.Clone(string(ref.Hash))), Bytes: ref.Bytes}}
	refs.next = (refs.next + 1) % len(refs.entries)
}

func (refs *legacyCanonicalPreparedKeyRefs) retained(heap *docRangeHeaderHeapEstimate) {
	for _, entry := range refs.entries {
		heap.allocation(int64(len(entry.key)+len(entry.ref.Hash)), 1)
	}
}
