package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	appconfig "github.com/phillipod/go-dirstat/internal/config"
	"github.com/phillipod/go-dirstat/internal/format"
	"github.com/phillipod/go-dirstat/internal/history"
	"github.com/phillipod/go-dirstat/internal/index"
	"github.com/phillipod/go-dirstat/internal/storefs"
)

const (
	stateKindAll     = "all"
	stateKindIndex   = "index"
	stateKindHistory = "history"
)

type stateOptions struct {
	output       string
	kind         string
	cacheStore   string
	historyStore string
}

type stateStores struct {
	cache         *index.Store
	history       *history.Store
	cacheDir      string
	historyDir    string
	cacheErr      error
	historyErr    error
	cachePolicy   index.Policy
	historyPolicy history.Policy
}

type stateEntry struct {
	Kind        string    `json:"kind"`
	Store       string    `json:"store"`
	ID          string    `json:"id"`
	Root        string    `json:"root,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	ScannedAt   time.Time `json:"scanned_at,omitempty"`
	ModifiedAt  time.Time `json:"modified_at"`
	SizeBytes   int64     `json:"size_bytes"`
	Complete    bool      `json:"complete"`
	Valid       bool      `json:"valid"`
	Safe        bool      `json:"safe"`
	Issue       string    `json:"issue,omitempty"`
}

type stateSummary struct {
	Kind              string `json:"kind"`
	Store             string `json:"store"`
	Exists            bool   `json:"exists"`
	Entries           int    `json:"entries"`
	Valid             int    `json:"valid"`
	Unsafe            int    `json:"unsafe"`
	SizeBytes         int64  `json:"size_bytes"`
	SizeScope         string `json:"size_scope"`
	MaxBytes          int64  `json:"max_bytes"`
	TTLSeconds        int64  `json:"ttl_seconds"`
	Owned             bool   `json:"owned"`
	OwnershipIssue    string `json:"ownership_issue,omitempty"`
	ManagedBytes      int64  `json:"managed_bytes"`
	UnmanagedBytes    int64  `json:"unmanaged_bytes"`
	SizeComplete      bool   `json:"size_complete"`
	Managed           bool   `json:"managed"`
	Safe              bool   `json:"safe"`
	InventoryComplete bool   `json:"inventory_complete"`
	Issue             string `json:"issue,omitempty"`
}

type stateAction struct {
	Kind           string     `json:"kind"`
	Store          string     `json:"store"`
	Entry          stateEntry `json:"entry"`
	Reason         string     `json:"reason"`
	DryRun         bool       `json:"dry_run"`
	Removed        bool       `json:"removed"`
	MayHaveMutated bool       `json:"may_have_mutated,omitempty"`
	Error          string     `json:"error,omitempty"`
}

func newStateCommand() *cobra.Command {
	opts := &stateOptions{output: outputFormatText, kind: stateKindAll}
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Inspect and safely maintain cache and history state",
		Long: `Inspect and safely maintain dirstat's private cache and durable history.

Status, list, and size never create state. Prune, clear, and migrate require
exactly one of --dry-run or --yes; no interactive prompt makes the contract
safe for scripts and automation. Symlinked stores and entries are never
followed, and foreign entries are reported but not deleted.

Reported size_bytes uses size_scope=policy_payload: it is the logical byte
length of inventory entries governed by TTL and quota policy. Ownership
markers, lock files, and directory metadata are intentionally excluded.`,
		Args: cobra.NoArgs,
	}
	cmd.PersistentFlags().StringVar(&opts.output, "format", outputFormatText, "output format: text|json")
	cmd.PersistentFlags().StringVar(&opts.kind, "kind", stateKindAll, "state kind: all|index|history")
	cmd.PersistentFlags().StringVar(&opts.cacheStore, "cache-store", "", "cache store directory (default: platform user cache)")
	cmd.PersistentFlags().StringVar(&opts.historyStore, "history-store", "", "history store directory (default: platform user state)")
	cmd.AddCommand(newStateStatusCommand(opts))
	cmd.AddCommand(newStateListCommand(opts))
	cmd.AddCommand(newStateSizeCommand(opts))
	cmd.AddCommand(newStateMutationCommand(opts, "prune"))
	cmd.AddCommand(newStateMutationCommand(opts, "clear"))
	cmd.AddCommand(newStateMigrateCommand(opts))
	return cmd
}

func newStateStatusCommand(opts *stateOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report store existence, policy, validity, and size",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			stores, err := openStateStores(opts, false, true)
			if err != nil {
				return err
			}
			summaries, _, err := collectState(cmd.Context(), stores, opts.kind)
			if err != nil {
				return err
			}
			if opts.output == outputFormatJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(summaries)
			}
			for _, summary := range summaries {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\texists=%t\towned=%t\tmanaged=%t\tsafe=%t\tinventory_complete=%t\tentries=%d\tvalid=%d\tunsafe=%d\tbytes=%d\tsize_scope=%s\tmanaged_bytes=%d\tunmanaged_bytes=%d\tsize_complete=%t\tmax_bytes=%d\tttl_seconds=%d\t%s\n",
					summary.Kind, format.SafeText(summary.Store), summary.Exists, summary.Owned, summary.Managed, summary.Safe, summary.InventoryComplete, summary.Entries,
					summary.Valid, summary.Unsafe, summary.SizeBytes, summary.SizeScope, summary.ManagedBytes, summary.UnmanagedBytes,
					summary.SizeComplete, summary.MaxBytes, summary.TTLSeconds, format.SafeText(summary.Issue)); err != nil {
					return fmt.Errorf("write state status: %w", err)
				}
			}
			return nil
		},
	}
}

func newStateListCommand(opts *stateOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List valid, corrupt, and unsafe store entries",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			stores, err := openStateStores(opts, false, true)
			if err != nil {
				return err
			}
			_, entries, err := collectState(cmd.Context(), stores, opts.kind)
			if err != nil {
				return err
			}
			if opts.output == outputFormatJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(entries)
			}
			for _, entry := range entries {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%d\tvalid=%t\tsafe=%t\tcomplete=%t\t%s\t%s\t%s\n",
					entry.Kind, format.SafeText(entry.ID), entry.SizeBytes, entry.Valid, entry.Safe, entry.Complete,
					format.SafeText(entry.Root), format.SafeText(entry.Fingerprint), format.SafeText(entry.Issue)); err != nil {
					return fmt.Errorf("write state list: %w", err)
				}
			}
			return nil
		},
	}
}

func newStateSizeCommand(opts *stateOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "size",
		Short: "Report policy-accounted payload bytes without creating state",
		Long: `Report policy-accounted payload bytes without creating state.

The policy_payload scope excludes ownership markers, lock files, and directory
metadata; it matches the bytes considered by TTL and quota enforcement.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			stores, err := openStateStores(opts, false, true)
			if err != nil {
				return err
			}
			summaries, _, err := collectState(cmd.Context(), stores, opts.kind)
			if err != nil {
				return err
			}
			if opts.output == outputFormatJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(summaries)
			}
			for _, summary := range summaries {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%d\tscope=%s\t%s\n", summary.Kind, summary.SizeBytes, summary.SizeScope, format.SafeText(summary.Store)); err != nil {
					return fmt.Errorf("write state size: %w", err)
				}
			}
			return nil
		},
	}
}

