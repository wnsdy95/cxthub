//go:build linux

package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
)

func readProcessArgv(pid int) ([]string, error) {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 256<<10))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) == 256<<10 || raw[len(raw)-1] != 0 {
		return nil, fmt.Errorf("truncated arguments")
	}
	parts := bytes.Split(raw[:len(raw)-1], []byte{0})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out, nil
}
