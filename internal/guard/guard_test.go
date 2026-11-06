package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivekyarra/dibs/internal/config"
	"github.com/vivekyarra/dibs/internal/tracker"
)

const teamYAML = `repo: acme/shop
team_room: shop-team
seniors:
  - handle: ravi
    github: ravi-gh
juniors:
  - handle: asha
    github: Asha-Dev
  - handle: ben
    github: ben-codes
`

const skillsYAML = `- name: idempotency
  description: retries
  paths: ["app/orders/**"]
`

// writeDibs creates root/.dibs with a valid config and returns root.
func writeDibs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	d := filepath.Join(root, ".dibs")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(d, "team.yaml"), []byte(teamYAML), 0o644))
	must(t, os.WriteFile(filepath.Join(d, "skills.yaml"), []byte(skillsYAML), 0o644))
	t.Setenv("DIBS_DB", filepath.Join(root, "dibs.sqlite"))
	return root
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func loadCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join(writeDibs(t), ".dibs"))
	must(t, err)
	return cfg
}

// fixture: #1 reserved for asha, #2 unreserved, #3 reserved for ben but released,
// #4 reserved for a handle nobody has, #5 reserved for ben.
func fixture() *tracker.Memory {
	m := tracker.NewMemory("acme/shop")
	m.CreateIssue("idempotent POST", "", "dibs:reserved:asha", "skill:idempotency")
	m.CreateIssue("typo", "")
	m.CreateIssue("retries", "", "dibs:reserved:ben", "dibs:released")
	m.CreateIssue("ghost", "", "dibs:reserved:zoe")
	m.CreateIssue("webhooks", "", "dibs:reserved:ben")
	return m
}

