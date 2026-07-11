package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/phillipod/go-dirstat/internal/diagnose"
	"github.com/phillipod/go-dirstat/internal/format"
)

func (m *model) pressureBadges() []string {
	if m.volumeErr != nil {
		return []string{errStyle.Render("volume: " + format.SafeText(m.volumeErr.Error()))}
	}
	if m.volumeLoadedAt.IsZero() {
		return nil
	}
	badges := []string{
		badgeStyle.Render(fmt.Sprintf("pressure:%.1f%%", m.volume.CallerPressurePct)),
		dimStyle.Render("available:" + formatVolumeBytes(m.volume.Available)),
	}
	if m.volume.Inodes > 0 {
		badges = append(badges, dimStyle.Render(fmt.Sprintf("inodes:%.1f%%", m.volume.InodePct)))
	}
	forecast := m.pressureForecast()
	if m.targetAvailable > 0 {
		badges = append(badges, dimStyle.Render("target:"+formatVolumeBytes(m.targetAvailable)))
		if forecast.targetGapAfter == 0 {
			badges = append(badges, badgeStyle.Render("forecast:target met"))
		} else {
			badges = append(badges, errStyle.Render("forecast gap:"+formatVolumeBytes(forecast.targetGapAfter)))
		}
	}
	if forecast.queued > 0 {
		badges = append(badges, dimStyle.Render(fmt.Sprintf("after queue:%s/%.1f%%", formatVolumeBytes(forecast.availableAfter), forecast.callerPressureAfter)))
	}
	return badges
}

func (m *model) growthBody() string {
	w, avail := m.bodyWidth(), m.availHeight()
	lines := []string{titleStyle.Render("Measured growth history")}
	state := m.growth
	if state.loading {
		lines = append(lines, "", badgeStyle.Render("Comparing the complete scan with retained CLI history…"), dimStyle.Render("c cancels this analysis"))
		return renderBoundedLines(lines, w, avail)
	}
	if state.err != nil {
		lines = append(lines, "", errStyle.Render(format.SafeText(state.err.Error())))
		return renderBoundedLines(lines, w, avail)
	}
	if !state.loadedAt.IsZero() {
		lines = append(lines, dimStyle.Render(fmt.Sprintf("current %s old · refreshed %s ago · complete:%t",
			ageSince(state.currentAt), ageSince(state.loadedAt), state.currentComplete)))
	}
	if state.baselineMissing {
		message := "No retained baseline is available; read-only mode did not create one."
		if state.baselineRecorded {
			message = "Recorded this complete scan as the first growth baseline."
		}
		lines = append(lines, "", badgeStyle.Render(message))
		return renderBoundedLines(lines, w, avail)
	}
	if !state.baselineAt.IsZero() {
		lines = append(lines, dimStyle.Render("baseline "+state.baselineAt.Format(time.RFC3339)))
	}
	if state.truncated {
		lines = append(lines, errStyle.Render(fmt.Sprintf("Showing the first %d changes; result was capped.", len(state.deltas))))
	}
	if len(state.deltas) == 0 {
		lines = append(lines, "", dimStyle.Render("No measured size changes since the retained baseline."))
		return renderBoundedLines(lines, w, avail)
	}
	for i := m.offset; i < min(len(state.deltas), m.offset+max(1, avail-len(lines))); i++ {
		delta := state.deltas[i]
		line := fmt.Sprintf("%-7s %9s allocated  %9s apparent  %s", delta.Change,
			signedBytes(delta.AllocatedDelta), signedBytes(delta.ApparentDelta), format.SafeText(delta.Path))
		line = truncate(line, w)
		if i == m.cursor {
			line = cursorBg.Render(line)
		}
		lines = append(lines, line)
	}
	return renderBoundedLines(lines, w, avail)
}

