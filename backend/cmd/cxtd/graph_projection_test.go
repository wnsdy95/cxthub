package main

import (
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGraphProjectionRepositoriesWorkerOptIn(t *testing.T) {
	first, second := domain.HashContent([]byte("first")), domain.HashContent([]byte("second"))
	for _, value := range []string{"", " \n\t "} {
		repos, err := graphProjectionRepositories(value)
		if err != nil || repos != nil {
			t.Fatal("graph worker must default off", repos, err)
		}
	}
	repos, err := graphProjectionRepositories(" " + string(first) + ", " + string(second) + "," + string(first) + " ")
	if err != nil || !reflect.DeepEqual(repos, []domain.ContentHash{first, second}) {
		t.Fatal("explicit worker selection changed", repos, err)
	}
	for _, value := range []string{"*", "true", "all", "first", string(first) + ",", "," + string(first), string(first) + ",," + string(second)} {
		if repos, err := graphProjectionRepositories(value); repos != nil || err == nil || err.Error() != "CXT_GRAPH_PROJECTION_REPOS must contain repository content hashes" {
			t.Fatal("invalid worker selection accepted or mislabeled", value, repos, err)
		}
	}
	if _, err := pendingProjectionRepositories("*"); err == nil || err.Error() != "CXT_PENDING_PROJECTION_REPOS must contain repository content hashes" {
		t.Fatal("pending configuration error changed", err)
	}
}
