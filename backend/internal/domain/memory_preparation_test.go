package domain

import (
	"strings"
	"testing"
)

func TestMemoryReadPreparationRequestBounds(test *testing.T) {
	valid := MemoryReadPreparationRequest{Version: 1, RepoID: HashContent([]byte("repo")),
		Selection: EffectiveMemorySelection{SnapshotID: HashContent([]byte("snapshot")), CodeCommit: strings.Repeat("1", 40)},
		Cursor:    "pending", DocumentIdentities: []DocumentIdentity{DocumentIdentityLegacy, DocumentIdentityRootV1}}
	if err := valid.Validate(); err != nil {
		test.Fatal(err)
	}
	for _, name := range []string{"version", "repo", "selection", "content", "empty", "cursor", "identities", "duplicate", "escaped_size"} {
		test.Run(name, func(test *testing.T) {
			request := valid
			switch name {
			case "version":
				request.Version++
			case "repo":
				request.RepoID = "invalid"
			case "selection":
				request.Selection.CodeCommit = "main"
			case "content":
				request.Content = "all"
			case "empty":
				request.Cursor = ""
			case "cursor":
				request.Cursor = strings.Repeat("a", MaxPreparedMemoryCursorBytes+1)
			case "identities":
				request.DocumentIdentities = []DocumentIdentity{"unknown"}
			case "duplicate":
				request.DocumentIdentities = []DocumentIdentity{DocumentIdentityRootV1, DocumentIdentityRootV1}
			case "escaped_size":
				request.Cursor = strings.Repeat("\x00", MaxPreparedMemoryCursorBytes)
			}
			if err := request.Validate(); err == nil {
				test.Fatal("invalid preparation accepted")
			}
		})
	}
}
