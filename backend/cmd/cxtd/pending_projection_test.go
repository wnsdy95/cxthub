package main

import (
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPendingProjectionRepositoriesExplicitRollout(t *testing.T) {
	first, second := domain.HashContent([]byte("first")), domain.HashContent([]byte("second"))
	for _, value := range []string{"", " \n\t "} {
		repos, err := pendingProjectionRepositories(value)
		if err != nil || len(repos) != 0 {
			t.Fatal("default rollout must be disabled", repos, err)
		}
	}
	repos, err := pendingProjectionRepositories(" " + string(first) + "," + string(second) + "," + string(first) + " ")
	if err != nil || !reflect.DeepEqual(repos, []domain.ContentHash{first, second}) {
		t.Fatal("explicit repository list changed", repos, err)
	}
	for _, value := range []string{"*", "true", "all", "first", string(first) + ",", "," + string(first), string(first) + ",," + string(second)} {
		if repos, err := pendingProjectionRepositories(value); err == nil || repos != nil {
			t.Fatal("invalid rollout accepted", value, repos, err)
		}
	}
}
