//go:build darwin

package nativeclaude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Python allocates terminals only. The Go-owned resume plan, never a Python
// subprocess, must launch the native child whose lifecycle is being tested.
const idlePTYAllocator = `
import array,fcntl,os,pty,socket,struct,termios
control=socket.socket(fileno=3)
master,slave=pty.openpty()
fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack("HHHH",32,120,0,0))
control.sendmsg([os.ttyname(slave).encode()],[(socket.SOL_SOCKET,socket.SCM_RIGHTS,array.array("i",[master,slave]))])
os.close(master)
os.close(slave)
control.close()
`

type idleNativePTY struct {
	master, slave *os.File
	slavePath     string
}

func allocateIdleNativePTY(t *testing.T) idleNativePTY {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required for the requested native PTY fixture")
	}
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal("allocate PTY descriptor socket", err)
	}
	syscall.CloseOnExec(pair[0])
	syscall.CloseOnExec(pair[1])
	parent := os.NewFile(uintptr(pair[0]), "pty-control-parent")
	child := os.NewFile(uintptr(pair[1]), "pty-control-child")
	defer parent.Close()
	defer child.Close()
	connection, err := net.FileConn(parent)
	if err != nil {
		t.Fatal("open PTY descriptor socket", err)
	}
	defer connection.Close()
	unix, ok := connection.(*net.UnixConn)
	if !ok {
		t.Fatal("PTY descriptor socket is not Unix")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-c", idlePTYAllocator)
	cmd.ExtraFiles = []*os.File{child}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "LANG=en_US.UTF-8"}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal("start PTY allocator", err)
	}
	_ = child.Close()
	_ = unix.SetReadDeadline(time.Now().Add(4 * time.Second))
	message, ancillary := make([]byte, 256), make([]byte, syscall.CmsgSpace(2*4))
	n, oobn, flags, _, receiveErr := unix.ReadMsgUnix(message, ancillary)
	waitErr := cmd.Wait()
	if receiveErr != nil {
		t.Fatal("receive PTY descriptors", receiveErr)
	}
	control, err := syscall.ParseSocketControlMessage(ancillary[:oobn])
	if err != nil {
		t.Fatal("parse PTY descriptor message", err)
	}
	var descriptors []int
	for _, m := range control {
		fds, err := syscall.ParseUnixRights(&m)
		if err != nil {
			t.Fatal("parse PTY descriptor rights", err)
		}
		descriptors = append(descriptors, fds...)
	}
	owned := false
	defer func() {
		if !owned {
			for _, fd := range descriptors {
				_ = syscall.Close(fd)
			}
		}
	}()
	if waitErr != nil || flags&(syscall.MSG_CTRUNC|syscall.MSG_TRUNC) != 0 || len(descriptors) != 2 || !strings.HasPrefix(string(message[:n]), "/dev/ttys") {
		t.Fatal("invalid or incomplete PTY allocation")
	}
	for _, fd := range descriptors {
		syscall.CloseOnExec(fd)
	}
	if err := syscall.SetNonblock(descriptors[0], true); err != nil {
		t.Fatal("configure PTY reader", err)
	}
	pty := idleNativePTY{master: os.NewFile(uintptr(descriptors[0]), "owned-pty-master"), slave: os.NewFile(uintptr(descriptors[1]), "owned-pty-slave"), slavePath: string(message[:n])}
	owned = true
	t.Cleanup(func() { _ = pty.master.Close(); _ = pty.slave.Close() })
	return pty
}

func TestIdleNativePTYDescriptorTransfer(t *testing.T) {
	pty := allocateIdleNativePTY(t)
	if filepath.Clean(pty.slavePath) != pty.slavePath {
		t.Fatal("unclean terminal path")
	}
	if _, err := pty.slave.Write([]byte("synthetic-pty-ready\n")); err != nil {
		t.Fatal(err)
	}
	if err := pty.master.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal("PTY deadline unavailable", err)
	}
	buf := make([]byte, 128)
	n, err := pty.master.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "synthetic-pty-ready") {
		t.Fatal("PTY transfer failed", err)
	}
}

const (
	idleTerminalRingBytes  = 2 << 20
	idleTerminalTotalBytes = 8 << 20
	idleCursorReplyLimit   = 64
)

