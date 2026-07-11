package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	appconfig "github.com/phillipod/go-dirstat/internal/config"
	"github.com/phillipod/go-dirstat/internal/format"
	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/index"
	querypkg "github.com/phillipod/go-dirstat/internal/query"
	"github.com/phillipod/go-dirstat/internal/scan"
	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

type queryFlags struct {
	output        string
	fields        string
	older         string
	newer         string
	owners        []string
	groups        []string
	extensions    []string
	kinds         []string
	pathGlob      string
	pathRegexp    string
	sorts         []string
	metadata      bool
	allowPartial  bool
	stream        bool
	requireMatch  bool
	failIfMatch   bool
	limit         int
	minSize       string
	maxSize       string
	indexMode     string
	indexEvidence string
}

const (
	queryFieldSize           = "size"
	queryFieldAllocatedHuman = "allocated-human"
	queryIndexLive           = "live"
	queryIndexPrefer         = "prefer"
	queryIndexOnly           = "only"
	queryIndexRefresh        = "refresh"
	queryIndexEvidenceText   = "text"
	queryIndexEvidenceJSONL  = "jsonl"
	unknownAge               = "n/a"
)

func newQueryCommand(cfg *Config) *cobra.Command {
	qf := queryFlags{}
	cmd := &cobra.Command{
		Use:   "query [path...]",
		Short: "Find measured disk-usage candidates for scripts or review",
		Long: `Find measured disk-usage candidates for scripts or review.

Sorted queries may use --limit to retain only the best records. --stream emits
deterministic tree order without sorting or retaining a record set and cannot
be combined with --sort.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQuery(cmd, cfg, qf, args)
		},
	}
	f := cmd.Flags()
	f.StringVar(&qf.output, "format", outputFormatTSV, "output format: tsv|jsonl|nul")
	f.StringVar(&qf.fields, "fields", "path,kind,size,size-human,mtime", "comma-separated TSV fields, including raw or *-human sizes")
	f.StringVar(&qf.minSize, "min-size", "", "include candidates at least SIZE in the selected size mode")
	f.StringVar(&qf.maxSize, "max-size", "", "include candidates no larger than SIZE in the selected size mode")
	f.StringVar(&qf.older, "older-than", "", "include entries at least this old (for example 7d or 24h)")
	f.StringVar(&qf.newer, "newer-than", "", "include entries no more than this old")
	f.StringArrayVar(&qf.owners, "owner", nil, "include only this owner name or uid (repeatable; unavailable on Windows)")
	f.StringArrayVar(&qf.groups, "group", nil, "include only this group name or gid (repeatable; unavailable on Windows)")
	f.StringArrayVar(&qf.extensions, "extension", nil, "include only this file extension (repeatable)")
	f.StringArrayVar(&qf.kinds, "kind", nil, "include only file or directory entries (repeatable)")
	f.StringVar(&qf.pathGlob, "path-glob", "", "match the portable relative path with a glob")
	f.StringVar(&qf.pathRegexp, "path-regexp", "", "match the portable relative path with a regular expression")
	f.StringArrayVar(&qf.sorts, "sort", []string{"size:desc"}, "sort FIELD[:asc|desc] (repeatable)")
	f.BoolVar(&qf.metadata, "metadata", false, "inspect owner, group, mode, identity, and link metadata")
	f.BoolVar(&qf.allowPartial, "allow-partial", false, "emit incomplete results and warn instead of exiting with status 3")
	f.BoolVar(&qf.stream, "stream", false, "emit deterministic tree order without sorting or retaining all records")
	f.BoolVar(&qf.requireMatch, "require-match", false, "exit 6 when no candidate matches")
	f.BoolVar(&qf.failIfMatch, "fail-if-match", false, "exit 6 when one or more candidates match")
	f.IntVar(&qf.limit, "limit", 0, "maximum records per scanned root (0 = unlimited)")
	f.StringVar(&qf.indexMode, "index", queryIndexLive, "query source: live|prefer|only|refresh")
	f.StringVar(&qf.indexEvidence, "index-evidence", queryIndexEvidenceText, "index evidence on stderr: text|jsonl")
	return cmd
}

func runQuery(cmd *cobra.Command, cfg *Config, flags queryFlags, paths []string) error {
	if flags.output != outputFormatTSV && flags.output != outputFormatJSONL && flags.output != outputFormatNUL {
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
	if flags.output == outputFormatNUL && cmd.Flags().Changed("fields") {
		return fmt.Errorf("--fields cannot be used with --format=nul; NUL output is always the absolute path")
	}
	if flags.limit < 0 {
		return fmt.Errorf("--limit must be zero or greater")
	}
	if flags.stream && cmd.Flags().Changed("sort") {
		return fmt.Errorf("--sort cannot be used with --stream; streaming preserves deterministic tree order")
	}
	if flags.requireMatch && flags.failIfMatch {
		return fmt.Errorf("--require-match and --fail-if-match cannot be used together")
	}
	if !validQueryIndexMode(flags.indexMode) {
		return fmt.Errorf("invalid --index %q: expected live, prefer, only, or refresh", flags.indexMode)
	}
	if flags.indexEvidence != queryIndexEvidenceText && flags.indexEvidence != queryIndexEvidenceJSONL {
		return fmt.Errorf("invalid --index-evidence %q: expected text or jsonl", flags.indexEvidence)
	}
	if flags.indexMode == queryIndexLive && cmd.Flags().Changed("index-evidence") {
		return errors.New("--index-evidence is only valid with --index=prefer, --index=only, or --index=refresh")
	}
	if flags.indexMode == queryIndexRefresh && flags.allowPartial {
		return errors.New("--allow-partial cannot be used with --index=refresh; partial indexes are never published")
	}
	if flags.indexMode == queryIndexOnly && flags.allowPartial {
		return errors.New("--allow-partial cannot be used with --index=only; persisted indexes are always complete")
	}
	if flags.indexEvidence == queryIndexEvidenceJSONL && flags.allowPartial {
		return errors.New("--allow-partial cannot be used with --index-evidence=jsonl; stderr must remain parseable JSONL")
	}
	kinds, err := parseQueryKinds(flags.kinds)
	if err != nil {
		return err
	}
	sorts, err := parseQuerySorts(flags.sorts, cfg.sizeMode())
	if err != nil {
		return err
	}
	if err := validateQueryOwnershipCapability(fsinfo.OwnershipAvailable(), flags, fields, sorts); err != nil {
		return err
	}
	if flags.indexMode == queryIndexOnly || flags.indexMode == queryIndexPrefer {
		if flags.metadata || needsMetadata || len(flags.owners) > 0 || len(flags.groups) > 0 || querySortNeedsLiveMetadata(sorts) {
			return errors.New("live metadata fields, filters, and sorts cannot be used with --index=prefer or --index=only")
		}
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
	var indexPolicy index.Policy
	if flags.indexMode != queryIndexLive {
		userCfg, err := appconfig.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		indexPolicy = index.Policy{
			MaxBytes: userCfg.State.CacheMaxBytes,
			MaxAge:   time.Duration(userCfg.State.CacheTTLHours) * time.Hour,
		}
	}
	matched := 0
	for _, path := range paths {
		root, _, err := queryRoot(cmd, path, policy, cfg.Jobs, flags.indexMode, flags.indexEvidence, indexPolicy, flags.allowPartial)
		if err != nil {
			return err
		}
		options := querypkg.Options{
			Filter: filter, Sort: sorts, Metadata: flags.metadata || needsMetadata,
			FollowMetadata: cfg.Follow, Limit: flags.limit,
		}
		if flags.stream {
			options.Sort = nil
			err = querypkg.StreamContext(cmd.Context(), root, path, options, func(record querypkg.Record) error {
				if err := renderQueryRecords(cmd, flags, fields, []querypkg.Record{record}, cfg.sizeMode()); err != nil {
					return err
				}
				matched++
				return nil
			})
		} else {
			var records []querypkg.Record
			records, err = querypkg.BuildContext(cmd.Context(), root, path, options)
			if err == nil {
				err = renderQueryRecords(cmd, flags, fields, records, cfg.sizeMode())
				matched += len(records)
			}
		}
		if err != nil {
			return fmt.Errorf("query %q: %w", path, err)
		}
	}
	if flags.requireMatch && matched == 0 {
		return &conditionError{code: ExitCandidateState, message: "query matched no candidates"}
	}
	if flags.failIfMatch && matched > 0 {
		return &conditionError{code: ExitCandidateState, message: fmt.Sprintf("query matched %d candidate(s)", matched)}
	}
	return nil
}

func validQueryIndexMode(mode string) bool {
	switch mode {
	case queryIndexLive, queryIndexPrefer, queryIndexOnly, queryIndexRefresh:
		return true
	default:
		return false
	}
}

func querySortNeedsLiveMetadata(sorts []querypkg.SortKey) bool {
	for _, key := range sorts {
		if key.Field == querypkg.SortOwner || key.Field == querypkg.SortGroup {
			return true
		}
	}
	return false
}

func rejectOperationalQueryRoot(root string) error {
	statePaths := defaultStateExclusions()
	resolvedRoot := resolvedPath(root)
	for _, store := range statePaths {
		contained, _ := pathContainedBy(resolvedPath(store), resolvedRoot)
		if contained {
			return fmt.Errorf("query root %q is inside dirstat operational state %q; use the state command instead", root, store)
		}
	}
	return nil
}

func queryRoot(
	cmd *cobra.Command,
	displayPath string,
	policy scope.Policy,
	jobs int,
	mode string,
	evidenceFormat string,
	storePolicy index.Policy,
	allowPartial bool,
) (*tree.Node, scan.Stats, error) {
	absRoot, err := absolutePath(displayPath)
	if err != nil {
		return nil, scan.Stats{}, fmt.Errorf("resolve query root %q: %w", displayPath, err)
	}
	if err := rejectOperationalQueryRoot(absRoot); err != nil {
		return nil, scan.Stats{}, err
	}
	if mode == queryIndexLive {
		return liveQueryRoot(cmd, displayPath, policy, jobs, allowPartial)
	}
	fingerprint := index.Fingerprint(absRoot, policy)
	storeDir, storeDirErr := index.DefaultStoreDir()
	if storeDirErr != nil && mode != queryIndexPrefer {
		return nil, scan.Stats{}, fmt.Errorf("resolve query index store: %w", storeDirErr)
	}
	if mode == queryIndexPrefer || mode == queryIndexOnly {
		var snap *index.Snapshot
		loadErr := storeDirErr
		if loadErr == nil {
			store, openErr := index.OpenStoreAtWithPolicy(storeDir, storePolicy)
			if openErr != nil {
				loadErr = fmt.Errorf("open query index: %w", openErr)
			} else {
				snap, loadErr = store.LoadContext(cmd.Context(), absRoot, fingerprint)
			}
		}
		if loadErr == nil {
			loadErr = validateQuerySnapshot(snap, storePolicy.MaxAge, time.Now())
		}
		if loadErr == nil {
			node := snap.ToTree()
			if node == nil {
				loadErr = index.ErrIncompatible
			} else {
				if err := writeQueryIndexEvidence(cmd, evidenceFormat, mode, "index", absRoot, fingerprint, snap.ScannedAt, true, ""); err != nil {
					return nil, scan.Stats{}, err
				}
				return node, scan.Stats{
					Files: snap.Files, Dirs: snap.Dirs, Errors: snap.Errors,
					RootFS: snap.RootFS, Complete: snap.Complete,
				}, nil
			}
		}
		if mode == queryIndexOnly {
			return nil, scan.Stats{}, fmt.Errorf(
				"query index for %q is unavailable: %w; run query --index=refresh for this root",
				displayPath, loadErr,
			)
		}
		node, stats, liveErr := liveQueryRoot(cmd, displayPath, policy, jobs, allowPartial)
		if liveErr != nil {
			return nil, scan.Stats{}, liveErr
		}
		if err := writeQueryIndexEvidence(cmd, evidenceFormat, mode, "live", absRoot, fingerprint, time.Now().UTC(), stats.Complete, "fallback="+loadErr.Error()); err != nil {
			return nil, scan.Stats{}, err
		}
		return node, stats, nil
	}

	if err := cmd.Context().Err(); err != nil {
		return nil, scan.Stats{}, err
	}
	writeStore, err := index.NewStoreAtWithPolicy(storeDir, storePolicy)
	if err != nil {
		return nil, scan.Stats{}, fmt.Errorf("create query index store: %w", err)
	}
	node, stats, err := liveQueryRoot(cmd, displayPath, policy, jobs, false)
	if err != nil {
		return nil, scan.Stats{}, err
	}
	snap := index.FromTree(node, fingerprint, stats.RootFS, stats.Files, stats.Dirs, stats.Errors, stats.Complete, time.Now().UTC())
	snap.Root = absRoot
	if err := writeStore.SaveContext(cmd.Context(), snap); err != nil {
		return nil, scan.Stats{}, fmt.Errorf("publish query index for %q: %w", displayPath, err)
	}
	if err := writeQueryIndexEvidence(cmd, evidenceFormat, mode, "live", absRoot, fingerprint, snap.ScannedAt, true, "published"); err != nil {
		return nil, scan.Stats{}, err
	}
	return node, stats, nil
}

func liveQueryRoot(cmd *cobra.Command, path string, policy scope.Policy, jobs int, allowPartial bool) (*tree.Node, scan.Stats, error) {
	root, stats, err := scan.Scan(cmd.Context(), path, scan.Options{Policy: policy, Concurrency: jobs})
	if err != nil {
		return nil, scan.Stats{}, fmt.Errorf("%q: %w", path, err)
	}
	if err := acceptScan(cmd, path, stats, allowPartial); err != nil {
		return nil, scan.Stats{}, err
	}
	return root, stats, nil
}

func validateQuerySnapshot(snap *index.Snapshot, maxAge time.Duration, now time.Time) error {
	if snap == nil || !snap.Complete || snap.Errors != 0 {
		return errors.New("persisted snapshot is incomplete")
	}
	if snap.ScannedAt.IsZero() {
		return errors.New("persisted snapshot has no scan timestamp")
	}
	if snap.ScannedAt.After(now.Add(time.Minute)) {
		return errors.New("persisted snapshot timestamp is in the future")
	}
	if now.Sub(snap.ScannedAt) > maxAge {
		return fmt.Errorf("persisted snapshot is stale (age %s exceeds %s)", format.Age(now.Sub(snap.ScannedAt)), format.Age(maxAge))
	}
	return nil
}

func writeQueryIndexEvidence(
	cmd *cobra.Command,
	evidenceFormat string,
	mode, source, root, fingerprint string,
	scannedAt time.Time,
	complete bool,
	detail string,
) error {
	age := unknownAge
	if !scannedAt.IsZero() {
		age = format.Age(time.Since(scannedAt))
	}
	if evidenceFormat == queryIndexEvidenceJSONL {
		return json.NewEncoder(cmd.ErrOrStderr()).Encode(struct {
			Mode        string    `json:"mode"`
			Source      string    `json:"source"`
			Root        string    `json:"root"`
			Age         string    `json:"age"`
			Fingerprint string    `json:"fingerprint"`
			Complete    bool      `json:"complete"`
			ScannedAt   time.Time `json:"scanned_at,omitempty"`
			Detail      string    `json:"detail,omitempty"`
		}{mode, source, root, age, fingerprint, complete, scannedAt, detail})
	}
	if detail != "" {
		detail = " detail=" + format.SafeText(detail)
	}
	_, err := fmt.Fprintf(
		cmd.ErrOrStderr(),
		"dirstat: query-index mode=%s source=%s root=%s age=%s fingerprint=%s complete=%t%s\n",
		mode, source, format.SafeText(root), age, fingerprint, complete, detail,
	)
	if err != nil {
		return fmt.Errorf("write query-index evidence: %w", err)
	}
	return nil
}

func renderQueryRecords(cmd *cobra.Command, flags queryFlags, fields []queryOutputField, records []querypkg.Record, mode tree.SizeMode) error {
	switch flags.output {
	case outputFormatJSONL:
		if cmd.Flags().Changed("fields") {
			return writeQueryJSONL(cmd.OutOrStdout(), records, fields, mode)
		}
		return querypkg.WriteJSONL(cmd.OutOrStdout(), records)
	case outputFormatNUL:
		return querypkg.WriteNUL(cmd.OutOrStdout(), records)
	default:
		return writeQueryTSV(cmd.OutOrStdout(), records, fields, mode)
	}
}

func validateQueryOwnershipCapability(available bool, flags queryFlags, fields []queryOutputField, sorts []querypkg.SortKey) error {
	if available {
		return nil
	}
	if len(flags.owners) > 0 || len(flags.groups) > 0 {
		return errors.New("owner and group query filters are unavailable on this platform")
	}
	for _, field := range fields {
		switch field.raw {
		case querypkg.FieldOwner, querypkg.FieldGroup, querypkg.FieldUID, querypkg.FieldGID:
			return fmt.Errorf("query field %q is unavailable on this platform", field.name)
		case querypkg.FieldPath, querypkg.FieldRelative, querypkg.FieldName, querypkg.FieldExtension,
			querypkg.FieldKind, querypkg.FieldApparent, querypkg.FieldAllocated, querypkg.FieldFiles,
			querypkg.FieldDirectories, querypkg.FieldMTime, querypkg.FieldMode, querypkg.FieldModeText,
			querypkg.FieldLinks, querypkg.FieldDevice, querypkg.FieldFileID, querypkg.FieldHardlink,
			querypkg.FieldScanError, querypkg.FieldMetadataError:
		}
	}
	for _, key := range sorts {
		if key.Field == querypkg.SortOwner || key.Field == querypkg.SortGroup {
			return fmt.Errorf("query sort %q is unavailable on this platform", key.Field)
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
		case queryFieldSize, queryFieldSizeHuman, "apparent-human", queryFieldAllocatedHuman:
			fields = append(fields, queryOutputField{name: name, sized: name})
		default:
			field, ok := queryRawFields[name]
			if !ok {
				return nil, false, fmt.Errorf("unsupported --fields value %q", name)
			}
			fields = append(fields, queryOutputField{name: name, raw: field})
			switch field {
			case querypkg.FieldPath, querypkg.FieldRelative, querypkg.FieldName, querypkg.FieldExtension,
				querypkg.FieldKind, querypkg.FieldApparent, querypkg.FieldAllocated, querypkg.FieldFiles,
				querypkg.FieldDirectories, querypkg.FieldMTime, querypkg.FieldHardlink, querypkg.FieldScanError:
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
		case queryFieldAllocatedHuman:
			value = record.Allocated
		case queryFieldSize, queryFieldSizeHuman:
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
	case querypkg.FieldPath:
		return record.Path
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
				case queryFieldAllocatedHuman:
					value = record.Allocated
				case queryFieldSize, queryFieldSizeHuman:
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
