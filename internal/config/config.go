// Package config loads a team's .dibs/ folder: who is on the team, which
// skills the codebase teaches, and the learning-budget policy.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/vivekyarra/dibs/internal/engine"
	"gopkg.in/yaml.v3"
)

// Person is a human on the team.
type Person struct {
	Handle       string                  `yaml:"handle" json:"handle"`
	GitHub       string                  `yaml:"github" json:"github"`
	Chat         []string                `yaml:"chat,omitempty" json:"chat,omitempty"`
	PracticeRoom string                  `yaml:"practice_room,omitempty" json:"practice_room,omitempty"`
	Skills       map[string]engine.Level `yaml:"skills,omitempty" json:"skills,omitempty"`
	Role         string                  `yaml:"-" json:"role"`
}

// Team is .dibs/team.yaml.
type Team struct {
	Repo     string            `yaml:"repo" json:"repo"`
	TeamRoom string            `yaml:"team_room" json:"team_room"`
	Agents   map[string]string `yaml:"agents" json:"agents"` // role → handle used in chat: dibs, builder, reviewer
	Seniors  []Person          `yaml:"seniors" json:"seniors"`
	Juniors  []Person          `yaml:"juniors" json:"juniors"`
}

// Skill is one entry of .dibs/skills.yaml.
type Skill struct {
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Paths       []string `yaml:"paths" json:"paths"`
	Keywords    []string `yaml:"keywords,omitempty" json:"keywords,omitempty"`
}

// Config is the whole .dibs/ folder, loaded and validated.
type Config struct {
	Dir    string        `json:"dir"`
	Team   Team          `json:"team"`
	Skills []Skill       `json:"skills"`
	Policy engine.Policy `json:"policy"`
	DB     string        `json:"db"`
}

type policyFile struct {
	engine.Policy `yaml:",inline"`
	DB            string `yaml:"db"`
}

var (
	handleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	repoRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	skillRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// Find walks up from start looking for a .dibs directory.
func Find(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		cand := filepath.Join(dir, ".dibs")
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			return cand, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no .dibs/ folder found here or in any parent: run `dibs scaffold --senior <login> --junior <login>` in your repo root, then `dibs init`")
		}
		dir = parent
	}
}

