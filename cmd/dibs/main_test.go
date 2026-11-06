package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivekyarra/dibs/internal/tracker"
)

const teamYAML = `repo: acme/shop
team_room: payments-team
seniors:
  - {handle: ravi, github: ravi-gh, chat: [ravi]}
juniors:
  - handle: asha
    github: asha-gh
    chat: [asha]
    skills: {idempotency: 1, testing: 3}
`

const skillsYAML = `- {name: idempotency, description: retries, paths: ["app/orders/**"], keywords: [idempotent, retry]}
- {name: testing, description: tests, paths: ["tests/**"], keywords: [flaky]}
- {name: observability, description: logs, paths: ["app/logging.py"], keywords: [log]}
`

type cli struct {
	t   *testing.T
	dir string
	trk string
}

func newCLI(t *testing.T) *cli {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".dibs")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"team.yaml": teamYAML, "skills.yaml": skillsYAML, "dibs.yaml": "# defaults\n"} {
		if err := os.WriteFile(filepath.Join(cfg, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	trk := filepath.Join(dir, "tracker.json")
	m, err := tracker.LoadMemory(trk, "acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	m.CreateIssue("POST /orders is not idempotent", "Retries duplicate orders in `app/orders/service.py`.")
	for i := 0; i < 5; i++ {
		m.CreateIssue("Log format tweak", "`app/logging.py`")
	}
	t.Setenv("DIBS_TRACKER", "memory:"+trk)
	t.Setenv("DIBS_DB", filepath.Join(dir, "ledger.sqlite"))
	t.Setenv("DIBS_NOW", "2026-11-02T09:00:00Z")
	return &cli{t: t, dir: dir, trk: trk}
}

func (c *cli) run(want int, args ...string) map[string]any {
	c.t.Helper()
	var out, errb bytes.Buffer
	code := run(append([]string{"--config", filepath.Join(c.dir, ".dibs")}, args...), &out, &errb)
	if code != want {
		c.t.Fatalf("dibs %s: exit %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, want, out.String(), errb.String())
	}
	var v map[string]any
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		c.t.Fatalf("dibs %s: output is not JSON: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return v
}

func messages(v map[string]any) string {
	var b strings.Builder
	ms, _ := v["messages"].([]any)
	for _, m := range ms {
		mm := m.(map[string]any)
		b.WriteString(mm["room"].(string) + ": " + mm["text"].(string) + "\n")
	}
	return b.String()
}

func TestCLIEndToEnd(t *testing.T) {
	c := newCLI(t)
	c.run(0, "init")
	c.run(0, "doctor")

	sw := c.run(0, "sweep")
	if tasks, _ := sw["agent_tasks"].([]any); len(tasks) < 2 {
		t.Fatalf("sweep should ask for triage: %v", sw)
	}
	c.run(0, "triage", "1", "--skills", "idempotency:p", "--est", "0.5", "--paths", "app/orders/service.py")
	for n := 2; n <= 6; n++ {
		c.run(0, "triage", "#"+string(rune('0'+n)), "--accept")
	}
	card := messages(c.run(0, "score"))
	if !strings.Contains(card, "DIBS for @asha → #1") {
		t.Fatalf("card: %s", card)
	}
	c.run(0, "why", "1")

	// Rule refusals exit 2 with a JSON error.
	ref := c.run(2, "approve", "1", "--by", "asha")
	if e := ref["error"].(map[string]any); e["code"] != "not_senior" {
		t.Fatalf("refusal: %v", ref)
	}
	c.run(2, "approve", "1") // missing --by is a usage refusal
	c.run(2, "frobnicate")

	c.run(0, "approve", "1", "--by", "ravi")
	c.run(0, "start", "1", "--by", "asha")
	st := c.run(0, "status", "1")
	if d := st["data"].(map[string]any); d["your_role"] != "coach" {
		t.Fatalf("status: %v", st)
	}
	hr := c.run(0, "hint", "request", "1", "--level", "3", "--agent", "builder")
	if d := hr["data"].(map[string]any); d["granted_level"].(float64) != 1 {
		t.Fatalf("hint request: %v", hr)
	}
	hint := filepath.Join(c.dir, "hint.txt")
	os.WriteFile(hint, []byte("```\nkey = req.headers['Idempotency-Key']\n```"), 0o644)
	c.run(2, "hint", "give", "1", "--level", "1", "--agent", "builder", "--text-file", hint)
	os.WriteFile(hint, []byte("What would the server need to remember about a request it has already seen?"), 0o644)
	c.run(0, "hint", "give", "1", "--level", "1", "--agent", "builder", "--text-file", hint)
	c.run(0, "attempt", "1", "--by", "asha", "--note", "hashed the body")
	c.run(0, "unlock", "1", "L3", "--by", "ravi")

	m, _ := tracker.LoadMemory(c.trk, "acme/shop")
	pr := m.OpenPR("asha-gh", "asha/idem", "Idempotent POST /orders", "Closes #1")
	c.run(0, "sync")
	c.run(0, "review", "--pr", itoa(pr), "--verdict", "ready", "--teaching", "Keys never expire.", "--agent", "reviewer")
	m, _ = tracker.LoadMemory(c.trk, "acme/shop")
	m.MergePR(pr, m.PRsM[pr].MergedAt)
	rec := messages(c.run(0, "sync"))
	if !strings.Contains(rec, "idempotency Exposed→Guided") {
		t.Fatalf("record: %s", rec)
	}
	c.run(0, "map", "asha")
	c.run(0, "mirror")
	html := filepath.Join(c.dir, "dash.html")
	c.run(0, "report", "--html", html)
	if b, err := os.ReadFile(html); err != nil || !bytes.Contains(b, []byte("<svg")) {
		t.Fatalf("dashboard not written: %v", err)
	}
	c.run(0, "budget", "--slots", "2", "--share", "0.3", "--by", "ravi")
	c.run(0, "skills")
	c.run(0, "team")
	c.run(0, "autopilot", "--once", "--dry-run")
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestCLIMisc(t *testing.T) {
	var out, errb bytes.Buffer
	if run([]string{"help"}, &out, &errb) != 0 || !strings.Contains(out.String(), "Daily flow") {
		t.Fatal("help")
	}
	out.Reset()
	if run([]string{"version"}, &out, &errb) != 0 || !strings.HasPrefix(out.String(), "dibs ") {
		t.Fatal("version")
	}
	out.Reset()
	if run([]string{"template"}, &out, &errb) != 0 || !strings.Contains(out.String(), "version: 1") {
		t.Fatal("template")
	}
	// A missing .dibs folder is an error (exit 1) with a helpful message.
	t.Chdir(t.TempDir())
	out.Reset()
	if code := run([]string{"--human", "status", "1"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), ".dibs") {
		t.Fatalf("no config: %d %s", code, errb.String())
	}
	// The offline demo runs end to end.
	dir := t.TempDir()
	out.Reset()
	if code := run([]string{"demo", "--out", dir, "--quiet"}, &out, &errb); code != 0 {
		t.Fatalf("demo: %d %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "dashboard.html")); err != nil {
		t.Fatal(err)
	}
}
