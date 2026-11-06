package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vivekyarra/dibs/internal/engine"
)

const fixture = "testdata/valid/.dibs"

func loadFixture(t *testing.T) *Config {
	t.Helper()
	t.Setenv("DIBS_DB", "")
	c, err := Load(fixture)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// writeDibs writes a .dibs folder in a temp dir; an empty string skips that file.
func writeDibs(t *testing.T, team, skills, dibs string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".dibs")
	os.MkdirAll(dir, 0o755)
	for name, body := range map[string]string{"team.yaml": team, "skills.yaml": skills, "dibs.yaml": dibs} {
		if body != "" {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

const okTeam = `repo: o/r
team_room: room
seniors: [{handle: maya, github: maya-gh}]
juniors: [{handle: asha, github: asha-gh}]
`
const okSkills = `- {name: idempotency, description: d, paths: [app/]}
`

func TestLoadValidFixture(t *testing.T) {
	c := loadFixture(t)
	if c.Dir != fixture || c.Team.Repo != "acme/demo-shop" || c.Team.TeamRoom != "dibs-team" {
		t.Fatalf("%+v", c.Team)
	}
	if len(c.Skills) != 2 || c.Skills[0].Keywords[1] != "duplicate" || c.Skills[1].Paths[0] != "app/api/" {
		t.Fatalf("skills %+v", c.Skills)
	}
	asha := c.Team.Juniors[0]
	if asha.Skills["idempotency"] != engine.Exposed || asha.Skills["http-handlers"] != engine.Independent || asha.Role != "junior" {
		t.Fatalf("asha %+v", asha)
	}
	if c.Team.Seniors[0].Role != "senior" {
		t.Fatal("senior role")
	}
	if c.DB != "custom.sqlite" {
		t.Fatalf("db %q", c.DB)
	}
}

func TestDefaultsApplied(t *testing.T) {
	c := loadFixture(t)
	wantAgents := map[string]string{"dibs": "dibs-bot", "builder": "builder", "reviewer": "reviewer"}
	if !reflect.DeepEqual(c.Team.Agents, wantAgents) {
		t.Fatalf("agents %v", c.Team.Agents)
	}
	if c.Agent("dibs") != "dibs-bot" || c.Agent("builder") != "builder" || c.Agent("nope") != "" {
		t.Fatal("Agent()")
	}
	if c.Team.Juniors[0].PracticeRoom != "asha-practice" || c.Team.Juniors[1].PracticeRoom != "ben-room" {
		t.Fatalf("practice rooms %q %q", c.Team.Juniors[0].PracticeRoom, c.Team.Juniors[1].PracticeRoom)
	}
	if c.Team.Seniors[0].PracticeRoom != "" {
		t.Fatal("seniors get no practice room")
	}
	// dibs.yaml set two fields; every other policy field keeps its default.
	want := engine.DefaultPolicy()
	want.RiskPaths = []string{"migrations/", "app/auth/"}
	want.ScoreThreshold = 4
	if !reflect.DeepEqual(c.Policy, want) {
		t.Fatalf("policy\n got %+v\nwant %+v", c.Policy, want)
	}
}

func TestMissingDibsYAMLUsesDefaultsAndHomeDB(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("DIBS_DB", "")
	dir := writeDibs(t, okTeam, okSkills, "")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Policy, engine.DefaultPolicy()) {
		t.Fatalf("policy %+v", c.Policy)
	}
	if want := filepath.Join(home, ".dibs", "o__r.sqlite"); c.DB != want {
		t.Fatalf("db %q want %q", c.DB, want)
	}
}

func TestDIBSDBEnvOverride(t *testing.T) {
	t.Setenv("DIBS_DB", "/tmp/override.sqlite")
	c, err := Load(fixture) // fixture sets db: custom.sqlite; env wins
	if err != nil {
		t.Fatal(err)
	}
	if c.DB != "/tmp/override.sqlite" {
		t.Fatalf("db %q", c.DB)
	}
}

func TestUnknownYAMLKeysRejected(t *testing.T) {
	t.Setenv("DIBS_DB", "x")
	cases := map[string][3]string{
		"team.yaml":   {okTeam + "colour: blue\n", okSkills, ""},
		"skills.yaml": {okTeam, "- {name: a, summary: unknown key, paths: []}\n", ""},
		"dibs.yaml":   {okTeam, okSkills, "scoreThreshold: 3\n"},
		"person":      {strings.Replace(okTeam, "github: asha-gh", "github: asha-gh, slack: asha", 1), okSkills, ""},
	}
	for name, files := range cases {
		_, err := Load(writeDibs(t, files[0], files[1], files[2]))
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%s: unknown key accepted: %v", name, err)
		}
	}
}

func TestMissingRequiredFiles(t *testing.T) {
	if _, err := Load(writeDibs(t, "", okSkills, "")); err == nil {
		t.Error("missing team.yaml")
	}
	if _, err := Load(writeDibs(t, okTeam, "", "")); err == nil {
		t.Error("missing skills.yaml")
	}
	if _, err := Load(writeDibs(t, "repo: [unclosed", okSkills, "")); err == nil || !strings.Contains(err.Error(), "team.yaml") {
		t.Errorf("syntax error should name the file: %v", err)
	}
}

// BUG: a dibs.yaml that exists but holds only comments (what a scaffold that
// documents the defaults would write) fails with "dibs.yaml: EOF" instead of
// meaning "all defaults".
func TestCommentOnlyDibsYAMLMeansDefaults(t *testing.T) {
	t.Setenv("DIBS_DB", "x")
	c, err := Load(writeDibs(t, okTeam, okSkills, "# score_threshold: 3\n# slack_safety: 1.5\n"))
	if err != nil {
		t.Fatalf("comment-only dibs.yaml rejected: %v", err)
	}
	if !reflect.DeepEqual(c.Policy, engine.DefaultPolicy()) {
		t.Fatalf("policy %+v", c.Policy)
	}
}

func TestValidateCollectsEveryError(t *testing.T) {
	c := &Config{
		Team: Team{Repo: "not-a-repo", TeamRoom: "",
			Seniors: []Person{{Handle: "Maya", GitHub: ""}, {Handle: "dup", GitHub: "d1"}},
			Juniors: []Person{{Handle: "dup", GitHub: "d2", Skills: map[string]engine.Level{"ghost": 1, "good": 7}}}},
		Skills: []Skill{{Name: "good"}, {Name: "Bad_Name"}, {Name: "good"}},
		Policy: engine.Policy{MaxActivePerJunior: 0, MaxBacklogShare: 1.5, SlackSafety: 0.5, ReleaseStuckMultiplier: 0.9,
			DefaultDueDays: 0, ProposalTimeoutHours: 1, HoldMaxHours: 1, StuckAnswerMinutes: 1, MaxTeachingPerPR: -1},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("want errors")
	}
	msg := err.Error()
	for _, want := range []string{
		`repo "not-a-repo" must be owner/name`,
		"team_room is required",
		`name "Bad_Name" must be lowercase-with-dashes`,
		`duplicate skill "good"`,
		`senior handle "Maya" must be lowercase`,
		`senior "Maya" needs a github login`,
		`handle "dup" appears twice`,
		`junior "dup" has level for unknown skill "ghost"`,
		`junior "dup" skill "good" level 7 must be 0-4`,
		"max_active_per_junior must be ≥ 1",
		"max_backlog_share must be in (0, 1]",
		"slack_safety must be ≥ 1",
		"release_stuck_multiplier must be ≥ 1",
		"stuck_answer_minutes must be > 0",
		"max_teaching_per_pr and baseline_senior_minutes must be ≥ 0",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
	// Errors are sorted, one per line, so the output is stable.
	lines := strings.Split(msg, "\n  - ")[1:]
	for i := 1; i < len(lines); i++ {
		if lines[i-1] > lines[i] {
			t.Fatalf("errors not sorted:\n%s", msg)
		}
	}
	if len(lines) != 15 {
		t.Errorf("got %d errors, want 15:\n%s", len(lines), msg)
	}

	empty := &Config{Policy: engine.DefaultPolicy(), Team: Team{Repo: "o/r", TeamRoom: "x"}}
	msg = empty.Validate().Error()
	for _, want := range []string{"at least one senior", "at least one junior", "at least one skill"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %s", want, msg)
		}
	}
	p := engine.DefaultPolicy()
	p.MaxBacklogShare = 0
	p.HoldMaxHours = -1
	p.BaselineSeniorMinutes = -5
	bad := loadFixture(t)
	bad.Policy = p
	msg = bad.Validate().Error()
	for _, want := range []string{"max_backlog_share must be in (0, 1]", "hold_max_hours", "baseline_senior_minutes must be ≥ 0"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %s", want, msg)
		}
	}
}

