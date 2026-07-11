package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	appconfig "github.com/phillipod/go-dirstat/internal/config"
	"github.com/phillipod/go-dirstat/internal/format"
	"github.com/phillipod/go-dirstat/internal/history"
	"github.com/phillipod/go-dirstat/internal/index"
	"github.com/phillipod/go-dirstat/internal/scan"
	"github.com/phillipod/go-dirstat/internal/scope"
)

func newHistoryCommand(cfg *Config) *cobra.Command {
	var storeDir string
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Record scans and report disk-usage growth",
		Args:  cobra.NoArgs,
	}
	cmd.PersistentFlags().StringVar(&storeDir, "store", "", "history store directory (default: user state directory)")
	cmd.AddCommand(newHistoryListCommand(cfg, &storeDir))
	cmd.AddCommand(newHistoryGrowthCommand(cfg, &storeDir))
	return cmd
}

func newHistoryListCommand(cfg *Config, storeDir *string) *cobra.Command {
	var output string
	var raw bool
	cmd := &cobra.Command{
		Use:   "list [path]",
		Short: "List retained snapshots for a path and scan policy",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != outputFormatText && output != outputFormatJSON {
				return fmt.Errorf("invalid --format %q: expected text or json", output)
			}
			maxRecords, statePolicy, err := configuredHistoryPolicy()
			if err != nil {
				return err
			}
			store, err := openHistoryStore(*storeDir, maxRecords, statePolicy, false)
			if err != nil {
				return err
			}
			root, _, fingerprint, _, err := historyKey(cfg, firstPath(args), store.Dir())
			if err != nil {
				return err
			}
			records, err := store.ListContext(cmd.Context(), root, fingerprint)
			if err != nil {
				return err
			}
			if output == outputFormatJSON {
				if records == nil {
					records = []history.Record{}
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(records)
			}
			if len(records) == 0 {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "No snapshots recorded for this path and scan policy.")
				return err
			}
			for _, record := range records {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s  %s allocated  %s apparent  %d files  %d dirs  %d errors\n",
					record.ScannedAt.Format(time.RFC3339Nano), historySize(record.Allocated, raw),
					historySize(record.Apparent, raw), record.Files, record.Dirs, record.Errors); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&output, "format", "text", "output format: text|json")
	cmd.Flags().BoolVar(&raw, "bytes", false, "print raw bytes instead of human sizes")
	return cmd
}

