package render

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/phillipod/go-dirstat/internal/tree"
)

// TSV writes a stable, headerless two-column stream of SIZE<TAB>PATH records.
// The display root is cleaned once, while descendant paths remain qualified by
// it so records from multiple scan roots can be concatenated unambiguously.
func TSV(w io.Writer, root *tree.Node, displayRoot string, opts TextOptions) error {
	root.Sort(opts.Sort, opts.Size)
	r := tsvRenderer{
		w:    w,
		root: filepath.ToSlash(filepath.Clean(displayRoot)),
		opts: opts,
	}
	return r.node(root, "", 0)
}

type tsvRenderer struct {
	w    io.Writer
	root string
	opts TextOptions
}

func (r *tsvRenderer) node(n *tree.Node, rel string, depth int) error {
	displayPath := qualifiedPath(r.root, rel)
	if _, err := fmt.Fprintf(r.w, "%s\t%s\n", r.opts.sizeValue(n.Size(r.opts.Size)), escapeTSVPath(displayPath)); err != nil {
		return fmt.Errorf("write TSV row: %w", err)
	}

	if r.opts.Depth > 0 && depth >= r.opts.Depth {
		return nil
	}
	children := n.Children
	if !r.opts.Files {
		children = onlyDirs(children)
	}
	if r.opts.Limit > 0 && len(children) > r.opts.Limit {
		children = children[:r.opts.Limit]
	}
	for _, child := range children {
		if err := r.node(child, filepath.Join(rel, child.Name), depth+1); err != nil {
			return err
		}
	}
	return nil
}

func qualifiedPath(root, rel string) string {
	if rel == "" {
		return root
	}
	if root == "." {
		return "./" + filepath.ToSlash(rel)
	}
	return filepath.ToSlash(filepath.Join(root, rel))
}

// escapeTSVPath makes arbitrary filesystem byte strings safe for a line-based
// tab-separated stream. Backslash makes every escape reversible; valid graphic
// Unicode is preserved for readability, while invalid UTF-8 is escaped byte by
// byte rather than replaced.
func escapeTSVPath(path string) string {
	var b strings.Builder
	b.Grow(len(path))
	for i := 0; i < len(path); {
		c := path[i]
		if c < utf8.RuneSelf {
			switch c {
			case '\\':
				b.WriteString(`\\`)
			case '\t':
				b.WriteString(`\t`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			default:
				if c < 0x20 || c == 0x7f {
					writeHexEscape(&b, c)
				} else {
					b.WriteByte(c)
				}
			}
			i++
			continue
		}

		_, size := utf8.DecodeRuneInString(path[i:])
		if size == 1 {
			writeHexEscape(&b, c)
			i++
			continue
		}
		b.WriteString(path[i : i+size])
		i += size
	}
	return b.String()
}

func writeHexEscape(b *strings.Builder, c byte) {
	const hex = "0123456789ABCDEF"
	b.WriteString(`\x`)
	b.WriteByte(hex[c>>4])
	b.WriteByte(hex[c&0x0f])
}
