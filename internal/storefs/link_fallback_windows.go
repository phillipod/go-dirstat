//go:build windows

package storefs

// See link_fallback_unix.go: unsupported hard-link publication fails closed
// because a direct exclusive write is not crash-atomic.
func canFallbackAtomicCreateLink(_ error) bool { return false }
