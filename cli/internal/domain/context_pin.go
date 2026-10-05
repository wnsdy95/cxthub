package domain

// IsPinnedContextEvent identifies an explicit context/memory observation.
// Renaming or archiving a branch does not select a new PR source memory.
func IsPinnedContextEvent(e HistoryEvent) bool {
	if !e.MemoryPinned || e.Target == "" {
		return false
	}
	switch e.Kind {
	case "birth", "orphan", "attach", "position", "advance":
		return true
	default:
		return false
	}
}
