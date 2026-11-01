package main

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

func deviceLoginClient(base string) (outbound.DeviceLoginClient, error) {
	canonical, err := remotecfg.CanonicalEndpoint(base)
	if err != nil {
		return nil, err
	}
	if canonical != base {
		return nil, fmt.Errorf("device login requires a canonical API endpoint")
	}
	return backendclient.NewDeviceLoginClient(base), nil
}

func openDeviceLoginBrowser(target string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", target).Start()
	case "linux":
		_ = exec.Command("xdg-open", target).Start()
	case "windows":
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	}
}
