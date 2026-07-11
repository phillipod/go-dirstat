package skills

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefinitionsArePortableAndTamperEvident(t *testing.T) {
	for _, tc := range []struct {
		profile Profile
		rules   string
		name    string
	}{
		{profile: ProfileReadOnly, name: "read only"},
		{profile: ProfileOperator, name: "operator"},
		{profile: ProfileAdministrator, rules: "- Delete only files under /srv/archive.\n- Keep audit logging enabled.", name: "administrator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			definition, err := Definition(tc.profile, tc.rules)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(definition, []byte("---\nname: ")) || !bytes.Contains(definition, []byte("dirstat-managed")) {
				t.Fatalf("definition does not look like a managed SKILL.md:\n%s", definition)
			}
			if tc.profile == ProfileAdministrator && !bytes.Contains(definition, []byte(tc.rules)) {
				t.Fatal("administrator rules were not copied into the definition")
			}
			for _, command := range []string{
				"dirstat status --format=json /srv",
				"dirstat diagnose --format=json /srv",
				"dirstat extensions /srv",
				"dirstat query --kind=file --min-size=1G --format=tsv",
				"dirstat inspect --format=json /srv/archive/old.log",
				"dirstat history growth /srv",
			} {
				if !bytes.Contains(definition, []byte(command)) {
					t.Fatalf("definition missing command guide %q:\n%s", command, definition)
				}
			}
			if got := managedState(definition, tc.profile); got != StateInstalled {
				t.Fatalf("managed state = %q, want installed", got)
			}
			modified := append([]byte(nil), definition...)
			modified[len(modified)-2] ^= 1
			if got := managedState(modified, tc.profile); got != StateModified {
				t.Fatalf("modified state = %q, want modified", got)
			}
		})
	}

	if _, err := Definition(ProfileAdministrator, ""); err == nil || !strings.Contains(err.Error(), "requires at least one rule") {
		t.Fatalf("missing administrator rules error = %v", err)
	}
}

func TestAdministratorRulesTemplateMustBeEdited(t *testing.T) {
	template := string(RulesTemplate())
	if _, err := NormalizeAdministratorRules(template); err == nil || !strings.Contains(err.Error(), "must be edited") {
		t.Fatalf("unchanged template error = %v", err)
	}
	rules := template + "\n- Delete only files under /srv/archive older than 30 days.\n"
	normalized, err := NormalizeAdministratorRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(normalized, "older than 30 days") || !strings.HasSuffix(normalized, "\n") {
		t.Fatalf("normalized rules = %q", normalized)
	}
	guarded, err := NormalizeAdministratorRules(string(RulesTemplateWithGuardedCleanup()))
	if err != nil || !strings.Contains(guarded, "dirstat apply --dry-run") {
		t.Fatalf("guarded template = %q, %v", guarded, err)
	}
}

func TestParseTargetsAndProfiles(t *testing.T) {
	targets, err := ParseTargets([]string{"claude", "codex", "claude"})
	if err != nil || len(targets) != 2 || targets[0] != TargetCodex || targets[1] != TargetClaude {
		t.Fatalf("targets = %#v, %v", targets, err)
	}
	profiles, err := ParseProfiles([]string{"operator", "read-only"})
	if err != nil || len(profiles) != 2 || profiles[0] != ProfileReadOnly || profiles[1] != ProfileOperator {
		t.Fatalf("profiles = %#v, %v", profiles, err)
	}
	for _, values := range [][]string{{"all", "codex"}, {"unknown"}} {
		if _, err := ParseTargets(values); err == nil {
			t.Fatalf("ParseTargets(%q) unexpectedly succeeded", values)
		}
	}
	for _, values := range [][]string{{"all", "operator"}, {"unknown"}} {
		if _, err := ParseProfiles(values); err == nil {
			t.Fatalf("ParseProfiles(%q) unexpectedly succeeded", values)
		}
	}
}