func newStateMutationCommand(opts *stateOptions, operation string) *cobra.Command {
	var dryRun, yes bool
	cmd := &cobra.Command{
		Use:   operation,
		Short: map[string]string{"prune": "Apply global TTL and quota policy", "clear": "Remove all safely owned state entries"}[operation],
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateStateMutation(opts, dryRun, yes); err != nil {
				return err
			}
			stores, err := openStateStores(opts, false, false)
			if err != nil {
				return err
			}
			actions, mutationErr := runStateMutation(cmd.Context(), stores, opts.kind, operation, dryRun)
			if err := renderStateActions(cmd, opts.output, actions); err != nil {
				return err
			}
			return mutationErr
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report deterministic actions without changing state")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm state removal without an interactive prompt")
	return cmd
}

func newStateMigrateCommand(opts *stateOptions) *cobra.Command {
	var dryRun, yes bool
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Move legacy history and invalidate incompatible cache formats",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateStateMutation(opts, dryRun, yes); err != nil {
				return err
			}
			stores, err := openStateStores(opts, true, false)
			if err != nil {
				return err
			}
			actions, migrationErr := runStateMigration(cmd.Context(), stores, opts.kind, dryRun)
			sortStateActions(actions)
			if err := renderStateActions(cmd, opts.output, actions); err != nil {
				return err
			}
			return migrationErr
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report migration and invalidation without changing state")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm migration and invalidation without an interactive prompt")
	return cmd
}

