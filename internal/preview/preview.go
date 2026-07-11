// Package preview provides bounded, non-blocking file content inspection.
package preview

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"unicode/utf8"
)

const DefaultLimit int64 = 64 * 1024

type Options struct {
	Limit int64
	Tail  bool
}

type Result struct {
	Data      []byte `json:"data,omitempty"`
	Text      string `json:"text,omitempty"`
	Hex       string `json:"hex,omitempty"`
	Binary    bool   `json:"binary"`
	Truncated bool   `json:"truncated"`
	Offset    int64  `json:"offset"`
	Size      int64  `json:"size"`
}

func Read(path string, opts Options) (Result, error) {
	if opts.Limit <= 0 {
		opts.Limit = DefaultLimit
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Result{}, err
	}
	if !info.Mode().IsRegular() {
		return Result{}, fmt.Errorf("preview requires a regular file, got %s", info.Mode().Type())
	}
	f, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = f.Close() }()
	r := Result{Size: info.Size(), Truncated: info.Size() > opts.Limit}
	if opts.Tail && info.Size() > opts.Limit {
		r.Offset = info.Size() - opts.Limit
		if _, err := f.Seek(r.Offset, io.SeekStart); err != nil {
			return Result{}, err
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, opts.Limit))
	if err != nil {
		return Result{}, err
	}
	r.Data = data
	r.Binary = isBinary(data)
	if r.Binary {
		r.Hex = hex.Dump(data)
	} else {
		r.Text = string(data)
	}
	return r, nil
}

func isBinary(data []byte) bool {
	if !utf8.Valid(data) {
		return true
	}
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}
