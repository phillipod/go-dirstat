package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	appconfig "github.com/phillipod/go-dirstat/internal/config"
	agentskills "github.com/phillipod/go-dirstat/internal/skills"
)

const maxSkillRulesBytes = 64 << 10

var errAdministratorRulesRequired = errors.New("administrator installation requires at least one rule")

type skillFlags struct {
	scope, projectDir, codexPath, claudePath, rulesFile string
	profiles                                            []string
	rules                                               []string
	force                                               bool
	editRules                                           bool
}

func newSkillsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "View and manage dirstat skills for Codex and Claude",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newSkillViewCommand())
	cmd.AddCommand(newSkillInstallCommand())
	cmd.AddCommand(newSkillStatusCommand())
	cmd.AddCommand(newSkillRemoveCommand())
	cmd.AddCommand(newSkillRulesCommand())
	return cmd
}

func newSkillViewCommand() *cobra.Command {
	flags := skillFlags{}
	cmd := &cobra.Command{
		Use:   "view",
		Short: "Print a dirstat SKILL.md definition without installing it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			profiles, err := agentskills.ParseProfiles(flags.profiles)
			if err != nil {
				return err
			}
			if len(profiles) != 1 {
				return errors.New("skills view requires exactly one --profile")
			}
			rules, err := loadSkillRules(flags, profiles)
			if err != nil {
				return err
			}
			definition, err := agentskills.Definition(profiles[0], rules)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(definition)
			return err
		},
	}
	bindSkillProfileFlags(cmd, &flags)
	return cmd
}

func newSkillInstallCommand() *cobra.Command {
	flags := skillFlags{scope: string(agentskills.ScopeUser), projectDir: "."}
	cmd := &cobra.Command{
		Use:   "install [codex|claude|all]",
		Short: "Install dirstat skill definitions",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			locations, profiles, err := resolveSkillLocations(flags, args)
			if err != nil {
				return err
			}
			rules, err := loadInstallSkillRules(cmd, flags, profiles)
			if err != nil {
				return err
			}
			results, err := agentskills.Install(locations, rules, flags.force)
			if err != nil {
				return err
			}
			return writeSkillResults(cmd, "install", results)
		},
	}
	bindSkillLocationFlags(cmd, &flags)
	bindSkillProfileFlags(cmd, &flags)
	cmd.Flags().BoolVar(&flags.force, "force", false, "replace a changed skill definition")
	cmd.Flags().BoolVar(&flags.editRules, "edit-rules", false, "open the administrator policy template in configured tools.editor")
	return cmd
}

func newSkillRulesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rules",
		Short: "Print or edit an administrator policy template",
		Args:  cobra.NoArgs,
	}
	includeGuardedCleanup := false
	templateCmd := &cobra.Command{
		Use:   "template",
		Short: "Print the non-authorizing administrator policy template",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			template := agentskills.RulesTemplate()
			if includeGuardedCleanup {
				template = agentskills.RulesTemplateWithGuardedCleanup()
			}
			_, err := cmd.OutOrStdout().Write(template)
			return err
		},
	}
	templateCmd.Flags().BoolVar(&includeGuardedCleanup, "guarded-cleanup", false, "include built-in plan, dry-run, audit, and symlink safeguards")
	cmd.AddCommand(templateCmd)
	cmd.AddCommand(&cobra.Command{
		Use:   "edit",
		Short: "Edit the administrator policy template and print the resulting rules",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rules, err := editAdministratorRules(cmd)
			if err != nil {
				return err
			}
			_, err = io.WriteString(cmd.OutOrStdout(), rules)
			return err
		},
	})
	return cmd
}

func newSkillStatusCommand() *cobra.Command {
	flags := skillFlags{scope: string(agentskills.ScopeUser), projectDir: "."}
	cmd := &cobra.Command{
		Use:   "status [codex|claude|all]",
		Short: "Show dirstat skill installation status",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			locations, _, err := resolveSkillLocations(flags, args)
			if err != nil {
				return err
			}
			results, err := agentskills.Status(locations)
			if err != nil {
				return err
			}
			return writeSkillResults(cmd, "status", results)
		},
	}
	bindSkillLocationFlags(cmd, &flags)
	bindSkillProfileFlags(cmd, &flags)
	return cmd
}

func newSkillRemoveCommand() *cobra.Command {
	flags := skillFlags{scope: string(agentskills.ScopeUser), projectDir: "."}
	cmd := &cobra.Command{
		Use:   "remove [codex|claude|all]",
		Short: "Remove dirstat skill definitions",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			locations, _, err := resolveSkillLocations(flags, args)
			if err != nil {
				return err
			}
			results, err := agentskills.Remove(locations, flags.force)
			if err != nil {
				return err
			}
			return writeSkillResults(cmd, "remove", results)
		},
	}
	bindSkillLocationFlags(cmd, &flags)
	bindSkillProfileFlags(cmd, &flags)
	cmd.Flags().BoolVar(&flags.force, "force", false, "remove a changed skill definition")
	return cmd
}

