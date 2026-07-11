package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/phillipod/go-dirstat/internal/format"
	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/scope"
)

func newStatusCommand() *cobra.Command {
	var output string
	var raw bool
	cmd := &cobra.Command{
		Use:   "status [path...]",
		Short: "Show filesystem capacity and inode pressure",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != outputFormatText && output != outputFormatJSON {
				return fmt.Errorf("invalid --format %q: expected text or json", output)
			}
			if len(args) == 0 {
				args = []string{"."}
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			policy := scope.New()
			for _, path := range args {
				v, err := fsinfo.VolumeFor(path)
				if err != nil {
					return err
				}
				v.Filesystem = policy.FSOf(v.Path)
				if output == outputFormatJSON {
					if err := enc.Encode(v); err != nil {
						return err
					}
					continue
				}
				inode := "n/a"
				if v.Inodes > 0 {
					inode = fmt.Sprintf("%.1f%%", v.InodePct)
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s  %s used / %s total  %.1f%%  %s available  inodes %s  %s\n",
					format.SafeText(v.Path), diagnosticSize(v.Used, raw), diagnosticSize(v.Total, raw), v.UsedPct,
					diagnosticSize(v.Available, raw), inode, format.SafeText(strings.TrimSpace(v.Filesystem))); err != nil {
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
