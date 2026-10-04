package capture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeWrapperSessionOwnsConcurrentThreadsAndRetiresOnlyOne(t *testing.T) {
	root := t.TempDir()
	const a = "11111111-1111-4111-8111-111111111111"
	const b = "22222222-2222-4222-8222-222222222222"
	first, err := BindNativeWrapperSession(root, 100, a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BindNativeWrapperSession(root, 100, b)
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	for _, id := range []string{a, b} {
		if got, err := NativeWrapperSession(root, 100, id); err != nil || got != id {
			t.Fatal("lost owned thread", err)
		}
		if _, err := NativeWrapperSession(root, 101, id); err == nil {
			t.Fatal("cross-wrapper capture accepted")
		}
	}
	rel, _ := nativeWrapperSessionPath(100, a)
	info, err := os.Stat(filepath.Join(root, rel))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("binding is not private", err)
	}
	if err := first(); err != nil {
		t.Fatal(err)
	}
	if err := first(); err != nil {
		t.Fatal("cleanup not idempotent", err)
	}
	if _, err := NativeWrapperSession(root, 100, a); err == nil {
		t.Fatal("retired thread accepted")
	}
	if _, err := NativeWrapperSession(root, 100, b); err != nil {
		t.Fatal("retirement removed replacement", err)
	}
	rel, _ = nativeWrapperSessionPath(100, b)
	if err := os.WriteFile(filepath.Join(root, rel), []byte(`{"version":1,"wrapper_pid":100,"session_id":"other"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NativeWrapperSession(root, 100, b); err == nil {
		t.Fatal("corrupt binding accepted")
	}
}
