//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package storefs

func canFallbackAtomicCreateLink(_ error) bool { return false }
