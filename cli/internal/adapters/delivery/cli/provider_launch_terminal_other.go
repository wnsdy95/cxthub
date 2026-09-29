//go:build !darwin && !linux

package cli

import "os"

// An unverified terminal adapter must not claim interactive package delivery.
func providerLaunchTerminal(_ *os.File) bool { return false }
