package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/phillipod/go-dirstat/internal/format"
	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/preview"
)

const inspectKindFile = "file"

func newInspectCommand() *cobra.Command {
	var output string
	var content, tail, rawContent bool
	var limit int64
	cmd := &cobra.Command{
		Use:   "inspect PATH",
		Short: "Inspect metadata and bounded file content",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "text" && output != "json" {
				return fmt.Errorf("invalid --format %q: expected text or json", output)
			}
			if limit <= 0 {
				return fmt.Errorf("--limit must be greater than zero")
			}
			if tail && !content {
				return fmt.Errorf("--tail requires --content")
			}
			if rawContent && !content {
				return fmt.Errorf("--raw-content requires --content")
			}
			if rawContent && output != "text" {
				return fmt.Errorf("--raw-content is only valid with --format=text; JSON is already lossless")
			}
			e, err := fsinfo.Inspect(args[0], false)
			if err != nil {
				return err
			}
			var p *preview.Result
			if content && e.Kind == inspectKindFile {
				got, err := preview.Read(e.Path, preview.Options{Limit: limit, Tail: tail})
				if err != nil {
					return err
				}
				p = &got
			}
			if output == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					Entry   fsinfo.Entry    `json:"entry"`
					Preview *preview.Result `json:"preview,omitempty"`
				}{e, p})
			}
			if rawContent {
				if p == nil {
					return fmt.Errorf("--raw-content requires a regular file")
				}
				_, err = cmd.OutOrStdout().Write(p.Data)
				return err
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\n  type: %s\n  size: %d\n  allocated: %d\n  mode: %s\n  modified: %s\n  owner: %s:%s\n  links: %d\n",
				format.SafeText(e.Path), format.SafeText(e.Kind), e.Size, e.Allocated, format.SafeText(e.ModeText),
				e.ModTime.Format("2006-01-02T15:04:05Z07:00"), format.SafeText(e.Owner), format.SafeText(e.Group), e.Links); err != nil {
				return err
			}
			if p != nil {
				body := p.Text
				if p.Binary {
					body = p.Hex
				} else {
					body = format.SafeText(body)
				}
				_, err = fmt.Fprint(cmd.OutOrStdout(), body)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&output, "format", "text", "output format: text|json")
	cmd.Flags().BoolVar(&content, "content", false, "include bounded regular-file content")
	cmd.Flags().BoolVar(&tail, "tail", false, "read from the end of the file")
	cmd.Flags().BoolVar(&rawContent, "raw-content", false, "write preview bytes without terminal escaping (text output only)")
	cmd.Flags().Int64Var(&limit, "limit", preview.DefaultLimit, "maximum content bytes")
	return cmd
}