type idleTerminalCapture struct {
	mu            sync.Mutex
	ring          []byte
	total         int
	screen        idleScreenSegment
	cursorReplies int
	err           error
	done          chan struct{}
}

// Terminal bytes remain synthetic private fixture data. Diagnostics expose
// only fixed classifications and counts, never the captured screen or argv.
func captureIdleTerminal(ctx context.Context, master *os.File) *idleTerminalCapture {
	c := &idleTerminalCapture{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		buf := make([]byte, 32<<10)
		var cursor idleCursorResponder
		for {
			if ctx.Err() != nil {
				return
			}
			if err := master.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
				c.mu.Lock()
				c.err = err
				c.mu.Unlock()
				return
			}
			n, err := master.Read(buf)
			if n > 0 {
				if !c.append(buf[:n]) {
					return
				}
				// Only the terminal's exact DSR request can produce input. This
				// bounded reply has no Enter/newline and is never a user prompt.
				writeErr := master.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
				if writeErr == nil {
					writeErr = cursor.reply(master, buf[:n])
				}
				c.mu.Lock()
				c.cursorReplies = cursor.replies
				if writeErr != nil {
					c.err = writeErr
				}
				c.mu.Unlock()
				if writeErr != nil {
					return
				}
			}
			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					continue
				}
				if errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed) {
					return
				}
				c.mu.Lock()
				c.err = err
				c.mu.Unlock()
				return
			}
		}
	}()
	return c
}

type idleCursorResponder struct {
	pending []byte
	replies int
}

func (r *idleCursorResponder) reply(w io.Writer, chunk []byte) error {
	const query, response = "\x1b[6n", "\x1b[1;1R"
	raw := append(r.pending, chunk...)
	count := bytes.Count(raw, []byte(query))
	if count > idleCursorReplyLimit-r.replies {
		return ErrLimit
	}
	for i := 0; i < count; i++ {
		n, err := io.WriteString(w, response)
		if err != nil {
			return err
		}
		if n != len(response) {
			return io.ErrShortWrite
		}
		r.replies++
	}
	// Keep only the possible incomplete query prefix, including chunk splits.
	r.pending = append([]byte(nil), raw[max(0, len(raw)-(len(query)-1)):]...)
	return nil
}

