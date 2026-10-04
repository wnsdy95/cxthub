package nativecodex

import "strings"

// SupportedHostIdentity checks the build-version component of native's
// initialize userAgent. In 0.157.1 the originator before '/' can be overridden
// by the desktop environment (including spaces); it is not the build version.
// Keep the full identity in runtime bindings. This is protocol compatibility,
// not binary attestation, and never supplies model/window evidence by itself.
func SupportedHostIdentity(host string) bool {
	if len(host) > 512 || strings.IndexFunc(host, func(r rune) bool { return r < 32 || r > 126 }) >= 0 {
		return false
	}
	originator, rest, ok := strings.Cut(host, "/")
	if !ok || originator == "" || strings.TrimSpace(originator) != originator {
		return false
	}
	version, suffix, ok := strings.Cut(rest, " ")
	return ok && version == ModelWindowNativeVersion && suffix != ""
}
