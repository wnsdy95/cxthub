//go:build !darwin && !linux

package nativecodex

import "os"

func openWindowCatalog(string) (*os.File, error) { return nil, ErrState }
