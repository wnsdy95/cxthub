package domain

type legacyNativeCheckpointBlockRecord struct {
	Schema ContentHash                   `json:"schema"`
	Values legacyNativeCheckpointStrings `json:"values"`
}

type legacyNativeCheckpointBlockArray struct {
	Nil      bool                       `json:"nil"`
	Length   int                        `json:"length"`
	Capacity int                        `json:"capacity"`
	Records  legacyNativeCheckpointTree `json:"records"`
}

type legacyNativeCheckpointBlockListState struct {
	Schema       ContentHash                      `json:"schema"`
	Version      uint8                            `json:"v"`
	Binding      ContentHash                      `json:"binding"`
	SourceBytes  int                              `json:"source_bytes"`
	Position     int                              `json:"position"`
	End          int                              `json:"end"`
	ParentDepth  int                              `json:"parents"`
	Maximum      int                              `json:"maximum"`
	Count        int                              `json:"count"`
	Copied       int                              `json:"copied"`
	ChildStart   int                              `json:"child_start"`
	Literal      int                              `json:"literal"`
	Seeded       int                              `json:"seeded"`
	Phase        uint8                            `json:"phase"`
	Prefix       bool                             `json:"prefix"`
	Paged        bool                             `json:"paged"`
	Initialized  bool                             `json:"initialized"`
	Decoded      legacyNativeCheckpointBlockArray `json:"decoded"`
	Previous     legacyNativeCheckpointBlockArray `json:"previous"`
	HasData      bool                             `json:"has_data"`
	DataCount    int                              `json:"data_count"`
	DataRecords  legacyNativeCheckpointTree       `json:"data_records"`
	HasFrame     bool                             `json:"has_frame"`
	Frame        legacyNativeCheckpointRef        `json:"frame"`
	Object       legacyNativeCheckpointRef        `json:"object"`
	BlockAliases uint8                            `json:"block_aliases"`
}

type legacyNativeBlockListRestored struct {
	List   *legacyNativeBlockList
	Source LegacyByteSource
}
