package domain

import (
	"strings"
	"testing"
)

func lifecycleHash(ch byte) ContentHash {
	return ContentHash("sha256:" + strings.Repeat(string(ch), 64))
}

func TestBranchLifecycleRefRejectsMalformedReservedTags(t *testing.T) {
	repo := lifecycleHash('0')
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
