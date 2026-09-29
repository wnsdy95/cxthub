package cli

import "fmt"

// RepairFromServerOptions shares the public registry's parsing and validation
// with the isolated repair composition root.
type RepairFromServerOptions struct {
	RemoteURL string
}

func ParseRepairFromServerArgs(args []string) (RepairFromServerOptions, error) {
	p, help, err := parseCommand("repair", args)
	if err != nil {
		return RepairFromServerOptions{}, argumentError{err}
	}
	if help || !p.has("--from-server") {
		return RepairFromServerOptions{}, argumentError{fmt.Errorf("server repair requires --from-server")}
	}
	return RepairFromServerOptions{RemoteURL: p.flags["--remote"]}, nil
}
