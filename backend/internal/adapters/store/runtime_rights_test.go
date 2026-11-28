package store

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

const runtimeRights0161 = "../../../../schemas/db/runtime_rights/0161_0170.sql"

var (
	runtimeRightsList   = regexp.MustCompile(`(?s)new_tables text\[\] := ARRAY\[(.*?)\];`)
	runtimeRightsName   = regexp.MustCompile(`'([a-z_][a-z0-9_]*)'`)
	migrationCreates    = regexp.MustCompile(`(?im)^\s*CREATE\s+(?:UNLOGGED\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:public\.)?([a-z_][a-z0-9_]*)`)
	migrationDropsTable = regexp.MustCompile(`(?is)DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?([a-z0-9_.,\s]+?)\s*(?:;|CASCADE|RESTRICT)`)
)

// runtimeRightsTables reads the tables a rights script gives rights on.
func runtimeRightsTables(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	list := runtimeRightsList.FindSubmatch(raw)
	if list == nil {
		t.Fatalf("%s names no tables", path)
	}
	var names []string
	for _, name := range runtimeRightsName.FindAllSubmatch(list[1], -1) {
		names = append(names, string(name[1]))
	}
	return names
}

// The rights script of migrations 0161 to 0170 names every table those
// migrations create and leave, and no other, so that no table of the release
// is left without the rights the server's accounts need.
func TestRuntimeRightsNameEveryTableTheirMigrationsCreate(t *testing.T) {
	created := map[string]bool{}
	for number := 161; number <= 170; number++ {
		paths, err := filepath.Glob(fmt.Sprintf("../../../../schemas/db/migrations/%04d_*.sql", number))
		if err != nil || len(paths) != 1 {
			t.Fatalf("migration %04d: %v %v", number, paths, err)
		}
		raw, err := os.ReadFile(paths[0])
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range migrationCreates.FindAllSubmatch(raw, -1) {
			created[string(name[1])] = true
		}
		for _, list := range migrationDropsTable.FindAllSubmatch(raw, -1) {
			for _, name := range regexp.MustCompile(`[a-z_][a-z0-9_]*`).FindAll(list[1], -1) {
				delete(created, string(name))
			}
		}
	}
	var want []string
	for name := range created {
		want = append(want, name)
	}
	slices.Sort(want)
	got := runtimeRightsTables(t, runtimeRights0161)
	slices.Sort(got)
	if len(want) != 17 || !slices.Equal(got, want) {
		t.Fatalf("the script gives rights on\n%v\nthe migrations create\n%v", got, want)
	}
}
