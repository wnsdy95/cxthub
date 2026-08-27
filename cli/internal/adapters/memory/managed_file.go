package memory

import (
	"fmt"
	"os"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// writeManagedMemory preserves user-owned instructions and replaces only one
// well-formed cxt region. Malformed or duplicate markers fail closed: guessing
// a replacement range would risk deleting user content.
func writeManagedMemory(path string, digest domain.MemoryDigest) error {
	managed := renderMemoryMarkdown(digest)
	existing, err := providerfs.ReadRegularFile(path)
	perm := os.FileMode(0o644)
	switch {
	case err == nil:
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return statErr
		}
		perm = info.Mode().Perm()
	case os.IsNotExist(err):
		existing = nil
	default:
		return err
	}
	merged, err := mergeManagedMemory(string(existing), managed)
	if err != nil {
		return err
	}
	return providerfs.WriteRegularFileAtomic(path, []byte(merged), perm)
}

func mergeManagedMemory(existing, managed string) (string, error) {
	starts := strings.Count(existing, managedMemoryBegin)
	ends := strings.Count(existing, managedMemoryEnd)
	if starts == 0 && ends == 0 {
		separator := ""
		if existing != "" {
			switch {
			case strings.HasSuffix(existing, "\n\n"):
			case strings.HasSuffix(existing, "\n"):
				separator = "\n"
			default:
				separator = "\n\n"
			}
		}
		return existing + separator + managed, nil
	}
	if starts != 1 || ends != 1 {
		return "", fmt.Errorf("malformed cxt managed memory markers: begin=%d end=%d", starts, ends)
	}
	start := strings.Index(existing, managedMemoryBegin)
	end := strings.Index(existing, managedMemoryEnd)
	if start < 0 || end < start+len(managedMemoryBegin) || !markerIsWholeLine(existing, start, len(managedMemoryBegin)) || !markerIsWholeLine(existing, end, len(managedMemoryEnd)) {
		return "", fmt.Errorf("malformed cxt managed memory marker placement")
	}
	replaceEnd := end + len(managedMemoryEnd)
	if strings.HasPrefix(existing[replaceEnd:], "\r\n") {
		replaceEnd += 2
	} else if strings.HasPrefix(existing[replaceEnd:], "\n") {
		replaceEnd++
	}
	return existing[:start] + managed + existing[replaceEnd:], nil
}

func markerIsWholeLine(text string, start, length int) bool {
	beforeOK := start == 0 || text[start-1] == '\n'
	after := start + length
	afterOK := after == len(text) || text[after] == '\n' || text[after] == '\r'
	return beforeOK && afterOK
}
