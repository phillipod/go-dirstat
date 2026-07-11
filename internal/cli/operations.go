package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	appconfig "github.com/phillipod/go-dirstat/internal/config"
	"github.com/phillipod/go-dirstat/internal/fsops"
)

type planFlags struct {
	root, output, mode, size, format           string
	files0From, operationsFrom, destinationDir string
	sources                                    []string
	uid, gid                                   int
	recursive, summary                         bool
}

func newPlanCommand() *cobra.Command {
	flags := planFlags{root: ".", output: "-", uid: -1, gid: -1}
	cmd := &cobra.Command{
		Use:   "plan [ACTION [SOURCE [DESTINATION]]]",
		Short: "Create a guarded filesystem operation plan",
		Long: `Create a guarded filesystem operation plan.

The positional ACTION SOURCE [DESTINATION] form creates one operation. For a
uniform batch, use repeatable --source or bounded NUL input with --files0-from.
Mixed actions use strict request-only JSONL with --operations-from. All inputs
are normalized, guarded, and prevalidated before plan JSONL is emitted.

Windows rejects chmod, chown, and explicit POSIX modes before a plan can be
applied; default mkdir and touch behavior remains available.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			built, err := buildOperationPlanDetailed(cmd, flags, args)
			if err != nil {
				return err
			}
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			if err := writeOperationPlan(cmd, flags.output, built.plan); err != nil {
				return err
			}
			if flags.summary {
				return writePlanSummary(cmd, built.summary)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&flags.root, "root", ".", "confine all operation paths to this directory")
	f.StringVarP(&flags.output, "output", "o", "-", "write JSONL plan to this file (- for stdout)")
	f.StringVar(&flags.mode, "mode", "", "POSIX octal mode for mkdir, touch, or chmod (unsupported on Windows)")
	f.StringVar(&flags.size, "size", "", "target size for truncate (bytes or K/M/G/T/P/E suffix)")
	f.IntVar(&flags.uid, "uid", -1, "numeric owner ID for chown (unsupported on Windows)")
	f.IntVar(&flags.gid, "gid", -1, "numeric group ID for chown (unsupported on Windows)")
	f.StringVar(&flags.format, "archive-format", "", "archive format: tar|tar.gz|zip (otherwise inferred)")
	f.BoolVarP(&flags.recursive, "recursive", "r", false, "allow cancellable recursive directory deletion")
	f.StringArrayVar(&flags.sources, "source", nil, "source path for a uniform action (repeatable)")
	f.StringVar(&flags.files0From, "files0-from", "", "read NUL-terminated source paths from FILE (- for stdin; maximum 64 MiB)")
	f.StringVar(&flags.operationsFrom, "operations-from", "", "read strict operation-request JSONL from FILE (- for stdin; maximum 64 MiB)")
	f.StringVar(&flags.destinationDir, "destination-dir", "", "destination directory for batch copy, move, or rename")
	f.BoolVar(&flags.summary, "summary", false, "write aggregate plan-impact JSON to stderr")
	return cmd
}

func parseAction(value string) (fsops.Action, error) {
	action := fsops.Action(strings.ToLower(strings.TrimSpace(value)))
	switch action {
	case fsops.ActionDelete, fsops.ActionCopy, fsops.ActionMove, fsops.ActionRename,
		fsops.ActionMkdir, fsops.ActionTouch, fsops.ActionTruncate, fsops.ActionChmod,
		fsops.ActionChown, fsops.ActionArchive, fsops.ActionExtract:
		return action, nil
	default:
		return "", fmt.Errorf("unsupported action %q: expected delete, copy, move, rename, mkdir, touch, truncate, chmod, chown, archive, or extract", value)
	}
}

func validatePlanFlags(cmd *cobra.Command, action fsops.Action, op fsops.Operation) error {
	if action == fsops.ActionTruncate && op.Size == nil {
		return errors.New("truncate requires --size")
	}
	if action == fsops.ActionChmod && op.Mode == nil {
		return errors.New("chmod requires --mode")
	}
	if action == fsops.ActionChown && op.UID == nil && op.GID == nil {
		return errors.New("chown requires --uid or --gid")
	}
	if cmd.Flags().Changed("size") && action != fsops.ActionTruncate {
		return fmt.Errorf("--size cannot be used with %s", action)
	}
	if cmd.Flags().Changed("mode") && action != fsops.ActionMkdir && action != fsops.ActionTouch && action != fsops.ActionChmod {
		return fmt.Errorf("--mode cannot be used with %s", action)
	}
	if (cmd.Flags().Changed("uid") || cmd.Flags().Changed("gid")) && action != fsops.ActionChown {
		return fmt.Errorf("--uid and --gid cannot be used with %s", action)
	}
	if cmd.Flags().Changed("archive-format") && action != fsops.ActionArchive && action != fsops.ActionExtract {
		return fmt.Errorf("--archive-format cannot be used with %s", action)
	}
	if cmd.Flags().Changed("recursive") && action != fsops.ActionDelete {
		return fmt.Errorf("--recursive cannot be used with %s", action)
	}
	return nil
}

func canonicalOperationRoot(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("operation root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("operation root %q: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("operation root %q is not a directory", abs)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve operation root %q: %w", abs, err)
	}
	return resolved, nil
}

func parseOperationMode(value string) (uint32, error) {
	original := value
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "0o")
	if value == "" {
		return 0, errors.New("--mode requires an octal value")
	}
	parsed, err := strconv.ParseUint(value, 8, 32)
	if err != nil || parsed > 0o7777 {
		return 0, fmt.Errorf("invalid --mode %q: expected an octal permission mode up to 7777", original)
	}
	return uint32(parsed), nil
}

func writeOperationPlan(cmd *cobra.Command, destination string, plan fsops.Plan) (err error) {
	var encoded bytes.Buffer
	if err := fsops.WritePlan(&encoded, plan); err != nil {
		return err
	}
	if encoded.Len() > int(fsops.MaxPlanBytes) {
		return fmt.Errorf("generated plan exceeds maximum size of %d bytes", fsops.MaxPlanBytes)
	}
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	if destination == "-" {
		written, writeErr := cmd.OutOrStdout().Write(encoded.Bytes())
		if writeErr != nil {
			return writeErr
		}
		if written != encoded.Len() {
			return io.ErrShortWrite
		}
		return nil
	}
	if strings.TrimSpace(destination) == "" {
		return errors.New("--output must not be empty")
	}
	f, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create plan: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(destination)
		}
	}()
	written, err := f.Write(encoded.Bytes())
	if err != nil {
		return err
	}
	if written != encoded.Len() {
		return io.ErrShortWrite
	}
	return f.Sync()
}

func newApplyCommand() *cobra.Command {
	var dryRun, yes, noAudit bool
	var conflict, auditPath string
	cmd := &cobra.Command{
		Use:   "apply PLAN",
		Short: "Validate and apply a filesystem operation plan",
		Long: `Validate and apply a complete filesystem operation plan.

PLAN is strict JSONL with exactly one object per physical line and a maximum
size of 64 MiB. Unknown fields, duplicate keys or operation IDs, trailing JSON,
and invalid operation dependencies are rejected before any mutation. Owned
audit logs sync an intent before mutation and an outcome afterward.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !dryRun && !yes {
				return errors.New("refusing to mutate without explicit --yes (use --dry-run to validate safely)")
			}
			if noAudit && cmd.Flags().Changed("audit") {
				return errors.New("--audit and --no-audit cannot be used together")
			}
			policy := fsops.ConflictPolicy(conflict)
			if policy != fsops.ConflictFail && policy != fsops.ConflictOverwrite {
				return fmt.Errorf("invalid --conflict %q: expected fail or overwrite", conflict)
			}
			plan, err := readOperationPlan(cmd, args[0])
			if err != nil {
				return err
			}
			if len(plan.Operations) == 0 {
				return errors.New("plan contains no operations")
			}
			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if cfg.ReadOnly && !dryRun {
				return errors.New("configuration is read-only; only --dry-run is allowed")
			}
			if !noAudit && !cmd.Flags().Changed("audit") {
				auditPath = cfg.AuditPath
				if auditPath == "" {
					auditPath, err = appconfig.DefaultAuditPath()
					if err != nil {
						return fmt.Errorf("default audit path: %w", err)
					}
				}
			}
			if !noAudit {
				if strings.TrimSpace(auditPath) == "" {
					return errors.New("--audit must not be empty")
				}
			}
			results, applyErr := fsops.Apply(cmd.Context(), plan, fsops.ApplyOptions{
				DryRun: dryRun, Conflict: policy, AuditPath: auditPath, DisableAudit: noAudit,
			})
			if err := fsops.WriteResults(cmd.OutOrStdout(), results); err != nil {
				return fmt.Errorf("write results: %w", err)
			}
			return applyErr
		},
	}
	f := cmd.Flags()
	f.BoolVar(&dryRun, "dry-run", false, "validate every operation without mutating files")
	f.BoolVar(&yes, "yes", false, "confirm filesystem mutations")
	f.StringVar(&conflict, "conflict", string(fsops.ConflictFail), "destination conflict policy: fail|overwrite")
	f.StringVar(&auditPath, "audit", "", "append and sync intent/outcome JSONL to this audit file")
	f.BoolVar(&noAudit, "no-audit", false, "explicitly disable audit logging")
	return cmd
}

func readOperationPlan(cmd *cobra.Command, path string) (plan fsops.Plan, err error) {
	if path == "-" {
		plan, err = fsops.ReadPlanLimited(cmd.InOrStdin(), fsops.MaxPlanBytes)
	} else {
		f, openErr := os.Open(path)
		if openErr != nil {
			return fsops.Plan{}, fmt.Errorf("open plan: %w", openErr)
		}
		defer func() {
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
		}()
		plan, err = fsops.ReadPlanLimited(f, fsops.MaxPlanBytes)
	}
	if err != nil {
		return fsops.Plan{}, fmt.Errorf("read plan: %w", err)
	}
	return plan, nil
}
