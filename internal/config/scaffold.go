package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ScaffoldOptions describes the team a new .dibs folder is written for.
type ScaffoldOptions struct {
	RepoDir  string // repository root
	Repo     string // owner/name; detected from the git remote when empty
	TeamRoom string // Switch room name; "<repo-name>-team" when empty
	Senior   string // senior GitHub login
	Junior   string // junior GitHub login
	Force    bool   // overwrite an existing .dibs folder
}

var remoteRe = regexp.MustCompile(`github\.com[:/]([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// DetectRepo reads owner/name from the origin remote of dir.
func DetectRepo(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", errors.New("no git remote `origin`; pass --repo owner/name")
	}
	m := remoteRe.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return "", fmt.Errorf("origin %q is not a GitHub URL; pass --repo owner/name", strings.TrimSpace(string(out)))
	}
	return m[1], nil
}

var sourceExt = map[string]bool{".go": true, ".py": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".rs": true,
	".java": true, ".kt": true, ".swift": true, ".rb": true, ".php": true, ".cs": true, ".cpp": true, ".c": true, ".sql": true}

var skipDir = map[string]bool{".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true, "dist": true,
	"build": true, "target": true, "__pycache__": true, ".dibs": true, ".github": true, "docs": true, "examples": true}

// InferSkills proposes one skill per source folder (up to two levels deep),
// plus a testing skill when tests exist. It is a starting point to edit.
func InferSkills(root string) []Skill {
	counts := map[string]int{}
	tests := map[string]bool{}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable folders are skipped, not fatal
		}
		if d.IsDir() {
			if skipDir[d.Name()] || (strings.HasPrefix(d.Name(), ".") && p != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !sourceExt[filepath.Ext(p)] {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil //nolint:nilerr // a path outside root is ignored
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 2 {
			return nil
		}
		low := strings.ToLower(rel)
		if parts[0] == "tests" || parts[0] == "test" || strings.Contains(low, "_test.") || strings.Contains(low, "test_") || strings.Contains(low, ".test.") || strings.Contains(low, ".spec.") {
			tests[parts[0]+"/**"] = true
			if parts[0] == "tests" || parts[0] == "test" {
				return nil
			}
		}
		key := parts[0]
		if len(parts) >= 3 {
			key = parts[0] + "/" + parts[1]
		}
		counts[key]++
		return nil
	})
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > 10 {
		keys = keys[:10]
	}
	sort.Strings(keys)
	var out []Skill
	seen := map[string]bool{}
	for _, k := range keys {
		name := skillName(k)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, Skill{Name: name, Description: "Code in " + k + "/ (edit this description)", Paths: []string{k + "/**"}})
	}
	if len(tests) > 0 && !seen["testing"] {
		var globs []string
		for g := range tests {
			globs = append(globs, g)
		}
		sort.Strings(globs)
		out = append(out, Skill{Name: "testing", Description: "Writing and fixing tests", Paths: globs, Keywords: []string{"test", "flaky", "fixture"}})
	}
	return out
}

var nonSkill = regexp.MustCompile(`[^a-z0-9]+`)

func skillName(path string) string {
	parts := strings.Split(path, "/")
	n := parts[len(parts)-1]
	if (n == "internal" || n == "src" || n == "app" || n == "lib" || n == "pkg") && len(parts) > 1 {
		n = parts[0]
	}
	n = strings.Trim(nonSkill.ReplaceAllString(strings.ToLower(n), "-"), "-")
	if n == "" || n == "cmd" {
		return n
	}
	return n
}

// Scaffold writes .dibs/{team,skills,dibs}.yaml into opts.RepoDir and returns the folder.
func Scaffold(o ScaffoldOptions) (string, error) {
	if o.RepoDir == "" {
		o.RepoDir = "."
	}
	dir := filepath.Join(o.RepoDir, ".dibs")
	if _, err := os.Stat(dir); err == nil && !o.Force {
		return dir, fmt.Errorf("%s already exists (use --force to overwrite)", dir)
	}
	if o.Repo == "" {
		r, err := DetectRepo(o.RepoDir)
		if err != nil {
			return dir, err
		}
		o.Repo = r
	}
	if !repoRe.MatchString(o.Repo) {
		return dir, fmt.Errorf("repo %q must be owner/name", o.Repo)
	}
	if o.TeamRoom == "" {
		o.TeamRoom = strings.ToLower(strings.SplitN(o.Repo, "/", 2)[1]) + "-team"
	}
	if o.Senior == "" || o.Junior == "" {
		return dir, errors.New("pass --senior and --junior GitHub logins")
	}
	skills := InferSkills(o.RepoDir)
	if len(skills) == 0 {
		skills = []Skill{{Name: "codebase", Description: "General work in this repository", Paths: []string{"**"}}}
	}
	handle := func(login string) string {
		h := strings.Trim(nonSkill.ReplaceAllString(strings.ToLower(login), "-"), "-")
		if h == "" {
			h = "person"
		}
		return h
	}
	jh := handle(o.Junior)
	var team strings.Builder
	fmt.Fprintf(&team, "# Who is on the team. Chat names are how people appear in your Switch rooms.\nrepo: %s\nteam_room: %s\nagents: {dibs: dibs, builder: builder, reviewer: reviewer}\nseniors:\n  - {handle: %s, github: %s, chat: [%s]}\njuniors:\n  - handle: %s\n    github: %s\n    chat: [%s]\n    practice_room: %s-practice\n    # Starting level per skill: 0 Unseen · 1 Exposed · 2 Guided · 3 Independent · 4 Fluent\n    skills: {",
		o.Repo, o.TeamRoom, handle(o.Senior), o.Senior, handle(o.Senior), jh, o.Junior, jh, jh)
	for i, s := range skills {
		if i > 0 {
			team.WriteString(", ")
		}
		fmt.Fprintf(&team, "%s: 1", s.Name)
	}
	team.WriteString("}\n")

	var sk strings.Builder
	sk.WriteString("# The skills this codebase teaches. Paths are globs; keywords help `dibs suggest`.\n")
	for _, s := range skills {
		fmt.Fprintf(&sk, "- name: %s\n  description: %q\n  paths: [%s]\n", s.Name, s.Description, quoteList(s.Paths))
		if len(s.Keywords) > 0 {
			fmt.Fprintf(&sk, "  keywords: [%s]\n", quoteList(s.Keywords))
		}
	}
	policy := `# Learning budget and timers. Every field is optional; these are the defaults.
max_active_per_junior: 1      # reps a junior holds at once
max_backlog_share: 0.2        # at most this share of open tickets are reps
score_threshold: 3
risk_paths: []                # e.g. ["migrations/", "app/auth/"]: never reps
proposal_timeout_hours: 24
hold_max_hours: 48
release_stuck_multiplier: 2
stuck_answer_minutes: 30
slack_safety: 1.5
default_due_days: 7
baseline_senior_minutes: 45
max_teaching_per_pr: 2
auto_delegate: true
`
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return dir, err
	}
	for name, body := range map[string]string{"team.yaml": team.String(), "skills.yaml": sk.String(), "dibs.yaml": policy} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return dir, err
		}
	}
	if _, err := Load(dir); err != nil {
		return dir, fmt.Errorf("scaffold produced an invalid config (please report): %w", err)
	}
	return dir, nil
}

func quoteList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = fmt.Sprintf("%q", x)
	}
	return strings.Join(q, ", ")
}
