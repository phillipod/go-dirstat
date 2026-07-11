package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	appconfig "github.com/phillipod/go-dirstat/internal/config"
	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/fsops"
)

type planFlags struct {
	root, output, mode, size, format string
	uid, gid                         int
	recursive                        bool
}

func newPlanCommand() *cobra.Command {
	flags := planFlags{root: ".", output: "-", uid: -1, gid: -1}
	cmd := &cobra.Command{
		Use:   "plan ACTION SOURCE [DESTINATION]",
		Short: "Create a guarded filesystem operation plan",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := buildOperationPlan(cmd, flags, args)
			if err != nil {
				return err
			}
			return writeOperationPlan(cmd, flags.output, plan)
		},
	}
	f := cmd.Flags()
	f.StringVar(&flags.root, "root", ".", "confine all operation paths to this directory")
	f.StringVarP(&flags.output, "output", "o", "-", "write JSONL plan to this file (- for stdout)")
	f.StringVar(&flags.mode, "mode", "", "octal mode for mkdir, touch, or chmod")
	f.StringVar(&flags.size, "size", "", "target size for truncate (bytes or K/M/G/T/P/E suffix)")
	f.IntVar(&flags.uid, "uid", -1, "numeric owner ID for chown")
	f.IntVar(&flags.gid, "gid", -1, "numeric group ID for chown")
	f.StringVar(&flags.format, "archive-format", "", "archive format: tar|tar.gz|zip (otherwise inferred)")
	f.BoolVarP(&flags.recursive, "recursive", "r", false, "allow recursive directory deletion")
	return cmd
}

func buildOperationPlan(cmd *cobra.Command, flags planFlags, args []string) (fsops.Plan, error) {
	action, err := parseAction(args[0])
	if err != nil {
		return fsops.Plan{}, err
	}
	wantsDestination := action == fsops.ActionCopy || action == fsops.ActionMove || action == fsops.ActionRename ||
		action == fsops.ActionArchive || action == fsops.ActionExtract
	if wantsDestination && len(args) != 3 {
		return fsops.Plan{}, fmt.Errorf("%s requires SOURCE and DESTINATION", action)
	}
	if !wantsDestination && len(args) != 2 {
		return fsops.Plan{}, fmt.Errorf("%s accepts SOURCE only", action)
	}

	root, err := canonicalOperationRoot(flags.root)
	if err != nil {
		return fsops.Plan{}, err
	}
	source := operationPath(root, args[1])
	op := fsops.Operation{ID: string(action) + "-1", Action: action, Source: source, Recursive: flags.recursive}
	if wantsDestination {
		op.Destination = operationPath(root, args[2])
	}
	if cmd.Flags().Changed("mode") {
		mode, err := parseOperationMode(flags.mode)
		if err != nil {
			return fsops.Plan{}, err
		}
		op.Mode = &mode
	}
	if cmd.Flags().Changed("size") {
		size, err := parseSize(flags.size)
		if err != nil {
			return fsops.Plan{}, fmt.Errorf("invalid --size %q: %w", flags.size, err)
		}
		op.Size = &size
	}
	if cmd.Flags().Changed("uid") {
		if flags.uid < 0 {
			return fsops.Plan{}, errors.New("--uid must be zero or greater")
		}
		op.UID = &flags.uid
	}
	if cmd.Flags().Changed("gid") {
		if flags.gid < 0 {
			return fsops.Plan{}, errors.New("--gid must be zero or greater")
		}
		op.GID = &flags.gid
	}
	op.Format = flags.format
	if err := validatePlanFlags(cmd, action, op); err != nil {
		return fsops.Plan{}, err
	}

	plan := fsops.Plan{
		Header:     fsops.PlanHeader{Version: fsops.PlanVersion, Root: root, CreatedAt: time.Now().UTC()},
		Operations: []fsops.Operation{op},
	}
	// Validate confinement and parameters before inspecting a target. This
	// avoids reading metadata through an escaping symlink parent.
	if _, err := fsops.Apply(cmd.Context(), plan, fsops.ApplyOptions{DryRun: true, Conflict: fsops.ConflictOverwrite, DisableAudit: true, AllowUnguarded: true}); err != nil {
		return fsops.Plan{}, fmt.Errorf("validate plan: %w", err)
	}
	entry, err := fsinfo.Inspect(source, false)
	if err == nil {
		plan.Operations[0].Expected = &entry
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fsops.Plan{}, fmt.Errorf("inspect source: %w", err)
	}
	if _, err := fsops.Apply(cmd.Context(), plan, fsops.ApplyOptions{DryRun: true, Conflict: fsops.ConflictOverwrite, DisableAudit: true}); err != nil {
		return fsops.Plan{}, fmt.Errorf("validate expected source: %w", err)
	}
	return plan, nil
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

func operationPath(root, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return filepath.Clean(path)
	}
	return abs
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
	if destination == "-" {
		return fsops.WritePlan(cmd.OutOrStdout(), plan)
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
	if err := fsops.WritePlan(f, plan); err != nil {
		return err
	}
	return f.Sync()
}

func newApplyCommand() *cobra.Command {
	var dryRun, yes, noAudit bool
	var conflict, auditPath string
	cmd := &cobra.Command{
		Use:   "apply PLAN",
		Short: "Validate and apply a filesystem operation plan",
		Args:  cobra.ExactArgs(1),
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
				if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
					return fmt.Errorf("create audit directory: %w", err)
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
	f.StringVar(&auditPath, "audit", "", "append result JSONL to this audit file")
	f.BoolVar(&noAudit, "no-audit", false, "explicitly disable audit logging")
	return cmd
}

func readOperationPlan(cmd *cobra.Command, path string) (plan fsops.Plan, err error) {
	if path == "-" {
		plan, err = fsops.ReadPlan(io.LimitReader(cmd.InOrStdin(), 64<<20))
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
		plan, err = fsops.ReadPlan(io.LimitReader(f, 64<<20))
	}
	if err != nil {
		return fsops.Plan{}, fmt.Errorf("read plan: %w", err)
	}
	return plan, nil
}