func (m *model) openDeletedBody() string {
	w, avail := m.bodyWidth(), m.availHeight()
	lines := []string{titleStyle.Render("Unique open-deleted pressure")}
	state := m.openDeleted
	if state.loading {
		lines = append(lines, "", badgeStyle.Render("Inspecting bounded host descriptor evidence…"), dimStyle.Render("c cancels this analysis"))
		return renderBoundedLines(lines, w, avail)
	}
	if state.err != nil {
		lines = append(lines, "", errStyle.Render(format.SafeText(state.err.Error())))
		return renderBoundedLines(lines, w, avail)
	}
	if !state.loadedAt.IsZero() {
		lines = append(lines, dimStyle.Render("refreshed "+ageSince(state.loadedAt)+" ago"))
	}
	if capability := openDeletedCapability(state.result); capability != nil && !capability.Available {
		lines = append(lines, "", errStyle.Render("Unavailable: "+format.SafeText(capability.Reason)))
		return renderBoundedLines(lines, w, avail)
	}
	if summary := state.result.OpenDeletedSummary; summary != nil {
		label := "unique reclaimable"
		coverage := "complete"
		if !summary.Coverage.Complete {
			label, coverage = "observed lower bound", "partial"
		}
		lines = append(lines,
			badgeStyle.Render(fmt.Sprintf("%s %s · %d object(s) · %d holder(s) / %d fd(s)", label,
				format.Bytes(summary.ReclaimableBytes), summary.Objects, summary.Holders, summary.Descriptors)),
			dimStyle.Render(fmt.Sprintf("coverage:%s · processes %d/%d · descriptors %d/%d · skipped %d/%d",
				coverage, summary.Coverage.ProcessesScanned, summary.Coverage.ProcessEntries,
				summary.Coverage.DescriptorsScanned, summary.Coverage.DescriptorEntries,
				summary.Coverage.ProcessesSkipped, summary.Coverage.DescriptorsSkipped)),
		)
	}
	if len(state.result.OpenDeleted) == 0 {
		lines = append(lines, "", dimStyle.Render("No open-deleted objects were observed in scope."))
	}
	for i := m.offset; i < min(len(state.result.OpenDeleted), m.offset+max(1, avail-len(lines))); i++ {
		file := state.result.OpenDeleted[i]
		line := truncate(fmt.Sprintf("%9s reclaimable  dev=%d ino=%d  %s", format.Bytes(file.Allocated), file.Device, file.Inode, format.SafeText(file.Path)), w)
		if i == m.cursor {
			line = cursorBg.Render(line)
		}
		lines = append(lines, line)
	}
	for _, warning := range state.result.Warnings {
		if len(lines) >= avail {
			break
		}
		lines = append(lines, errStyle.Render("warning: "+format.SafeText(strings.TrimSpace(warning))))
	}
	return renderBoundedLines(lines, w, avail)
}

func (m *model) analysisDetailLine() string {
	switch m.view {
	case viewTree, viewExt, viewLargest, viewHelp:
		return ""
	case viewGrowth:
		if m.cursor >= 0 && m.cursor < len(m.growth.deltas) {
			delta := m.growth.deltas[m.cursor]
			return dimStyle.Render(fmt.Sprintf("%s · %s allocated · %s apparent", format.SafeText(delta.Path), signedBytes(delta.AllocatedDelta), signedBytes(delta.ApparentDelta)))
		}
	case viewOpenDeleted:
		if m.cursor >= 0 && m.cursor < len(m.openDeleted.result.OpenDeleted) {
			file := m.openDeleted.result.OpenDeleted[m.cursor]
			return dimStyle.Render(fmt.Sprintf("%s · %s reclaimable · dev=%d ino=%d", format.SafeText(file.Path), format.Bytes(file.Allocated), file.Device, file.Inode))
		}
	}
	return ""
}

