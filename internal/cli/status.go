package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/phillipod/go-dirstat/internal/format"
	"github.com/phillipod/go-dirstat/internal/fsinfo"
)

func newStatusCommand() *cobra.Command {
	var output string
	var raw bool
	var maxBytePressure, maxInodePressure float64
	cmd := &cobra.Command{
		Use:   "status [path...]",
		Short: "Show filesystem capacity and inode pressure",
		Long: `Show filesystem capacity and inode pressure.

Physical use counts allocated blocks. Caller pressure also accounts for blocks
reserved from ordinary callers. JSON output is one array; JSONL emits one
volume object per line.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != outputFormatText && output != outputFormatJSON && output != outputFormatJSONL {
				return fmt.Errorf("invalid --format %q: expected text, json, or jsonl", output)
			}
			if len(args) == 0 {
				args = []string{"."}
			}
			thresholds, err := validatePressureThresholds(maxBytePressure, maxInodePressure)
			if err != nil {
				return err
			}
			volumes := make([]fsinfo.Volume, 0, len(args))
			for _, path := range args {
				v, err := fsinfo.VolumeFor(path)
				if err != nil {
					return err
				}
				volumes = append(volumes, v)
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			if output == outputFormatJSON {
				if err := enc.Encode(volumes); err != nil {
					return err
				}
				return thresholds.breach(volumes)
			}
			if output == outputFormatJSONL {
				for _, volume := range volumes {
					if err := enc.Encode(volume); err != nil {
						return err
					}
				}
				return thresholds.breach(volumes)
			}
			for _, v := range volumes {
				inode := "n/a"
				if v.Inodes > 0 {
					inode = fmt.Sprintf("%.1f%%", v.InodePct)
				}
				identity := strings.TrimSpace(strings.Join([]string{v.MountPoint, v.Filesystem, v.Device}, " "))
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s  %s used / %s total  %.1f%% physical  %.1f%% caller pressure  %s available  inodes %s  %s\n",
					format.SafeText(v.Path), diagnosticSize(v.PhysicalUsed, raw), diagnosticSize(v.Total, raw),
					v.PhysicalUsedPct, v.CallerPressurePct, diagnosticSize(v.Available, raw), inode,
					format.SafeText(identity)); err != nil {
					return err
				}
			}
			return thresholds.breach(volumes)
		},
	}
	cmd.Flags().StringVar(&output, "format", "text", "output format: text|json|jsonl")
	cmd.Flags().BoolVar(&raw, "bytes", false, "print raw bytes instead of human sizes")
	cmd.Flags().Float64Var(&maxBytePressure, "max-byte-pressure", -1, "exit 4 when caller byte pressure exceeds PERCENT (-1 disables)")
	cmd.Flags().Float64Var(&maxInodePressure, "max-inode-pressure", -1, "exit 4 when inode pressure exceeds PERCENT (-1 disables)")
	return cmd
}
