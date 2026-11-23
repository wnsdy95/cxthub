package domain

const legacyCanonicalProjectionRootPosition = -1

type legacyCanonicalSourceProjectionCapture struct {
	root          map[string]any
	keysPosition  int
	fieldPosition int
}
