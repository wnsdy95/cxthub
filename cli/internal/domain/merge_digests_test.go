package domain

import (
	"reflect"
	"testing"
)

// TestMergeDigestsOpenTasksAuthority fixes the OpenTasks merge authority rules:
// If fresh is a struct extraction (TasksAuthoritative), it does not inherit the ancestor list — to prevent regressions in completed tasks being permanently accumulated in the lineage (review P1). Non-struct fresh behaves as before, prioritizing the union of prior.
func TestMergeDigestsOpenTasksAuthority(t *testing.T) {
	prior := MemoryDigest{OpenTasks: []string{"done task from last week", "still open"}}

	fresh := MemoryDigest{OpenTasks: []string{"still open", "new task"}, TasksAuthoritative: true}
	got := MergeDigests(prior, fresh)
	if !reflect.DeepEqual(got.OpenTasks, []string{"still open", "new task"}) {
		t.Fatalf("Authority fresh but ancestor list inherited: %v", got.OpenTasks)
	}

	// Empty list for authority fresh is respected (all tasks completed states).
	got = MergeDigests(prior, MemoryDigest{TasksAuthoritative: true})
	if len(got.OpenTasks) != 0 {
		t.Fatalf("Empty list for authority fresh but ancestor remains: %v", got.OpenTasks)
	}

	// Non-struct fresh: maintains previous behavior (prior union and dedup).
	got = MergeDigests(prior, MemoryDigest{OpenTasks: []string{"still open", "another"}})
	want := []string{"done task from last week", "still open", "another"}
	if !reflect.DeepEqual(got.OpenTasks, want) {
		t.Fatalf("Non-struct merge = %v, want %v", got.OpenTasks, want)
	}
}