func runStateMigration(ctx context.Context, stores stateStores, kind string, dryRun bool) ([]stateAction, error) {
	var actions []stateAction
	indexAdopt, historyAdopt, historyInitialize, legacyAdopt := false, false, false, false
	var legacy *history.Store

	// Validate the entire requested transaction before the first mutation.
	if kind == stateKindAll || kind == stateKindIndex {
		var err error
		indexAdopt, err = stores.cache.AdoptContext(ctx, true)
		if err != nil {
			return actions, fmt.Errorf("validate cache adoption: %w", err)
		}
		if indexAdopt {
			actions = append(actions, adoptionAction(stateKindIndex, stores.cache.Dir(), dryRun))
		}
		preview, err := stores.cache.PreviewInvalidationAfterAdoption(ctx)
		if err != nil {
			return actions, fmt.Errorf("preview cache invalidation: %w", err)
		}
		actions = append(actions, convertIndexActions(stores.cache.Dir(), preview, dryRun)...)
	}
	if kind == stateKindAll || kind == stateKindHistory {
		var err error
		historyExists, err := storefs.CheckDir(stores.history.Dir())
		if err != nil {
			return actions, fmt.Errorf("validate history destination: %w", err)
		}
		historyAdopt, err = stores.history.AdoptContext(ctx, true)
		if err != nil {
			return actions, fmt.Errorf("validate history adoption: %w", err)
		}
		if historyAdopt {
			actions = append(actions, adoptionAction(stateKindHistory, stores.history.Dir(), dryRun))
		}
		legacyDir := filepath.Join(stores.cacheDir, "history")
		legacy, err = history.OpenStoreAtWithPolicy(legacyDir, history.MaxRecords, stores.history.Policy())
		if err != nil {
			return actions, fmt.Errorf("open legacy history store: %w", err)
		}
		legacyAdopt, err = legacy.AdoptContext(ctx, true)
		if err != nil {
			return actions, fmt.Errorf("validate legacy history ownership: %w", err)
		}
		if legacyAdopt {
			actions = append(actions, adoptionAction(stateKindHistory, legacy.Dir(), dryRun))
		}
		preview, err := legacy.MigrateToContext(ctx, stores.history, true)
		if err != nil {
			return actions, fmt.Errorf("preview legacy history migration: %w", err)
		}
		historyInitialize = !historyExists && len(preview) > 0
		if historyInitialize {
			actions = append(actions, initializationAction(stateKindHistory, stores.history.Dir(), dryRun))
		}
		actions = append(actions, convertHistoryActions(legacy.Dir(), preview, dryRun)...)
	}
	if dryRun {
		return actions, nil
	}

	// Apply history first; cache invalidation can include the legacy parent.
	actions = actions[:0]
	if kind == stateKindAll || kind == stateKindHistory {
		if historyInitialize {
			actions = append(actions, initializationAction(stateKindHistory, stores.history.Dir(), false))
		}
		if historyAdopt {
			if _, err := stores.history.AdoptContext(ctx, false); err != nil {
				return actions, fmt.Errorf("adopt history store: %w", err)
			}
			actions = append(actions, adoptionAction(stateKindHistory, stores.history.Dir(), false))
		}
		if legacyAdopt {
			if _, err := legacy.AdoptContext(ctx, false); err != nil {
				return actions, fmt.Errorf("adopt legacy history store: %w", err)
			}
			actions = append(actions, adoptionAction(stateKindHistory, legacy.Dir(), false))
		}
		migrated, err := legacy.MigrateToContext(ctx, stores.history, false)
		actions = append(actions, convertHistoryActions(legacy.Dir(), migrated, false)...)
		if err != nil {
			return actions, fmt.Errorf("migrate legacy history: %w", err)
		}
	}
	if kind == stateKindAll || kind == stateKindIndex {
		if indexAdopt {
			if _, err := stores.cache.AdoptContext(ctx, false); err != nil {
				return actions, fmt.Errorf("adopt cache store: %w", err)
			}
			actions = append(actions, adoptionAction(stateKindIndex, stores.cache.Dir(), false))
		}
		invalidated, err := stores.cache.InvalidateContext(ctx, false)
		actions = append(actions, convertIndexActions(stores.cache.Dir(), invalidated, false)...)
		if err != nil {
			return actions, fmt.Errorf("invalidate cache: %w", err)
		}
	}
	return actions, nil
}

