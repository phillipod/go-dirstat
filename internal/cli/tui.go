package cli

import (
	"github.com/spf13/cobra"

	"github.com/phillipod/go-dirstat/internal/tui"
)

// newTUICommand launches the full-screen interactive browser.
func newTUICommand(cfg *Config) *cobra.Command {
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
			app := tui.New(path, policy, cfg.sizeMode(), cfg.Jobs, tui.Options{
				UseCache: !cfg.NoCache,
			})
			return app.Run(cmd.Context())
		},
		SilenceUsage: true,
	}
	cmd.Flags().BoolVar(&cfg.NoCache, "no-cache", false, "do not read/write the scan cache")
	return cmd
}
