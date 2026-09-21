//go:build !darwin && !linux

package cli

import "fmt"

func readProcessArgv(pid int) ([]string, error) {
	return nil, fmt.Errorf("process argument capture unavailable")
}
