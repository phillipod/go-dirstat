package render

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/phillipod/go-dirstat/internal/tree"
)

func TestTSVBytesStableRecords(t *testing.T) {
	root := &tree.Node{IsDir: true, Apparent: 3072}
	dir := &tree.Node{Name: "sub dir", IsDir: true, Apparent: 2048}
	dir.Adopt(&tree.Node{Name: "line\nbreak\\name", Apparent: 2048})
	root.Adopt(dir)
	root.Adopt(&tree.Node{Name: "tab\tname", Apparent: 1024})

	var b strings.Builder
	err := TSV(&b, root, "/scan root", TextOptions{
		Sort:  tree.SortNameAsc,
		Size:  tree.SizeApparent,
		Files: true,
		Bytes: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "3072\t/scan root\n" +
		"2048\t/scan root/sub dir\n" +
		"2048\t/scan root/sub dir/line\\nbreak\\\\name\n" +
		"1024\t/scan root/tab\\tname\n"
	if got := b.String(); got != want {
		t.Fatalf("TSV output:\n%q\nwant:\n%q", got, want)
	}
	for _, line := range strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n") {
		if strings.Count(line, "\t") != 1 {
			t.Fatalf("record %q does not contain exactly one tab", line)
		}
	}
}

func TestTSVHumanDepthAndLimitOmitsSyntheticRows(t *testing.T) {
	root := &tree.Node{IsDir: true, Apparent: 3584}
	alpha := &tree.Node{Name: "alpha", IsDir: true, Apparent: 2048}
	alpha.Adopt(&tree.Node{Name: "nested", Apparent: 2048})
	root.Adopt(alpha)
	root.Adopt(&tree.Node{Name: "beta", IsDir: true, Apparent: 1024})
	root.Adopt(&tree.Node{Name: "gamma", IsDir: true, Apparent: 512})

	var b strings.Builder
	err := TSV(&b, root, ".", TextOptions{
		Depth: 1,
		Limit: 2,
		Sort:  tree.SortNameAsc,
		Size:  tree.SizeApparent,
		Files: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "3.50K\t.\n" +
		"2.00K\t./alpha\n" +
		"1.00K\t./beta\n"
	if got := b.String(); got != want {
		t.Fatalf("TSV output:\n%q\nwant:\n%q", got, want)
	}
	if strings.Contains(b.String(), "more") || strings.Contains(b.String(), "gamma") || strings.Contains(b.String(), "nested") {
		t.Fatalf("limited TSV contains a synthetic or excluded row: %q", b.String())
	}
}

func TestEscapeTSVPathIsSingleLineAndLossless(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "graphic", input: "space and 雪/quote\"", want: "space and 雪/quote\""},
		{name: "separators", input: "back\\slash\ttab\nline\rcarriage", want: `back\\slash\ttab\nline\rcarriage`},
		{name: "controls", input: string([]byte{0x00, 0x01, 0x1f, 0x7f}), want: `\x00\x01\x1F\x7F`},
		{name: "invalid utf8", input: string([]byte{'x', 0xff, 0xc3, 'y'}), want: `x\xFF\xC3y`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := escapeTSVPath(tt.input)
			if got != tt.want {
				t.Fatalf("escapeTSVPath(%q) = %q, want %q", tt.input, got, tt.want)
			}
			if strings.ContainsAny(got, "\t\n\r") {
				t.Fatalf("escaped field contains a record delimiter: %q", got)
			}
			decoded, err := decodeTSVPath(got)
			if err != nil {
				t.Fatal(err)
			}
			if decoded != tt.input {
				t.Fatalf("decoded path = %q, want original %q", decoded, tt.input)
			}
		})
	}
}

func TestTSVReturnsWriterError(t *testing.T) {
	wantErr := errors.New("output closed")
	err := TSV(errorWriter{err: wantErr}, &tree.Node{IsDir: true}, ".", TextOptions{Size: tree.SizeApparent})
	if !errors.Is(err, wantErr) {
		t.Fatalf("TSV error = %v, want wrapped %v", err, wantErr)
	}
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

func decodeTSVPath(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 >= len(s) {
			return "", io.ErrUnexpectedEOF
		}
		i++
		switch s[i] {
		case '\\':
			b.WriteByte('\\')
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 'x':
			if i+2 >= len(s) {
				return "", io.ErrUnexpectedEOF
			}
			value, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return "", err
			}
			b.WriteByte(byte(value))
			i += 2
		default:
			return "", errors.New("unknown TSV path escape")
		}
	}
	return b.String(), nil
}
