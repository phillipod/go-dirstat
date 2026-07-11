package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/phillipod/go-dirstat/internal/format"
	querypkg "github.com/phillipod/go-dirstat/internal/query"
	"github.com/phillipod/go-dirstat/internal/scan"
	"github.com/phillipod/go-dirstat/internal/tree"
)

type queryFlags struct {
	output     string
	fields     string
	older      string
	newer      string
	owners     []string
	groups     []string
	extensions []string
	kinds      []string
	pathGlob   string
	pathRegexp string
	sorts      []string
	metadata   bool
	minSize    string
	maxSize    string
}

func newQueryCommand(cfg *Config) *cobra.Command {
	qf := queryFlags{}
	cmd := &cobra.Command{
		Use:   "query [path...]",
		Short: "Find measured disk-usage candidates for scripts or review",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQuery(cmd, cfg, qf, args)
		},
	}
	f := cmd.Flags()
	f.StringVar(&qf.output, "format", "tsv", "output format: tsv|jsonl|nul")
	f.StringVar(&qf.fields, "fields", "path,kind,size,size-human,mtime", "comma-separated TSV fields, including raw or *-human sizes")
	f.StringVar(&qf.minSize, "min-size", "", "include candidates at least SIZE in the selected size mode")
	f.StringVar(&qf.maxSize, "max-size", "", "include candidates no larger than SIZE in the selected size mode")
	f.StringVar(&qf.older, "older-than", "", "include entries at least this old (for example 7d or 24h)")
	f.StringVar(&qf.newer, "newer-than", "", "include entries no more than this old")
	f.StringArrayVar(&qf.owners, "owner", nil, "include only this owner name or uid (repeatable)")
	f.StringArrayVar(&qf.groups, "group", nil, "include only this group name or gid (repeatable)")
	f.StringArrayVar(&qf.extensions, "extension", nil, "include only this file extension (repeatable)")
	f.StringArrayVar(&qf.kinds, "kind", nil, "include only file or directory entries (repeatable)")
	f.StringVar(&qf.pathGlob, "path-glob", "", "match the portable relative path with a glob")
	f.StringVar(&qf.pathRegexp, "path-regexp", "", "match the portable relative path with a regular expression")
	f.StringArrayVar(&qf.sorts, "sort", []string{"size:desc"}, "sort FIELD[:asc|desc] (repeatable)")
	f.BoolVar(&qf.metadata, "metadata", false, "inspect owner, group, mode, identity, and link metadata")
	return cmd
}

