package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	appconfig "github.com/phillipod/go-dirstat/internal/config"
	"github.com/phillipod/go-dirstat/internal/tui"
)

// newTUICommand launches the full-screen interactive browser.
func newTUICommand(cfg *Config) *cobra.Command {
	var readOnly, noAudit bool
	var auditPath string
	cmd := &cobra.Command{
		Use:   "tui [path]",
		Short: "Interactive full-screen directory browser",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "."
			if len(args) > 0 {
				path = args[0]
			}
			policy, err := cfg.policy()
			if err != nil {
				return err
			}
			userCfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			effectiveReadOnly, resolvedAudit, disableAudit, err := resolveTUIAudit(cmd, userCfg, readOnly, noAudit, auditPath)
			if err != nil {
				return err
			}
			if !disableAudit && resolvedAudit != "" {
				auditAbs, absErr := filepath.Abs(filepath.Clean(resolvedAudit))
				if absErr != nil {
					return fmt.Errorf("resolve audit path for scan exclusion: %w", absErr)
				}
				policy.ExcludePaths = append(append([]string(nil), policy.ExcludePaths...), auditAbs, resolvedPath(auditAbs))
			}
			app := tui.New(path, policy, cfg.sizeMode(), cfg.Jobs, tui.Options{
				UseCache:             !cfg.NoCache,
				ReadOnly:             effectiveReadOnly,
				Editor:               userCfg.Tools.Editor,
				Pager:                userCfg.Tools.Pager,
				Shell:                userCfg.Tools.Shell,
				AuditPath:            resolvedAudit,
				DisableAudit:         disableAudit,
				TargetAvailableBytes: userCfg.TUI.TargetAvailableBytes,
				QueueMaxOperations:   userCfg.TUI.QueueMaxOperations,
				QueueMaxReclaimBytes: userCfg.TUI.QueueMaxReclaimBytes,
				HistoryMax:           userCfg.HistoryMax,
				CacheMaxBytes:        userCfg.State.CacheMaxBytes,
				CacheMaxAge:          time.Duration(userCfg.State.CacheTTLHours) * time.Hour,
				HistoryMaxBytes:      userCfg.State.HistoryMaxBytes,
				HistoryMaxAge:        time.Duration(userCfg.State.HistoryTTLDays) * 24 * time.Hour,
			})
			return app.Run(cmd.Context())
		},
		SilenceUsage: true,
	}
	cmd.Flags().BoolVar(&cfg.NoCache, "no-cache", false, "do not read/write the scan cache")
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "disable filesystem mutations and external editor/shell actions")
	cmd.Flags().StringVar(&auditPath, "audit", "", "append mutation result JSONL to this audit file")
	cmd.Flags().BoolVar(&noAudit, "no-audit", false, "explicitly disable mutation audit logging")
	return cmd
}

func resolveTUIAudit(cmd *cobra.Command, userCfg appconfig.Config, readOnly, noAudit bool, auditPath string) (bool, string, bool, error) {
	effectiveReadOnly := readOnly || userCfg.ReadOnly
	if noAudit && cmd.Flags().Changed("audit") {
		return false, "", false, fmt.Errorf("--audit and --no-audit cannot be used together")
	}
	if effectiveReadOnly {
		// Analysis-only startup must not resolve or create mutation state.
		return true, "", true, nil
	}
	if noAudit {
		return false, "", true, nil
	}
	if !cmd.Flags().Changed("audit") {
		auditPath = userCfg.AuditPath
		if auditPath == "" {
			var err error
			auditPath, err = appconfig.DefaultAuditPath()
			if err != nil {
				return false, "", false, fmt.Errorf("default audit path: %w", err)
			}
		}
	}
	if strings.TrimSpace(auditPath) == "" {
		return false, "", false, fmt.Errorf("--audit must not be empty")
	}
	// The filesystem package creates the parent only when an apply starts.
	return false, auditPath, false, nil
}
