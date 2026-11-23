package domain

type legacyCanonicalSourceKeyReference struct {
	Source legacyCanonicalSourceReference `json:"source"`
	Member uint64                         `json:"member"`
}