func TestEvaluate(t *testing.T) {
	cfg := loadCfg(t)
	tests := []struct {
		name     string
		author   string
		title    string
		body     string
		pass     bool
		checked  []int
		reasonIn string
	}{
		{"reserved, wrong author", "builder-bot", "", "Closes #1", false, []int{1}, "#1 is reserved for @Asha-Dev as a learning rep. Agents coach, they don't solve. Ask @dibs to release it first."},
		{"reserved, senior author", "ravi-gh", "", "fixes #1", false, []int{1}, "@Asha-Dev"},
		{"reserved, right author case-insensitive", "asha-dev", "", "Resolves #1", true, []int{1}, ""},
		{"reserved, right author with @", "@ASHA-DEV", "", "closes #1", true, []int{1}, ""},
		{"released passes", "builder-bot", "", "Fixes #3", true, []int{3}, ""},
		{"unreserved passes", "builder-bot", "", "Fixes #2", true, []int{2}, ""},
		{"no refs passes", "builder-bot", "Refactor", "touches #1 but does not close it", true, []int{}, ""},
		{"multiple refs, one bad", "asha-dev", "", "Closes #1\nCloses #5\nFixes #2", false, []int{1, 2, 5}, "#5 is reserved for @ben-codes"},
		{"unknown handle fails", "anyone", "", "Closes #4", false, []int{4}, `reserved for "zoe"`},
		{"cross-repo form", "builder-bot", "", "Fixes acme/shop#1", false, []int{1}, "@Asha-Dev"},
		{"ref in title", "builder-bot", "Fix #1: dedupe orders", "", false, []int{1}, "@Asha-Dev"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pr := tracker.PR{Number: 99, Author: tc.author, Title: tc.title, Body: tc.body}
			v, err := Evaluate(cfg, fixture(), pr)
			must(t, err)
			if v.Pass != tc.pass {
				t.Fatalf("pass = %v, want %v (reasons %q)", v.Pass, tc.pass, v.Reasons)
			}
			if !equalInts(v.Checked, tc.checked) {
				t.Errorf("checked = %v, want %v", v.Checked, tc.checked)
			}
			if tc.reasonIn != "" && !strings.Contains(strings.Join(v.Reasons, "\n"), tc.reasonIn) {
				t.Errorf("reasons %q missing %q", v.Reasons, tc.reasonIn)
			}
			if tc.pass && len(v.Reasons) > 0 {
				t.Errorf("passing verdict has reasons %q", v.Reasons)
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type errTracker struct {
	tracker.Tracker
	err error
}

func (e errTracker) Issue(int) (tracker.Issue, error) { return tracker.Issue{}, e.err }

func TestEvaluateErrors(t *testing.T) {
	cfg := loadCfg(t)
	pr := tracker.PR{Author: "x", Body: "Closes #7"}
	if _, err := Evaluate(nil, fixture(), pr); err == nil {
		t.Error("nil config should error")
	}
	if _, err := Evaluate(cfg, nil, pr); err == nil {
		t.Error("nil tracker should error")
	}
	if _, err := Evaluate(cfg, errTracker{err: errors.New("boom")}, pr); err == nil || !strings.Contains(err.Error(), "#7") {
		t.Errorf("want wrapped error, got %v", err)
	}
	v, err := Evaluate(cfg, errTracker{err: &tracker.APIError{Status: 404}}, pr)
	must(t, err)
	if !v.Pass || len(v.Checked) != 0 || len(v.Notes) != 1 {
		t.Errorf("404 should be skipped with a note: %+v", v)
	}
}

func TestSummary(t *testing.T) {
	cfg := loadCfg(t)
	pr := tracker.PR{Number: 12, Author: "builder-bot", Title: "dedupe", Body: "Closes #1, closes #3"}
	v, err := Evaluate(cfg, fixture(), pr)
	must(t, err)
	s := Summary(pr, v)
	for _, want := range []string{"blocked", "PR #12 by @builder-bot: dedupe", "Checked: #1, #3", "### Why", "@Asha-Dev", "### Notes", "released"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
	s = Summary(tracker.PR{Number: 1, Author: "a"}, Verdict{Pass: true})
	if !strings.Contains(s, "passed") || !strings.Contains(s, "No `Closes") {
		t.Errorf("pass summary:\n%s", s)
	}
}

func TestParseEvent(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		ok     bool
		err    bool
		author string
		body   string
		num    int
		head   string
	}{
		{"webhook", `{"action":"opened","number":7,"pull_request":{"number":7,"title":"t","body":"Closes #1","html_url":"u","user":{"login":"bot"},"head":{"ref":"fix-1"}}}`, true, false, "bot", "Closes #1", 7, "fix-1"},
		{"null body, number from top", `{"number":8,"pull_request":{"title":"t","body":null,"user":{"login":"bot"}}}`, true, false, "bot", "", 8, ""},
		{"bare webhook PR", `{"number":3,"body":"Fixes #2","user":{"login":"x"}}`, true, false, "x", "Fixes #2", 3, ""},
		{"tracker.PR shape", `{"number":4,"body":"Fixes #2","author":"y","head_ref":"b"}`, true, false, "y", "Fixes #2", 4, "b"},
		{"push event", `{"ref":"refs/heads/main","commits":[]}`, false, false, "", "", 0, ""},
		{"null pull_request", `{"pull_request":null}`, false, false, "", "", 0, ""},
		{"no author", `{"pull_request":{"number":1}}`, false, true, "", "", 0, ""},
		{"bad json", `{`, false, true, "", "", 0, ""},
		{"bad pr json", `{"pull_request":{"number":"x"}}`, false, true, "", "", 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pr, ok, err := ParseEvent([]byte(tc.raw))
			if (err != nil) != tc.err {
				t.Fatalf("err = %v, want err=%v", err, tc.err)
			}
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if pr.Author != tc.author || pr.Body != tc.body || pr.Number != tc.num || pr.HeadRef != tc.head {
				t.Errorf("got %+v", pr)
			}
		})
	}
}

// --- Main ---

type env struct {
	root, event, summary string
	tf                   string
}

// setupMain prepares a repo with .dibs, an offline tracker file, and an event.
func setupMain(t *testing.T, author, body string) env {
	t.Helper()
	root := writeDibs(t)
	for _, k := range []string{"GITHUB_EVENT_PATH", "GITHUB_REPOSITORY", "GITHUB_STEP_SUMMARY", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
	tf := filepath.Join(root, "tracker.json")
	m, err := tracker.LoadMemory(tf, "acme/shop")
	must(t, err)
	m.CreateIssue("idempotent POST", "", "dibs:reserved:asha")
	m.CreateIssue("typo", "")
	ev := map[string]any{"action": "opened", "number": 42, "pull_request": map[string]any{
		"number": 42, "title": "PR", "body": body, "user": map[string]any{"login": author}, "head": map[string]any{"ref": "b"},
	}}
	b, _ := json.Marshal(ev)
	evp := filepath.Join(root, "event.json")
	must(t, os.WriteFile(evp, b, 0o644))
	return env{root: root, event: evp, summary: filepath.Join(root, "summary.md"), tf: tf}
}

func runMain(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestMainFailWritesSummaryAndAnnotation(t *testing.T) {
	e := setupMain(t, "builder-bot", "Closes #1")
	t.Setenv("GITHUB_EVENT_PATH", e.event)
	t.Setenv("GITHUB_STEP_SUMMARY", e.summary)
	code, out, _ := runMain("--config-dir", filepath.Join(e.root, ".dibs"), "--tracker-file", e.tf)
	if code != ExitFail {
		t.Fatalf("code = %d, out:\n%s", code, out)
	}
	if !strings.Contains(out, "::error title=dibs-guard::#1 is reserved for @Asha-Dev") {
		t.Errorf("missing annotation:\n%s", out)
	}
	sum, err := os.ReadFile(e.summary)
	must(t, err)
	if !strings.Contains(string(sum), "blocked") || !strings.Contains(string(sum), "PR #42 by @builder-bot") {
		t.Errorf("summary:\n%s", sum)
	}
}

func TestMainPass(t *testing.T) {
	e := setupMain(t, "ASHA-DEV", "Closes #1, fixes #2")
	t.Setenv("GITHUB_STEP_SUMMARY", e.summary)
	// --config-dir pointing at the repo root (containing .dibs) and --pr-json.
	code, out, errs := runMain("--config-dir", e.root, "--pr-json", e.event, "--tracker-file", e.tf)
	if code != ExitPass {
		t.Fatalf("code = %d\n%s\n%s", code, out, errs)
	}
	if strings.Contains(out, "::error") {
		t.Errorf("unexpected annotation:\n%s", out)
	}
	if _, err := os.Stat(e.summary); err != nil {
		t.Errorf("summary not written: %v", err)
	}
}

func TestMainFindsConfigFromCwd(t *testing.T) {
	e := setupMain(t, "builder-bot", "no refs")
	sub := filepath.Join(e.root, "app", "orders")
	must(t, os.MkdirAll(sub, 0o755))
	t.Chdir(sub)
	code, out, errs := runMain("--pr-json", e.event, "--tracker-file", e.tf)
	if code != ExitPass {
		t.Fatalf("code = %d\n%s\n%s", code, out, errs)
	}
}

func TestMainGitHubTracker(t *testing.T) {
	e := setupMain(t, "builder-bot", "Fixes #1\nFixes #9")
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/repos/acme/fork/issues/1":
			gotPath = r.URL.Path
			w.Write([]byte(`{"number":1,"state":"open","labels":[{"name":"dibs:reserved:asha"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("GITHUB_API_URL", srv.URL)
	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("GITHUB_REPOSITORY", "acme/fork")
	code, out, errs := runMain("--config-dir", filepath.Join(e.root, ".dibs"), "--pr-json", e.event)
	if code != ExitFail {
		t.Fatalf("code = %d\n%s\n%s", code, out, errs)
	}
	if gotAuth != "Bearer test-token" || gotPath != "/repos/acme/fork/issues/1" {
		t.Errorf("auth %q path %q", gotAuth, gotPath)
	}
	if !strings.Contains(out, "#9 was not found") {
		t.Errorf("missing note for #9:\n%s", out)
	}
}

func TestMainSetupErrors(t *testing.T) {
	e := setupMain(t, "x", "Closes #1")
	bad := filepath.Join(e.root, "bad.json")
	must(t, os.WriteFile(bad, []byte("{"), 0o644))
	push := filepath.Join(e.root, "push.json")
	must(t, os.WriteFile(push, []byte(`{"ref":"main"}`), 0o644))
	cd := filepath.Join(e.root, ".dibs")
	tests := []struct {
		name string
		args []string
		code int
		msg  string
	}{
		{"no event", []string{"--config-dir", cd}, ExitSetup, "no pull request"},
		{"missing event file", []string{"--pr-json", filepath.Join(e.root, "nope.json")}, ExitSetup, "reading PR event"},
		{"bad event json", []string{"--pr-json", bad}, ExitSetup, "parsing bad.json"},
		{"not a PR", []string{"--pr-json", push}, ExitPass, ""},
		{"bad config dir", []string{"--pr-json", e.event, "--config-dir", t.TempDir()}, ExitSetup, "no team.yaml"},
		{"missing tracker file", []string{"--pr-json", e.event, "--config-dir", cd, "--tracker-file", filepath.Join(e.root, "none.json")}, ExitSetup, "tracker file"},
		{"corrupt tracker file", []string{"--pr-json", e.event, "--config-dir", cd, "--tracker-file", bad}, ExitSetup, "bad.json"},
		{"extra args", []string{"oops"}, ExitSetup, "unexpected arguments"},
		{"bad flag", []string{"--nope"}, ExitSetup, ""},
		{"help", []string{"-h"}, ExitPass, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errs := runMain(tc.args...)
			if code != tc.code {
				t.Fatalf("code = %d, want %d\n%s\n%s", code, tc.code, out, errs)
			}
			if tc.msg != "" && !strings.Contains(errs, tc.msg) {
				t.Errorf("stderr %q missing %q", errs, tc.msg)
			}
			if tc.code == ExitSetup && tc.msg != "" && !strings.Contains(out, "::error title=dibs-guard setup::") {
				t.Errorf("missing setup annotation:\n%s", out)
			}
		})
	}
}

func TestMainNoTokenAndTrackerError(t *testing.T) {
	e := setupMain(t, "x", "Closes #1")
	cd := filepath.Join(e.root, ".dibs")
	// Tracker error other than 404 → setup error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	t.Setenv("GITHUB_API_URL", srv.URL)
	t.Setenv("GH_TOKEN", "t")
	if code, out, errs := runMain("--pr-json", e.event, "--config-dir", cd); code != ExitSetup {
		t.Fatalf("code = %d\n%s\n%s", code, out, errs)
	}
	// No token at all: hide gh so Token() cannot fall back to it.
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", t.TempDir())
	if code, _, errs := runMain("--pr-json", e.event, "--config-dir", cd); code != ExitSetup || !strings.Contains(errs, "token") {
		t.Fatalf("code = %d, stderr %q", code, errs)
	}
}

func TestMainSummaryWriteFailureIsNotFatal(t *testing.T) {
	e := setupMain(t, "x", "no refs")
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(e.root, "missing-dir", "s.md"))
	code, _, errs := runMain("--pr-json", e.event, "--config-dir", filepath.Join(e.root, ".dibs"), "--tracker-file", e.tf)
	if code != ExitPass || !strings.Contains(errs, "job summary") {
		t.Fatalf("code = %d, stderr %q", code, errs)
	}
}

func TestEscapeData(t *testing.T) {
	if got := escapeData("50%\r\nnext"); got != "50%25%0D%0Anext" {
		t.Errorf("got %q", got)
	}
}
