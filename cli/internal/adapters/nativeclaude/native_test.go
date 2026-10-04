//go:build darwin

package nativeclaude

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This opt-in fixture denies all network access at the OS boundary and permits
// only synthetic private state. It proves native protocol and persistence,
// never provider acceptance, exact token counting, or interactive readiness.
func TestNativeClaudeOfflineReference(t *testing.T) {
	for _, size := range []int{1024, 1572864} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			opts, root, binaryHash := offlineClaudeFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			s, err := Start(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			before, err := s.ContextSummary(ctx)
			if err != nil {
				t.Fatal(err)
			}
			unit := "CXTHub synthetic archived reference. \ud55c\uae00 \ud655\uc778. Original role: user.\n"
			payload := strings.Repeat(unit, size/len(unit)) + strings.Repeat("x", size%len(unit))
			receipt, err := s.AppendReference(ctx, payload)
			if err != nil {
				t.Fatal(err)
			}
			if !receipt.NoTurnAcknowledged || receipt.Persisted || receipt.SessionID != s.SessionID() || receipt.ProviderAcceptance != "unverified" || receipt.UTF8Bytes != size {
				t.Fatalf("incorrect no-turn evidence: %+v", receipt)
			}
			after, err := s.ContextSummary(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if after.TotalTokens <= before.TotalTokens || after.Model != before.Model || after.MaxTokens != before.MaxTokens || after.RawMaxTokens != before.RawMaxTokens {
				t.Fatal("local context estimate did not reflect the reference in the same runtime")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			// Native's versioned isSynthetic projection marks archival text as
			// non-user content. Preserve and independently verify that exact prefix.
			nativeText := "[MESSAGE FROM NON-USER SOURCE - NOT USER INPUT]\n" + payload
			archivePath := verifyNativeArchive(t, root, receipt, nativeText)
			persisted, err := s.VerifyArchive(ctx, archivePath)
			if err != nil || !persisted.Persisted || persisted.PayloadHash != receipt.PayloadHash || persisted.ProviderAcceptance != "unverified" {
				t.Fatalf("native archive verification failed: %v", err)
			}
			t.Logf("host=%s binary_sha256=%s bytes=%d payload_hash=%s no_turn_ack=true replay=%v exact_archive=true network=OS-denied local_estimate_before=%d local_estimate_after=%d provider_acceptance=unverified", s.HostVersion(), binaryHash, size, receipt.PayloadHash, receipt.ReplayAcknowledged, before.TotalTokens, after.TotalTokens)
		})
	}
}

func verifyNativeArchive(t *testing.T, root string, receipt ReferenceReceipt, payload string) string {
	t.Helper()
	found := 0
	archivePath := ""
	err := filepath.WalkDir(filepath.Join(root, "config"), func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			t.Fatal("unexpected symlink in synthetic archive")
		}
		if d.IsDir() || d.Name() != receipt.SessionID+".jsonl" {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64<<10), 32<<20)
		for scanner.Scan() {
			var row struct {
				UUID      string `json:"uuid"`
				SessionID string `json:"sessionId"`
				Type      string `json:"type"`
				Message   struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
				return err
			}
			if row.Type == "assistant" {
				t.Fatal("unexpected persisted assistant/model output")
			}
			if row.UUID != receipt.MessageID {
				continue
			}
			found++
			archivePath = path
			var text string
			if json.Unmarshal(row.Message.Content, &text) != nil {
				var blocks []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(row.Message.Content, &blocks) != nil || len(blocks) != 1 || blocks[0].Type != "text" {
					t.Fatal("unexpected persisted reference shape")
				}
				text = blocks[0].Text
			}
			if row.Type != "user" || row.Message.Role != "user" || row.SessionID != receipt.SessionID || text != payload {
				t.Fatalf("native archive mismatch: user_type=%v user_role=%v same_session=%v exact_text=%v contains_payload=%v stored_bytes=%d sent_bytes=%d", row.Type == "user", row.Message.Role == "user", row.SessionID == receipt.SessionID, text == payload, strings.Contains(text, payload), len(text), len(payload))
			}
		}
		return scanner.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Fatalf("persisted matching reference count=%d, want 1", found)
	}
	return archivePath
}

