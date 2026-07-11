//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package storefs

// A direct O_EXCL write makes the destination visible before its contents are
// durable. If hard-link publication is unavailable, fail closed instead of
// exposing a partial ownership marker after a crash.
func canFallbackAtomicCreateLink(_ error) bool { return false }
