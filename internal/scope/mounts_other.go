//go:build !linux && !darwin

package scope

// On platforms without a portable filesystem-type API, fstype resolution is a
// no-op and filesystem filters are unavailable.
type mountTable struct{}

func loadMounts() *mountTable { return nil }

func (mt *mountTable) fstype(string) string { return "" }
