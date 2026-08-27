package memory

import (
	"fmt"
	"strings"
	"testing"
)

func TestClaudeAutoLoadedPrefixUsesConservativeCompleteLines(t *testing.T) {
	t.Run("small memory is fully loaded", func(t *testing.T) {
		const text = "first memory\nsecond memory"
		if got := claudeAutoLoadedPrefix(text); got != text {
			t.Fatalf("prefix=%q want full memory", got)
		}
	})

	t.Run("line-limited memory retains the unproven tail", func(t *testing.T) {
		lines := make([]string, 250)
		for i := range lines {
			lines[i] = fmt.Sprintf("memory-line-%03d", i)
		}
		got := claudeAutoLoadedPrefix(strings.Join(lines, "\n"))
		if !strings.Contains(got, "memory-line-198") || strings.Contains(got, "memory-line-199") {
			t.Fatalf("line-safe prefix ended at the wrong boundary: lines=%d tail=%q", strings.Count(got, "\n")+1, got[len(got)-32:])
		}
	})

	t.Run("byte-limited memory never cuts a long line", func(t *testing.T) {
		text := "safe complete line\n" + strings.Repeat("x", 30<<10) + "\nUNLOADED TAIL"
		if got := claudeAutoLoadedPrefix(text); got != "safe complete line" {
			t.Fatalf("prefix cut an incomplete line: bytes=%d tail=%q", len(got), got[max(0, len(got)-32):])
		}
	})
}
