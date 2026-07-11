//go:build !windows

package fsops

import (
	"errors"
	"syscall"
)

func isCrossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }
