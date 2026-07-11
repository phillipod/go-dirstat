package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/phillipod/go-dirstat/internal/version"
)

// New returns the root cobra command for dirstat. Flags are bound once here and
// shared by the subcommands that need them via the persistent flag set.
func New() *cobra.Command {
	cfg := newConfig()

	root := &cobra.Command{
		Use:   "dirstat [path...]",
		Short: "Analyze disk usage and apply guarded cleanup plans",
		Long: `dirstat is a terminal disk-usage analyzer and guarded space manager.
It measures directory trees, reports capacity and growth, finds scriptable
cleanup candidates, and provides a full-screen interactive TUI. The default
listing is rich text for people; TSV, JSONL, JSON, and NUL-delimited command
surfaces support shell and operations automation.

Scanning is concurrent (GOMAXPROCS workers) and, where device identity is
available, stays on one filesystem by default. It skips /proc, /sys, /dev and
/run unless explicitly requested otherwise. Filesystem changes require a
reviewable plan and explicit confirmation, are confined to the plan root,
revalidate source metadata, and run only with the caller's privileges.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg.Paths = args
			return runText(cmd, cfg)
		},
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       version.Info(),
	}
	root.SetVersionTemplate("dirstat {{.Version}}\n")
	bindFlags(root, cfg)

	root.AddCommand(newTUICommand(cfg))
	root.AddCommand(newExtensionsCommand(cfg))
	root.AddCommand(newStatusCommand())
	root.AddCommand(newInspectCommand())
	root.AddCommand(newQueryCommand(cfg))
	root.AddCommand(newDiagnoseCommand())
	root.AddCommand(newHistoryCommand(cfg))
	root.AddCommand(newPlanCommand())
	root.AddCommand(newApplyCommand())
	root.AddCommand(newVersionCmd())
	return root
}

// bindFlags attaches every flag to cfg. Persistent flags are inherited by
// subcommands so shared validation runs before every scan or TUI launch.
func bindFlags(root *cobra.Command, cfg *Config) {
	pf := root.PersistentFlags()

	// Scope / safety.
	pf.BoolVar(&cfg.CrossDevice, "cross-device", false, "cross filesystem boundaries")
	pf.BoolVarP(&cfg.OneFileSystem, "one-file-system", "x", false, "stay on one filesystem (explicit default, like du -x)")
	pf.BoolVarP(&cfg.Follow, "follow", "L", false, "follow symlinked directories (with loop protection)")
	pf.BoolVar(&cfg.NoVirtual, "no-virtual-exclude", false, "do NOT skip /proc /sys /dev /run and kernel pseudo-filesystems")
	pf.BoolVar(&cfg.NoHidden, "no-hidden", false, "skip dotfile entries")
	pf.StringArrayVar(&cfg.Exclude, "exclude", nil, "exclude basename/path glob (repeatable), du --exclude style")
	pf.StringArrayVar(&cfg.ExcludePath, "exclude-path", nil, "exclude absolute path prefix (repeatable)")
	pf.StringArrayVar(&cfg.IncludePath, "include-path", nil, "scan ONLY these path prefixes (repeatable)")
	pf.StringArrayVar(&cfg.IncludeFS, "include-fs", nil, "include ONLY these filesystem types (Linux/macOS; repeatable)")
	pf.StringArrayVar(&cfg.ExcludeFS, "exclude-fs", nil, "exclude these filesystem types (Linux/macOS; repeatable)")
	pf.StringVar(&cfg.MinSize, "min-size", "", "skip files with logical size smaller than SIZE (e.g. 10M, 1G)")
	pf.StringVar(&cfg.MaxSize, "max-size", "", "skip files with logical size larger than SIZE")
	pf.IntVarP(&cfg.Jobs, "jobs", "j", 0, "maximum concurrent scan workers (default GOMAXPROCS, max 4096)")
	pf.BoolVarP(&cfg.Apparent, "apparent", "A", false, "use apparent file size (default: on-disk, like du)")

	// Output shaping belongs to the default text command. Subcommands bind only
	// the subset they can honor, keeping their help and behavior aligned.
	f := root.Flags()
	f.IntVarP(&cfg.Depth, "depth", "d", 0, "max directory depth to print (0 = unlimited)")
	f.IntVarP(&cfg.Limit, "limit", "n", 0, "max entries shown per directory (0 = unlimited)")
	f.StringVarP(&cfg.Sort, "sort", "s", "size", "sort: size|size-asc|count|mtime|name")
	f.BoolVarP(&cfg.ByExt, "by-ext", "e", false, "show a by-extension breakdown instead of the tree")
	f.BoolVarP(&cfg.Files, "files", "a", false, "list individual files too (du -a); default shows directories only")
	f.StringVar(&cfg.Format, "format", "text", "output format: text|tsv")
	bindRichOutputFlags(root, cfg)
}

func bindRichOutputFlags(cmd *cobra.Command, cfg *Config) {
	f := cmd.Flags()
	f.BoolVar(&cfg.Bytes, "bytes", false, "print raw byte counts instead of human units")
	f.BoolVar(&cfg.NoBar, "no-bar", false, "hide proportional bars")
	f.BoolVar(&cfg.NoCol, "no-color", false, "disable ANSI color (auto-disabled when piping)")
	f.BoolVar(&cfg.NoCt, "no-counts", false, "hide file/dir counts")
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the dirstat version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), versionLine())
			return err
		},
	}
}
