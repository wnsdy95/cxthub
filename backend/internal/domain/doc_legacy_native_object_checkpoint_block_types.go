package domain

type legacyNativeObjectCheckpointBlockField struct {
	Destination legacyNativeCheckpointBlockArray `json:"destination"`
	Binding     uint8                            `json:"binding"`
	Parser      legacyNativeCheckpointRef        `json:"parser"`
	Initialized bool                             `json:"initialized"`
	Paged       bool                             `json:"paged"`
}

type legacyNativeObjectBlockRestored struct {
	Values []ContentBlock
	Parser *legacyNativeBlockList
	Heap   docRangeHeaderHeapEstimate
	Excess int64
	Source LegacyByteSource
}
