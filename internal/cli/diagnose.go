package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/phillipod/go-dirstat/internal/diagnose"
	"github.com/phillipod/go-dirstat/internal/format"
)

func newDiagnoseCommand() *cobra.Command {
	var output string
	var bytes bool
	cmd := &cobra.Command{
		Use:   "diagnose [path...]",
		Short: "Explain filesystem pressure and open-deleted files",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != outputFormatText && output != outputFormatJSON {
				return fmt.Errorf("invalid --format %q: expected text or json", output)
			}
			if len(args) == 0 {
				args = []string{"."}
			}
			result := diagnose.Gather(cmd.Context(), args)
			if output == outputFormatJSON {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
					return fmt.Errorf("encode diagnostics: %w", err)
				}
				return nil
			}
			return writeDiagnosticsText(cmd, result, bytes)
		},
	}
	cmd.Flags().StringVar(&output, "format", "text", "output format: text|json")
	cmd.Flags().BoolVar(&bytes, "bytes", false, "print raw bytes instead of human sizes")
	return cmd
}

func writeDiagnosticsText(cmd *cobra.Command, result diagnose.Result, raw bool) error {
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintln(out, "Volumes:"); err != nil {
		return err
	}
	if len(result.Volumes) == 0 {
		if _, err := fmt.Fprintln(out, "  (none)"); err != nil {
			return err
		}
	}
	for _, volume := range result.Volumes {
		inode := "n/a"
		if volume.Inodes > 0 {
			inode = fmt.Sprintf("%.1f%%", volume.InodePct)
		}
		if _, err := fmt.Fprintf(out, "  %s  %.1f%% used  %s available  inodes %s\n",
			format.SafeText(volume.Path), volume.UsedPct, diagnosticSize(volume.Available, raw), inode); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(out, "Capabilities:"); err != nil {
		return err
	}
	for _, capability := range result.Capabilities {
		state := "available"
		if !capability.Available {
			state = "unavailable"
			if capability.Reason != "" {
				state += ": " + format.SafeText(capability.Reason)
			}
		}
		if _, err := fmt.Fprintf(out, "  %s: %s\n", format.SafeText(capability.Name), state); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(out, "Open deleted files:"); err != nil {
		return err
	}
	if len(result.OpenDeleted) == 0 {
		if _, err := fmt.Fprintln(out, "  (none)"); err != nil {
			return err
		}
	}
	for _, file := range result.OpenDeleted {
		if _, err := fmt.Fprintf(out, "  %s  pid=%d  fd=%s  %s  %s\n",
			diagnosticSize(uint64(file.Size), raw), file.PID, format.SafeText(file.Descriptor),
			format.SafeText(file.Process), format.SafeText(file.Path)); err != nil {
			return err
		}
	}
	for _, warning := range result.Warnings {
		if _, err := fmt.Fprintf(out, "warning: %s\n", format.SafeText(strings.TrimSpace(warning))); err != nil {
			return err
		}
	}
	return nil
}

func diagnosticSize(value uint64, raw bool) string {
	if raw {
		return fmt.Sprintf("%dB", value)
	}
	if value > uint64(^uint64(0)>>1) {
		return fmt.Sprintf("%dB", value)
	}
	return format.Bytes(int64(value))
}
