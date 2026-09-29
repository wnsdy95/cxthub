package app

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestHistoryPageWireParityWithCLICodecs(t *testing.T) {
	raw, err := os.ReadFile("../../../schemas/testdata/agent-history-page-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name string                  `json:"name"`
		Doc  domain.SessionDoc       `json:"doc"`
		Page domain.AgentHistoryPage `json:"page"`
	}
	if err = json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	st, repo, ctx := store.NewFSStore(t.TempDir()), h('1'), systemTestContext()
	svc := NewService(st, st, nil, nil, nil)
	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			if _, err := st.PutDoc(ctx, repo, f.Doc); err != nil {
				t.Fatal("CLI canonical source mismatch", err)
			}
			got, err := svc.ReadAgentHistoryPage(ctx, repo, f.Doc.Hash, domain.AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: 4 << 20})
			if err != nil {
				t.Fatal(err)
			}
			wire, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var normalized domain.AgentHistoryPage
			if err = json.Unmarshal(wire, &normalized); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(normalized, f.Page) {
				t.Fatalf("backend event union/hash differs from CLI fixture\ngot: %s\nwant: %+v", wire, f.Page)
			}
		})
	}
}
