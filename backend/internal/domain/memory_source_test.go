package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRepositoryMemorySourceGeneration(test *testing.T) {
	valid := RepositoryMemorySourceGeneration{
		Version: RepositoryMemorySourceVersion,
		Graph: RepositoryViewSourceCheckpoint{
			Version: RepositoryViewSourceVersion, Scope: RepositoryGraphSourceScope,
			RepoID: HashContent([]byte("memory source")), Epoch: "12345678-1234-4321-9876-123456789abc", Sequence: 7,
		},
		Epoch: "23456789-1234-4321-9876-123456789abc", Sequence: 9007199254740993,
	}
	if err := valid.Validate(); err != nil {
		test.Fatal(err)
	}
	encoded, err := json.Marshal(valid)
	if err != nil || !strings.Contains(string(encoded), `"sequence":"9007199254740993"`) {
		test.Fatalf("source wire precision: %s %v", encoded, err)
	}
	var decoded RepositoryMemorySourceGeneration
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != valid {
		test.Fatal("source wire round trip", err)
	}
	for _, mutate := range []func(*RepositoryMemorySourceGeneration){
		func(value *RepositoryMemorySourceGeneration) { value.Version++ },
		func(value *RepositoryMemorySourceGeneration) { value.Graph.Scope = RepositoryViewSourceScope },
		func(value *RepositoryMemorySourceGeneration) { value.Graph.RepoID = "foreign" },
		func(value *RepositoryMemorySourceGeneration) { value.Graph.Sequence = -1 },
		func(value *RepositoryMemorySourceGeneration) { value.Epoch = "" },
		func(value *RepositoryMemorySourceGeneration) { value.Epoch = "00000000-0000-0000-0000-000000000000" },
		func(value *RepositoryMemorySourceGeneration) { value.Sequence = -1 },
	} {
		invalid := valid
		mutate(&invalid)
		if invalid.Validate() == nil {
			test.Fatalf("accepted invalid generation: %+v", invalid)
		}
	}
}
