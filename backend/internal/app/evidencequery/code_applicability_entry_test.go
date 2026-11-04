package evidencequery

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestCachedGitEntryPreservesScannerTraversalAndSharedCache(test *testing.T) {
	file := domain.GitEntry{OID: "blob", Mode: "100755"}
	reader := &gitEvidenceTestTrees{nodes: map[string]domain.GitTreeNode{
		"root":  {Entries: map[string]domain.GitEntry{"file": file, "dir": {OID: "child", Mode: "040000"}}},
		"child": {Entries: map[string]domain.GitEntry{"nested": file}},
	}}
	cache := map[string]domain.GitTreeNode{}
	for _, scenario := range []struct {
		path  string
		entry domain.GitEntry
	}{
		{"file", file},
		{"dir/nested", file},
		{"dir", domain.GitEntry{}},
		{"absent", domain.GitEntry{}},
		{"dir/absent", domain.GitEntry{}},
		{"file/nested", domain.GitEntry{}},
		{"", domain.GitEntry{}},
	} {
		entry, err := CachedGitEntry(context.Background(), reader, gitEvidenceTestRepo, gitEvidenceTestOrigin, "root", scenario.path, cache)
		if err != nil || entry != scenario.entry {
			test.Fatal("scanner path lookup changed file/directory/absence semantics", scenario.path, entry, err)
		}
	}
	if len(reader.nodeCalls) != 2 || len(cache) != 2 {
		test.Fatal("path lookup replaced or ignored the caller's node cache", reader.nodeCalls, cache)
	}
	for _, call := range reader.nodeCalls {
		if call.repo != gitEvidenceTestRepo || call.origin != gitEvidenceTestOrigin {
			test.Fatal("scanner lookup changed the caller's scope", call)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if entry, err := CachedGitEntry(canceled, reader, gitEvidenceTestRepo, gitEvidenceTestOrigin, "root", "file", cache); entry != (domain.GitEntry{}) || !errors.Is(err, context.Canceled) || len(reader.nodeCalls) != 2 {
		test.Fatal("cached path lookup ignored cancellation", entry, err)
	}
}

func TestCachedGitEntryPreservesDepthBoundAndErrorCaching(test *testing.T) {
	file := domain.GitEntry{OID: "blob", Mode: "100644"}
	reader := &gitEvidenceTestTrees{nodes: map[string]domain.GitTreeNode{
		"root": {Entries: map[string]domain.GitEntry{"part": {OID: "root", Mode: "040000"}, "file": file}},
	}}
	cache := map[string]domain.GitTreeNode{}
	path := strings.Repeat("part/", 127) + "file"
	if entry, err := CachedGitEntry(context.Background(), reader, gitEvidenceTestRepo, gitEvidenceTestOrigin, "root", path, cache); entry != file || err != nil || len(reader.nodeCalls) != 1 {
		test.Fatal("exact 128-component path bound changed", entry, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if entry, err := CachedGitEntry(canceled, reader, gitEvidenceTestRepo, gitEvidenceTestOrigin, "root", "part/"+path, cache); entry != (domain.GitEntry{}) || !errors.Is(err, domain.ErrValidation) || len(reader.nodeCalls) != 1 {
		test.Fatal("depth validation no longer precedes context and I/O", entry, err)
	}
	failure := errors.New("scanner node reader failure")
	reader.nodeErrors = map[string]error{"missing": failure}
	for repeat := 0; repeat < 2; repeat++ {
		entry, err := CachedGitEntry(context.Background(), reader, gitEvidenceTestRepo, gitEvidenceTestOrigin, "missing", "file", cache)
		if entry != (domain.GitEntry{}) || !errors.Is(err, failure) || len(reader.nodeCalls) != repeat+2 {
			test.Fatal("scanner helper changed reader errors or cached failures independently", entry, err, reader.nodeCalls)
		}
		if _, found := cache["missing"]; found {
			test.Fatal("failed node read entered the caller's success cache")
		}
	}
}
