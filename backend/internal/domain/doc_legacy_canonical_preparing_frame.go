package domain

type legacyCanonicalPreparingFrameState struct {
	Heap        int64 `json:"heap"`
	Depth       int   `json:"depth"`
	Position    int   `json:"position"`
	Length      int   `json:"length"`
	Count       int   `json:"count"`
	MapPosition int   `json:"map_position"`
	KeyBytes    int   `json:"key_bytes"`
	Kind        byte  `json:"kind"`
	Phase       uint8 `json:"phase"`
	Prepare     bool  `json:"prepare"`
}