func adoptionAction(kind, store string, dryRun bool) stateAction {
	return stateAction{
		Kind: kind, Store: store, Reason: "adopt", DryRun: dryRun,
		Entry: stateEntry{Kind: kind, Store: store, ID: ".dirstat-store", Safe: true},
	}
}

func initializationAction(kind, store string, dryRun bool) stateAction {
	return stateAction{
		Kind: kind, Store: store, Reason: "initialize", DryRun: dryRun,
		Entry: stateEntry{Kind: kind, Store: store, ID: ".dirstat-store", Safe: true},
	}
}

func openStateStores(opts *stateOptions, needLegacyHistorySource, tolerateInventoryErrors bool) (stateStores, error) {
	if err := validateStateOptions(opts); err != nil {
		return stateStores{}, err
	}
	cfg, err := appconfig.Load()
	if err != nil {
		return stateStores{}, fmt.Errorf("load config: %w", err)
	}
	needCacheStore := opts.kind == stateKindAll || opts.kind == stateKindIndex
	needCachePath := needCacheStore || (needLegacyHistorySource && opts.kind == stateKindHistory)
	needHistoryStore := opts.kind == stateKindAll || opts.kind == stateKindHistory
	var cachePathErr, historyPathErr error
	cacheDir := opts.cacheStore
	if needCachePath && cacheDir == "" {
		cacheDir, err = index.DefaultStoreDir()
		if err != nil {
			cachePathErr = err
			if !tolerateInventoryErrors {
				return stateStores{}, err
			}
		}
	}
	historyDir := opts.historyStore
	if needHistoryStore && historyDir == "" {
		historyDir, err = history.DefaultStoreDir()
		if err != nil {
			historyPathErr = err
			if !tolerateInventoryErrors {
				return stateStores{}, err
			}
		}
	}
	cachePolicy := index.Policy{MaxBytes: cfg.State.CacheMaxBytes, MaxAge: time.Duration(cfg.State.CacheTTLHours) * time.Hour}
	historyPolicy := history.Policy{MaxBytes: cfg.State.HistoryMaxBytes, MaxAge: time.Duration(cfg.State.HistoryTTLDays) * 24 * time.Hour}
	stores := stateStores{cacheDir: cacheDir, historyDir: historyDir, cachePolicy: cachePolicy, historyPolicy: historyPolicy}
	if needCacheStore {
		if cacheDir == "" {
			stores.cacheErr = cachePathErr
			if stores.cacheErr == nil {
				stores.cacheErr = errors.New("cache store path is unavailable")
			}
		} else {
			stores.cache, err = index.OpenStoreAtWithPolicy(cacheDir, cachePolicy)
			if err != nil {
				stores.cacheErr = fmt.Errorf("open cache store: %w", err)
			}
		}
		if stores.cacheErr != nil && !tolerateInventoryErrors {
			return stateStores{}, stores.cacheErr
		}
	}
	if needHistoryStore {
		if historyDir == "" {
			stores.historyErr = historyPathErr
			if stores.historyErr == nil {
				stores.historyErr = errors.New("history store path is unavailable")
			}
		} else {
			stores.history, err = history.OpenStoreAtWithPolicy(historyDir, cfg.HistoryMax, historyPolicy)
			if err != nil {
				stores.historyErr = fmt.Errorf("open history store: %w", err)
			}
		}
		if stores.historyErr != nil && !tolerateInventoryErrors {
			return stateStores{}, stores.historyErr
		}
	}
	return stores, nil
}