func runQuery(cmd *cobra.Command, cfg *Config, flags queryFlags, paths []string) error {
	if flags.output != "tsv" && flags.output != "jsonl" && flags.output != "nul" {
		return fmt.Errorf("invalid --format %q: expected tsv, jsonl, or nul", flags.output)
	}
	older, err := parseQueryAge("older-than", flags.older)
	if err != nil {
		return err
	}
	newer, err := parseQueryAge("newer-than", flags.newer)
	if err != nil {
		return err
	}
	fields, needsMetadata, err := parseQueryFields(flags.fields)
	if err != nil {
		return err
	}
	if flags.output == "nul" && cmd.Flags().Changed("fields") {
		return fmt.Errorf("--fields cannot be used with --format=nul; NUL output is always the absolute path")
	}
	kinds, err := parseQueryKinds(flags.kinds)
	if err != nil {
		return err
	}
	sorts, err := parseQuerySorts(flags.sorts, cfg.sizeMode())
	if err != nil {
		return err
	}
	minValue, maxValue := flags.minSize, flags.maxSize
	// Cobra accepts persistent flags before the subcommand name. Honor those
	// bindings too, even though query shadows the flags with candidate-specific
	// help and semantics when they appear after "query".
	if minValue == "" {
		minValue = cfg.MinSize
	}
	if maxValue == "" {
		maxValue = cfg.MaxSize
	}
	min, max, err := queryBounds(minValue, maxValue)
	if err != nil {
		return err
	}
	if flags.pathGlob != "" {
		if _, err := path.Match(filepath.ToSlash(flags.pathGlob), "probe"); err != nil {
			return fmt.Errorf("invalid --path-glob %q: %w", flags.pathGlob, err)
		}
	}
	if flags.pathRegexp != "" {
		if _, err := regexp.Compile(flags.pathRegexp); err != nil {
			return fmt.Errorf("invalid --path-regexp %q: %w", flags.pathRegexp, err)
		}
	}

	// Query thresholds select completed candidate records. Do not also apply
	// them during traversal: doing so would corrupt directory aggregates and
	// would interpret on-disk thresholds as apparent-size scan thresholds.
	scanCfg := *cfg
	scanCfg.MinSize, scanCfg.MaxSize = "", ""
	policy, err := scanCfg.policy()
	if err != nil {
		return err
	}
	filter := querypkg.Filter{
		MinSize: min, MaxSize: max, SizeMode: cfg.sizeMode(),
		OlderThan: older, NewerThan: newer,
		Owners: flags.owners, Groups: flags.groups, Extensions: flags.extensions,
		Kinds: kinds, PathGlob: flags.pathGlob, PathRegexp: flags.pathRegexp,
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	for _, path := range paths {
		root, _, err := scan.Scan(cmd.Context(), path, scan.Options{Policy: policy, Concurrency: cfg.Jobs})
		if err != nil {
			return fmt.Errorf("%q: %w", path, err)
		}
		records, err := querypkg.Build(root, path, querypkg.Options{
			Filter: filter, Sort: sorts, Metadata: flags.metadata || needsMetadata,
		})
		if err != nil {
			return fmt.Errorf("query %q: %w", path, err)
		}
		switch flags.output {
		case "jsonl":
			if cmd.Flags().Changed("fields") {
				err = writeQueryJSONL(cmd.OutOrStdout(), records, fields, cfg.sizeMode())
			} else {
				err = querypkg.WriteJSONL(cmd.OutOrStdout(), records)
			}
		case "nul":
			err = querypkg.WriteNUL(cmd.OutOrStdout(), records)
		default:
			err = writeQueryTSV(cmd.OutOrStdout(), records, fields, cfg.sizeMode())
		}
		if err != nil {
			return fmt.Errorf("render query for %q: %w", path, err)
		}
	}
	return nil
}

func parseQueryAge(name, value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	original := value
	multiplier := time.Duration(1)
	if strings.HasSuffix(value, "d") || strings.HasSuffix(value, "D") {
		multiplier, value = 24, value[:len(value)-1]+"h"
	} else if strings.HasSuffix(value, "w") || strings.HasSuffix(value, "W") {
		multiplier, value = 7*24, value[:len(value)-1]+"h"
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid --%s %q: %w", name, original, err)
	}
	if multiplier > 1 {
		if duration > time.Duration(1<<63-1)/multiplier {
			return 0, fmt.Errorf("--%s is too large", name)
		}
		duration *= multiplier
	}
	if duration < 0 {
		return 0, fmt.Errorf("--%s must not be negative", name)
	}
	return duration, nil
}

func queryBounds(minValue, maxValue string) (*int64, *int64, error) {
	min, err := parseSize(minValue)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid --min-size %q: %w", minValue, err)
	}
	max, err := parseSize(maxValue)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid --max-size %q: %w", maxValue, err)
	}
	var minPtr, maxPtr *int64
	if strings.TrimSpace(minValue) != "" {
		minPtr = &min
	}
	if strings.TrimSpace(maxValue) != "" {
		if max == 0 {
			return nil, nil, fmt.Errorf("--max-size must be greater than zero; omit it for no upper bound")
		}
		maxPtr = &max
	}
	if minPtr != nil && maxPtr != nil && min > max {
		return nil, nil, fmt.Errorf("--min-size (%s) must not exceed --max-size (%s)", minValue, maxValue)
	}
	return minPtr, maxPtr, nil
}

