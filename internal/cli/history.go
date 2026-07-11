package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

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
	cmd.PersistentFlags().StringVar(&storeDir, "store", "", "history store directory (default: user cache directory)")
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
			if output != "text" && output != "json" {
				return fmt.Errorf("invalid --format %q: expected text or json", output)
			}
			root, policy, fingerprint, err := historyKey(cfg, firstPath(args))
			if err != nil {
				return err
			}
			_ = policy
			store, err := openHistoryStore(*storeDir)
			if err != nil {
				return err
			}
			records, err := store.List(root, fingerprint)
			if err != nil {
				return err
			}
			if output == "json" {
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
	cmd := &cobra.Command{
		Use:   "growth [path]",
		Short: "Record a fresh scan and compare it with the previous snapshot",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "text" && output != "json" {
				return fmt.Errorf("invalid --format %q: expected text or json", output)
			}
			root, policy, fingerprint, err := historyKey(cfg, firstPath(args))
			if err != nil {
				return err
			}
			store, err := openHistoryStore(*storeDir)
			if err != nil {
				return err
			}
			previous, previousErr := store.Previous(root, fingerprint, time.Time{})
			if previousErr != nil && !errors.Is(previousErr, fs.ErrNotExist) {
				return previousErr
			}
			node, stats, err := scan.Scan(cmd.Context(), root, scan.Options{Policy: policy, Concurrency: cfg.Jobs})
			if err != nil {
				return fmt.Errorf("scan history root %q: %w", root, err)
			}
			current := index.FromTree(node, fingerprint, stats.RootFS, stats.Files, stats.Dirs, stats.Errors, time.Now().UTC())
			current.Root = root
			record, err := store.RecordSnapshot(current)
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
			}
			if output == "json" {
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
	return cmd
}

func historyKey(cfg *Config, path string) (string, scope.Policy, string, error) {
	policy, err := cfg.policy()
	if err != nil {
		return "", scope.Policy{}, "", err
	}
	root, err := absolutePath(path)
	if err != nil {
		return "", scope.Policy{}, "", err
	}
	return root, policy, index.Fingerprint(root, policy), nil
}

func openHistoryStore(dir string) (*history.Store, error) {
	if dir == "" {
		return history.NewStore()
	}
	return history.NewStoreAt(dir)
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
