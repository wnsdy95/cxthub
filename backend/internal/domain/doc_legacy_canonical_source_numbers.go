package domain

type legacyCanonicalSourceNumberReference struct {
	Record  uint64 `json:"record"`
	Ordinal int    `json:"ordinal"`
}
