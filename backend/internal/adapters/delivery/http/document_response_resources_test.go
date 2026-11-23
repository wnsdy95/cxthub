package http

import (
	"testing"
)

func TestDocumentResponseOwnerRejectsLateRetentionAndClosesOnce(test *testing.T) {
	owner := &documentResponseResources{open: true}
	released := 0
	if !owner.retain(func() { released++ }) {
		test.Fatal("live response owner rejected its final read")
	}
	owner.close()
	owner.close()
	if released != 1 || owner.retain(func() { released++ }) || released != 1 {
		test.Fatal("response owner reused or duplicated a released read", released)
	}
}