func offlineClaudeFixture(t *testing.T) (Options, string, string) {
	t.Helper()
	binary := os.Getenv("CXT_TEST_NATIVE_CLAUDE")
	if binary == "" {
		t.Skip("set CXT_TEST_NATIVE_CLAUDE to an absolute installed binary for isolated offline protocol validation")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("native binary must be an absolute path")
	}
	binary, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(bytes)
	binaryHash := hex.EncodeToString(digest[:])
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"home", "config", "work", "tmp"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// ICU is OS data, not a user's timezone preference. Resolve the OS revision
	// and permit one root-owned regular data file, without opening keychain or
	// general preference access to make initialization pass.
	format, err := os.ReadFile("/usr/share/icu/icutzformat.txt")
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(format))
	if len(version) == 0 || len(version) > 8 || strings.ContainsAny(version, "/\\.\n\r\t ") {
		t.Fatal("unexpected OS ICU format")
	}
	icu, err := filepath.EvalSymlinks("/private/var/db/timezone/icutz/icutz" + version + ".dat")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(icu)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !strings.HasPrefix(icu, "/private/var/db/timezone/tz/") || !info.Mode().IsRegular() || !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		t.Fatal("ICU exception is not immutable root-owned OS data")
	}
	quote := strconv.Quote
	profile := `(version 1)
(deny default)
(deny network*)
(allow process-fork)
(allow process-exec)
(allow signal (target self))
(allow sysctl-read)
(allow file-read-metadata)
(allow file-read* (subpath ` + quote(root) + `) (literal ` + quote(binary) + `)
 (subpath "/System/Library") (subpath "/usr/lib") (subpath "/usr/share")
 (subpath "/private/var/db/dyld")
 (subpath "/usr/bin") (subpath "/usr/sbin") (subpath "/bin") (subpath "/sbin")
 (literal "/dev/null") (literal "/dev/random") (literal "/dev/urandom"))
(allow file-write* (subpath ` + quote(root) + `))
(allow file-read-data (literal "/") (literal ` + quote(icu) + `))
(allow mach-lookup (global-name "com.apple.system.logger")
 (global-name "com.apple.system.notification_center") (global-name "com.apple.logd"))
`
	profilePath := filepath.Join(root, "sandbox.sb")
	if err := os.WriteFile(profilePath, []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + filepath.Join(root, "home"), "CLAUDE_CONFIG_DIR=" + filepath.Join(root, "config"), "TMPDIR=" + filepath.Join(root, "tmp") + "/", "CLAUDE_CODE_TMPDIR=" + filepath.Join(root, "tmp"), "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "TERM=dumb", "LANG=en_US.UTF-8", "DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
	outside := filepath.Join(t.TempDir(), "synthetic-sentinel")
	if err := os.WriteFile(outside, []byte("synthetic outside data"), 0600); err != nil {
		t.Fatal(err)
	}
	preflightCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe := `use strict; use Socket; use Fcntl;
open(my $in, '>', $ARGV[0]) or die 'inside write'; print $in 'inside'; close($in);
open(my $read, '<', $ARGV[0]) or die 'inside read'; close($read);
sysopen(my $out, $ARGV[1], O_RDONLY) and die 'outside read allowed'; (0+$!) == 1 or die 'wrong read denial';
sysopen(my $write, $ARGV[1], O_WRONLY) and die 'outside write allowed'; (0+$!) == 1 or die 'wrong write denial';
if (socket(my $sock, AF_INET, SOCK_STREAM, 0)) { connect($sock, sockaddr_in(9, inet_aton('127.0.0.1'))) and die 'network allowed'; (0+$!) == 1 or die 'wrong connect denial'; }
else { (0+$!) == 1 or die 'wrong socket denial'; }
print 'isolated';`
	cmd := exec.CommandContext(preflightCtx, "/usr/bin/sandbox-exec", "-f", profilePath, "/usr/bin/perl", "-e", probe, filepath.Join(root, "inside"), outside)
	cmd.Env, cmd.Dir = env, filepath.Join(root, "work")
	if output, err := cmd.CombinedOutput(); err != nil || string(output) != "isolated" {
		t.Fatalf("synthetic OS isolation preflight failed: %v %q", err, output)
	}
	if output, err := os.ReadFile(outside); err != nil || string(output) != "synthetic outside data" {
		t.Fatal("outside synthetic sentinel was changed")
	}
	launcher := filepath.Join(root, "claude-isolated")
	shellQuote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	script := "#!/bin/sh\nexec /usr/bin/sandbox-exec -f " + shellQuote(profilePath) + " " + shellQuote(binary) + " \"$@\"\n"
	if err := os.WriteFile(launcher, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return Options{Executable: launcher, Cwd: filepath.Join(root, "work"), Env: env, Model: "sonnet", ConfigArgs: []string{"--bare", "--safe-mode", "--setting-sources=", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--disable-slash-commands", "--no-chrome", "--tools", "", "--permission-mode", "dontAsk"}}, root, binaryHash
}
