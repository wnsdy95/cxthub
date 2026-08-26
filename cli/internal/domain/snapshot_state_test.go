package domain

import (
	"testing"
	"time"
)

func TestSnapshotStateHashParityAndMutableMetadata(t *testing.T) {
	base := Snapshot{
		ID: HashContent([]byte("snapshot")), RepoID: string(HashContent([]byte("repo"))), Branch: "main",
		Parents: []ContentHash{HashContent([]byte("parent"))}, DocHash: HashContent([]byte("snapshot")),
		MemoryHash: HashContent([]byte("memory")), ClaudeSettings: HashContent([]byte("claude")),
		AgentsSettings: HashContent([]byte("agents")), CodexSettings: HashContent([]byte("codex")),
		Provider: ProviderCodex, Fidelity: FidelityFull, Message: "[git abc123] commit",
		Author:    TeamIdentity{Name: "J", Email: "j@example.test", Team: "core"},
		CreatedAt: time.Date(2026, 8, 27, 1, 2, 3, 4, time.FixedZone("KST", 9*60*60)),
		Grafted:   true, GraftParents: []ContentHash{HashContent([]byte("graft"))}, GraftSeq: 7,
		SessionID: "session-1", Models: []string{"gpt-5", "gpt-5.3-codex"}, CompactionCount: 2,
	}
	got, err := SnapshotStateHash(base)
	if err != nil {
		t.Fatal(err)
	}
	const want ContentHash = "sha256:e67fd0f252c9bf3ba8170a3d62a285976b44b6c719ecd9858598487debbc4736"
	if got != want {
		t.Fatalf("snapshot state hash = %s, want %s", got, want)
	}

	mutations := map[string]func(*Snapshot){
		"branch":       func(s *Snapshot) { s.Branch = "feature" },
		"message":      func(s *Snapshot) { s.Message = "promoted" },
		"memory":       func(s *Snapshot) { s.MemoryHash = HashContent([]byte("memory-2")) },
		"graft parent": func(s *Snapshot) { s.GraftParents = append(s.GraftParents, HashContent([]byte("graft-2"))) },
		"graft seq":    func(s *Snapshot) { s.GraftSeq++ },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := base
			changed.Parents = append([]ContentHash{}, base.Parents...)
			changed.GraftParents = append([]ContentHash{}, base.GraftParents...)
			changed.Models = append([]string{}, base.Models...)
			mutate(&changed)
			hash, err := SnapshotStateHash(changed)
			if err != nil {
				t.Fatal(err)
			}
			if hash == got {
				t.Fatalf("%s mutation did not change state hash", name)
			}
		})
	}
}

func TestSnapshotStateHashIgnoresImmutableReplicaDifferences(t *testing.T) {
	base := Snapshot{ID: HashContent([]byte("snapshot")), DocHash: HashContent([]byte("snapshot")), Branch: "main", Message: "commit"}
	changed := base
	changed.RepoID = string(HashContent([]byte("repo")))
	changed.Parents = []ContentHash{HashContent([]byte("parent"))}
	changed.CodexSettings = HashContent([]byte("settings"))
	changed.Provider = ProviderCodex
	changed.Fidelity = FidelityFull
	changed.Author = TeamIdentity{Name: "author"}
	changed.CreatedAt = time.Now()
	changed.SessionID = "session"
	changed.Models = []string{"model"}
	changed.CompactionCount = 3
	left, err := SnapshotStateHash(base)
	if err != nil {
		t.Fatal(err)
	}
	right, err := SnapshotStateHash(changed)
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("immutable replica fields changed state hash: %s != %s", left, right)
	}
}

func TestSnapshotStateHashNormalizesNilSlices(t *testing.T) {
	left := Snapshot{ID: HashContent([]byte("snapshot")), DocHash: HashContent([]byte("snapshot"))}
	right := left
	right.Parents = []ContentHash{}
	right.GraftParents = []ContentHash{}
	right.Models = []string{}
	a, err := SnapshotStateHash(left)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SnapshotStateHash(right)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("nil/empty state hashes differ: %s != %s", a, b)
	}
}
