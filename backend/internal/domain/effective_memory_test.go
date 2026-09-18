package domain

import "testing"

func TestEffectiveMemoryScopeAssessment(t *testing.T) {
	old := GitEntry{OID: "old", Mode: "100644"}
	fresh := GitEntry{OID: "new", Mode: "100644"}
	other := GitEntry{OID: "other", Mode: "100644"}
	claim := MemoryClaim{Kind: "code", Code: &MemoryCodeScope{Paths: []string{"a", "b"}}}
	path := func(name string, entry GitEntry) CodePathState {
		return CodePathState{Path: name, State: "applied", Before: old, After: fresh, Selected: entry}
	}
	for _, tt := range []struct {
		name, integration, want string
		paths                   []CodePathState
	}{
		{"applied", "verified", "applied", []CodePathState{path("a", fresh), path("b", fresh)}},
		{"full inverse", "verified", "inactive", []CodePathState{path("a", old), path("b", old)}},
		{"partial inverse", "verified", "review", []CodePathState{path("a", old), path("b", fresh)}},
		{"later edit", "verified", "review", []CodePathState{path("a", other), path("b", fresh)}},
		{"equivalent is not inclusion", "absent", "review", []CodePathState{path("a", fresh), path("b", fresh)}},
		{"before source", "absent", "inactive", []CodePathState{path("a", old), path("b", old)}},
		{"unknown integration", "unknown", "review", []CodePathState{path("a", old), path("b", old)}},
		{"missing path", "verified", "review", []CodePathState{path("a", fresh)}},
		{"duplicate path", "verified", "review", []CodePathState{path("a", fresh), path("a", fresh)}},
		{"wrong path", "verified", "review", []CodePathState{path("a", fresh), path("c", fresh)}},
		{"pending path", "verified", "review", []CodePathState{path("a", fresh), {Path: "b", State: "unknown"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := AssessMemoryClaim(claim, tt.paths, tt.integration)
			if got.State != tt.want {
				t.Fatalf("got %+v want %s", got, tt.want)
			}
		})
	}
	for _, kind := range []string{"decision", "rationale"} {
		if got := AssessMemoryClaim(MemoryClaim{Kind: kind}, nil, "absent"); got.State != "retained" {
			t.Fatal(got)
		}
	}
}
