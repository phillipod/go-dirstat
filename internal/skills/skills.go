// Package skills manages portable dirstat Agent Skills definitions for local
// coding agents. It deliberately owns only dirstat skill files and never
// enumerates or modifies other agent configuration.
package skills

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Target names one supported agent skill location.
type Target string

const (
	TargetCodex  Target = "codex"
	TargetClaude Target = "claude"
)

// Profile selects the authority granted by a dirstat skill definition.
type Profile string

const (
	ProfileReadOnly      Profile = "read-only"
	ProfileOperator      Profile = "operator"
	ProfileAdministrator Profile = "administrator"
)

// Scope selects the default root for skill installation.
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

// State describes the current relation between a target file and its expected
// generated definition.
type State string

const (
	StateMissing   State = "missing"
	StateInstalled State = "installed"
	StateModified  State = "modified"
)

const rulesTemplate = `# dirstat administrator policy

# This template grants no authority by itself. Replace or add rules that state
# exactly which paths, actions, safeguards, and approvals are permitted.

## Permitted actions

- REPLACE: action, allowed path scope, and selection criteria.

## Required safeguards

- REPLACE: required dry-run, backup, audit, retention, or approval checks.

## Prohibited actions

- REPLACE: paths, actions, or conditions that are never permitted.

## Escalation

- REPLACE: when to stop and request the user's explicit authorization.
`

const guardedCleanupRules = `

## Enabled template: guarded cleanup

- Before a filesystem mutation, create a dirstat plan scoped to the approved root and review every operation.
- Run dirstat apply --dry-run and resolve every failure before a real apply.
- Keep audit logging enabled and record the incident or change reference with the action.
- Do not follow symlinks, cross filesystem boundaries, or weaken dirstat scope protections unless a separate policy rule explicitly permits it.
- Stop and request authorization if an operation differs from the reviewed plan or no longer matches its expected metadata.
`

// Location is one resolved target skill file.
type Location struct {
	Target  Target
	Profile Profile
	Path    string
}

// Result records a completed installation or removal decision.
type Result struct {
	Location Location
	State    State
	Changed  bool
}

// ResolveOptions controls where agent skill files are managed. Explicit paths
// are exact SKILL.md destinations; they take precedence over Scope defaults.
type ResolveOptions struct {
	Targets    []Target
	Profiles   []Profile
	Scope      Scope
	Home       string
	ProjectDir string
	CodexPath  string
	ClaudePath string
}

// Definition returns a portable SKILL.md definition for profile. The
// administrator profile requires non-empty UTF-8 rules, which are copied into
// the definition and become its authority boundary.
func Definition(profile Profile, rules string) ([]byte, error) {
	if err := validateProfile(profile); err != nil {
		return nil, err
	}
	rules, err := normalizedRules(profile, rules)
	if err != nil {
		return nil, err
	}
	prefix := frontmatter(profile)
	payload := body(profile, rules)
	hash := sha256.Sum256(append(append([]byte(nil), prefix...), payload...))
	marker := []byte(fmt.Sprintf("<!-- dirstat-managed: profile=%s sha256=%s -->\n", profile, hex.EncodeToString(hash[:])))
	definition := make([]byte, 0, len(prefix)+len(marker)+len(payload))
	definition = append(definition, prefix...)
	definition = append(definition, marker...)
	definition = append(definition, payload...)
	return definition, nil
}

// RulesTemplate returns the starting point for an administrator policy. It is
// intentionally non-authorizing and must be edited before it can be installed.
func RulesTemplate() []byte { return []byte(rulesTemplate) }

// RulesTemplateWithGuardedCleanup returns the policy template with the
// built-in guarded-cleanup safeguards enabled. The safeguards do not permit a
// filesystem action; a policy still needs an explicit permission for one.
func RulesTemplateWithGuardedCleanup() []byte {
	return []byte(rulesTemplate + guardedCleanupRules)
}

// NormalizeAdministratorRules validates administrator policy text and returns
// the canonical form embedded in a generated skill definition.
func NormalizeAdministratorRules(rules string) (string, error) {
	return normalizedRules(ProfileAdministrator, rules)
}