func TestIdleTerminalCursorReplyIsBoundedProtocolOnly(t *testing.T) {
	var r idleCursorResponder
	var output strings.Builder
	for _, chunk := range []string{"ordinary text\x1b[", "6n", "more\x1b[6n\x1b[5n"} {
		if err := r.reply(&output, []byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if output.String() != "\x1b[1;1R\x1b[1;1R" || r.replies != 2 {
		t.Fatal("terminal response was missing, duplicated or not protocol-only")
	}
	r.replies = idleCursorReplyLimit
	if err := r.reply(&output, []byte("\x1b[6n")); !errors.Is(err, ErrLimit) {
		t.Fatal("unbounded cursor responses")
	}
}

func (c *idleTerminalCapture) append(raw []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(raw) > idleTerminalTotalBytes-c.total {
		c.err = ErrLimit
		return false
	}
	c.screen.append(raw, c.total)
	c.total += len(raw)
	if len(raw) >= idleTerminalRingBytes {
		c.ring = append(c.ring[:0], raw[len(raw)-idleTerminalRingBytes:]...)
	} else {
		if excess := len(c.ring) + len(raw) - idleTerminalRingBytes; excess > 0 {
			copy(c.ring, c.ring[excess:])
			c.ring = c.ring[:len(c.ring)-excess]
		}
		c.ring = append(c.ring, raw...)
	}
	return true
}

// Track only full display erasure (CSI 2 J) and terminal reset (ESC c).
// Parser state survives read boundaries; offsets and parameters use constant
// space. This delimits screen segments, not arbitrary cursor edits or a full
// terminal emulator. Escape strings cannot manufacture a display reset.
type idleScreenSegment struct {
	start, generation int
	state             byte
	parameter         byte // Saturates at 3; only the value 2 is relevant.
	validParameter    bool
}

const (
	idleScreenText byte = iota
	idleScreenEscape
	idleScreenCSI
	idleScreenOSC
	idleScreenOSCEscape
	idleScreenString
	idleScreenStringEscape
)

func (s *idleScreenSegment) append(raw []byte, offset int) {
	reset := func(end int) { s.start, s.generation = end, s.generation+1 }
	for i, b := range raw {
		switch s.state {
		case idleScreenText:
			if b == '\x1b' {
				s.state = idleScreenEscape
			}
		case idleScreenEscape:
			s.state = idleScreenText
			switch b {
			case '[':
				s.state, s.parameter, s.validParameter = idleScreenCSI, 0, true
			case 'c':
				reset(offset + i + 1)
			case ']':
				s.state = idleScreenOSC
			case 'P', 'X', '^', '_':
				s.state = idleScreenString
			case '\x1b':
				s.state = idleScreenEscape
			}
		case idleScreenCSI:
			switch {
			case b == '\x1b':
				s.state = idleScreenEscape
			case b >= '0' && b <= '9':
				s.parameter = min(3, s.parameter*10+b-'0')
			case b >= 0x40 && b <= 0x7e:
				if b == 'J' && s.validParameter && s.parameter == 2 {
					reset(offset + i + 1)
				}
				s.state = idleScreenText
			default:
				s.validParameter = false
			}
		case idleScreenOSC, idleScreenString:
			if b == '\x07' && s.state == idleScreenOSC {
				s.state = idleScreenText
			} else if b == '\x1b' {
				s.state++
			}
		case idleScreenOSCEscape, idleScreenStringEscape:
			if b == '\\' || b == '\x07' && s.state == idleScreenOSCEscape {
				s.state = idleScreenText
			} else if b != '\x1b' {
				s.state--
			}
		}
	}
}

// OSC and CSI sequences are stripped for fixed UI markers only. This is not a
// terminal emulator or a protocol claim about the TUI's hidden runtime state.
var idleTerminalEscape = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-?]*[ -/]*[@-~]|\x1b[()][0-2A-Z]|\x1b[=>]`)

// Ink can encode inter-word spaces as horizontal cursor movement. Replace
// each move with one space for fixed phrase matching, without expanding an
// untrusted numeric distance or pretending to emulate the full terminal.
var idleTerminalForward = regexp.MustCompile(`\x1b\[[0-9]*C`)

type idleScreenEvidence struct {
	screenGeneration                                     int
	diagnostics                                          string
	blocker                                              string
	composer, reference, session, modelStart, compaction bool
	bytes, cursorReplies                                 int
}

func (c *idleTerminalCapture) inspect(marker, session string) (idleScreenEvidence, error) {
	c.mu.Lock()
	raw, total, replies, err := string(c.ring), c.total, c.cursorReplies, c.err
	screenOffset, generation := max(0, c.screen.start-(c.total-len(c.ring))), c.screen.generation
	c.mu.Unlock()
	normalize := func(raw string) string {
		return idleTerminalEscape.ReplaceAllString(idleTerminalForward.ReplaceAllString(raw, " "), "")
	}
	history := strings.ToLower(normalize(raw))
	text := normalize(raw[screenOffset:])
	lower := strings.ToLower(text)
	e := idleScreenEvidence{screenGeneration: generation, bytes: total, cursorReplies: replies, reference: strings.Contains(text, marker), session: strings.Contains(text, session)}
	contains := func(needles ...string) bool {
		for _, needle := range needles {
			if strings.Contains(lower, needle) {
				return true
			}
		}
		return false
	}
	// Fixed labels expose no screen text, paths, account values or native errors.
	var diagnostics []string
	for _, marker := range []struct{ text, label string }{
		{"operation not permitted", "eperm"}, {"permission denied", "eacces"},
		{"raw mode", "raw_mode"}, {"setrawmode", "set_raw_mode"}, {"ioctl", "ioctl"},
		{"terminal", "terminal"}, {"theme", "theme"}, {"welcome", "welcome"},
		{"login", "login"}, {"log in", "log_in"}, {"sign in", "sign_in"},
		{"trust", "trust"}, {"enter", "enter"}, {"resume", "resume"},
		{"session", "session"}, {"loading", "loading"}, {"shortcuts", "shortcuts"},
		{"unsupported", "unsupported"}, {"error", "error"},
	} {
		if contains(marker.text) {
			diagnostics = append(diagnostics, marker.label)
		}
	}
	e.diagnostics = strings.Join(diagnostics, ",")
	switch {
	case contains("select login method", "not logged in", "login required", "please log in", "sign in to", "log in to", "authentication required"):
		e.blocker = "authentication"
	case contains("do you trust", "trust this folder", "trust the files", "trust this project", "workspace trust"):
		e.blocker = "trust"
	case contains("choose the text style", "select a theme", "let’s get started", "let's get started", "press enter to continue"):
		e.blocker = "setup"
	}
	e.composer = strings.Contains(text, "❯") && contains("for shortcuts") && e.blocker == ""
	// Danger signals retain the historical ring, including erased screens.
	lower = history
	e.modelStart = contains("thinking…", "thinking...", "api error", "unable to connect to api", "retrying request")
	e.compaction = contains("compacting conversation", "compacting context", "auto-compacting", "compact_boundary")
	return e, err
}

func TestIdleTerminalEvidenceRequiresLoadedReferenceAndUnblockedComposer(t *testing.T) {
	for _, tc := range []struct {
		text, blocker       string
		composer, reference bool
	}{
		{"❯ \n? for shortcuts\nSYNTHETIC_REFERENCE", "", true, true},
		{"❯ \n? for shortcuts", "", true, false},
		{"Select login method\n❯ \n? for shortcuts\nSYNTHETIC_REFERENCE", "authentication", false, true},
		{"Do you trust the files in this folder?\n❯ \n? for shortcuts", "trust", false, false},
		{"Choose the text style\n❯ Dark mode\n? for shortcuts", "setup", false, false},
		{"Choose\x1b[1Cthe\x1b[1Ctext\x1b[1Cstyle\n❯ Dark mode\n? for shortcuts", "setup", false, false},
		{"Do\x1b[1Cyou\x1b[1Ctrust\x1b[1Cthe\x1b[1Cfiles\n❯ \n? for shortcuts", "trust", false, false},
	} {
		c := &idleTerminalCapture{}
		c.append([]byte(tc.text))
		e, err := c.inspect("SYNTHETIC_REFERENCE", "owned-session")
		if err != nil || e.composer != tc.composer || e.reference != tc.reference || e.blocker != tc.blocker {
			t.Fatal("incorrect fixed screen classification")
		}
	}
	c := &idleTerminalCapture{}
	chunk := []byte(strings.Repeat("x", 32<<10))
	for i := 0; i < idleTerminalTotalBytes/len(chunk); i++ {
		if !c.append(chunk) {
			t.Fatal("premature terminal limit")
		}
	}
	if len(c.ring) != idleTerminalRingBytes || c.total != idleTerminalTotalBytes || c.append([]byte("x")) || !errors.Is(c.err, ErrLimit) {
		t.Fatal("unbounded terminal capture")
	}
}

func idleComposerReady(evidence idleScreenEvidence) bool {
	return evidence.composer && evidence.reference
}

type idleComposerTimer struct {
	generation int
	since      time.Time
}

func (r *idleComposerTimer) observe(evidence idleScreenEvidence, now time.Time) bool {
	if r.generation != evidence.screenGeneration {
		r.generation, r.since = evidence.screenGeneration, time.Time{}
	}
	if !idleComposerReady(evidence) {
		r.since = time.Time{}
		return false
	}
	if r.since.IsZero() {
		r.since = now
	}
	return now.Sub(r.since) >= time.Second
}

func TestIdleTerminalClearDiscardsReadinessButRetainsDanger(t *testing.T) {
	const marker, session = "SYNTHETIC_REFERENCE", "owned-session"
	const readyScreen = "❯ \n? for shortcuts\n" + marker + "\n" + session
	for _, clear := range []string{"\x1b[2J\x1b[H", "\x1b[02J", "\x1bc"} {
		for split := 0; split <= len(clear); split++ {
			capture := &idleTerminalCapture{}
			capture.append([]byte("Thinking... Compacting context\n" + readyScreen))
			before, err := capture.inspect(marker, session)
			if err != nil || !idleComposerReady(before) {
				t.Fatal("initial screen did not contain readiness markers")
			}
			capture.append([]byte(clear[:split]))
			capture.append([]byte(clear[split:] + "Loading"))
			after, err := capture.inspect(marker, session)
			if err != nil || after.screenGeneration != 1 || after.composer || after.reference || after.session || idleComposerReady(after) {
				t.Fatal("erased screen retained readiness markers", split)
			}
			if !after.modelStart || !after.compaction {
				t.Fatal("screen clear discarded historical danger evidence")
			}
		}
	}
	capture := &idleTerminalCapture{}
	capture.append([]byte(readyScreen + "\x1b[2J" + readyScreen + "\x1bcLoading"))
	evidence, err := capture.inspect(marker, session)
	if err != nil || evidence.screenGeneration != 2 || idleComposerReady(evidence) {
		t.Fatal("readiness did not use the most recent reset")
	}
	// Terminal-title data is not an executed screen-clear sequence.
	capture = &idleTerminalCapture{}
	capture.append([]byte(readyScreen + "\x1b]0;title\x1b[2J\x07"))
	evidence, err = capture.inspect(marker, session)
	if err != nil || evidence.screenGeneration != 0 {
		t.Fatal("escape string manufactured a screen generation")
	}
}

func TestIdleTerminalReadyTimerRestartsForScreenGeneration(t *testing.T) {
	const marker = "SYNTHETIC_REFERENCE"
	const readyScreen = "❯ \n? for shortcuts\n" + marker
	capture := &idleTerminalCapture{}
	capture.append([]byte(readyScreen))
	evidence, err := capture.inspect(marker, "owned-session")
	if err != nil {
		t.Fatal(err)
	}
	var timer idleComposerTimer
	start := time.Unix(1, 0)
	if timer.observe(evidence, start) || timer.observe(evidence, start.Add(900*time.Millisecond)) {
		t.Fatal("initial readiness did not require a stable interval")
	}
	// Even an immediate replacement with both markers needs a new full interval.
	capture.append([]byte("\x1b[2J\x1b[H" + readyScreen))
	evidence, err = capture.inspect(marker, "owned-session")
	if err != nil || !idleComposerReady(evidence) || evidence.screenGeneration != 1 {
		t.Fatal("replacement fixture did not contain fresh readiness markers")
	}
	replaced := start.Add(1100 * time.Millisecond)
	if timer.observe(evidence, replaced) || timer.observe(evidence, replaced.Add(999*time.Millisecond)) {
		t.Fatal("replacement screen inherited the previous readiness interval")
	}
	if !timer.observe(evidence, replaced.Add(time.Second)) {
		t.Fatal("replacement screen failed its own stable interval")
	}
}

func TestIdleTerminalComposerRequiresReferenceNotSessionUUID(t *testing.T) {
	const session = "12345678-1234-4234-8234-123456789abc"
	const marker = "SYNTHETIC_REFERENCE"
	capture := &idleTerminalCapture{}
	capture.append([]byte("❯ \n? for shortcuts\n" + session))
	evidence, err := capture.inspect(marker, session)
	if err != nil || !evidence.composer || !evidence.session || evidence.reference {
		t.Fatal("fixture must show a composer and session UUID without the reference")
	}
	if idleComposerReady(evidence) {
		t.Fatal("session UUID substituted for visible reference evidence")
	}
	capture.append([]byte("\n" + marker))
	evidence, err = capture.inspect(marker, session)
	if err != nil || !idleComposerReady(evidence) {
		t.Fatal("composer with visible reference was not recognized")
	}
}

// This test is deliberately a readiness assertion, not a passing diagnostic.
// An auth/trust/setup screen or an archive-only observation fails the requested
// probe. It never supplies a prompt or changes fixture credentials/trust state.
func TestNativeClaudeOfflineIdleHandoff(t *testing.T) {
	if os.Getenv("CXT_TEST_NATIVE_CLAUDE") == "" {
		t.Skip("native idle fixture not requested; no readiness evidence produced")
	}
	minimalReady := false
	for _, size := range []int{1024, 1572864} {
		if !t.Run(fmt.Sprint(size), func(t *testing.T) {
			if size > 1024 && !minimalReady {
				t.Fatal("1 KiB composer readiness must be verified before the larger probe")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			opts, root, binaryHash := offlineClaudeFixture(t)
			if binaryHash != "51f09bd1e021d9fa8a1864c179799bd37cb39962a937935c5cf6823398e86db4" {
				t.Fatal("native idle probe requires the pinned 2.1.285 executable")
			}
			pty := allocateIdleNativePTY(t)
			s, err := Start(ctx, opts)
			if err != nil {
				t.Fatal("no-turn helper initialization failed", err)
			}
			defer s.Close()
			marker := fmt.Sprintf("CXTHUB_IDLE_REFERENCE_%d", size)
			unit := "Synthetic main history reference. Original role: user.\n"
			bodyBytes := size - 2*len(marker) - 2
			payload := marker + "\n" + strings.Repeat(unit, bodyBytes/len(unit)) + strings.Repeat("x", bodyBytes%len(unit)) + "\n" + marker
			receipt, err := s.AppendReference(ctx, payload)
			if err != nil || !receipt.NoTurnAcknowledged || receipt.Persisted || receipt.UTF8Bytes != size {
				t.Fatal("no-turn append evidence unavailable", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal("helper retirement failed; idle child not launched", err)
			}
			if s.process.cmd.ProcessState == nil || !s.process.cmd.ProcessState.Exited() {
				t.Fatal("helper was not reaped before handoff")
			}
			archive := verifyNativeArchive(t, root, receipt, nativeReferencePrefix+payload)
			verified, err := s.VerifyArchive(ctx, archive)
			if err != nil || !verified.Persisted {
				t.Fatal("pre-handoff archive verification failed", err)
			}
			plan, err := s.PrepareIdleResume(ctx, archive)
			if err != nil {
				t.Fatal("owned idle resume preparation failed", err)
			}
			capture := captureIdleTerminal(ctx, pty.master)
			child, err := plan.Start(ctx, pty.slave, pty.slave, pty.slave)
			if err != nil {
				t.Fatal("owned idle process start failed", err)
			}
			defer child.Close()
			_ = pty.slave.Close()
			evidence, observeErr := observeIdleComposer(ctx, capture, marker, receipt.SessionID)
			closeErr := child.Close()
			select {
			case <-capture.done:
			case <-time.After(time.Second):
				t.Fatal("idle terminal did not close after process retirement")
			}
			post, archiveErr := s.VerifyArchive(ctx, archive)
			exactArchive := archiveErr == nil && post.Persisted && post.SessionID == receipt.SessionID && post.MessageID == receipt.MessageID && post.NativeContentHash == receipt.NativeContentHash && post.NativeUTF8Bytes == receipt.NativeUTF8Bytes
			t.Logf("host=%s binary_sha256=%s bytes=%d helper_retired=true no_turn_ack=true archive_exact=%t composer=%t reference_visible=%t session_visible=%t blocker=%s model_start_observed=%t compaction_observed=%t terminal_bytes=%d cursor_replies=%d prompt_submitted=false network=OS-denied provider_acceptance=unverified", s.HostVersion(), binaryHash, size, exactArchive, evidence.composer, evidence.reference, evidence.session, evidence.blocker, evidence.modelStart, evidence.compaction, evidence.bytes, evidence.cursorReplies)
			t.Logf("terminal_fixed_flags=%s", evidence.diagnostics)
			if closeErr != nil {
				t.Fatal("owned idle cleanup failed", closeErr)
			}
			if !exactArchive {
				t.Fatal("idle handoff changed or lost the exact owned reference", archiveErr)
			}
			if observeErr != nil {
				t.Fatal("idle composer readiness not established", observeErr)
			}
			minimalReady = true
		}) {
			// A larger payload cannot establish readiness when the minimal
			// fixture is blocked; do not repeat native initialization needlessly.
			return
		}
	}
}

func observeIdleComposer(ctx context.Context, capture *idleTerminalCapture, marker, session string) (idleScreenEvidence, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	var evidence idleScreenEvidence
	var ready idleComposerTimer
	var blockedSince time.Time
	lastTotal := -1
	for {
		capture.mu.Lock()
		total, captureErr := capture.total, capture.err
		capture.mu.Unlock()
		if captureErr != nil {
			return evidence, ErrLimit
		}
		if total != lastTotal {
			var err error
			evidence, err = capture.inspect(marker, session)
			if err != nil {
				return evidence, ErrLimit
			}
			lastTotal = total
		}
		if evidence.modelStart || evidence.compaction {
			return evidence, ErrProtocol
		}
		if evidence.blocker != "" {
			if blockedSince.IsZero() {
				blockedSince = time.Now()
			}
			if time.Since(blockedSince) >= time.Second {
				return evidence, ErrState
			}
		}
		if ready.observe(evidence, time.Now()) {
			return evidence, nil
		}
		select {
		case <-ctx.Done():
			return evidence, ctx.Err()
		case <-capture.done:
			evidence, _ = capture.inspect(marker, session)
			return evidence, ErrClosed
		case <-deadline.C:
			return evidence, context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}