func bindSkillProfileFlags(cmd *cobra.Command, flags *skillFlags) {
	cmd.Flags().StringArrayVar(&flags.profiles, "profile", nil, "skill profile: read-only|operator|administrator|all (repeatable; default read-only)")
	cmd.Flags().StringArrayVar(&flags.rules, "rule", nil, "administrator policy rule (repeatable)")
	cmd.Flags().StringVar(&flags.rulesFile, "rules-file", "", "read administrator policy rules from this UTF-8 file")
}

func bindSkillLocationFlags(cmd *cobra.Command, flags *skillFlags) {
	cmd.Flags().StringVar(&flags.scope, "scope", string(agentskills.ScopeUser), "install scope: user|project")
	cmd.Flags().StringVar(&flags.projectDir, "project-dir", ".", "project root for --scope=project")
	cmd.Flags().StringVar(&flags.codexPath, "codex-path", "", "exact Codex SKILL.md destination")
	cmd.Flags().StringVar(&flags.claudePath, "claude-path", "", "exact Claude SKILL.md destination")
}

func resolveSkillLocations(flags skillFlags, values []string) ([]agentskills.Location, []agentskills.Profile, error) {
	targets, err := agentskills.ParseTargets(values)
	if err != nil {
		return nil, nil, err
	}
	profiles, err := agentskills.ParseProfiles(flags.profiles)
	if err != nil {
		return nil, nil, err
	}
	if flags.codexPath != "" && !hasSkillTarget(targets, agentskills.TargetCodex) {
		return nil, nil, errors.New("--codex-path requires the codex target")
	}
	if flags.claudePath != "" && !hasSkillTarget(targets, agentskills.TargetClaude) {
		return nil, nil, errors.New("--claude-path requires the claude target")
	}
	if (flags.codexPath != "" || flags.claudePath != "") && len(profiles) != 1 {
		return nil, nil, errors.New("explicit skill paths require exactly one --profile")
	}

	needsHome := agentskills.Scope(flags.scope) == agentskills.ScopeUser &&
		((hasSkillTarget(targets, agentskills.TargetCodex) && flags.codexPath == "") ||
			(hasSkillTarget(targets, agentskills.TargetClaude) && flags.claudePath == ""))
	home := ""
	if needsHome {
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, nil, fmt.Errorf("resolve user home for skills: %w", err)
		}
	}
	locations, err := agentskills.Resolve(agentskills.ResolveOptions{
		Targets: targets, Profiles: profiles, Scope: agentskills.Scope(flags.scope), Home: home,
		ProjectDir: flags.projectDir, CodexPath: flags.codexPath, ClaudePath: flags.claudePath,
	})
	if err != nil {
		return nil, nil, err
	}
	return locations, profiles, nil
}

func hasSkillTarget(targets []agentskills.Target, want agentskills.Target) bool {
	for _, target := range targets {
		if target == want {
			return true
		}
	}
	return false
}

func loadSkillRules(flags skillFlags, profiles []agentskills.Profile) (string, error) {
	needsRules := false
	for _, profile := range profiles {
		if profile == agentskills.ProfileAdministrator {
			needsRules = true
			break
		}
	}
	if flags.rulesFile != "" && len(flags.rules) > 0 {
		return "", errors.New("--rules-file and --rule cannot be used together")
	}
	if !needsRules {
		if flags.rulesFile != "" || len(flags.rules) > 0 {
			return "", errors.New("administrator rules require --profile=administrator")
		}
		return "", nil
	}
	if flags.rulesFile != "" {
		return readSkillRulesFile(flags.rulesFile)
	}
	rules := strings.Join(flags.rules, "\n")
	if strings.TrimSpace(rules) == "" {
		return "", errAdministratorRulesRequired
	}
	return rules, nil
}

func loadInstallSkillRules(cmd *cobra.Command, flags skillFlags, profiles []agentskills.Profile) (string, error) {
	rules, err := loadSkillRules(flags, profiles)
	if !errors.Is(err, errAdministratorRulesRequired) {
		if err == nil && flags.editRules {
			return "", errors.New("--edit-rules requires --profile=administrator")
		}
		return rules, err
	}
	if flags.editRules {
		return editInstallAdministratorRules(cmd)
	}
	if !commandIsInteractive(cmd) {
		return "", errors.New("administrator installation requires --rule, --rules-file, or --edit-rules in an interactive terminal")
	}
	config, err := appconfig.Load()
	if err != nil {
		return "", fmt.Errorf("load dirstat configuration: %w", err)
	}
	if len(config.Tools.Editor) == 0 {
		return "", errors.New("administrator installation needs --rule or --rules-file; configure tools.editor to use the interactive policy template")
	}
	confirmed, err := confirmRulesEditor(cmd)
	if err != nil {
		return "", err
	}
	if !confirmed {
		return "", errAdministratorRulesRequired
	}
	return editInstallAdministratorRulesWithCommand(cmd, config.Tools.Editor)
}

