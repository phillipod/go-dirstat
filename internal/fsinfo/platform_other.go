//go:build !linux && !darwin && !windows

package fsinfo

import (
	"errors"
	"io/fs"
)

func allocatedBytes(info fs.FileInfo) int64                  { return info.Size() }
func identity(string, fs.FileInfo) Identity                  { return Identity{} }
func linkCount(string, fs.FileInfo) uint64                   { return 0 }
func ownership(fs.FileInfo) (string, string, string, string) { return "", "", "", "" }
func volumeFor(string) (Volume, error)                       { return Volume{}, errors.New("volume statistics unavailable") }
