//go:build postgres

package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocToolKeyMapPGDuplicatePointCost(test *testing.T) {
	baseline := make(map[string]float64)
	for _, count := range []int{64, 640} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			output := `{` + strings.Repeat(`"duplicate":0,`, count) + `"after":1}`
			fixture, prepared, owner, _ := docToolKeyMapStoreOutput(test, output)
			tree, keys := *prepared.ToolTree, *prepared.ToolKeys
			if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
				original := read.Value(repositoryTxKey{}).(*repositoryTx)
				reader := &postgresDocEventDisplayWork{storage: fixture.storage, owner: original, source: owner, tree: &tree, keys: &keys}
				members, err := reader.ReadToolKeyMembers(read, tree, keys, tree.Root.From, nil, 64)
				if err != nil || len(members) != 2 {
					test.Fatal("duplicate group fixture changed semantic cardinality", len(members), err)
				}
				forged := members[0]
				forged.Last.Node.Ordinal++
				if rows, err := reader.ReadToolKeyMembers(read, tree, keys, tree.Root.From, &forged, 1); err != domain.ErrIntegrity || rows != nil {
					test.Fatal("canonical cursor-node forgery was accepted", rows, err)
				}
				for _, selected := range []string{"late", "terminal", "missing"} {
					trace := &docToolOrdersPGTrace{Tx: original.Tx}
					bound := *original
					bound.Tx = trace
					observed := context.WithValue(read, repositoryTxKey{}, &bound)
					reader := &postgresDocEventDisplayWork{storage: fixture.storage, owner: &bound, source: owner, tree: &tree, keys: &keys}
					if selected == "missing" {
						node, err := readDocToolCanonicalNode(observed, trace, tree, tree.Root.From+1)
						if err != domain.ErrNotFound || node != (domain.DocToolTreeNode{}) {
							test.Fatal("missing source identity returned a neighboring node", node, err)
						}
					} else {
						cursor, expected := members[0], 1
						if selected == "terminal" {
							cursor, expected = members[1], 0
						}
						rows, err := reader.ReadToolKeyMembers(observed, tree, keys, tree.Root.From, &cursor, 1)
						if err != nil || len(rows) != expected || trace.rows != expected {
							test.Fatal("bounded duplicate-group continuation failed", selected, rows, err)
						}
					}
					for _, sequential := range []string{"on", "off"} {
						if _, err := original.Tx.Exec(read, `SET LOCAL enable_seqscan=`+sequential); err != nil {
							return err
						}
						for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
							blocks := float64(0)
							for index, query := range trace.queries {
								encoded := explainDocToolOrdersPGTraceQuery(test, read, original.Tx, query, trace.arguments[index], mode)
								blocks += assertDocToolOrdersPGPointPlan(test, encoded, mode+"/seqscan="+sequential, index, query)
							}
							key := selected + "/" + mode + "/" + sequential
							if count == 64 {
								baseline[key] = blocks
							} else if initial, found := baseline[key]; !found || blocks > initial*2+32 {
								test.Fatal("tenfold duplicate growth increased selected-read work", key, baseline[key], blocks)
							}
							test.Logf("duplicate_occurrences=%d selection=%s plan=%s seqscan=%s queries=%d total_blocks=%.0f", count, selected, mode, sequential, len(trace.queries), blocks)
						}
					}
				}
				return nil
			}); err != nil {
				test.Fatal(err)
			}
		})
	}
}
