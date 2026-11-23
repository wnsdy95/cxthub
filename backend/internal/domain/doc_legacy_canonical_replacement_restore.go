package domain

type legacyCanonicalReplacementRestored struct {
	projection docRangeHeaderHeapEstimate
}

type legacyCanonicalReplacementRestore struct {
	source  *legacyCanonicalReplacement
	binding ContentHash
	budget  int64
	state   legacyCanonicalReplacementState
	result  legacyCanonicalReplacementRestored
	loaded  bool
	ready   bool
	closed  bool
	failure error
}
