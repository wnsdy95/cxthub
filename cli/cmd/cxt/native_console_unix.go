//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Read only the current line, without a background reader or read-ahead into
// later approvals/the native TUI. Polling lets a withdrawn dialog cancel while
// leaving the caller's terminal open and its settings unchanged.
func nativeConsoleLine(ctx context.Context, file *os.File) (string, error) {
	defer runtime.KeepAlive(file)
	if file == nil || uint64(file.Fd()) > uint64(1<<31-1) {
		return "", io.ErrClosedPipe
	}
	fd := int(file.Fd())
	var line []byte
	var b [1]byte
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := unix.Poll(poll, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return "", err
		}
		if poll[0].Revents == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := unix.Read(fd, b[:])
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", io.EOF
		}
		if b[0] == '\n' {
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if !utf8.Valid(line) {
				return "", io.ErrUnexpectedEOF
			}
			return string(line), nil
		}
		if len(line) >= 64<<10 {
			return "", io.ErrShortBuffer
		}
		line = append(line, b[0])
	}
}
