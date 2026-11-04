package serverruntime

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPreparedMemoryRepositoryConfiguration(test *testing.T) {
	first, second := domain.HashContent([]byte("first")), domain.HashContent([]byte("second"))
	for _, value := range []string{"", " \n"} {
		if repos, err := preparedMemoryRepositories(value); err != nil || len(repos) != 0 {
			test.Fatal("default enabled", repos, err)
		}
	}
	repos, err := preparedMemoryRepositories(string(first) + ", " + string(second) + "," + string(first))
	if err != nil || !reflect.DeepEqual(repos, []domain.ContentHash{first, second}) {
		test.Fatal("canonical scope", repos, err)
	}
	for _, value := range []string{"*", "all", string(first) + ",", "," + string(first), "invalid", strings.Repeat(string(first)+",", domain.MaxMemoryReadWorkerRepos) + string(first)} {
		if _, err := preparedMemoryRepositories(value); err == nil || !strings.Contains(err.Error(), "CXT_MEMORY_READ_REPOS") {
			test.Fatal("invalid scope accepted", value, err)
		}
	}
}

func TestPreparedMemoryRuntimeRejectsUnsupportedStore(test *testing.T) {
	test.Setenv("CXT_AUTH", "dev")
	test.Setenv("CXT_PUBLIC_URL", "http://127.0.0.1:8908")
	test.Setenv("CXT_POSTGRES_DSN", "")
	test.Setenv("CXT_MEMORY_READ_REPOS", string(domain.HashContent([]byte(test.Name()))))
	if runtime, err := Open(context.Background(), "127.0.0.1:8908", test.TempDir(), false); err == nil {
		runtime.Close()
		test.Fatal("runtime enabled prepared reads without a supported store")
	}
}
