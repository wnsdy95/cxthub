//go:build !darwin && !linux

package nativecodex

import (
	"net"
	"os/exec"
)

func localSocketSupported() bool                     { return false }
func signalOwnedProcess(cmd *exec.Cmd, _ bool) error { return cmd.Process.Kill() }
func ownedProcessAlive(*exec.Cmd) bool               { return false }
func peerProcess(net.Conn) (int, error)              { return 0, ErrProtocol }

func newProcessExitObserver() (processExitObserver, error) { return nil, ErrProtocol }
