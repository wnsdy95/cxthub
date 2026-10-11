package domain

import (
	"strings"
	"testing"
)

func TestGitAssociationStatusRequest(t *testing.T) {
	for _, kind := range []string{"publication", "integration", "memory"} {
		length := 64
		if kind == "publication" {
			length = 32
		}
		request := GitAssociationStatusRequest{Kind: kind, State: "pending", Limit: 30, Cursor: strings.Repeat("a", length)}
		if err := request.Validate(); err != nil {
			t.Fatal(err)
		}
		for _, cursor := range []string{strings.Repeat("A", length), strings.Repeat("g", length), strings.Repeat("a", length-1), "../cursor"} {
			bad := request
			bad.Cursor = cursor
			if bad.Validate() == nil {
				t.Fatal("accepted malformed cursor", cursor)
			}
		}
		for _, limit := range []int{-1, 0, 101} {
			bad := request
			bad.Limit = limit
			if bad.Validate() == nil {
				t.Fatal("accepted unbounded request", limit)
			}
		}
		request.Kind = "guessed"
		if request.Validate() == nil {
			t.Fatal("accepted unknown kind")
		}
		request.Kind, request.State = kind, "completed"
		if request.Validate() == nil {
			t.Fatal("accepted unsupported filter")
		}
	}
}