func newHistoryGrowthCommand(cfg *Config, storeDir *string) *cobra.Command {
	var output string
	var raw bool
	var kind string
	var maxDepth, limit int
	var leafOnly bool
	cmd := &cobra.Command{
		Use:   "growth [path]",
		Short: "Record a fresh scan and compare it with the previous snapshot",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != outputFormatText && output != outputFormatJSON {
				return fmt.Errorf("invalid --format %q: expected text or json", output)
			}
			deltaFilter, err := parseHistoryDeltaFilter(kind, maxDepth, limit, leafOnly)
			if err != nil {
				return err
			}
			maxRecords, statePolicy, err := configuredHistoryPolicy()
			if err != nil {
				return err
			}
			store, err := openHistoryStore(*storeDir, maxRecords, statePolicy, false)
			if err != nil {
				return err
			}
			root, policy, fingerprint, containedStore, err := historyKey(cfg, firstPath(args), store.Dir())
			if err != nil {
				return err
			}
			store, err = openHistoryStore(store.Dir(), maxRecords, statePolicy, true)
			if err != nil {
				return err
			}
			if containedStore && *storeDir != "" {
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "dirstat: excluding history store %s from scan\n", format.SafeText(store.Dir())); err != nil {
					return fmt.Errorf("write history-store warning: %w", err)
				}
			}
			previous, previousErr := store.PreviousContext(cmd.Context(), root, fingerprint, time.Time{})
			if previousErr != nil && !errors.Is(previousErr, fs.ErrNotExist) {
				return previousErr
			}
			node, stats, err := scan.Scan(cmd.Context(), root, scan.Options{Policy: policy, Concurrency: cfg.Jobs})
			if err != nil {
				return fmt.Errorf("scan history root %q: %w", root, err)
			}
			if err := acceptScan(cmd, root, stats, false); err != nil {
				return fmt.Errorf("history refuses to record partial data: %w", err)
			}
			current := index.FromTree(node, fingerprint, stats.RootFS, stats.Files, stats.Dirs, stats.Errors, stats.Complete, time.Now().UTC())
			current.Root = root
			record, err := store.RecordSnapshotContext(cmd.Context(), current)
			if err != nil {
				return err
			}
			baseline := previous == nil
			deltas := make([]history.Delta, 0)
			if !baseline {
				deltas, err = history.Compare(previous, current)
				if err != nil {
					return err
				}
				deltas, err = history.FilterDeltas(deltas, root, deltaFilter)
				if err != nil {
					return err
				}
			}
			if output == outputFormatJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					Baseline bool            `json:"baseline"`
					Current  history.Record  `json:"current"`
					Changes  []history.Delta `json:"changes"`
				}{Baseline: baseline, Current: record, Changes: deltas})
			}
			if baseline {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Recorded baseline for %s at %s.\n", format.SafeText(root), record.ScannedAt.Format(time.RFC3339Nano))
				return err
			}
			if len(deltas) == 0 {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "No measured size changes since the previous snapshot.")
				return err
			}
			for _, delta := range deltas {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s  %s allocated  %s apparent  %s\n",
					delta.Change, signedHistorySize(delta.AllocatedDelta, raw),
					signedHistorySize(delta.ApparentDelta, raw), format.SafeText(delta.Path)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&output, "format", "text", "output format: text|json")
	cmd.Flags().BoolVar(&raw, "bytes", false, "print raw byte deltas instead of human sizes")
	cmd.Flags().StringVar(&kind, "kind", string(history.DeltaKindAll), "include changes for all, file, or directory paths")
	cmd.Flags().IntVar(&maxDepth, "depth", -1, "maximum path depth relative to the root (-1 = unlimited)")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum changes to report (0 = unlimited)")
	cmd.Flags().BoolVar(&leafOnly, "leaf-only", false, "suppress changed paths that have changed descendants")
	return cmd
}

func parseHistoryDeltaFilter(kind string, maxDepth, limit int, leafOnly bool) (history.DeltaFilter, error) {
	filter := history.DeltaFilter{
		Kind:     history.DeltaKind(strings.ToLower(strings.TrimSpace(kind))),
		MaxDepth: maxDepth, Limit: limit, LeafOnly: leafOnly,
	}
	if filter.Kind != history.DeltaKindAll && filter.Kind != history.DeltaKindFile && filter.Kind != history.DeltaKindDirectory {
		return history.DeltaFilter{}, fmt.Errorf("invalid --kind %q: expected all, file, or directory", kind)
	}
	if maxDepth < -1 {
		return history.DeltaFilter{}, fmt.Errorf("--depth must be -1 or greater")
	}
	if limit < 0 {
		return history.DeltaFilter{}, fmt.Errorf("--limit must be zero or greater")
	}
	return filter, nil
}

