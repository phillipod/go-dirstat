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
	var requireComplete bool
	var maxBytePressure, maxInodePressure float64
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
			thresholds, err := validatePressureThresholds(maxBytePressure, maxInodePressure)
			if err != nil {
				return err
			}
			result := diagnose.Gather(cmd.Context(), args)
			if output == outputFormatJSON {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
					return fmt.Errorf("encode diagnostics: %w", err)
				}
			} else if err := writeDiagnosticsText(cmd, result, bytes); err != nil {
				return err
			}
			// Completeness has precedence over pressure because a partial diagnostic
			// cannot prove that the observed state is the complete host state.
			if requireComplete && diagnosticPartial(result) {
				return &conditionError{code: ExitDiagnosticPartial, message: "diagnostic evidence is partial or unavailable"}
			}
			return thresholds.breach(result.Volumes)
		},
	}
	cmd.Flags().StringVar(&output, "format", "text", "output format: text|json")
	cmd.Flags().BoolVar(&bytes, "bytes", false, "print raw bytes instead of human sizes")
	cmd.Flags().BoolVar(&requireComplete, "require-complete", false, "exit 5 when any requested diagnostic is partial or unavailable")
	cmd.Flags().Float64Var(&maxBytePressure, "max-byte-pressure", -1, "exit 4 when caller byte pressure exceeds PERCENT (-1 disables)")
	cmd.Flags().Float64Var(&maxInodePressure, "max-inode-pressure", -1, "exit 4 when inode pressure exceeds PERCENT (-1 disables)")
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
		if _, err := fmt.Fprintf(out, "  %s  %.1f%% caller pressure  %.1f%% physically used  %s available  inodes %s\n",
			format.SafeText(volume.Path), volume.CallerPressurePct, volume.PhysicalUsedPct,
			diagnosticSize(volume.Available, raw), inode); err != nil {
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
		if _, err := fmt.Fprintf(out, "  %s reclaimable  %s logical  dev=%d ino=%d  %s\n",
			diagnosticSize(nonnegativeDiagnosticSize(file.Allocated), raw),
			diagnosticSize(nonnegativeDiagnosticSize(file.Size), raw), file.Device, file.Inode,
			format.SafeText(file.Path)); err != nil {
			return err
		}
		for _, holder := range file.Holders {
			process := ""
			if holder.Process != "" {
				process = "  " + format.SafeText(holder.Process)
			}
			if _, err := fmt.Fprintf(out, "    pid=%d  fds=%s%s\n", holder.PID,
				format.SafeText(strings.Join(holder.Descriptors, ",")), process); err != nil {
				return err
			}
		}
		for _, path := range file.Paths {
			if path == file.Path {
				continue
			}
			if _, err := fmt.Fprintf(out, "    also observed as %s\n", format.SafeText(path)); err != nil {
				return err
			}
		}
	}
	if summary := result.OpenDeletedSummary; summary != nil {
		label := "Unique reclaimable"
		if !summary.Coverage.Complete {
			label = "Observed unique reclaimable"
		}
		if _, err := fmt.Fprintf(out, "%s: %s in %d object(s)  %s logical  %d holder(s) / %d fd(s)\n",
			label, diagnosticSize(nonnegativeDiagnosticSize(summary.ReclaimableBytes), raw), summary.Objects,
			diagnosticSize(nonnegativeDiagnosticSize(summary.LogicalBytes), raw), summary.Holders, summary.Descriptors); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "Open-deleted coverage: %s\n", diagnosticCoverage(summary.Coverage)); err != nil {
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

func nonnegativeDiagnosticSize(value int64) uint64 {
	if value < 0 {
		return 0
	}
	return uint64(value)
}

func diagnosticCoverage(coverage diagnose.OpenDeletedCoverage) string {
	state := "complete"
	if !coverage.Complete {
		state = "partial"
	}
	details := fmt.Sprintf("%s; processes %d/%d scanned (%d skipped); descriptors %d/%d scanned (%d skipped)",
		state, coverage.ProcessesScanned, coverage.ProcessEntries, coverage.ProcessesSkipped,
		coverage.DescriptorsScanned, coverage.DescriptorEntries, coverage.DescriptorsSkipped)
	flags := make([]string, 0, 3)
	if coverage.ProcessLimitReached {
		flags = append(flags, "process limit reached")
	}
	if coverage.DescriptorLimitReached {
		flags = append(flags, "descriptor limit reached")
	}
	if coverage.Canceled {
		flags = append(flags, "canceled")
	}
	if len(flags) > 0 {
		details += "; " + strings.Join(flags, ", ")
	}
	return details
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
