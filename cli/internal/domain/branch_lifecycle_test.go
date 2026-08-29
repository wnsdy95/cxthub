package domain

import (
	"strings"
	"testing"
)

func lifecycleHash(ch byte) ContentHash {
	return ContentHash("sha256:" + strings.Repeat(string(ch), 64))
}

func TestBranchLifecycleRefRoundTripAndOrdering(t *testing.T) {
	repo := string(lifecycleHash('0'))
	target := lifecycleHash('a')
	archived, err := NewBranchLifecycleRef(repo, "feature/auth", target, 7, BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	if archived.Kind != RefTag || !strings.HasPrefix(archived.Name, BranchLifecycleTagPrefix) {
		t.Fatalf("archive ref = %+v", archived)
	}
	event, ok, err := ParseBranchLifecycleRef(archived)
	if err != nil || !ok {
		t.Fatalf("parse = %+v, %v, %v", event, ok, err)
	}
	if event.Branch != "feature/auth" || event.Target != target || event.Generation != 7 || event.State != BranchArchived {
		t.Fatalf("round trip = %+v", event)
	}

	active, err := NewBranchLifecycleRef(repo, "feature/auth", target, 7, BranchActive)
	if err != nil {
		t.Fatal(err)
	}
	latest, ok, err := LatestBranchLifecycle([]Ref{archived, active}, "feature/auth")
	if err != nil || !ok || latest.State != BranchActive {
		t.Fatalf("same-generation preservation = %+v, %v, %v", latest, ok, err)
	}

	newer, err := NewBranchLifecycleRef(repo, "feature/auth", target, 8, BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	latest, ok, err = LatestBranchLifecycle([]Ref{newer, active}, "feature/auth")
	if err != nil || !ok || latest.State != BranchArchived || latest.Generation != 8 {
		t.Fatalf("newest generation = %+v, %v, %v", latest, ok, err)
	}
	states, err := BranchLifecycleStates([]Ref{newer, active})
	if err != nil || len(states) != 1 || states["feature/auth"].Ref.Name != newer.Name {
		t.Fatalf("single-pass states = %+v, %v", states, err)
	}
}

func TestBranchLifecycleRefRejectsMalformedReservedTags(t *testing.T) {
	repo := string(lifecycleHash('0'))
	target := lifecycleHash('a')
	malformed := []Ref{
		{Kind: RefTag, Name: BranchLifecycleTagPrefix + "7/archived/" + strings.TrimPrefix(string(target), "sha256:") + "/main", RepoID: repo, Target: target},
		{Kind: RefTag, Name: BranchLifecycleTagPrefix + "00000000000000000007/removed/" + strings.TrimPrefix(string(target), "sha256:") + "/main", RepoID: repo, Target: target},
		{Kind: RefTag, Name: BranchLifecycleTagPrefix + "00000000000000000007/archived/" + strings.Repeat("b", 64) + "/main", RepoID: repo, Target: target},
	}
	for _, ref := range malformed {
		if _, ok, err := ParseBranchLifecycleRef(ref); !ok || err == nil {
			t.Fatalf("malformed reserved tag accepted: %+v, ok=%v err=%v", ref, ok, err)
		}
	}

	ordinary := Ref{Kind: RefTag, Name: "release/v1", RepoID: repo, Target: target}
	if _, ok, err := ParseBranchLifecycleRef(ordinary); ok || err != nil {
		t.Fatalf("ordinary tag classified as lifecycle: ok=%v err=%v", ok, err)
	}
}