func frontmatter(profile Profile) []byte {
	name, description := profileMetadata(profile)
	return []byte("---\nname: " + name + "\ndescription: " + description + "\n---\n")
}

func profileMetadata(profile Profile) (string, string) {
	switch profile {
	case ProfileOperator:
		return "dirstat-operator", "Investigate disk pressure and prepare guarded dirstat cleanup plans with dry-run validation."
	case ProfileAdministrator:
		return "dirstat-administrator", "Operate dirstat under the administrator policy embedded in this skill."
	case ProfileReadOnly:
		return "dirstat", "Analyze disk usage and propose safe cleanup with dirstat."
	default:
		return "", ""
	}
}

func body(profile Profile, rules string) []byte {
	common := `# dirstat

Use **dirstat** to establish disk pressure, locate specific candidates, and make a reviewable cleanup decision. Start with **dirstat --help** or **dirstat COMMAND --help** if a flag is uncertain; do not invent flags or parse display output as data.

## How to investigate

1. Establish capacity and inode pressure:

       dirstat status /srv
       dirstat status --format=json /srv
       dirstat diagnose --format=json /srv

   Use **diagnose** when host-level evidence is needed, especially to identify open-but-deleted files that consume space without appearing in the directory tree.
   In JSON, use **caller_pressure_percent** for ordinary-user pressure and **physical_used_percent** for allocated blocks. Treat an incomplete open-deleted total as an observed lower bound; inspect its coverage before making a reclaim claim.

2. Find where space is concentrated. The default command shows a hierarchy; extensions groups space by file type:

       dirstat --depth 3 --limit 25 /srv
       dirstat extensions /srv
       dirstat --apparent --by-ext /srv

   The default is allocated bytes, stays on one filesystem, skips virtual filesystems, and does not follow symlinks. Use **--apparent** for logical file sizes, **--bytes** for raw bytes, **--files** to include individual files, and **--cross-device** or **--follow** only when explicitly required. Narrow a scan with **--exclude**, **--exclude-path**, **--include-path**, **--include-fs**, or **--exclude-fs**.

3. Produce precise candidates with **query**. TSV is headerless and configurable; JSONL is structured; NUL emits only absolute paths and is safest for unusual names:

       dirstat query --kind=file --min-size=1G --format=tsv \
         --fields=path,size,size-human,mtime /srv
       dirstat query --kind=file --extension=log --older-than=30d \
         --sort=size:desc --format=jsonl /srv
       dirstat query --kind=file --path-regexp='\.tmp$' --format=nul /srv

   Add **--metadata** when owner, group, mode, link count, or stable identity belongs in the candidate evidence.
   Use **--limit** for a bounded sorted candidate set. Use **--stream** only when sort order is unnecessary and a low-memory TSV/JSONL/NUL pipeline is more important.

4. Verify a candidate before proposing it. **inspect** reports type, size, allocation, ownership, links, and time; **--content** reads only a bounded preview. **history growth** records a scan and compares it with the prior one:

       dirstat inspect --format=json /srv/archive/old.log
       dirstat inspect --content --limit=65536 /srv/archive/old.log
       dirstat history growth /srv
       dirstat history growth --leaf-only --limit=50 --format=json /srv

   Default history rows include both changed ancestors and descendants and must not be summed. Use **--leaf-only** when additive cleanup evidence is required.

## Evidence to report

- Name the filesystem/root, selected size mode (allocated or apparent), and command used.
- For each proposed candidate, give path, size, age or growth evidence, owner/type when relevant, and why it is safe to consider.
- Prefer **query --format=jsonl** or **--format=nul** for automation. Do not scrape the visual tree or human-size columns.
`
	switch profile {
	case ProfileOperator:
		return []byte(common + `
## Guarded operator workflow

- Create one scoped plan per intended action, then validate it:

      dirstat plan delete --root /srv /srv/archive/old.log -o cleanup.jsonl
      dirstat apply --dry-run cleanup.jsonl

- You may create plans and run dry-runs. Do not run **dirstat apply --yes**, confirm a mutating TUI action, or otherwise delete, move, truncate, or overwrite files without the user's explicit authorization.
- Keep the plan root narrow. Respect configured audit logging and filesystem/symlink boundaries; do not disable or loosen them unless the user explicitly asks.
`)
	case ProfileAdministrator:
		return []byte(common + `
## Administrator policy

The policy below was supplied when this skill was installed. It is the complete authority boundary for filesystem actions. You may execute an action only when a rule explicitly permits it; otherwise propose the action and request authorization. Continue to use guarded plans, dry-runs when the policy requires them, and audit logging.

` + rules)
	case ProfileReadOnly:
		return []byte(common + `
## Safety boundary

- Propose cleanup actions first. Do not delete, move, truncate, overwrite, or run **dirstat apply --yes** without the user's explicit authorization.
- Keep audit logging and filesystem/symlink boundaries enabled unless the user explicitly directs otherwise.
`)
	default:
		return nil
	}
}

