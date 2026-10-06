package nativeclaude

import "strings"

// A context summary retains Claude Code's [1m] window selector; API responses
// may omit it. This compares identity only: the original summary and measured
// window stay bound by admission. Never resolve aliases or infer a window here.
func sameResponseModel(actual, selected string) bool {
	if actual == selected {
		return actual != ""
	}
	if selected != actual+"[1m]" || !strings.HasPrefix(actual, "claude-") || len(actual) == len("claude-") {
		return false
	}
	for _, ch := range actual {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
			return false
		}
	}
	return true
}
