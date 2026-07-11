package cli

import (
	"fmt"
	"math"
	"strings"

	"github.com/phillipod/go-dirstat/internal/diagnose"
	"github.com/phillipod/go-dirstat/internal/format"
	"github.com/phillipod/go-dirstat/internal/fsinfo"
)

type pressureThresholds struct {
	bytes  float64
	inodes float64
}

func validatePressureThresholds(bytes, inodes float64) (pressureThresholds, error) {
	values := []struct {
		name  string
		value float64
	}{{name: "max-byte-pressure", value: bytes}, {name: "max-inode-pressure", value: inodes}}
	for _, threshold := range values {
		if math.IsNaN(threshold.value) || math.IsInf(threshold.value, 0) || threshold.value < -1 || threshold.value > 100 {
			return pressureThresholds{}, fmt.Errorf("--%s must be between 0 and 100, or -1 to disable", threshold.name)
		}
	}
	return pressureThresholds{bytes: bytes, inodes: inodes}, nil
}

func (thresholds pressureThresholds) breach(volumes []fsinfo.Volume) error {
	unavailable := make([]string, 0)
	violations := make([]string, 0)
	for _, volume := range volumes {
		if thresholds.bytes >= 0 && volume.CallerPressurePct > thresholds.bytes {
			violations = append(violations, fmt.Sprintf("%s byte pressure %.1f%% exceeds %.1f%%",
				format.SafeText(volume.Path), volume.CallerPressurePct, thresholds.bytes))
		}
		if thresholds.inodes >= 0 {
			if volume.Inodes == 0 {
				unavailable = append(unavailable, fmt.Sprintf("%s inode pressure is unavailable", format.SafeText(volume.Path)))
			} else if volume.InodePct > thresholds.inodes {
				violations = append(violations, fmt.Sprintf("%s inode pressure %.1f%% exceeds %.1f%%",
					format.SafeText(volume.Path), volume.InodePct, thresholds.inodes))
			}
		}
	}
	if len(unavailable) > 0 {
		return &conditionError{code: ExitDiagnosticPartial, message: strings.Join(unavailable, "; ")}
	}
	if len(violations) == 0 {
		return nil
	}
	return &conditionError{code: ExitPressureThreshold, message: strings.Join(violations, "; ")}
}

func diagnosticPartial(result diagnose.Result) bool {
	if len(result.Warnings) > 0 {
		return true
	}
	for _, capability := range result.Capabilities {
		if !capability.Available {
			return true
		}
	}
	return result.OpenDeletedSummary != nil && !result.OpenDeletedSummary.Coverage.Complete
}
