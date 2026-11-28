package domain

// DocChunkSweep reports one repository visit of the orphan document chunk
// cleanup. An orphan is a chunk the repository owns that no stored document
// manifest of that repository and no pending finalization job names.
type DocChunkSweep struct {
	// Repo is empty when the pass had no repository left to visit.
	Repo ContentHash
	// Orphans counts the unreferenced chunk grants seen in this visit.
	Orphans int
	// Waiting counts the orphans whose grace period has not elapsed, or that
	// this process is not tracking yet.
	Waiting int
	// Removed counts the repository's chunk grants deleted in this visit.
	Removed int
	// Bodies and Bytes describe the stored chunk bodies deleted because no
	// repository owned them any more. A body another repository still owns
	// stays, so Bodies can be smaller than Removed.
	Bodies int
	Bytes  int64
	// Deferred counts orphans that were due but left for a later visit: in use
	// by a concurrent writer, named again in the meantime, or out of time.
	Deferred int
}