func validateStateOptions(opts *stateOptions) error {
	if opts.output != outputFormatText && opts.output != outputFormatJSON {
		return fmt.Errorf("invalid --format %q: expected text or json", opts.output)
	}
	switch opts.kind {
	case stateKindAll, stateKindIndex, stateKindHistory:
		return nil
	default:
		return fmt.Errorf("invalid --kind %q: expected all, index, or history", opts.kind)
	}
}

func validateStateMutation(opts *stateOptions, dryRun, yes bool) error {
	if err := validateStateOptions(opts); err != nil {
		return err
	}
	if dryRun == yes {
		return errors.New("state mutation requires exactly one of --dry-run or --yes")
	}
	return nil
}

func collectState(ctx context.Context, stores stateStores, kind string) ([]stateSummary, []stateEntry, error) {
	summaries := make([]stateSummary, 0, 2)
	entries := make([]stateEntry, 0)
	if kind == stateKindAll || kind == stateKindIndex {
		if stores.cacheErr != nil {
			summary, entry := failedStateInventory(stateKindIndex, stores.cacheDir, stores.cachePolicy.MaxBytes, stores.cachePolicy.MaxAge, stores.cacheErr)
			summaries = append(summaries, summary)
			entries = append(entries, entry)
		} else {
			indexEntries, err := stores.cache.ListContext(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil, nil, err
				}
				summary, entry := failedStateInventory(stateKindIndex, stores.cache.Dir(), stores.cache.Policy().MaxBytes, stores.cache.Policy().MaxAge, fmt.Errorf("list cache store: %w", err))
				summaries = append(summaries, summary)
				entries = append(entries, entry)
			} else {
				converted := convertIndexEntries(stores.cache.Dir(), indexEntries)
				exists, checkErr := storefs.CheckDir(stores.cache.Dir())
				if checkErr != nil {
					summary, entry := failedStateInventory(stateKindIndex, stores.cache.Dir(), stores.cache.Policy().MaxBytes, stores.cache.Policy().MaxAge, checkErr)
					summaries = append(summaries, summary)
					entries = append(entries, entry)
				} else {
					owned, issue := stores.cache.Owned()
					summaries = append(summaries, summarizeState(stateKindIndex, stores.cache.Dir(), exists, owned, issue, converted, stores.cache.Policy().MaxBytes, stores.cache.Policy().MaxAge))
					entries = append(entries, converted...)
				}
			}
		}
	}
	if kind == stateKindAll || kind == stateKindHistory {
		if stores.historyErr != nil {
			summary, entry := failedStateInventory(stateKindHistory, stores.historyDir, stores.historyPolicy.MaxBytes, stores.historyPolicy.MaxAge, stores.historyErr)
			summaries = append(summaries, summary)
			entries = append(entries, entry)
		} else {
			historyEntries, err := stores.history.ListEntriesContext(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil, nil, err
				}
				summary, entry := failedStateInventory(stateKindHistory, stores.history.Dir(), stores.history.Policy().MaxBytes, stores.history.Policy().MaxAge, fmt.Errorf("list history store: %w", err))
				summaries = append(summaries, summary)
				entries = append(entries, entry)
			} else {
				converted := convertHistoryEntries(stores.history.Dir(), historyEntries)
				exists, checkErr := storefs.CheckDir(stores.history.Dir())
				if checkErr != nil {
					summary, entry := failedStateInventory(stateKindHistory, stores.history.Dir(), stores.history.Policy().MaxBytes, stores.history.Policy().MaxAge, checkErr)
					summaries = append(summaries, summary)
					entries = append(entries, entry)
				} else {
					policy := stores.history.Policy()
					owned, issue := stores.history.Owned()
					summaries = append(summaries, summarizeState(stateKindHistory, stores.history.Dir(), exists, owned, issue, converted, policy.MaxBytes, policy.MaxAge))
					entries = append(entries, converted...)
				}
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Kind == entries[j].Kind {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Kind < entries[j].Kind
	})
	return summaries, entries, nil
}