func confirmRulesEditor(cmd *cobra.Command) (bool, error) {
	if _, err := fmt.Fprint(cmd.OutOrStdout(), "No administrator rules were supplied. Create a policy from the dirstat template now? [y/N] "); err != nil {
		return false, err
	}
	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read policy editor confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func editAdministratorRules(cmd *cobra.Command) (string, error) {
	if !commandIsInteractive(cmd) {
		return "", errors.New("administrator rules editor requires an interactive terminal")
	}
	config, err := appconfig.Load()
	if err != nil {
		return "", fmt.Errorf("load dirstat configuration: %w", err)
	}
	return editAdministratorRulesWithCommand(cmd, config.Tools.Editor, agentskills.RulesTemplate())
}

func editInstallAdministratorRules(cmd *cobra.Command) (string, error) {
	if !commandIsInteractive(cmd) {
		return "", errors.New("administrator rules editor requires an interactive terminal")
	}
	config, err := appconfig.Load()
	if err != nil {
		return "", fmt.Errorf("load dirstat configuration: %w", err)
	}
	return editInstallAdministratorRulesWithCommand(cmd, config.Tools.Editor)
}

func editInstallAdministratorRulesWithCommand(cmd *cobra.Command, argv []string) (string, error) {
	includeGuardedCleanup, err := confirmGuardedCleanupRules(cmd)
	if err != nil {
		return "", err
	}
	template := agentskills.RulesTemplate()
	if includeGuardedCleanup {
		template = agentskills.RulesTemplateWithGuardedCleanup()
	}
	return editAdministratorRulesWithCommand(cmd, argv, template)
}

func confirmGuardedCleanupRules(cmd *cobra.Command) (bool, error) {
	if _, err := fmt.Fprint(cmd.OutOrStdout(), "Enable the built-in guarded-cleanup policy rules (plan, dry-run, audit, and symlink safeguards)? [y/N] "); err != nil {
		return false, err
	}
	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read guarded-cleanup confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func editAdministratorRulesWithCommand(cmd *cobra.Command, argv []string, template []byte) (rules string, err error) {
	if !commandIsInteractive(cmd) {
		return "", errors.New("administrator rules editor requires an interactive terminal")
	}
	dir, err := os.MkdirTemp("", "dirstat-rules-")
	if err != nil {
		return "", fmt.Errorf("create administrator rules workspace: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(dir); cleanupErr != nil && err == nil {
			rules, err = "", fmt.Errorf("remove administrator rules workspace: %w", cleanupErr)
		}
	}()
	path := filepath.Join(dir, "administrator-policy.md")
	if err := os.WriteFile(path, template, 0o600); err != nil {
		return "", fmt.Errorf("write administrator rules template: %w", err)
	}
	editor, err := rulesEditorCommand(argv, path)
	if err != nil {
		return "", err
	}
	editor.Stdin = cmd.InOrStdin()
	editor.Stdout = cmd.OutOrStdout()
	editor.Stderr = cmd.ErrOrStderr()
	if err := editor.Run(); err != nil {
		return "", fmt.Errorf("run administrator rules editor: %w", err)
	}
	rules, err = readSkillRulesFile(path)
	if err != nil {
		return "", err
	}
	rules, err = agentskills.NormalizeAdministratorRules(rules)
	if err != nil {
		return "", err
	}
	return rules, nil
}

func rulesEditorCommand(argv []string, path string) (*exec.Cmd, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return nil, errors.New("administrator rules editor: no command configured")
	}
	if strings.EqualFold(filepath.Base(argv[0]), "sudo") {
		return nil, errors.New("administrator rules editor: sudo is not permitted")
	}
	if path == "" {
		return nil, errors.New("administrator rules editor: no template path")
	}
	args := append(append([]string(nil), argv[1:]...), path)
	return exec.Command(argv[0], args...), nil
}

func commandIsInteractive(cmd *cobra.Command) bool {
	return writerIsTTY(cmd.InOrStdin()) && writerIsTTY(cmd.OutOrStdout()) && writerIsTTY(cmd.ErrOrStderr())
}

func readSkillRulesFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open administrator rules file: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSkillRulesBytes+1))
	if err != nil {
		_ = f.Close()
		return "", fmt.Errorf("read administrator rules file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close administrator rules file: %w", err)
	}
	if len(data) > maxSkillRulesBytes {
		return "", errors.New("administrator rules must not exceed 64 KiB")
	}
	return string(data), nil
}

func writeSkillResults(cmd *cobra.Command, action string, results []agentskills.Result) error {
	for _, result := range results {
		state := string(result.State)
		switch action {
		case "install":
			if result.Changed {
				state = "installed"
			} else {
				state = "unchanged"
			}
		case "remove":
			if result.Changed {
				state = "removed"
			} else {
				state = "absent"
			}
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", result.Location.Target, result.Location.Profile, state, result.Location.Path); err != nil {
			return err
		}
	}
	return nil
}
