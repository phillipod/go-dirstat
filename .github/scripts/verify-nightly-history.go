package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type historyDocument struct {
	Baseline bool `json:"baseline"`
	Changes  []struct {
		Path          string `json:"path"`
		Change        string `json:"change"`
		ApparentDelta int64  `json:"apparent_delta_bytes"`
	} `json:"changes"`
}

func main() {
	if len(os.Args) != 6 {
		fatalf("usage: verify-nightly-history SUBJECT TREE QUERY BASELINE GROWTH")
	}
	subject, err := filepath.Abs(os.Args[1])
	if err != nil {
		fatalf("absolute subject: %v", err)
	}
	assertFile(os.Args[2], expectedTree())
	assertFile(os.Args[3], expectedQuery())
	baseline := decode(os.Args[4])
	if !baseline.Baseline || len(baseline.Changes) != 0 {
		fatalf("unexpected history baseline: %+v", baseline)
	}
	growth := decode(os.Args[5])
	if growth.Baseline || len(growth.Changes) != 2 {
		fatalf("unexpected history growth: %+v", growth)
	}
	want := map[string]int64{
		filepath.Clean(subject):             2,
		filepath.Join(subject, "alpha.txt"): 2,
	}
	for _, change := range growth.Changes {
		path := filepath.Clean(change.Path)
		delta, ok := want[path]
		if !ok || change.Change != "grown" || change.ApparentDelta != delta {
			fatalf("unexpected history change: %+v", change)
		}
		delete(want, path)
	}
	if len(want) != 0 {
		fatalf("missing history changes: %v", want)
	}
}

func expectedTree() []byte {
	lines := []string{
		"19\t" + escapeTSV("subject"),
		"6\t" + escapeTSV("subject/alpha.txt"),
		"0\t" + escapeTSV("subject/empty"),
		"13\t" + escapeTSV("subject/sub"),
		"13\t" + escapeTSV("subject/sub/payload.bin"),
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func expectedQuery() []byte {
	return []byte("\tdirectory\t19\t2\t2\n" +
		"alpha.txt\tfile\t6\t0\t0\n" +
		"empty\tdirectory\t0\t0\t0\n" +
		"sub\tdirectory\t13\t1\t0\n" +
		"sub/payload.bin\tfile\t13\t0\t0\n")
}

func assertFile(path string, want []byte) {
	got, err := os.ReadFile(path)
	if err != nil {
		fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		fatalf("unexpected %s:\nwant:\n%s\ngot:\n%s", path, want, got)
	}
}

func escapeTSV(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "\t", `\t`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return strings.ReplaceAll(value, "\r", `\r`)
}

func decode(path string) historyDocument {
	data, err := os.ReadFile(path)
	if err != nil {
		fatalf("read %s: %v", path, err)
	}
	var document historyDocument
	if err := json.Unmarshal(data, &document); err != nil {
		fatalf("decode %s: %v", path, err)
	}
	return document
}

func fatalf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