func normalizedRules(profile Profile, rules string) (string, error) {
	switch profile {
	case ProfileReadOnly, ProfileOperator:
		return "", nil
	case ProfileAdministrator:
	default:
		return "", fmt.Errorf("unknown skill profile %q", profile)
	}
	if !utf8.ValidString(rules) {
		return "", errors.New("administrator rules must be valid UTF-8 text")
	}
	rules = strings.TrimSpace(rules)
	if rules == "" {
		return "", errors.New("administrator installation requires at least one rule")
	}
	if strings.Contains(rules, "REPLACE:") && !hasConcreteRule(rules) {
		return "", errors.New("administrator rules template must be edited to include at least one concrete rule")
	}
	if len(rules) > 64<<10 {
		return "", errors.New("administrator rules must not exceed 64 KiB")
	}
	return rules + "\n", nil
}

func hasConcreteRule(rules string) bool {
	for _, line := range strings.Split(rules, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "- REPLACE:") {
			return true
		}
	}
	return false
}

// ParseTargets turns command values into a stable, deduplicated target list.
// No values and the special value "all" both select every supported target.
func ParseTargets(values []string) ([]Target, error) {
	if len(values) == 0 {
		return allTargets(), nil
	}
	seen := make(map[Target]bool, len(values))
	for _, value := range values {
		target := Target(strings.ToLower(strings.TrimSpace(value)))
		switch target {
		case "all":
			if len(values) != 1 {
				return nil, errors.New("skill target all cannot be combined with other targets")
			}
			return allTargets(), nil
		case TargetCodex, TargetClaude:
			seen[target] = true
		default:
			return nil, fmt.Errorf("unknown skill target %q: expected codex, claude, or all", value)
		}
	}
	targets := make([]Target, 0, len(seen))
	for _, target := range allTargets() {
		if seen[target] {
			targets = append(targets, target)
		}
	}
	return targets, nil
}

// ParseProfiles turns flag values into a stable, deduplicated profile list.
// No values select the safest, read-only profile.
func ParseProfiles(values []string) ([]Profile, error) {
	if len(values) == 0 {
		return []Profile{ProfileReadOnly}, nil
	}
	seen := make(map[Profile]bool, len(values))
	for _, value := range values {
		profile := Profile(strings.ToLower(strings.TrimSpace(value)))
		switch profile {
		case "all":
			if len(values) != 1 {
				return nil, errors.New("skill profile all cannot be combined with other profiles")
			}
			return allProfiles(), nil
		case ProfileReadOnly, ProfileOperator, ProfileAdministrator:
			seen[profile] = true
		default:
			return nil, fmt.Errorf("unknown skill profile %q: expected read-only, operator, administrator, or all", value)
		}
	}
	profiles := make([]Profile, 0, len(seen))
	for _, profile := range allProfiles() {
		if seen[profile] {
			profiles = append(profiles, profile)
		}
	}
	return profiles, nil
}

func allTargets() []Target { return []Target{TargetCodex, TargetClaude} }

