//go:build darwin || linux

package nativecodex

import (
	"os"
	"syscall"
)

// O_NONBLOCK prevents a replaced catalog path (e.g. a FIFO) from hanging before
// descriptor-level regular-file validation. It does not change regular reads.
func openWindowCatalog(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
