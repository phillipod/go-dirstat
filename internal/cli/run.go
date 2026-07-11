package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/phillipod/go-dirstat/internal/agg"
	"github.com/phillipod/go-dirstat/internal/format"
	"github.com/phillipod/go-dirstat/internal/render"
	"github.com/phillipod/go-dirstat/internal/scan"
	"github.com/phillipod/go-dirstat/internal/version"
)

// runText is the default execution path: scan each path, then render the tree
// (or extension breakdown) plus a summary footer.
func runText(cmd *cobra.Command, c *Config) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	policy, err := c.policy()
	if err != nil {
		return err
	}
	opts := scan.Options{Policy: policy, Concurrency: c.Jobs}

	paths := c.Paths
	if len(paths) == 0 {
		paths = []string{"."}
	}
	topts := c.textOptions(out)
	multiple := len(paths) > 1

	for i, p := range paths {
		if c.Format == "text" && multiple && i > 0 {
			if _, err := fmt.Fprintln(out); err != nil {
				return fmt.Errorf("write output separator: %w", err)
			}
		}
		if c.Format == "text" && multiple {
			if _, err := fmt.Fprintf(out, "%s:\n", format.SafeText(p)); err != nil {
				return fmt.Errorf("write root heading: %w", err)
			}
		}
		root, stats, err := scan.Scan(ctx, p, opts)
		if err != nil {
			return fmt.Errorf("%q: %w", p, err)
		}
		if c.Format == "tsv" {
			if err := render.TSV(out, root, p, topts); err != nil {
				return fmt.Errorf("render %q: %w", p, err)
			}
			continue
		}
		if c.ByExt {
			if err := render.Extensions(out, agg.Extensions(root, c.sizeMode()), topts); err != nil {
				return fmt.Errorf("render extensions for %q: %w", p, err)
			}
			if err := render.TopFiles(out, agg.TopFiles(root, c.sizeMode(), 10), c.sizeMode(), topts); err != nil {
				return fmt.Errorf("render largest files for %q: %w", p, err)
			}
		} else {
			if err := render.Tree(out, root, topts); err != nil {
				return fmt.Errorf("render tree for %q: %w", p, err)
			}
		}
		if err := render.Summary(out, root, summaryData(stats), topts); err != nil {
			return fmt.Errorf("render summary for %q: %w", p, err)
		}
	}
	return nil
}

func summaryData(s scan.Stats) render.SummaryData {
	return render.SummaryData{
		Files:   s.Files,
		Dirs:    s.Dirs,
		Errors:  s.Errors,
		Elapsed: s.Elapsed.Round(time.Millisecond).String(),
		RootFS:  s.RootFS,
	}
}

func versionLine() string {
	return "dirstat " + version.Info()
}
