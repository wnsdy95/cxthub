package storage

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRootStagedIdentityDuplicateRejectedByAllRecordReaders(t *testing.T) {
	for _, kind := range []string{"index", "stash", "commit"} {
		for _, lastKey := range []string{"doc_identity", "DOC_IDENTITY"} {
			for _, lastValue := range []domain.DocumentIdentity{"", domain.DocumentIdentityRootV1} {
				name := kind + "/" + lastKey + "/empty"
				if lastValue != "" {
					name = kind + "/" + lastKey + "/same-root"
				}
				t.Run(name, func(t *testing.T) {
					var f frozenFixture
					if lastValue == domain.DocumentIdentityRootV1 {
						f, _ = rootFrozenFixture(t, false)
					} else {
						f = newFrozenFixture(t)
					}
					var value any = f.index
					path := f.store.stagingPath()
					id := strings.Repeat("d", 32)
					switch kind {
					case "stash":
						value = domain.StagingStash{Version: f.index.RecordVersion(), ID: id, Index: f.index, Position: f.position, CreatedAt: time.Now().UTC()}
						path, _ = f.store.stagingStashPath(id)
					case "commit":
						value = f.op
						path, _ = f.store.stagingOperationPath(f.op.ID)
					}
					raw, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					marker := `"doc_identity":"` + string(domain.DocumentIdentityRootV1) + `","` + lastKey + `":"` + string(lastValue) + `","doc_hash":`
					modified := strings.Replace(string(raw), `"doc_hash":`, marker, 1)
					if modified == string(raw) {
						t.Fatal("fixture has no entry")
					}
					if err := writeAtomic(path, []byte(modified)); err != nil {
						t.Fatal(err)
					}
					switch kind {
					case "index":
						_, _, err = f.store.ReadStaging(context.Background(), f.repo)
					case "stash":
						_, err = f.store.ListIndexStashes(context.Background(), f.repo)
					case "commit":
						_, err = f.store.ListStagingCommits(context.Background(), f.repo)
					}
					if err == nil {
						t.Fatal("record reader erased or accepted duplicate identity")
					}
					after, err := os.ReadFile(path)
					if err != nil || string(after) != modified {
						t.Fatal("decoder rewrote rejected record", err)
					}
				})
			}
		}
	}
}
