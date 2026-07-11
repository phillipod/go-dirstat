package query

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWriteJSONLEscapesRecordBoundaries(t *testing.T) {
	records := []Record{{Path: "/tmp/a\nb", Relative: "a\nb", Name: "a\nb", Kind: KindFile}}
	var out bytes.Buffer
	if err := WriteJSONL(&out, records); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), `a\nb`) {
		t.Fatalf("unsafe JSONL: %q", out.String())
	}
	var decoded Record
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &decoded); err != nil || decoded.Path != records[0].Path {
		t.Fatalf("round trip: %#v, %v", decoded, err)
	}
}

func TestWriteTSVIsHeaderlessSelectableAndControlSafe(t *testing.T) {
	when := time.Date(2026, 7, 11, 1, 2, 3, 4, time.UTC)
	records := []Record{{
		Path: "/tmp/a\tb\nc\\d\x01", Kind: KindFile, Apparent: 42,
		ModTime: when, ScanError: "bad\rthing",
	}}
	var out bytes.Buffer
	fields := []Field{FieldPath, FieldApparent, FieldMTime, FieldScanError}
	if err := WriteTSV(&out, records, fields); err != nil {
		t.Fatal(err)
	}
	want := "/tmp/a\\tb\\nc\\\\d\\x01\t42\t2026-07-11T01:02:03.000000004Z\tbad\\rthing\n"
	if out.String() != want {
		t.Fatalf("TSV\n got %q\nwant %q", out.String(), want)
	}
	if err := WriteTSV(&out, records, []Field{"bogus"}); err == nil {
		t.Fatal("unknown field unexpectedly accepted")
	}
}

func TestWriteNULPreservesPathsAndRejectsAmbiguity(t *testing.T) {
	records := []Record{{Path: "/tmp/a\nb"}, {Path: "/tmp/c\td"}}
	var out bytes.Buffer
	if err := WriteNUL(&out, records); err != nil {
		t.Fatal(err)
	}
	want := []byte("/tmp/a\nb\x00/tmp/c\td\x00")
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("got %q, want %q", out.Bytes(), want)
	}
	if err := WriteNUL(&out, []Record{{Path: "bad\x00path"}}); err == nil {
		t.Fatal("NUL-containing path unexpectedly accepted")
	}
}

func TestEncodersReturnWriterErrors(t *testing.T) {
	w := errorWriter{}
	records := []Record{{Path: "/tmp/a", Kind: KindFile}}
	for name, write := range map[string]func() error{
		"jsonl": func() error { return WriteJSONL(w, records) },
		"tsv":   func() error { return WriteTSV(w, records, nil) },
		"nul":   func() error { return WriteNUL(w, records) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := write(); !errors.Is(err, errWrite) {
				t.Fatalf("got %v, want writer error", err)
			}
		})
	}
}

var errWrite = errors.New("write failed")

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errWrite }
