package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	agentskills "github.com/phillipod/go-dirstat/internal/skills"
)

func TestSkillsViewPrintsDefinitionWithoutInstalling(t *testing.T) {
	want, err := agentskills.Definition(agentskills.ProfileReadOnly, "")
	if err != nil {
		t.Fatal(err)
	}
	out, err := executeCLI("skills", "view")
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Fatalf("view output:\n%s\nwant:\n%s", out, want)
	}

	admin, err := executeCLI("skills", "view", "--profile=administrator", "--rule=Delete only files named by the user.")
	if err != nil || !strings.Contains(admin, "dirstat-administrator") || !strings.Contains(admin, "Delete only files named by the user.") {
		t.Fatalf("administrator view = %q, %v", admin, err)
	}
}

func TestSkillsProjectInstallStatusAndRemove(t *testing.T) {
	project := t.TempDir()
	installed, err := executeCLI("skills", "install", "--scope=project", "--project-dir", project, "--profile=read-only")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"codex\tread-only\tinstalled", "claude\tread-only\tinstalled"} {
		if !strings.Contains(installed, target) {
			t.Fatalf("install output missing %q:\n%s", target, installed)
		}
	}
	for _, path := range []string{
		filepath.Join(project, ".codex", "skills", "dirstat", "SKILL.md"),
		filepath.Join(project, ".claude", "skills", "dirstat", "SKILL.md"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("installed skill missing at %q: %v", path, err)
		}
	}

	status, err := executeCLI("skills", "status", "claude", "--scope=project", "--project-dir", project)
	if err != nil || !strings.Contains(status, "claude\tread-only\tinstalled") {
		t.Fatalf("status = %q, %v", status, err)
	}
	removed, err := executeCLI("skills", "remove", "--scope=project", "--project-dir", project)
	if err != nil || strings.Count(removed, "\tremoved\t") != 2 {
		t.Fatalf("remove = %q, %v", removed, err)
	}
}

func TestSkillsOperatorAdministratorAndExplicitPath(t *testing.T) {
	project := t.TempDir()
	operatorPath := filepath.Join(t.TempDir(), "operator", "SKILL.md")
	if _, err := executeCLI("skills", "install", "codex", "--profile=operator", "--codex-path", operatorPath); err != nil {
		t.Fatal(err)
	}
	operator, err := os.ReadFile(operatorPath)
	if err != nil || !strings.Contains(string(operator), "dirstat-operator") {
		t.Fatalf("operator skill = %q, %v", operator, err)
	}

	rulesPath := filepath.Join(project, "admin-policy.md")
	if err := os.WriteFile(rulesPath, []byte("- Delete only files in /srv/archive older than 30 days."), 0o600); err != nil {
		t.Fatal(err)
	}
	adminOut, err := executeCLI("skills", "install", "claude", "--scope=project", "--project-dir", project,
		"--profile=administrator", "--rules-file", rulesPath)
	if err != nil || !strings.Contains(adminOut, "claude\tadministrator\tinstalled") {
		t.Fatalf("administrator install = %q, %v", adminOut, err)
	}
	adminPath := filepath.Join(project, ".claude", "skills", "dirstat-administrator", "SKILL.md")
	admin, err := os.ReadFile(adminPath)
	if err != nil || !strings.Contains(string(admin), "older than 30 days") {
		t.Fatalf("administrator skill = %q, %v", admin, err)
	}
}

func TestSkillsInstallAllProfilesWithAdministratorRules(t *testing.T) {
	project := t.TempDir()
	out, err := executeCLI("skills", "install", "--scope=project", "--project-dir", project,
		"--profile=all", "--rule=Delete only files named in the incident ticket.")
	if err != nil || strings.Count(out, "\tinstalled\t") != 6 {
		t.Fatalf("install all = %q, %v", out, err)
	}
	for _, agentDir := range []string{".codex", ".claude"} {
		for _, skill := range []string{"dirstat", "dirstat-operator", "dirstat-administrator"} {
			path := filepath.Join(project, agentDir, "skills", skill, "SKILL.md")
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("missing %q: %v", path, err)
			}
		}
	}
}

func TestSkillsRejectInvalidProfilesRulesAndPaths(t *testing.T) {
	project := t.TempDir()
	tests := [][]string{
		{"skills", "view", "--profile=all"},
		{"skills", "install", "--profile=administrator", "--scope=project", "--project-dir", project},
		{"skills", "install", "--profile=operator", "--rule=no mutations", "--scope=project", "--project-dir", project},
		{"skills", "install", "codex", "--profile=read-only", "--profile=operator", "--codex-path", filepath.Join(project, "SKILL.md")},
		{"skills", "install", "claude", "--codex-path", filepath.Join(project, "SKILL.md")},
	}
	for _, args := range tests {
		if _, err := executeCLI(args...); err == nil {
			t.Fatalf("Execute(%q) unexpectedly succeeded", args)
		}
	}
}

func TestSkillsRulesTemplateAndNonInteractiveEditorGuidance(t *testing.T) {
	template, err := executeCLI("skills", "rules", "template")
	if err != nil {
		t.Fatal(err)
	}
	if template != string(agentskills.RulesTemplate()) {
		t.Fatalf("template = %q", template)
	}
	guardedTemplate, err := executeCLI("skills", "rules", "template", "--guarded-cleanup")
	if err != nil || guardedTemplate != string(agentskills.RulesTemplateWithGuardedCleanup()) {
		t.Fatalf("guarded template = %q, %v", guardedTemplate, err)
	}
	project := t.TempDir()
	templatePath := filepath.Join(project, "template.md")
	if err := os.WriteFile(templatePath, agentskills.RulesTemplate(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"skills", "view", "--profile=administrator", "--rules-file", templatePath},
		{"skills", "install", "--profile=administrator", "--scope=project", "--project-dir", project},
		{"skills", "install", "--profile=administrator", "--edit-rules", "--scope=project", "--project-dir", project},
		{"skills", "rules", "edit"},
	} {
		if _, err := executeCLI(args...); err == nil {
			t.Fatalf("Execute(%q) unexpectedly succeeded", args)
		}
	}
}

func TestRulesEditorCommandUsesExactArgvAndRejectsSudo(t *testing.T) {
	cmd, err := rulesEditorCommand([]string{"editor", "--flag", "value;touch /tmp/bad"}, "/tmp/a file")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"editor", "--flag", "value;touch /tmp/bad", "/tmp/a file"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("argv = %q, want %q", cmd.Args, want)
	}
	if _, err := rulesEditorCommand([]string{"/usr/bin/sudo", "editor"}, "/tmp/file"); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("sudo editor command was accepted: %v", err)
	}
}

func TestSkillsViewReturnsWriterErrors(t *testing.T) {
	want := errors.New("closed")
	cmd := New()
	cmd.SetArgs([]string{"skills", "view"})
	cmd.SetOut(cliErrorWriter{err: want})
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); !errors.Is(err, want) {
		t.Fatalf("view error = %v, want %v", err, want)
	}
}
