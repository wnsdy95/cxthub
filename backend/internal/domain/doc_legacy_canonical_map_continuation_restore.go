package domain

type legacyCanonicalMapContinuationRestored struct {
}

type legacyCanonicalMapContinuationRestore struct {
	binding  ContentHash
	codec    legacyCanonicalMapContinuationValueCodec
	state    legacyCanonicalMapContinuationState
	entry    legacyCanonicalMapContinuationEntry
	result   legacyCanonicalMapContinuationRestored
	budget   int64
	count    int
	position int
	phase    uint8
	ready    bool
	closed   bool
	failure  error
}
