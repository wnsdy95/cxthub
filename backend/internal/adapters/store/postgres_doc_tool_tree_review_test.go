//go:build postgres

package store

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocToolTreeReviewNoDatabaseSealedPreparation(test *testing.T) {
	raw := []byte(`{"kind":"tool_result","output":[` + strings.Repeat(`{"nested":[0,true,null]},`, 40) + `{"nested":[0,true,null]}]}`)
	prepared, err := domain.NewDocEventDisplayPreparation(domain.DocRangeEntry{
		DocEventIndex: domain.DocEventIndex{Hash: domain.HashContent(raw), Length: len(raw)}, Kind: domain.EventToolResult,
	})
	if err != nil {
		test.Fatal(err)
	}
	objects := make(map[domain.ContentHash][]byte)
	nodes := make(map[int]domain.DocToolTreeNode)
	treeReadBytes := 0
	for attempts := 0; attempts < 1000; attempts++ {
		if prepared.Phase == "tool_tree_done" {
			if prepared.ToolTree == nil || prepared.Generation != "" || len(nodes) != 206 {
				test.Fatal("private preparation lost nodes or became public", prepared.Phase, len(nodes))
			}
			root, present := nodes[prepared.ToolTree.Root.From]
			if !present || root.Parent != -1 || root.Children != 41 {
				test.Fatal("root node does not cover every nested candidate", root)
			}
			for _, node := range nodes {
				if node.Parent == -1 {
					continue
				}
				parent, present := nodes[node.Parent]
				if !present || parent.Depth+1 != node.Depth || node.Ordinal >= parent.Children {
					test.Fatal("sealed nested nodes omitted their parent or ordinal", node)
				}
			}
			test.Logf("selected tree bytes=%d; requested tree source bytes=%d", prepared.ToolTree.Root.To-prepared.ToolTree.Root.From, treeReadBytes)
			return
		}
		step, err := domain.StepDocEventDisplay(context.Background(), prepared, func(_ context.Context, offset, length int) ([]byte, error) {
			if offset < 0 || length <= 0 || length > domain.DocFieldDiscoveryStepBytes || offset > len(raw)-length {
				return nil, domain.ErrIntegrity
			}
			if prepared.Phase == "tool_tree" {
				treeReadBytes += length
			}
			return bytes.Clone(raw[offset : offset+length]), nil
		}, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
			encoded, present := objects[hash]
			if !present {
				return nil, domain.ErrIntegrity
			}
			return bytes.Clone(encoded), nil
		})
		if err != nil {
			test.Fatal("in-memory private preparation failed", prepared.Phase, err)
		}
		if _, scan, found := step.ToolTreeScan(); found {
			if len(scan.Nodes()) > domain.DocToolTreeStepNodes || len(scan.Objects()) > domain.DocToolTreeStepObjects {
				test.Fatal("sealed scanner exceeded a batch bound")
			}
			for _, node := range scan.Nodes() {
				if _, present := nodes[node.Value.From]; present {
					test.Fatal("single-pass scanner emitted the same node twice", node.Value.From)
				}
				nodes[node.Value.From] = node
			}
		}
		for _, object := range step.Objects() {
			if prior, present := objects[object.Hash]; present && !bytes.Equal(prior, object.Bytes) {
				test.Fatal("immutable object changed during preparation")
			}
			objects[object.Hash] = object.Bytes
		}
		prepared = step.Preparation()
	}
	test.Fatal("in-memory private preparation did not converge")
}