func allProfiles() []Profile {
	return []Profile{ProfileReadOnly, ProfileOperator, ProfileAdministrator}
}

// Resolve returns the exact files for every target/profile pair. ProjectDir
// and explicit paths may be relative; returned paths are always absolute.
func Resolve(options ResolveOptions) ([]Location, error) {
	targets, err := ParseTargets(targetStrings(options.Targets))
	if err != nil {
		return nil, err
	}
	profiles, err := ParseProfiles(profileStrings(options.Profiles))
	if err != nil {
		return nil, err
	}
	if options.Scope == "" {
		options.Scope = ScopeUser
	}
	if options.Scope != ScopeUser && options.Scope != ScopeProject {
		return nil, fmt.Errorf("invalid skill scope %q: expected user or project", options.Scope)
	}

	projectDir := options.ProjectDir
	if projectDir == "" {
		projectDir = "."
	}
	projectDir, err = absolute(projectDir)
	if err != nil {
		return nil, fmt.Errorf("project skill directory: %w", err)
	}

	locations := make([]Location, 0, len(targets)*len(profiles))
	for _, target := range targets {
		for _, profile := range profiles {
			path, err := targetPath(target, profile, options, projectDir)
			if err != nil {
				return nil, err
			}
			locations = append(locations, Location{Target: target, Profile: profile, Path: path})
		}
	}
	if err := uniqueLocations(locations); err != nil {
		return nil, err
	}
	return locations, nil
}

func targetStrings(targets []Target) []string {
	values := make([]string, len(targets))
	for i, target := range targets {
		values[i] = string(target)
	}
	return values
}

func profileStrings(profiles []Profile) []string {
	values := make([]string, len(profiles))
	for i, profile := range profiles {
		values[i] = string(profile)
	}
	return values
}

func targetPath(target Target, profile Profile, options ResolveOptions, projectDir string) (string, error) {
	override := options.CodexPath
	if target == TargetClaude {
		override = options.ClaudePath
	}
	if override != "" {
		path, err := absolute(override)
		if err != nil {
			return "", fmt.Errorf("%s skill path: %w", target, err)
		}
		if filepath.Base(path) != "SKILL.md" {
			return "", fmt.Errorf("%s skill path %q must name SKILL.md", target, path)
		}
		return path, nil
	}

	var root string
	switch options.Scope {
	case ScopeUser:
		if strings.TrimSpace(options.Home) == "" {
			return "", errors.New("user skill home is required")
		}
		home, err := absolute(options.Home)
		if err != nil {
			return "", fmt.Errorf("user skill home: %w", err)
		}
		root = home
	case ScopeProject:
		root = projectDir
	}

	dir := ".codex"
	if target == TargetClaude {
		dir = ".claude"
	}
	return filepath.Join(root, dir, "skills", skillName(profile), "SKILL.md"), nil
}

func skillName(profile Profile) string {
	switch profile {
	case ProfileOperator:
		return "dirstat-operator"
	case ProfileAdministrator:
		return "dirstat-administrator"
	case ProfileReadOnly:
		return "dirstat"
	default:
		return ""
	}
}

func validateProfile(profile Profile) error {
	switch profile {
	case ProfileReadOnly, ProfileOperator, ProfileAdministrator:
		return nil
	default:
		return fmt.Errorf("unknown skill profile %q", profile)
	}
}

func absolute(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	return abs, nil
}

func uniqueLocations(locations []Location) error {
	seen := make(map[string]Location, len(locations))
	for _, location := range locations {
		if previous, ok := seen[location.Path]; ok {
			return fmt.Errorf("%s/%s and %s/%s both resolve to %q", previous.Target, previous.Profile, location.Target, location.Profile, location.Path)
		}
		seen[location.Path] = location
	}
	return nil
}

// Status reports the state of every requested file without modifying it.
func Status(locations []Location) ([]Result, error) {
	results := make([]Result, 0, len(locations))
	for _, location := range locations {
		state, err := stateAt(location)
		if err != nil {
			return nil, fmt.Errorf("read %s/%s skill %q: %w", location.Target, location.Profile, location.Path, err)
		}
		results = append(results, Result{Location: location, State: state})
	}
	return results, nil
}