func historyKey(cfg *Config, path, storeDir string) (string, scope.Policy, string, bool, error) {
	root, err := absolutePath(path)
	if err != nil {
		return "", scope.Policy{}, "", false, err
	}
	visibleRoot := root
	store, err := absolutePath(storeDir)
	if err != nil {
		return "", scope.Policy{}, "", false, fmt.Errorf("resolve history store: %w", err)
	}
	rootResolved := resolvedPath(root)
	storeResolved := resolvedPath(store)
	visibleStoreUnderRoot, visibleRelative := pathContainedBy(visibleRoot, store)
	resolvedStoreUnderRoot, resolvedRelative := pathContainedBy(rootResolved, storeResolved)
	visibleRootUnderStore, _ := pathContainedBy(store, visibleRoot)
	resolvedRootUnderStore, _ := pathContainedBy(storeResolved, rootResolved)
	if (visibleStoreUnderRoot && visibleRelative == ".") ||
		(resolvedStoreUnderRoot && resolvedRelative == ".") ||
		(visibleRootUnderStore && !visibleStoreUnderRoot) ||
		(resolvedRootUnderStore && !resolvedStoreUnderRoot) {
		return "", scope.Policy{}, "", false, errors.New("history store must not be the scan root or contain the scan root")
	}
	contained := visibleStoreUnderRoot || resolvedStoreUnderRoot

	// A store outside the measured root cannot affect the scan and must not
	// change its fingerprint. Only add the store's visible/canonical forms when
	// it overlaps the root; otherwise source and destination history stores
	// share the same key and migration remains queryable after relocation.
	scanCfg := *cfg
	scanCfg.ExcludePath = append([]string(nil), cfg.ExcludePath...)
	if contained {
		scanCfg.ExcludePath = append(scanCfg.ExcludePath, store, storeResolved)
		if visibleStoreUnderRoot && visibleRelative != "." {
			scanCfg.ExcludePath = append(scanCfg.ExcludePath, filepath.Join(root, visibleRelative))
		}
		if resolvedStoreUnderRoot && resolvedRelative != "." {
			scanCfg.ExcludePath = append(scanCfg.ExcludePath, filepath.Join(root, resolvedRelative))
		}
	}
	policy, err := scanCfg.policy()
	if err != nil {
		return "", scope.Policy{}, "", false, err
	}
	// Keep the user-visible absolute root in records and delta paths, but use a
	// resolved identity for the fingerprint where aliases are known to exist.
	// macOS /var -> /private/var otherwise splits otherwise identical history
	// keys. On Windows, EvalSymlinks may alternate between short and long
	// spellings on profile paths, so the stable absolute spelling is used for
	// both display and fingerprint while resolved paths still drive exclusions.
	fingerprintRoot := root
	if runtime.GOOS != windowsOS {
		fingerprintRoot = rootResolved
	}
	return visibleRoot, policy, index.Fingerprint(fingerprintRoot, policy), contained, nil
}

func resolvedPath(path string) string {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return filepath.Clean(path)
	}
	current := abs
	var missing []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved)
		}
		if !errors.Is(resolveErr, fs.ErrNotExist) {
			return filepath.Clean(path)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(path)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func pathContainedBy(parent, child string) (bool, string) {
	relative, err := filepath.Rel(parent, child)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false, ""
	}
	return true, relative
}

func configuredHistoryPolicy() (int, history.Policy, error) {
	cfg, err := appconfig.Load()
	if err != nil {
		return 0, history.Policy{}, fmt.Errorf("load config: %w", err)
	}
	return cfg.HistoryMax, history.Policy{
		MaxBytes: cfg.State.HistoryMaxBytes,
		MaxAge:   time.Duration(cfg.State.HistoryTTLDays) * 24 * time.Hour,
	}, nil
}

func openHistoryStore(dir string, maxRecords int, policy history.Policy, create bool) (*history.Store, error) {
	if dir == "" {
		defaultDir, err := history.DefaultStoreDir()
		if err != nil {
			return nil, err
		}
		if create {
			return history.NewStoreAtWithPolicy(defaultDir, maxRecords, policy)
		}
		return history.OpenStoreAtWithPolicy(defaultDir, maxRecords, policy)
	}
	if create {
		return history.NewStoreAtWithPolicy(dir, maxRecords, policy)
	}
	return history.OpenStoreAtWithPolicy(dir, maxRecords, policy)
}

func firstPath(args []string) string {
	if len(args) == 0 {
		return "."
	}
	return filepath.Clean(args[0])
}

func historySize(value int64, raw bool) string {
	if raw {
		return fmt.Sprintf("%dB", value)
	}
	return format.Bytes(value)
}

func signedHistorySize(value int64, raw bool) string {
	sign := "+"
	abs := value
	if value < 0 {
		sign, abs = "-", -value
	}
	return sign + historySize(abs, raw)
}