func readYAML(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

// Load reads and validates dir (a .dibs folder).
func Load(dir string) (*Config, error) {
	c := &Config{Dir: dir, Policy: engine.DefaultPolicy()}
	if err := readYAML(filepath.Join(dir, "team.yaml"), &c.Team); err != nil {
		return nil, err
	}
	if err := readYAML(filepath.Join(dir, "skills.yaml"), &c.Skills); err != nil {
		return nil, err
	}
	pf := policyFile{Policy: engine.DefaultPolicy()}
	if _, err := os.Stat(filepath.Join(dir, "dibs.yaml")); err == nil {
		if err := readYAML(filepath.Join(dir, "dibs.yaml"), &pf); err != nil {
			return nil, err
		}
	}
	c.Policy = pf.Policy
	c.DB = pf.DB
	if env := os.Getenv("DIBS_DB"); env != "" {
		c.DB = env
	}
	if c.DB == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		c.DB = filepath.Join(home, ".dibs", strings.ReplaceAll(c.Team.Repo, "/", "__")+".sqlite")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate returns every problem at once, so a person can fix them in one pass.
func (c *Config) Validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if !repoRe.MatchString(c.Team.Repo) {
		add("team.yaml: repo %q must be owner/name", c.Team.Repo)
	}
	if c.Team.TeamRoom == "" {
		add("team.yaml: team_room is required")
	}
	if c.Team.Agents == nil {
		c.Team.Agents = map[string]string{}
	}
	for _, role := range []string{"dibs", "builder", "reviewer"} {
		if c.Team.Agents[role] == "" {
			c.Team.Agents[role] = role
		}
	}
	if len(c.Team.Seniors) == 0 {
		add("team.yaml: at least one senior is required")
	}
	if len(c.Team.Juniors) == 0 {
		add("team.yaml: at least one junior is required")
	}
	skills := map[string]bool{}
	for i, s := range c.Skills {
		if !skillRe.MatchString(s.Name) {
			add("skills.yaml[%d]: name %q must be lowercase-with-dashes", i, s.Name)
		}
		if skills[s.Name] {
			add("skills.yaml: duplicate skill %q", s.Name)
		}
		skills[s.Name] = true
	}
	if len(c.Skills) == 0 {
		add("skills.yaml: at least one skill is required")
	}
	seen := map[string]bool{}
	check := func(p *Person, role string) {
		p.Role = role
		if !handleRe.MatchString(p.Handle) {
			add("team.yaml: %s handle %q must be lowercase letters, digits, . _ -", role, p.Handle)
		}
		if seen[p.Handle] {
			add("team.yaml: handle %q appears twice", p.Handle)
		}
		seen[p.Handle] = true
		if p.GitHub == "" {
			add("team.yaml: %s %q needs a github login", role, p.Handle)
		}
		for sk, lvl := range p.Skills {
			if !skills[sk] {
				add("team.yaml: %s %q has level for unknown skill %q", role, p.Handle, sk)
			}
			if lvl < engine.Unseen || lvl > engine.Fluent {
				add("team.yaml: %s %q skill %q level %d must be 0-4", role, p.Handle, sk, lvl)
			}
		}
	}
	for i := range c.Team.Seniors {
		check(&c.Team.Seniors[i], "senior")
	}
	for i := range c.Team.Juniors {
		check(&c.Team.Juniors[i], "junior")
		if c.Team.Juniors[i].PracticeRoom == "" {
			c.Team.Juniors[i].PracticeRoom = c.Team.Juniors[i].Handle + "-practice"
		}
	}
	p := c.Policy
	if p.MaxActivePerJunior < 1 {
		add("dibs.yaml: max_active_per_junior must be ≥ 1")
	}
	if p.MaxBacklogShare <= 0 || p.MaxBacklogShare > 1 {
		add("dibs.yaml: max_backlog_share must be in (0, 1]")
	}
	if p.SlackSafety < 1 {
		add("dibs.yaml: slack_safety must be ≥ 1")
	}
	if p.ReleaseStuckMultiplier < 1 {
		add("dibs.yaml: release_stuck_multiplier must be ≥ 1")
	}
	if p.DefaultDueDays <= 0 || p.ProposalTimeoutHours <= 0 || p.HoldMaxHours <= 0 || p.StuckAnswerMinutes <= 0 {
		add("dibs.yaml: default_due_days, proposal_timeout_hours, hold_max_hours and stuck_answer_minutes must be > 0")
	}
	if p.MaxTeachingPerPR < 0 || p.BaselineSeniorMinutes < 0 {
		add("dibs.yaml: max_teaching_per_pr and baseline_senior_minutes must be ≥ 0")
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return errors.New("invalid .dibs config:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

// People returns seniors then juniors.
func (c *Config) People() []Person {
	out := append([]Person{}, c.Team.Seniors...)
	return append(out, c.Team.Juniors...)
}

// Resolve finds a person by handle, GitHub login or chat name (case-insensitive, @ optional).
func (c *Config) Resolve(name string) (Person, bool) {
	n := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "@"))
	if n == "" {
		return Person{}, false
	}
	for _, p := range c.People() {
		if strings.ToLower(p.Handle) == n || strings.ToLower(p.GitHub) == n {
			return p, true
		}
		for _, ch := range p.Chat {
			if strings.ToLower(strings.TrimPrefix(ch, "@")) == n {
				return p, true
			}
		}
	}
	return Person{}, false
}

// ByGitHub finds a person by GitHub login.
func (c *Config) ByGitHub(login string) (Person, bool) {
	for _, p := range c.People() {
		if strings.EqualFold(p.GitHub, login) {
			return p, true
		}
	}
	return Person{}, false
}

// IsSenior reports whether name resolves to a senior.
func (c *Config) IsSenior(name string) bool {
	p, ok := c.Resolve(name)
	return ok && p.Role == "senior"
}

// Skill returns a skill by name.
func (c *Config) Skill(name string) (Skill, bool) {
	for _, s := range c.Skills {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// Agent returns the chat handle of an agent role (dibs, builder, reviewer).
func (c *Config) Agent(role string) string { return c.Team.Agents[role] }
