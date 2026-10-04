package nativecodex

import "testing"

func TestSupportedHostIdentityPreservesDesktopOriginator(t *testing.T) {
	for _, host := range []string{
		"cxthub_native_transport/0.157.1 synthetic",
		"Codex Desktop/0.157.1 (Mac OS 26.4.0; arm64) dumb (cxthub_native_transport; 1)",
		"codex_cli_rs/0.157.1 (Linux; x86_64)",
	} {
		if !SupportedHostIdentity(host) {
			t.Fatal("supported build rejected", host)
		}
	}
	for _, host := range []string{
		"", "/0.157.1 synthetic", "x/0.157.1", "x/0.157.1 ", "x/0.157.10 synthetic", "x/0.157.1-beta synthetic",
		"x/0.158.0 (cxthub_native_transport/0.157.1 synthetic)", "x/extra/0.157.1 synthetic", " x/0.157.1 synthetic",
		"x/0.157.1 bad\nline", "x/0.157.1 bad\x7f", "x/0.157.1 nonascii\u00e9",
	} {
		if SupportedHostIdentity(host) {
			t.Fatal("unsupported or ambiguous host accepted", host)
		}
	}
}
