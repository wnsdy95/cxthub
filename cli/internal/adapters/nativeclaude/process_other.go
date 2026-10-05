//go:build !darwin && !linux

package nativeclaude

import (
	"os"
	"os/exec"
)

func openArchive(*os.Root, string) (*os.File, error) { return nil, ErrState }

func newProcessExitObserver() (processExitObserver, error) { return nil, ErrState }
func configureProcess(*exec.Cmd)                           {}
func signalProcessGroup(*exec.Cmd, bool) error             { return ErrState }
func cleanupProcessGroup(*exec.Cmd) error                  { return ErrCleanup }
