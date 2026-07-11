// Package format renders byte counts, percentages, and proportional bars as
// compact strings shared by the text and TUI renderers. It is pure: no I/O,
// no colors, no terminal assumptions — those concerns belong upstream.
package format

import (
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// SafeText makes an arbitrary filesystem or error string safe to print on a
// terminal. Printable Unicode is preserved; record separators, terminal
// controls, and invalid UTF-8 bytes become visible escapes.
func SafeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			writeByteEscape(&b, s[i])
			i++
			continue
		}
		switch r {
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if unicode.IsControl(r) {
				switch {
				case r <= 0xff:
					writeByteEscape(&b, byte(r))
				case r <= 0xffff:
					writeRuneEscape(&b, `\u`, r, 4)
				default:
					writeRuneEscape(&b, `\U`, r, 8)
				}
			} else {
				b.WriteString(s[i : i+size])
			}
		}
		i += size
	}
	return b.String()
}

func writeByteEscape(b *strings.Builder, value byte) {
	const hex = "0123456789ABCDEF"
	b.WriteString(`\x`)
	b.WriteByte(hex[value>>4])
	b.WriteByte(hex[value&0x0f])
}

func writeRuneEscape(b *strings.Builder, prefix string, value rune, width int) {
	b.WriteString(prefix)
	hex := strconv.FormatInt(int64(value), 16)
	b.WriteString(strings.Repeat("0", width-len(hex)))
	b.WriteString(strings.ToUpper(hex))
}

// partial blocks, from 1/8 up to 7/8; index 0 == 1/8. '█' (8/8) is handled
// separately since it is the only "full" cell rune.
var partial = []rune{'▏', '▎', '▍', '▌', '▋', '▊', '▉'}

// Bytes renders n using adaptive IEC-ish precision (1.2G, 345M, 980B) like
// dust/ncdu: large magnitudes lose decimals to stay compact and columnar.
// n<0 renders as "?" since a measured size is never negative in practice.
func Bytes(n int64) string {
	if n < 0 {
		return "?"
	}
	if n < 1024 {
		return strconv.FormatInt(n, 10) + "B"
	}
	units := []string{"K", "M", "G", "T", "P", "E"}
	f := float64(n)
	i := -1
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	switch {
	case f >= 100:
		return strconv.FormatFloat(f, 'f', 0, 64) + units[i] // 512M
	case f >= 10:
		return strconv.FormatFloat(f, 'f', 1, 64) + units[i] // 12.3M
	default:
		return strconv.FormatFloat(f, 'f', 2, 64) + units[i] // 1.23G
	}
}

// Count renders a non-negative integer with thousands separators (12,345).
func Count(n int) string {
	if n < 0 {
		return "?"
	}
	s := strconv.Itoa(n)
	// insert commas from the right in groups of three.
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Age renders a duration as a compact "ago"-style suffix unit (12s, 5m, 3h, 2d).
func Age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

// Pct returns part/total as a percentage rounded to one decimal; total<=0 -> 0.
func Pct(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

// Frac returns part/total clamped to [0,1]; total<=0 -> 0.
func Frac(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	f := float64(part) / float64(total)
	return math.Max(0, math.Min(1, f))
}

// Bar renders a proportional bar of the given cell width using 8-level
// fractional block characters, so 1.2G out of 10G fills ~12% of the bar
// smoothly rather than snapping between whole cells.
func Bar(frac float64, width int) string {
	if width <= 0 {
		return ""
	}
	frac = math.Max(0, math.Min(1, frac))
	total := width * 8
	filled := int(math.Round(frac * float64(total))) // cells*8 sub-resolution
	var b strings.Builder
	for i := 0; i < width; i++ {
		f := filled - i*8
		switch {
		case f >= 8:
			b.WriteRune('█')
		case f >= 1:
			b.WriteRune(partial[f-1])
		default:
			b.WriteRune(' ')
		}
	}
	return b.String()
}