type queryOutputField struct {
	name  string
	raw   querypkg.Field
	sized string
}

var queryRawFields = map[string]querypkg.Field{
	"path": querypkg.FieldPath, "relative": querypkg.FieldRelative,
	"name": querypkg.FieldName, "extension": querypkg.FieldExtension,
	"kind": querypkg.FieldKind, "apparent": querypkg.FieldApparent,
	"allocated": querypkg.FieldAllocated, "files": querypkg.FieldFiles,
	"directories": querypkg.FieldDirectories, "mtime": querypkg.FieldMTime,
	"owner": querypkg.FieldOwner, "group": querypkg.FieldGroup,
	"uid": querypkg.FieldUID, "gid": querypkg.FieldGID,
	"mode": querypkg.FieldMode, "mode-text": querypkg.FieldModeText,
	"links": querypkg.FieldLinks, "device": querypkg.FieldDevice,
	"file-id": querypkg.FieldFileID, "hardlink": querypkg.FieldHardlink,
	"scan-error": querypkg.FieldScanError, "metadata-error": querypkg.FieldMetadataError,
}

func parseQueryFields(value string) ([]queryOutputField, bool, error) {
	parts := strings.Split(value, ",")
	fields := make([]queryOutputField, 0, len(parts))
	needsMetadata := false
	for _, part := range parts {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			return nil, false, fmt.Errorf("--fields must not contain an empty field")
		}
		switch name {
		case "size", "size-human", "apparent-human", "allocated-human":
			fields = append(fields, queryOutputField{name: name, sized: name})
		default:
			field, ok := queryRawFields[name]
			if !ok {
				return nil, false, fmt.Errorf("unsupported --fields value %q", name)
			}
			fields = append(fields, queryOutputField{name: name, raw: field})
			switch field {
			case querypkg.FieldOwner, querypkg.FieldGroup, querypkg.FieldUID, querypkg.FieldGID,
				querypkg.FieldMode, querypkg.FieldModeText, querypkg.FieldLinks,
				querypkg.FieldDevice, querypkg.FieldFileID, querypkg.FieldMetadataError:
				needsMetadata = true
			}
		}
	}
	return fields, needsMetadata, nil
}

func writeQueryJSONL(w io.Writer, records []querypkg.Record, fields []queryOutputField, mode tree.SizeMode) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	for i, record := range records {
		projected := make(map[string]any, len(fields))
		for _, field := range fields {
			projected[field.name] = queryJSONValue(record, field, mode)
		}
		if err := encoder.Encode(projected); err != nil {
			return fmt.Errorf("write JSONL record %d: %w", i, err)
		}
	}
	return nil
}

func queryJSONValue(record querypkg.Record, field queryOutputField, mode tree.SizeMode) any {
	if field.sized != "" {
		value := record.Apparent
		switch field.sized {
		case "allocated-human":
			value = record.Allocated
		case "size", "size-human":
			if mode == tree.SizeOnDisk {
				value = record.Allocated
			}
		}
		if strings.HasSuffix(field.sized, "-human") {
			return format.Bytes(value)
		}
		return value
	}
	switch field.raw {
	case querypkg.FieldRelative:
		return record.Relative
	case querypkg.FieldName:
		return record.Name
	case querypkg.FieldExtension:
		return record.Extension
	case querypkg.FieldKind:
		return record.Kind
	case querypkg.FieldApparent:
		return record.Apparent
	case querypkg.FieldAllocated:
		return record.Allocated
	case querypkg.FieldFiles:
		return record.FileCount
	case querypkg.FieldDirectories:
		return record.DirCount
	case querypkg.FieldMTime:
		if record.ModTime.IsZero() {
			return nil
		}
		return record.ModTime
	case querypkg.FieldOwner:
		return record.Owner
	case querypkg.FieldGroup:
		return record.Group
	case querypkg.FieldUID:
		return record.UID
	case querypkg.FieldGID:
		return record.GID
	case querypkg.FieldMode:
		return record.Mode
	case querypkg.FieldModeText:
		return record.ModeText
	case querypkg.FieldLinks:
		return record.Links
	case querypkg.FieldDevice:
		return record.Identity.Device
	case querypkg.FieldFileID:
		return record.Identity.File
	case querypkg.FieldHardlink:
		return record.Hardlink
	case querypkg.FieldScanError:
		return record.ScanError
	case querypkg.FieldMetadataError:
		return record.MetadataError
	default:
		return record.Path
	}
}

