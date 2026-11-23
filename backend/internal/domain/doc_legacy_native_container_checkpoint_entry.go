package domain

type legacyNativeMapCheckpointEntry struct {
}

type legacyNativeArrayCheckpointEntry struct {
	Position uint64
}

type legacyNativeMapEntrySnapshot struct {
	closed bool
}