func TestResolveScopesAndExplicitPaths(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	locations, err := Resolve(ResolveOptions{
		Targets: []Target{TargetCodex, TargetClaude}, Profiles: []Profile{ProfileReadOnly, ProfileOperator},
		Scope: ScopeUser, Home: home, ProjectDir: project,
	})
	if err != nil || len(locations) != 4 {
		t.Fatalf("user locations = %#v, %v", locations, err)
	}
	paths := map[string]bool{}
	for _, location := range locations {
		paths[location.Path] = true
	}
	for _, want := range []string{
		filepath.Join(home, ".codex", "skills", "dirstat", "SKILL.md"),
		filepath.Join(home, ".claude", "skills", "dirstat-operator", "SKILL.md"),
	} {
		if !paths[want] {
			t.Fatalf("resolved paths missing %q: %#v", want, locations)
		}
	}

	explicit := filepath.Join(t.TempDir(), "custom", "SKILL.md")
	locations, err = Resolve(ResolveOptions{
		Targets: []Target{TargetCodex}, Profiles: []Profile{ProfileAdministrator}, Scope: ScopeProject,
		ProjectDir: project, CodexPath: explicit,
	})
	if err != nil || len(locations) != 1 || locations[0].Path != explicit {
		t.Fatalf("explicit locations = %#v, %v", locations, err)
	}
	if _, err := Resolve(ResolveOptions{
		Targets: []Target{TargetCodex, TargetClaude}, Profiles: []Profile{ProfileReadOnly}, Scope: ScopeProject,
		CodexPath: explicit, ClaudePath: explicit,
	}); err == nil || !strings.Contains(err.Error(), "both resolve") {
		t.Fatalf("duplicate explicit path error = %v", err)
	}
}

func TestInstallStatusAndRemoveProtectChangedDefinitions(t *testing.T) {
	root := t.TempDir()
	readOnly := Location{Target: TargetCodex, Profile: ProfileReadOnly, Path: filepath.Join(root, "codex", "SKILL.md")}
	admin := Location{Target: TargetClaude, Profile: ProfileAdministrator, Path: filepath.Join(root, "claude", "SKILL.md")}

	results, err := Install([]Location{readOnly, admin}, "- Delete only files under /srv/archive.", false)
	if err != nil || len(results) != 2 || !results[0].Changed || !results[1].Changed {
		t.Fatalf("install = %#v, %v", results, err)
	}
	status, err := Status([]Location{readOnly, admin})
	if err != nil || status[0].State != StateInstalled || status[1].State != StateInstalled {
		t.Fatalf("status = %#v, %v", status, err)
	}
	updatedRules := "- Delete only files under /srv/archive older than 30 days."
	updated, err := Install([]Location{admin}, updatedRules, false)
	if err != nil || len(updated) != 1 || !updated[0].Changed {
		t.Fatalf("administrator policy update = %#v, %v", updated, err)
	}
	data, err := os.ReadFile(admin.Path)
	if err != nil || !bytes.Contains(data, []byte(updatedRules)) {
		t.Fatalf("updated administrator policy = %q, %v", data, err)
	}
	if err := os.WriteFile(readOnly.Path, []byte("user edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install([]Location{readOnly}, "", false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("protected install error = %v", err)
	}
	if _, err := Remove([]Location{readOnly}, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("protected remove error = %v", err)
	}
	if _, err := Install([]Location{readOnly}, "", true); err != nil {
		t.Fatalf("forced install: %v", err)
	}
	if _, err := Remove([]Location{readOnly, admin}, false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	status, err = Status([]Location{readOnly, admin})
	if err != nil || status[0].State != StateMissing || status[1].State != StateMissing {
		t.Fatalf("post-remove status = %#v, %v", status, err)
	}
}

func TestInstallPreflightsAllModifiedFiles(t *testing.T) {
	root := t.TempDir()
	modified := Location{Target: TargetCodex, Profile: ProfileReadOnly, Path: filepath.Join(root, "modified", "SKILL.md")}
	missing := Location{Target: TargetClaude, Profile: ProfileReadOnly, Path: filepath.Join(root, "missing", "SKILL.md")}
	if err := os.MkdirAll(filepath.Dir(modified.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modified.Path, []byte("user edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install([]Location{modified, missing}, "", false); err == nil {
		t.Fatal("install unexpectedly replaced a changed definition")
	}
	if _, err := os.Stat(missing.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight wrote the missing target: %v", err)
	}
}
