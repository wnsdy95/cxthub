package domain

// GraftQueueEvent is an ordered optimistic graft awaiting server acknowledgement.
type GraftQueueEvent struct {
	Snapshot    string   `json:"snapshot"`
	Parents     []string `json:"parents"`
	ExpectedSeq uint64   `json:"expected_seq"`
	// Legacy is an event promoted from the legacy map queue. The legacy queue did not upload a local GraftSeq, so a server projection must be re-fetched after success to match the seq.
	Legacy bool `json:"legacy,omitempty"`
}
