//go:build !linux && !darwin

package main

import (
	"context"
	"os"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativeclaude"
)

func nativeConsoleLine(context.Context, *os.File) (string, error) {
	return "", nativeclaude.ErrUnsupported
}
