package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGraphShadowRepositoriesSeparateOptIn(test *testing.T) {
	const variable = "CXT_GRAPH_SHADOW_REPOS"
	repo := domain.HashContent([]byte(test.Name()))
	for _, raw := range []string{"", "  "} {
		got, err := projectionRepositories(variable, raw)
		if err != nil || len(got) != 0 {
			test.Fatal("shadow enabled by default", got, err)
		}
	}
	got, err := projectionRepositories(variable, string(repo)+","+string(repo))
	if err != nil || !reflect.DeepEqual(got, []domain.ContentHash{repo}) {
		test.Fatal(got, err)
	}
	for _, raw := range []string{"*", "invalid", string(repo) + ","} {
		if _, err := projectionRepositories(variable, raw); err == nil || !strings.Contains(err.Error(), variable) {
			test.Fatal(raw, err)
		}
	}
}
