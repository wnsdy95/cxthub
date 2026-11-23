//go:build postgres

package store

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocToolKeyMapStoreDomainOrderMatchesUniqueInsertionSlots(test *testing.T) {
	tree := domain.DocToolTreeSource{Version: 1, Profile: domain.DocToolTreeProfile, EventHash: domain.HashContent([]byte("event")), EventBytes: 1000, Evidence: domain.HashContent([]byte("fields")), Root: domain.DocFindSpan{From: 0, To: 1000}, Type: domain.DocToolValueObject}
	var members []domain.DocToolKeyMember
	for _, item := range []struct {
		from  int
		index bool
		value uint32
	}{{50, false, 0}, {40, true, 10}, {30, true, 2}, {20, false, 0}} {
		node := domain.DocToolTreeNode{Value: domain.DocFindSpan{From: item.from, To: item.from + 1}, Type: domain.DocToolValueNumber, Parent: 0, Ordinal: 0, Key: &domain.DocFindSpan{From: item.from - 4, To: item.from - 1}, Depth: 1}
		member, err := domain.MergeDocToolKeyMember(tree, nil, domain.DocToolKeyItem{Node: node, Digest: domain.HashContent([]byte(fmt.Sprint(item.from))), IsArrayIndex: item.index, ArrayIndex: item.value})
		if err != nil {
			test.Fatal(err)
		}
		members = append(members, member)
	}
	slices.SortFunc(members, domain.CompareDocToolKeyMembers)
	var positions []int
	for _, member := range members {
		positions = append(positions, member.First)
	}
	if !reflect.DeepEqual(positions, []int{30, 40, 20, 50}) {
		test.Fatal("valid unique slots require no digest tie-break for SQL pagination", positions)
	}
}
