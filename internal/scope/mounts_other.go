//go:build !linux

package scope

// On non-Linux platforms there is no mountinfo to read; fstype resolution is a
// no-op and all fstype filters are inert (they simply match nothing/anything).
type mountTable struct{}

func loadMounts() *mountTable { return nil }

func (mt *mountTable) fstype(string) string { return "" }
