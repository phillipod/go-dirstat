package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/preview"
)

func newInspectCommand() *cobra.Command {
	var output string
	var content, tail bool
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
			e, err := fsinfo.Inspect(args[0], false)
			if err != nil {
				return err
			}
			var p *preview.Result
			if content && e.Kind == "file" {
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
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\n  type: %s\n  size: %d\n  allocated: %d\n  mode: %s\n  modified: %s\n  owner: %s:%s\n  links: %d\n",
				e.Path, e.Kind, e.Size, e.Allocated, e.ModeText, e.ModTime.Format("2006-01-02T15:04:05Z07:00"), e.Owner, e.Group, e.Links); err != nil {
				return err
			}
			if p != nil {
				body := p.Text
				if p.Binary {
					body = p.Hex
				}
				_, err = fmt.Fprint(cmd.OutOrStdout(), body)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&output, "format", "text", "output format: text|json")
	cmd.Flags().BoolVar(&content, "content", false, "include bounded regular-file content")
	cmd.Flags().BoolVar(&tail, "tail", false, "read from the end of the file")
	cmd.Flags().Int64Var(&limit, "limit", preview.DefaultLimit, "maximum content bytes")
	return cmd
}