func failedStateInventory(kind, store string, maxBytes int64, ttl time.Duration, inventoryErr error) (stateSummary, stateEntry) {
	exists := false
	if store != "" {
		_, err := os.Lstat(store)
		exists = err == nil || !errors.Is(err, fs.ErrNotExist)
	}
	issue := inventoryErr.Error()
	summary := stateSummary{
		Kind: kind, Store: store, Exists: exists, SizeScope: "policy_payload",
		MaxBytes: maxBytes, TTLSeconds: int64(ttl / time.Second),
		Safe: false, Managed: false, InventoryComplete: false, SizeComplete: false,
		OwnershipIssue: issue, Issue: issue, Entries: 1, Unsafe: 1,
	}
	entry := stateEntry{Kind: kind, Store: store, ID: ".", Safe: false, Issue: issue}
	return summary, entry
}

func summarizeState(kind, store string, exists, owned bool, ownershipIssue string, entries []stateEntry, maxBytes int64, ttl time.Duration) stateSummary {
	summary := stateSummary{
		Kind: kind, Store: store, Exists: exists, Owned: owned, OwnershipIssue: ownershipIssue,
		Entries: len(entries), MaxBytes: maxBytes, TTLSeconds: int64(ttl / time.Second),
		SizeScope: "policy_payload", SizeComplete: true, Managed: owned,
		Safe: !exists || owned, InventoryComplete: true, Issue: ownershipIssue,
	}
	for _, entry := range entries {
		var complete bool
		summary.SizeBytes, complete = addStateBytes(summary.SizeBytes, entry.SizeBytes)
		if !complete {
			summary.SizeComplete = false
		}
		if entry.Valid {
			summary.Valid++
		}
		if !entry.Safe {
			summary.Unsafe++
			summary.Safe = false
			summary.UnmanagedBytes, complete = addStateBytes(summary.UnmanagedBytes, entry.SizeBytes)
			if !complete {
				summary.SizeComplete = false
			}
			summary.SizeComplete = false
		} else {
			summary.ManagedBytes, complete = addStateBytes(summary.ManagedBytes, entry.SizeBytes)
			if !complete {
				summary.SizeComplete = false
			}
		}
	}
	return summary
}

func addStateBytes(total, value int64) (int64, bool) {
	const maxInt64 = int64(^uint64(0) >> 1)
	if value < 0 || total < 0 {
		return total, false
	}
	if value > maxInt64-total {
		return maxInt64, false
	}
	return total + value, true
}

