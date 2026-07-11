//go:build linux || darwin

package fsinfo

import (
	"errors"
	"sync"
	"testing"
)

const testOwnerName = "alice"

func TestCachedIdentityNameCachesSuccessfulAndFailedLookups(t *testing.T) {
	var cache sync.Map
	calls := 0
	lookup := func(string) (string, error) {
		calls++
		return testOwnerName, nil
	}
	if first := cachedIdentityName(&cache, "1000", lookup); first != testOwnerName {
		t.Fatalf("first lookup = %q", first)
	}
	if second := cachedIdentityName(&cache, "1000", lookup); second != testOwnerName || calls != 1 {
		t.Fatalf("cached lookup = %q, calls=%d", second, calls)
	}

	failed := func(string) (string, error) {
		calls++
		return "", errors.New("unknown identity")
	}
	if got := cachedIdentityName(&cache, "missing", failed); got != "" {
		t.Fatalf("failed lookup = %q", got)
	}
	if got := cachedIdentityName(&cache, "missing", failed); got != "" || calls != 2 {
		t.Fatalf("cached failed lookup = %q, calls=%d", got, calls)
	}
}
