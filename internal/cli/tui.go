package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
			if noAudit && cmd.Flags().Changed("audit") {
				return fmt.Errorf("--audit and --no-audit cannot be used together")
			}
			if !noAudit && !cmd.Flags().Changed("audit") {
				auditPath = userCfg.AuditPath
				if auditPath == "" {
					auditPath, err = appconfig.DefaultAuditPath()
					if err != nil {
						return fmt.Errorf("default audit path: %w", err)
					}
				}
			}
			if !noAudit {
				if strings.TrimSpace(auditPath) == "" {
					return fmt.Errorf("--audit must not be empty")
				}
				if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
					return fmt.Errorf("create audit directory: %w", err)
				}
			}
			app := tui.New(path, policy, cfg.sizeMode(), cfg.Jobs, tui.Options{
				UseCache:     !cfg.NoCache,
				ReadOnly:     readOnly || userCfg.ReadOnly,
				Editor:       userCfg.Tools.Editor,
				Pager:        userCfg.Tools.Pager,
				Shell:        userCfg.Tools.Shell,
				AuditPath:    auditPath,
				DisableAudit: noAudit,
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