func runStateMutation(ctx context.Context, stores stateStores, kind, operation string, dryRun bool) ([]stateAction, error) {
	actions := make([]stateAction, 0)
	if kind == stateKindAll || kind == stateKindIndex {
		var raw []index.Action
		var err error
		if operation == "prune" {
			raw, err = stores.cache.PruneContext(ctx, dryRun)
		} else {
			raw, err = stores.cache.ClearContext(ctx, dryRun)
		}
		if err != nil {
			return actions, fmt.Errorf("%s cache store: %w", operation, err)
		}
		actions = append(actions, convertIndexActions(stores.cache.Dir(), raw, dryRun)...)
	}
	if kind == stateKindAll || kind == stateKindHistory {
		var raw []history.Action
		var err error
		if operation == "prune" {
			raw, err = stores.history.PruneContext(ctx, dryRun)
		} else {
			raw, err = stores.history.ClearContext(ctx, dryRun)
		}
		if err != nil {
			return actions, fmt.Errorf("%s history store: %w", operation, err)
		}
		actions = append(actions, convertHistoryActions(stores.history.Dir(), raw, dryRun)...)
	}
	sortStateActions(actions)
	return actions, nil
}

func sortStateActions(actions []stateAction) {
	sort.Slice(actions, func(i, j int) bool {
		left, right := actions[i], actions[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.Store != right.Store {
			return left.Store < right.Store
		}
		if left.Entry.ID != right.Entry.ID {
			return left.Entry.ID < right.Entry.ID
		}
		return left.Reason < right.Reason
	})
}

func convertIndexEntries(store string, entries []index.Entry) []stateEntry {
	result := make([]stateEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, stateEntry{
			Kind: stateKindIndex, Store: store, ID: entry.ID, Root: entry.Root,
			Fingerprint: entry.Fingerprint, ScannedAt: entry.ScannedAt, ModifiedAt: entry.ModifiedAt,
			SizeBytes: entry.SizeBytes, Complete: entry.Complete, Valid: entry.Valid, Safe: entry.Safe, Issue: entry.Issue,
		})
	}
	return result
}

func convertHistoryEntries(store string, entries []history.Entry) []stateEntry {
	result := make([]stateEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, stateEntry{
			Kind: stateKindHistory, Store: store, ID: entry.ID, Root: entry.Root,
			Fingerprint: entry.Fingerprint, ScannedAt: entry.ScannedAt, ModifiedAt: entry.ModifiedAt,
			SizeBytes: entry.SizeBytes, Complete: entry.Complete, Valid: entry.Valid, Safe: entry.Safe, Issue: entry.Issue,
		})
	}
	return result
}

func convertIndexActions(store string, actions []index.Action, dryRun bool) []stateAction {
	result := make([]stateAction, 0, len(actions))
	for _, action := range actions {
		entry := convertIndexEntries(store, []index.Entry{action.Entry})[0]
		result = append(result, stateAction{Kind: stateKindIndex, Store: store, Entry: entry, Reason: action.Reason, DryRun: dryRun, Removed: action.Removed, MayHaveMutated: action.MayHaveMutated, Error: action.Error})
	}
	return result
}

func convertHistoryActions(store string, actions []history.Action, dryRun bool) []stateAction {
	result := make([]stateAction, 0, len(actions))
	for _, action := range actions {
		entry := convertHistoryEntries(store, []history.Entry{action.Entry})[0]
		result = append(result, stateAction{Kind: stateKindHistory, Store: store, Entry: entry, Reason: action.Reason, DryRun: dryRun, Removed: action.Removed, MayHaveMutated: action.MayHaveMutated, Error: action.Error})
	}
	return result
}

func renderStateActions(cmd *cobra.Command, output string, actions []stateAction) error {
	if actions == nil {
		actions = []stateAction{}
	}
	if output == outputFormatJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(actions)
	}
	for _, action := range actions {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\tdry_run=%t\tremoved=%t\tmay_have_mutated=%t\t%s\n",
			action.Kind, format.SafeText(action.Entry.ID), action.Reason, action.DryRun, action.Removed, action.MayHaveMutated, format.SafeText(action.Error)); err != nil {
			return fmt.Errorf("write state action: %w", err)
		}
	}
	return nil
}
