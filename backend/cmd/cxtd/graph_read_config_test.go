package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGraphReadRepositoriesSeparateOptIn(t *testing.T) {
	repo := domain.HashContent([]byte(t.Name()))
	for _, raw := range []string{"", "   "} {
		got, err := graphReadRepositories(raw)
		if err != nil || len(got) != 0 {
			t.Fatal("default enabled graph reads", got, err)
		}
	}
	got, err := graphReadRepositories(string(repo) + ", " + string(repo))
	if err != nil || !reflect.DeepEqual(got, []domain.ContentHash{repo}) {
		t.Fatal(got, err)
	}
	for _, raw := range []string{"*", string(repo) + ",", "invalid"} {
		if _, err := graphReadRepositories(raw); err == nil || !strings.Contains(err.Error(), "CXT_GRAPH_READ_REPOS") {
			t.Fatal(raw, err)
		}
	}
}
