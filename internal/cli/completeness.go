package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/phillipod/go-dirstat/internal/format"
	"github.com/phillipod/go-dirstat/internal/scan"
)

// ExitScanIncomplete is the process exit status used when a filesystem scan
// completed its traversal but one or more entries could not be measured.
const (
	ExitScanIncomplete    = 3
	ExitPressureThreshold = 4
	ExitDiagnosticPartial = 5
	ExitCandidateState    = 6
)

// IncompleteScanError distinguishes incomplete measurement from a fatal root
// error. Automation can use ExitCode instead of parsing this message.
type IncompleteScanError struct {
	Path   string
	Errors int64
}

func (e *IncompleteScanError) Error() string {
	return fmt.Sprintf("scan %q is incomplete: %d filesystem entries could not be measured", e.Path, e.Errors)
}

// ExitCode maps command errors to the documented process status.
func ExitCode(err error) int {
	var incomplete *IncompleteScanError
	if errors.As(err, &incomplete) {
		return ExitScanIncomplete
	}
	var condition *conditionError
	if errors.As(err, &condition) {
		return condition.code
	}
	return 1
}

type conditionError struct {
	code    int
	message string
}

func (e *conditionError) Error() string { return e.message }

func acceptScan(cmd *cobra.Command, path string, stats scan.Stats, allowPartial bool) error {
	if stats.Complete {
		return nil
	}
	err := &IncompleteScanError{Path: path, Errors: stats.Errors}
	if !allowPartial {
		return err
	}
	if _, writeErr := fmt.Fprintf(cmd.ErrOrStderr(), "dirstat: warning: %s; continuing because --allow-partial was set\n", format.SafeText(err.Error())); writeErr != nil {
		return fmt.Errorf("write incomplete-scan warning: %w", writeErr)
	}
	return nil
}