func (m *model) analysisContextBody(w int) string {
	lines := []string{titleStyle.Render("Evidence"), ""}
	switch m.view {
	case viewTree, viewExt, viewLargest, viewHelp:
		lines = append(lines, dimStyle.Render("No analytical evidence in this view."))
	case viewGrowth:
		if m.cursor < 0 || m.cursor >= len(m.growth.deltas) {
			lines = append(lines, dimStyle.Render("Select a measured change."))
			break
		}
		delta := m.growth.deltas[m.cursor]
		kind := "file"
		if delta.IsDir {
			kind = kindDirectory
		}
		lines = append(lines, format.SafeText(delta.Path), string(delta.Change)+" · "+kind,
			fmt.Sprintf("allocated %s → %s", format.Bytes(delta.BeforeAlloc), format.Bytes(delta.AfterAlloc)),
			fmt.Sprintf("apparent %s → %s", format.Bytes(delta.BeforeApparent), format.Bytes(delta.AfterApparent)))
	case viewOpenDeleted:
		if m.cursor < 0 || m.cursor >= len(m.openDeleted.result.OpenDeleted) {
			lines = append(lines, dimStyle.Render("Select an observed object."))
			break
		}
		file := m.openDeleted.result.OpenDeleted[m.cursor]
		lines = append(lines, format.SafeText(file.Path), fmt.Sprintf("device %d · inode %d", file.Device, file.Inode),
			fmt.Sprintf("logical %s · allocated %s", format.Bytes(file.Size), format.Bytes(file.Allocated)))
		for _, holder := range file.Holders {
			process := holder.Process
			if process == "" {
				process = "unknown process"
			}
			lines = append(lines, fmt.Sprintf("pid %d · %s · fds %s", holder.PID, format.SafeText(process), format.SafeText(strings.Join(holder.Descriptors, ","))))
		}
	}
	return renderBoundedLines(lines, w, m.availHeight())
}

func (m *model) destinationBody() string {
	w, avail := m.width, m.availHeight()
	lines := []string{titleStyle.Render(fmt.Sprintf("%s destination · %d selected", m.managementAction, len(m.actionPaths())))}
	state := m.destination
	lines = append(lines, truncate("Destination: "+format.SafeText(state.path), w))
	switch {
	case state.loading:
		lines = append(lines, badgeStyle.Render("Loading directories and capacity…"))
	case state.err != nil:
		lines = append(lines, errStyle.Render(format.SafeText(state.err.Error())))
	default:
		capacity := fmt.Sprintf("available %s · pressure %.1f%%", formatVolumeBytes(state.volume.Available), state.volume.CallerPressurePct)
		if m.volume.Device != "" && state.volume.Device != "" && m.volume.Device != state.volume.Device {
			capacity = "cross-device · " + capacity
		}
		if state.volume.Inodes > 0 {
			capacity += fmt.Sprintf(" · inodes %.1f%%", state.volume.InodePct)
		}
		lines = append(lines, dimStyle.Render(capacity), dimStyle.Render("Selected target: "+format.SafeText(m.selectedDestination())))
	}
	lines = append(lines, "")
	if len(state.entries) == 0 && !state.loading {
		lines = append(lines, dimStyle.Render("(no child directories; Tab selects current directory)"))
	}
	room := max(0, avail-len(lines)-2)
	start := 0
	if state.cursor >= room && room > 0 {
		start = state.cursor - room + 1
	}
	for i := start; i < min(len(state.entries), start+room); i++ {
		line := truncate("  ▸ "+format.SafeText(state.entries[i].name)+"/", w)
		if i == state.cursor {
			line = cursorBg.Render(line)
		}
		lines = append(lines, line)
	}
	lines = append(lines, "", "Manual destination: "+m.managementInput+"█")
	return renderBoundedLines(lines, w, avail)
}

func destinationHelp() string {
	return "↑/↓ select · Enter/→ open · ←/Backspace parent · Tab choose current · type manual path · Esc cancel"
}

func renderBoundedLines(lines []string, width, height int) string {
	if len(lines) > height {
		lines = lines[:height]
	}
	for i := range lines {
		lines[i] = truncate(lines[i], width)
	}
	return strings.Join(lines, "\n")
}

func openDeletedCapability(result diagnose.Result) *diagnose.Capability {
	for i := range result.Capabilities {
		if result.Capabilities[i].Name == "open_deleted" {
			return &result.Capabilities[i]
		}
	}
	if len(result.Capabilities) > 0 {
		return &result.Capabilities[0]
	}
	return nil
}

func signedBytes(value int64) string {
	if value < 0 {
		if value == -maxSignedInt64-1 {
			return "-9223372036854775808B"
		}
		return "-" + format.Bytes(-value)
	}
	return "+" + format.Bytes(value)
}

func formatVolumeBytes(value uint64) string {
	if value > uint64(^uint64(0)>>1) {
		return fmt.Sprintf("%dB", value)
	}
	return format.Bytes(int64(value))
}

func ageSince(instant time.Time) string {
	if instant.IsZero() {
		return unknownLabel
	}
	age := time.Since(instant)
	if age < 0 {
		age = 0
	}
	return format.Age(age)
}
