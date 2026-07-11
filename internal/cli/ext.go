package cli

import "github.com/spf13/cobra"

// newExtensionsCommand renders the by-extension + largest-files view.
func newExtensionsCommand(cfg *Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "extensions [path...]",
		Aliases: []string{"ext", "by-ext"},
		Short:   "Show a by-extension size breakdown and the largest files",
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg.Paths = args
			cfg.ByExt = true
			return runText(cmd, cfg)
		},
		SilenceUsage: true,
	}
	bindRichOutputFlags(cmd, cfg)
	return cmd
}
