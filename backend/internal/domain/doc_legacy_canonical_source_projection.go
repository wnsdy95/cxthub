package domain

type legacyCanonicalProjectionSlot string

const (
	legacyCanonicalProjectionRoot        legacyCanonicalProjectionSlot = "root"
	legacyCanonicalProjectionMetadata    legacyCanonicalProjectionSlot = "metadata"
	legacyCanonicalProjectionLocked      legacyCanonicalProjectionSlot = "locked"
	legacyCanonicalProjectionEmptyInput  legacyCanonicalProjectionSlot = "empty-input"
	legacyCanonicalProjectionBlocks      legacyCanonicalProjectionSlot = "blocks"
	legacyCanonicalProjectionReplacement legacyCanonicalProjectionSlot = "replacement"
)

type legacyCanonicalProjectionReference struct {
	Slot legacyCanonicalProjectionSlot `json:"slot"`
}
