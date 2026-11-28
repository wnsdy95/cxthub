package domain

import "strings"

// IsAgentContextPackageText lets distillation exclude synthetic input
// from new user decisions. Archived source records remain untouched.
func IsAgentContextPackageText(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "[cxt context package v1]\n")
}