func TestValidatePolicyBoundaries(t *testing.T) {
	c := loadFixture(t)
	c.Policy.MaxBacklogShare = 1 // inclusive upper bound
	c.Policy.SlackSafety = 1
	c.Policy.ReleaseStuckMultiplier = 1
	c.Policy.MaxTeachingPerPR = 0
	c.Policy.BaselineSeniorMinutes = 0
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadReportsValidationErrors(t *testing.T) {
	t.Setenv("DIBS_DB", "x")
	_, err := Load(writeDibs(t, okTeam, okSkills, "max_backlog_share: 2\nslack_safety: 0\n"))
	if err == nil || !strings.Contains(err.Error(), "max_backlog_share") || !strings.Contains(err.Error(), "slack_safety") {
		t.Fatalf("got %v", err)
	}
}

func TestResolve(t *testing.T) {
	c := loadFixture(t)
	cases := map[string]string{
		"maya": "maya", "MAYA": "maya", "@maya": "maya", "maya-gh": "maya", "@Maya-GH": "maya",
		"maya.s": "maya", "@MAYA.S": "maya", "  @asha  ": "asha", "asha-dev": "asha", "ben": "ben", "BEN-DEV": "ben",
	}
	for in, want := range cases {
		p, ok := c.Resolve(in)
		if !ok || p.Handle != want {
			t.Errorf("Resolve(%q) = %q %v, want %q", in, p.Handle, ok, want)
		}
	}
	for _, in := range []string{"", "@", "   ", "nobody", "@@maya", "dibs-bot"} {
		if p, ok := c.Resolve(in); ok {
			t.Errorf("Resolve(%q) matched %q", in, p.Handle)
		}
	}
	if p, ok := c.ByGitHub("ASHA-DEV"); !ok || p.Handle != "asha" || p.Role != "junior" {
		t.Error("ByGitHub")
	}
	if _, ok := c.ByGitHub("asha"); ok {
		t.Error("ByGitHub must only match GitHub logins")
	}
	if !c.IsSenior("@Maya") || c.IsSenior("asha") || c.IsSenior("nobody") {
		t.Error("IsSenior")
	}
	if s, ok := c.Skill("idempotency"); !ok || s.Description == "" {
		t.Error("Skill")
	}
	if _, ok := c.Skill("nope"); ok {
		t.Error("Skill unknown")
	}
	ppl := c.People()
	if len(ppl) != 3 || ppl[0].Handle != "maya" || ppl[1].Handle != "asha" || ppl[2].Handle != "ben" {
		t.Errorf("People order %+v", ppl)
	}
	ppl[0].Handle = "mutated"
	if c.Team.Seniors[0].Handle != "maya" {
		t.Error("People() must return a copy")
	}
}

func TestFindWalksUp(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	os.MkdirAll(deep, 0o755)
	os.MkdirAll(filepath.Join(root, ".dibs"), 0o755)
	got, err := Find(deep)
	if err != nil || got != filepath.Join(root, ".dibs") {
		t.Fatalf("Find = %q %v", got, err)
	}
	// The nearest .dibs wins.
	os.MkdirAll(filepath.Join(root, "a", ".dibs"), 0o755)
	if got, _ := Find(deep); got != filepath.Join(root, "a", ".dibs") {
		t.Fatalf("nearest: %q", got)
	}
	// From the folder itself.
	if got, _ := Find(filepath.Join(root, "a")); got != filepath.Join(root, "a", ".dibs") {
		t.Fatalf("self: %q", got)
	}
	// A *file* named .dibs is not a config folder.
	os.WriteFile(filepath.Join(deep, ".dibs"), []byte("x"), 0o644)
	if got, _ := Find(deep); got != filepath.Join(root, "a", ".dibs") {
		t.Fatalf("file .dibs: %q", got)
	}
	// Relative start paths work.
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	os.Chdir(deep)
	if got, _ := Find("."); got != filepath.Join(root, "a", ".dibs") {
		t.Fatalf("relative: %q", got)
	}
}
