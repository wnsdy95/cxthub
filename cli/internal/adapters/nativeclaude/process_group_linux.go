//go:build linux

package nativeclaude

// Linux does not have Darwin's zombie-only EPERM behavior. Without positive
// process-group evidence a denied final signal remains a cleanup failure.
func processGroupContainsOnlyZombies(int) bool { return false }