func writeQueryTSV(w io.Writer, records []querypkg.Record, fields []queryOutputField, mode tree.SizeMode) error {
	for i, record := range records {
		for column, field := range fields {
			if column > 0 {
				if _, err := io.WriteString(w, "\t"); err != nil {
					return err
				}
			}
			if field.sized != "" {
				value := record.Apparent
				switch field.sized {
				case "allocated-human":
					value = record.Allocated
				case "size", "size-human":
					if mode == tree.SizeOnDisk {
						value = record.Allocated
					}
				}
				text := fmt.Sprintf("%d", value)
				if strings.HasSuffix(field.sized, "-human") {
					text = format.Bytes(value)
				}
				if _, err := io.WriteString(w, text); err != nil {
					return err
				}
				continue
			}
			var cell strings.Builder
			if err := querypkg.WriteTSV(&cell, []querypkg.Record{record}, []querypkg.Field{field.raw}); err != nil {
				return err
			}
			if _, err := io.WriteString(w, strings.TrimSuffix(cell.String(), "\n")); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return fmt.Errorf("write TSV record %d: %w", i, err)
		}
	}
	return nil
}

func parseQueryKinds(values []string) ([]querypkg.Kind, error) {
	kinds := make([]querypkg.Kind, 0, len(values))
	for _, value := range values {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "file":
			kinds = append(kinds, querypkg.KindFile)
		case "dir", "directory":
			kinds = append(kinds, querypkg.KindDirectory)
		default:
			return nil, fmt.Errorf("invalid --kind %q: expected file or directory", value)
		}
	}
	return kinds, nil
}

func parseQuerySorts(values []string, mode tree.SizeMode) ([]querypkg.SortKey, error) {
	fields := map[string]querypkg.SortField{
		"path": querypkg.SortPath, "name": querypkg.SortName,
		"apparent": querypkg.SortApparent, "allocated": querypkg.SortAllocated,
		"files": querypkg.SortFiles, "directories": querypkg.SortDirs,
		"mtime": querypkg.SortMTime, "owner": querypkg.SortOwner,
		"group": querypkg.SortGroup, "extension": querypkg.SortExtension,
		"kind": querypkg.SortKind,
	}
	if mode == tree.SizeApparent {
		fields["size"] = querypkg.SortApparent
	} else {
		fields["size"] = querypkg.SortAllocated
	}
	result := make([]querypkg.SortKey, 0, len(values)+1)
	for _, value := range values {
		parts := strings.Split(value, ":")
		if len(parts) > 2 {
			return nil, fmt.Errorf("invalid --sort %q", value)
		}
		field, ok := fields[strings.ToLower(strings.TrimSpace(parts[0]))]
		if !ok {
			return nil, fmt.Errorf("invalid --sort field %q", parts[0])
		}
		key := querypkg.SortKey{Field: field}
		if len(parts) == 2 {
			switch strings.ToLower(strings.TrimSpace(parts[1])) {
			case "asc":
			case "desc":
				key.Desc = true
			default:
				return nil, fmt.Errorf("invalid --sort direction %q", parts[1])
			}
		}
		result = append(result, key)
	}
	return result, nil
}

// Kept here so path cleaning follows the same native rules used by scanning.
func absolutePath(path string) (string, error) {
	return filepath.Abs(filepath.Clean(path))
}