// Install writes definitions for each location. Modified files are protected
// unless force is true. All conflict checks happen before the first write.
func Install(locations []Location, rules string, force bool) ([]Result, error) {
	states, err := Status(locations)
	if err != nil {
		return nil, err
	}
	definitions := make(map[Profile][]byte, len(locations))
	for _, location := range locations {
		if _, ok := definitions[location.Profile]; ok {
			continue
		}
		definition, err := Definition(location.Profile, rules)
		if err != nil {
			return nil, err
		}
		definitions[location.Profile] = definition
	}
	for _, result := range states {
		if result.State == StateModified && !force {
			return nil, fmt.Errorf("%s/%s skill %q differs from the managed definition (use --force to replace it)", result.Location.Target, result.Location.Profile, result.Location.Path)
		}
	}

	for i := range states {
		if states[i].State == StateInstalled {
			matches, err := matchesDefinition(states[i].Location.Path, definitions[states[i].Location.Profile])
			if err != nil {
				return nil, fmt.Errorf("read %s/%s skill %q: %w", states[i].Location.Target, states[i].Location.Profile, states[i].Location.Path, err)
			}
			if matches {
				continue
			}
		}
		if err := writeAtomic(states[i].Location.Path, definitions[states[i].Location.Profile]); err != nil {
			return nil, fmt.Errorf("install %s/%s skill %q: %w", states[i].Location.Target, states[i].Location.Profile, states[i].Location.Path, err)
		}
		states[i].State, states[i].Changed = StateInstalled, true
	}
	return states, nil
}

func matchesDefinition(path string, want []byte) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return bytes.Equal(data, want), nil
}

// Remove deletes only intact managed files unless force is true. Missing files
// are successful no-ops, and all modified-file checks happen before deletion.
func Remove(locations []Location, force bool) ([]Result, error) {
	states, err := Status(locations)
	if err != nil {
		return nil, err
	}
	for _, result := range states {
		if result.State == StateModified && !force {
			return nil, fmt.Errorf("%s/%s skill %q differs from the managed definition (use --force to remove it)", result.Location.Target, result.Location.Profile, result.Location.Path)
		}
	}
	for i := range states {
		if states[i].State == StateMissing {
			continue
		}
		if err := os.Remove(states[i].Location.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("remove %s/%s skill %q: %w", states[i].Location.Target, states[i].Location.Profile, states[i].Location.Path, err)
		}
		states[i].State, states[i].Changed = StateMissing, true
	}
	return states, nil
}

func stateAt(location Location) (State, error) {
	data, err := os.ReadFile(location.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return StateMissing, nil
	}
	if err != nil {
		return "", err
	}
	return managedState(data, location.Profile), nil
}

func managedState(data []byte, profile Profile) State {
	markerPrefix := []byte("<!-- dirstat-managed: profile=" + string(profile) + " sha256=")
	markerStart := bytes.Index(data, markerPrefix)
	if markerStart < 0 {
		return StateModified
	}
	markerEndOffset := bytes.IndexByte(data[markerStart:], '\n')
	if markerEndOffset < 0 {
		return StateModified
	}
	markerEnd := markerStart + markerEndOffset + 1
	marker := string(data[markerStart:markerEnd])
	wantSuffix := " -->\n"
	if !strings.HasSuffix(marker, wantSuffix) {
		return StateModified
	}
	wantHash := strings.TrimSuffix(strings.TrimPrefix(marker, string(markerPrefix)), wantSuffix)
	if len(wantHash) != sha256.Size*2 {
		return StateModified
	}
	payload := make([]byte, 0, len(data)-len(data[markerStart:markerEnd]))
	payload = append(payload, data[:markerStart]...)
	payload = append(payload, data[markerEnd:]...)
	gotHash := sha256.Sum256(payload)
	if !strings.EqualFold(wantHash, hex.EncodeToString(gotHash[:])) {
		return StateModified
	}
	return StateInstalled
}

func writeAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".dirstat-skill-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
