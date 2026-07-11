package query

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Field names a selectable TSV record field.
type Field string

const (
	FieldPath          Field = "path"
	FieldRelative      Field = "relative"
	FieldName          Field = "name"
	FieldExtension     Field = "extension"
	FieldKind          Field = "kind"
	FieldApparent      Field = "apparent"
	FieldAllocated     Field = "allocated"
	FieldFiles         Field = "files"
	FieldDirectories   Field = "directories"
	FieldMTime         Field = "mtime"
	FieldOwner         Field = "owner"
	FieldGroup         Field = "group"
	FieldUID           Field = "uid"
	FieldGID           Field = "gid"
	FieldMode          Field = "mode"
	FieldModeText      Field = "mode_text"
	FieldLinks         Field = "links"
	FieldDevice        Field = "device"
	FieldFileID        Field = "file_id"
	FieldHardlink      Field = "hardlink"
	FieldScanError     Field = "scan_error"
	FieldMetadataError Field = "metadata_error"
)

var defaultFields = []Field{FieldPath, FieldKind, FieldApparent, FieldAllocated, FieldMTime}

// DefaultTSVFields returns a copy of the concise, stable default TSV schema.
func DefaultTSVFields() []Field { return append([]Field(nil), defaultFields...) }

var supportedFields = map[Field]bool{
	FieldPath: true, FieldRelative: true, FieldName: true, FieldExtension: true,
	FieldKind: true, FieldApparent: true, FieldAllocated: true, FieldFiles: true,
	FieldDirectories: true, FieldMTime: true, FieldOwner: true, FieldGroup: true,
	FieldUID: true, FieldGID: true, FieldMode: true, FieldModeText: true,
	FieldLinks: true, FieldDevice: true, FieldFileID: true, FieldHardlink: true,
	FieldScanError: true, FieldMetadataError: true,
}

// WriteJSONL writes exactly one JSON object per record. encoding/json escapes
// embedded newlines and other controls, preserving record boundaries.
func WriteJSONL(w io.Writer, records []Record) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for i := range records {
		if err := enc.Encode(records[i]); err != nil {
			return fmt.Errorf("query: write JSONL record %d: %w", i, err)
		}
	}
	return nil
}

// WriteTSV writes headerless tab-separated records. Backslash and control
// bytes are C-escaped, so every record occupies one physical line and remains
// safe for cut/awk processing. An empty fields slice uses DefaultFields.
func WriteTSV(w io.Writer, records []Record, fields []Field) error {
	if len(fields) == 0 {
		fields = defaultFields
	}
	for _, field := range fields {
		if !supportedFields[field] {
			return fmt.Errorf("query: unsupported TSV field %q", field)
		}
	}
	bw := bufio.NewWriter(w)
	for row := range records {
		for column, field := range fields {
			if column > 0 {
				if err := bw.WriteByte('\t'); err != nil {
					return fmt.Errorf("query: write TSV record %d: %w", row, err)
				}
			}
			if _, err := bw.WriteString(escapeTSV(fieldValue(records[row], field))); err != nil {
				return fmt.Errorf("query: write TSV record %d: %w", row, err)
			}
		}
		if err := bw.WriteByte('\n'); err != nil {
			return fmt.Errorf("query: write TSV record %d: %w", row, err)
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("query: flush TSV: %w", err)
	}
	return nil
}

// WriteNUL writes absolute paths separated by NUL bytes. It rejects a path
// containing NUL rather than emitting an ambiguous stream. There is a trailing
// separator after every record, matching find -print0 conventions.
func WriteNUL(w io.Writer, records []Record) error {
	for i := range records {
		if strings.IndexByte(records[i].Path, 0) >= 0 {
			return fmt.Errorf("query: path in record %d contains NUL", i)
		}
		if _, err := io.WriteString(w, records[i].Path); err != nil {
			return fmt.Errorf("query: write NUL record %d: %w", i, err)
		}
		if _, err := w.Write([]byte{0}); err != nil {
			return fmt.Errorf("query: write NUL separator %d: %w", i, err)
		}
	}
	return nil
}

func fieldValue(r Record, field Field) string {
	switch field {
	case FieldRelative:
		return r.Relative
	case FieldName:
		return r.Name
	case FieldExtension:
		return r.Extension
	case FieldKind:
		return string(r.Kind)
	case FieldApparent:
		return strconv.FormatInt(r.Apparent, 10)
	case FieldAllocated:
		return strconv.FormatInt(r.Allocated, 10)
	case FieldFiles:
		return strconv.Itoa(r.FileCount)
	case FieldDirectories:
		return strconv.Itoa(r.DirCount)
	case FieldMTime:
		if r.ModTime.IsZero() {
			return ""
		}
		return r.ModTime.Format(time.RFC3339Nano)
	case FieldOwner:
		return r.Owner
	case FieldGroup:
		return r.Group
	case FieldUID:
		return r.UID
	case FieldGID:
		return r.GID
	case FieldMode:
		return fmt.Sprintf("%#o", r.Mode)
	case FieldModeText:
		return r.ModeText
	case FieldLinks:
		return strconv.FormatUint(r.Links, 10)
	case FieldDevice:
		return strconv.FormatUint(r.Identity.Device, 10)
	case FieldFileID:
		return strconv.FormatUint(r.Identity.File, 10)
	case FieldHardlink:
		return strconv.FormatBool(r.Hardlink)
	case FieldScanError:
		return r.ScanError
	case FieldMetadataError:
		return r.MetadataError
	default:
		return r.Path
	}
}

func escapeTSV(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case 0:
			b.WriteString(`\0`)
		default:
			if c < 0x20 || c == 0x7f {
				_, _ = fmt.Fprintf(&b, `\x%02x`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}
