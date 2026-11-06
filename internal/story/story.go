// Package story plays a complete Dibs team story through the real service, ledger
// and engine against an in-memory GitHub, so anyone can see the whole workflow
// in seconds without Switch, GitHub or an LLM. Every number in its dashboard
// comes from running the actual rules.
package story

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vivekyarra/dibs/internal/config"
	"github.com/vivekyarra/dibs/internal/engine"
	"github.com/vivekyarra/dibs/internal/ledger"
	"github.com/vivekyarra/dibs/internal/report"
	"github.com/vivekyarra/dibs/internal/service"
	"github.com/vivekyarra/dibs/internal/tracker"
)

const (
	senior    = "ravi"
	seniorGH  = "ravi-gh"
	junior    = "asha"
	juniorGH  = "asha-gh"
	builderGH = "builder-bot"
)

// Config returns the sample team: one senior, one junior, the demo-shop skills.
func Config(db string) (*config.Config, error) {
	c := &config.Config{
		Dir: "(built-in sample team)",
		DB:  db,
		Team: config.Team{
			Repo: "acme/dibs-demo-shop", TeamRoom: "payments-team",
			Agents:  map[string]string{"dibs": "dibs", "builder": "builder", "reviewer": "reviewer"},
			Seniors: []config.Person{{Handle: senior, GitHub: seniorGH, Chat: []string{"ravi"}}},
			Juniors: []config.Person{{Handle: junior, GitHub: juniorGH, Chat: []string{"asha"}, PracticeRoom: "asha-practice",
				Skills: map[string]engine.Level{"http-handlers": 2, "sql-queries": 2, "testing": 2, "idempotency": 1, "error-handling": 1}}},
		},
		Skills: []config.Skill{
			{Name: "http-handlers", Description: "FastAPI routes, request/response models, status codes", Paths: []string{"app/api/**"}},
			{Name: "sql-queries", Description: "Queries, joins, N+1 avoidance", Paths: []string{"app/**/repo.py"}},
			{Name: "migrations", Description: "Schema changes and migrations", Paths: []string{"migrations/**"}},
			{Name: "idempotency", Description: "Safe retries, idempotency keys, dedup", Paths: []string{"app/orders/**"}},
			{Name: "testing", Description: "pytest fixtures, mocks, flaky tests", Paths: []string{"tests/**"}},
			{Name: "concurrency", Description: "Locks, races, background workers", Paths: []string{"app/workers/**"}},
			{Name: "error-handling", Description: "Exceptions, retries with backoff, error responses", Paths: []string{"app/webhooks/**"}},
			{Name: "observability", Description: "Logging, correlation ids, metrics", Paths: []string{"app/logging.py"}},
		},
		Policy: engine.DefaultPolicy(),
	}
	c.Policy.RiskPaths = []string{"migrations/", "app/auth/", "app/payments/"}
	return c, c.Validate()
}

type story struct {
	svc   *service.Service
	tr    *tracker.Memory
	now   time.Time
	out   io.Writer
	log   strings.Builder
	quiet bool
	err   error
}

func (s *story) at(t time.Time)        { s.now = t }
func (s *story) after(d time.Duration) { s.now = s.now.Add(d) }

func (s *story) narrate(f string, a ...any) {
	line := fmt.Sprintf(f, a...)
	fmt.Fprintf(&s.log, "\n> %s\n", line)
	if !s.quiet {
		fmt.Fprintf(s.out, "\n\033[1;36m▶ %s\033[0m\n", line)
	}
}

func (s *story) show(r *service.Result, err error) *service.Result {
	if err != nil {
		if re, ok := service.IsRule(err); ok {
			fmt.Fprintf(&s.log, "\n**refused (%s):** %s\n", re.Code, re.Message)
			if !s.quiet {
				fmt.Fprintf(s.out, "  \033[33m✋ refused (%s): %s\033[0m\n", re.Code, re.Message)
			}
			return nil
		}
		if s.err == nil {
			s.err = err
		}
		return nil
	}
	for _, t := range r.AgentTasks {
		fmt.Fprintf(&s.log, "\n_task for the dibs agent:_ %s\n", t)
		if !s.quiet {
			fmt.Fprintf(s.out, "  \033[35m🤖 task for the dibs agent: %s\033[0m\n", t)
		}
	}
	for _, m := range r.Messages {
		fmt.Fprintf(&s.log, "\n**#%s** · %s\n\n%s\n", m.Room, s.now.Format("Mon 15:04"), m.Text)
		if !s.quiet {
			fmt.Fprintf(s.out, "\033[2m#%s · %s\033[0m\n%s\n", m.Room, s.now.Format("Mon 15:04"), indent(m.Text))
		}
	}
	return r
}

func indent(s string) string { return "  " + strings.ReplaceAll(s, "\n", "\n  ") }

func (s *story) must(r *service.Result, err error) *service.Result {
	if err != nil && s.err == nil {
		s.err = err
	}
	return r
}

func (s *story) issue(title, body string, skills []engine.SkillReq, est float64, paths ...string) int {
	n := s.tr.CreateIssue(title, body)
	s.must(s.svc.Sync("dibs"))
	s.must(s.svc.Triage(n, skills, est, paths, "dibs"))
	return n
}

func p(n string) engine.SkillReq   { return engine.SkillReq{Name: n, Weight: engine.Primary} }
func sec(n string) engine.SkillReq { return engine.SkillReq{Name: n, Weight: engine.Secondary} }

var hintText = map[int]string{
	1: "What would the server need to remember if the same request arrived twice?",
	2: "Look at where the service writes the new record, and read about the idempotency key pattern. Where would you check for an earlier result before writing?",
	3: "1. read the key the client sent\n2. look for an earlier result stored under that key\n3. if there is one, return it unchanged\n4. otherwise do the work, then store the result under the key",
}

// rep plays one learning rep from issue to recorded evidence.
func (s *story) rep(n int, hints []int, rounds, seniorComments int, teaching string, hours float64) {
	s.must(s.svc.Score("dibs"))
	s.after(20 * time.Minute)
	s.must(s.svc.Approve(n, senior))
	s.after(time.Hour)
	s.must(s.svc.Start(n, junior))
	for i, h := range hints {
		s.after(time.Duration(hours*60/float64(len(hints)+2)) * time.Minute)
		if i > 0 || h > 1 {
			s.must(s.svc.Attempt(n, junior, "tried an approach, a test still fails"))
		}
		s.must(s.svc.HintRequest(n, h, "builder"))
		s.must(s.svc.HintGive(n, h, "builder", hintText[h]))
	}
	s.after(time.Duration(hours*60/float64(len(hints)+2)) * time.Minute)
	pr := s.tr.OpenPR(juniorGH, fmt.Sprintf("asha/%d", n), fmt.Sprintf("Fix #%d", n), fmt.Sprintf("Closes #%d", n))
	s.must(s.svc.Sync("dibs"))
	for i := 0; i < rounds; i++ {
		s.after(40 * time.Minute)
		s.must(s.svc.Review(pr, "changes_requested", "", "reviewer"))
	}
	s.after(30 * time.Minute)
	s.must(s.svc.Review(pr, "ready", teaching, "reviewer"))
	if seniorComments > 0 {
		_ = s.tr.ReviewPR(pr, seniorGH, "COMMENTED", seniorComments)
	}
	_ = s.tr.ReviewPR(pr, seniorGH, "APPROVED", 0)
	s.must(s.svc.Sync("dibs"))
	s.after(15 * time.Minute)
	_ = s.tr.MergePR(pr, s.now)
	s.must(s.svc.Sync("dibs"))
}

// history plays three weeks of earlier reps so the dashboard has a trend.
func (s *story) history(start time.Time) {
	s.at(start)
	s.svc.Now = func() time.Time { return s.now }
	s.tr.NextNum = 101 // earlier weeks use high issue numbers; this week starts at #1
	// A normal backlog: the builder's work keeps the learning budget honest.
	var chores []int
	for i := 1; i <= 8; i++ {
		chores = append(chores, s.issue(fmt.Sprintf("Bump log format in service %d", i), "Routine change to `app/logging.py`.", []engine.SkillReq{p("observability")}, 0.3, "app/logging.py"))
	}
	type r struct {
		title, body string
		skill       string
		path        string
		hints       []int
		rounds      int
		comments    int
		teaching    string
		hours       float64
	}
	weeks := [][]r{
		{
			{"Return 422 with field errors on invalid order payloads", "Validation errors are a bare 400.", "http-handlers", "app/api/orders.py", []int{1, 2, 3}, 2, 3, "Error shape is a public contract: worth agreeing on the format once.", 5},
			{"Customer lookup does a full table scan", "Add the right index and query.", "sql-queries", "app/orders/repo.py", []int{1, 2}, 2, 2, "", 4},
			{"Fixture leaks state between tests", "Tests pass alone, fail together.", "testing", "tests/conftest.py", []int{1, 2, 3}, 1, 2, "Why shared fixtures should be function-scoped by default.", 4},
		},
		{
			{"Add GET /orders/{id}/items", "New read endpoint.", "http-handlers", "app/api/items.py", []int{1}, 1, 1, "", 3},
			{"Order totals query double-counts refunds", "Join is wrong.", "sql-queries", "app/orders/repo.py", []int{1, 2}, 1, 1, "", 3},
			{"Mock the clock in report tests", "Tests depend on wall time.", "testing", "tests/test_reports.py", []int{1}, 1, 0, "", 2},
		},
		{
			{"Add ETag to order responses", "Support conditional GET.", "http-handlers", "app/api/orders.py", nil, 0, 0, "", 2},
			{"Paginate the customer export query", "Export loads everything.", "sql-queries", "app/orders/repo.py", []int{1}, 0, 0, "", 2},
			{"Parametrize the status transition tests", "Copy-pasted tests.", "testing", "tests/test_status.py", nil, 1, 0, "", 1.5},
		},
	}
	for wi, week := range weeks {
		s.at(start.AddDate(0, 0, 7*wi))
		for i, x := range week {
			s.at(start.AddDate(0, 0, 7*wi+i*2))
			n := s.issue(x.title, x.body+"\n\nTouches `"+x.path+"`.", []engine.SkillReq{p(x.skill)}, 0.75, x.path)
			s.rep(n, x.hints, x.rounds, x.comments, x.teaching, x.hours)
		}
	}
	// The builder clears the chores.
	for _, n := range chores {
		pr := s.tr.OpenPR(builderGH, fmt.Sprintf("builder/%d", n), "Chore", fmt.Sprintf("Closes #%d", n))
		_ = s.tr.MergePR(pr, s.now)
	}
	s.must(s.svc.Sync("dibs"))
}

// demoIssues are the ten demo-shop issues, in issue-number order of the live part.
type demoIssue struct {
	title, body string
	skills      []engine.SkillReq
	est         float64
	path        string
}

var demoIssues = []demoIssue{
	{"Typo in 404 error message", "The not-found message says `Ordr not found`. Fix in `app/api/errors.py`.", []engine.SkillReq{p("http-handlers")}, 0.1, "app/api/errors.py"},
	{"GET /orders returns every row: add pagination", "Add `limit`/`cursor` to `app/api/orders.py`.", []engine.SkillReq{p("http-handlers"), sec("sql-queries")}, 1, "app/api/orders.py"},
	{"POST /orders creates duplicates when the client retries: make it idempotent", "Mobile clients retry on timeouts and customers get charged twice. Support an `Idempotency-Key` header in `app/orders/service.py`.\n\n## Acceptance criteria\n- Same key + same body returns the original order, status 200\n- Same key + different body returns 422\n- `tests/known_bugs/test_known_bugs.py::test_post_orders_idempotent` passes",
		[]engine.SkillReq{p("idempotency"), sec("error-handling"), sec("http-handlers")}, 0.5, "app/orders/service.py"},
	{"Partition the orders table by month", "Add `migrations/0002_partition.sql`.", []engine.SkillReq{p("migrations")}, 3, "migrations/0002_partition.sql"},
	{"Add per-API-key rate limiting", "New middleware in `app/api/ratelimit.py`.", []engine.SkillReq{p("concurrency"), p("observability")}, 2, "app/api/ratelimit.py"},
	{"Webhook sender gives up on the first failure: add retries with backoff", "`app/webhooks/sender.py` drops events on a single 5xx.", []engine.SkillReq{p("error-handling"), sec("concurrency")}, 1, "app/webhooks/sender.py"},
	{"test_order_totals is flaky", "Fails about 1 in 20 runs. See `tests/test_orders.py`.", []engine.SkillReq{p("testing")}, 0.5, "tests/test_orders.py"},
	{"N+1 query when listing orders with items", "`app/orders/repo.py` queries items per order.", []engine.SkillReq{p("sql-queries")}, 0.5, "app/orders/repo.py"},
	{"Add a correlation id to every log line", "Thread a request id through `app/logging.py`.", []engine.SkillReq{p("observability")}, 1, "app/logging.py"},
	{"Daily report uses server local time instead of UTC", "`app/reports/daily.py` uses `datetime.now()`.", []engine.SkillReq{p("testing"), sec("error-handling")}, 1, "app/reports/daily.py"},
}

// live plays the story shown in the video: the sweep, the rep, the guard, the release.
func (s *story) live(start time.Time) {
	s.at(start)
	s.tr.NextNum = 1
	nums := map[int]int{}
	for i, d := range demoIssues {
		nums[i+1] = s.tr.CreateIssue(d.title, d.body)
	}
	s.narrate("Monday 09:00. Ten new issues land in acme/dibs-demo-shop. The dibs agent sweeps.")
	s.must(s.svc.Sync("dibs"))
	for i, d := range demoIssues {
		s.must(s.svc.Triage(nums[i+1], d.skills, d.est, []string{d.path}, "dibs"))
	}
	s.show(s.svc.Score("dibs"))
	rep, next := nums[3], nums[6]

	s.after(10 * time.Minute)
	s.narrate("Asha (junior) tries to approve their own rep. Only a senior can.")
	s.show(s.svc.Approve(rep, junior))
	s.narrate("Ravi (senior) presses Allow in Switch; dibs runs `dibs approve`.")
	s.show(s.svc.Approve(rep, senior))

	s.narrate("The builder (Claude Code) takes the tickets that are not reps and merges them.")
	for _, n := range []int{nums[1], nums[2], nums[7], nums[8]} {
		pr := s.tr.OpenPR(builderGH, fmt.Sprintf("builder/%d", n), "Fix", fmt.Sprintf("Closes #%d", n))
		_ = s.tr.MergePR(pr, s.now)
	}
	s.must(s.svc.Sync("dibs"))

	s.after(30 * time.Minute)
	s.narrate("In the private practice room, Asha starts the rep.")
	s.show(s.svc.Start(rep, junior))

	s.after(5 * time.Minute)
	s.narrate("Asha asks the builder for the code. The builder asks the ledger for an L3 hint and gets L1.")
	if r := s.show(s.svc.HintRequest(rep, 3, "builder")); r != nil {
		d := r.Data.(map[string]any)
		s.narrate("Ledger: granted L%v — %v (%v)", d["granted_level"], d["reason"], d["next_unlock"])
	}
	s.narrate("The builder drafts a hint that pastes code. `dibs hint give` checks the text and refuses it.")
	s.show(s.svc.HintGive(rep, 1, "builder", "Do this:\n```python\nkey = request.headers['Idempotency-Key']\nif key in store:\n    return store[key]\n```"))
	s.narrate("The builder rewrites it as an L1 nudge. Accepted and posted.")
	if s.show(s.svc.HintGive(rep, 1, "builder", hintText[1])) != nil {
		s.show(&service.Result{Messages: []service.Message{{Room: "asha-practice", Text: "builder: " + hintText[1]}}}, nil)
	}

	s.after(25 * time.Minute)
	s.narrate("Asha tries something and reports it to the reviewer (Codex). The attempt unlocks L2; Codex obeys the same room rule.")
	s.show(s.svc.Attempt(rep, junior, "stored a hash of the body; duplicate row still inserted"))
	s.show(s.svc.HintGive(rep, 2, "reviewer", hintText[2]))
	s.show(&service.Result{Messages: []service.Message{{Room: "asha-practice", Text: "reviewer: " + hintText[2]}}}, nil)

	s.after(20 * time.Minute)
	s.narrate("Someone asks the builder to just fix #3. Its PR is flagged, and dibs-guard blocks it on GitHub.")
	s.tr.OpenPR(builderGH, "builder/3", "Make POST /orders idempotent", fmt.Sprintf("Closes #%d", rep))
	s.show(s.svc.Sync("dibs"))

	s.after(70 * time.Minute)
	s.narrate("Asha opens their PR. The reviewer does a Socratic review: one round of questions, then ready, with one teaching moment for Ravi.")
	pr := s.tr.OpenPR(juniorGH, "asha/idempotency-key", "Make POST /orders idempotent", fmt.Sprintf("Closes #%d", rep))
	s.show(s.svc.Sync("dibs"))
	s.after(30 * time.Minute)
	s.show(s.svc.Review(pr, "changes_requested", "", "reviewer"))
	s.after(40 * time.Minute)
	s.show(s.svc.Review(pr, "ready", "Correct, but keys are stored forever (no TTL). Retention trade-offs.", "reviewer"))
	_ = s.tr.ReviewPR(pr, seniorGH, "APPROVED", 1)
	s.must(s.svc.Sync("dibs"))
	s.after(10 * time.Minute)
	s.narrate("Ravi merges. dibs records the evidence and moves the skill map.")
	_ = s.tr.MergePR(pr, s.now)
	s.show(s.svc.Sync("dibs"))

	s.after(time.Hour)
	s.narrate("Next sweep: with idempotency now Guided, #6 (webhook retries) becomes the best rep.")
	s.show(s.svc.Score("dibs"))
	s.show(s.svc.Approve(next, senior))
	s.after(20 * time.Minute)
	s.must(s.svc.Start(next, junior))

	s.after(2 * time.Hour)
	s.narrate("A customer escalation moves #6's deadline to tonight. dibs releases it automatically; delivery never waits on a rep.")
	s.tr.SetLabels(next, "due:"+s.now.Add(time.Hour).Format("2006-01-02T15:04"))
	s.show(s.svc.Sync("dibs"))
	s.narrate("The builder finishes from Asha's branch and credits them. dibs asks for a walkthrough.")
	bpr := s.tr.OpenPR(builderGH, "builder/6", "Retry webhooks with backoff", fmt.Sprintf("Builds on @%s's work. Closes #%d", juniorGH, next))
	_ = bpr
	s.must(s.svc.Sync("dibs"))
	s.after(time.Hour)
	_ = s.tr.MergePR(bpr, s.now)
	s.show(s.svc.Sync("dibs"))

	s.narrate("End of the week: the ramp report.")
	s.show(s.svc.Report(junior))
	s.narrate("Asha's skill map, as mirrored into the Switch room document only dibs can edit.")
	s.show(s.svc.Map(junior))
}

// Run plays the full story and writes transcript.md, report.md and dashboard.html to outDir.
func Run(outDir string, out io.Writer, quiet bool) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	db := filepath.Join(outDir, "ledger.sqlite")
	for _, f := range []string{db, db + "-wal", db + "-shm"} {
		_ = os.Remove(f)
	}
	cfg, err := Config(db)
	if err != nil {
		return err
	}
	l, err := ledger.Open(db)
	if err != nil {
		return err
	}
	defer l.Close()
	tr := tracker.NewMemory(cfg.Team.Repo)
	st := &story{tr: tr, out: out, quiet: quiet}
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	st.now = start
	st.svc = &service.Service{Cfg: cfg, L: l, T: tr, Now: func() time.Time { return st.now }, Sample: true}
	if _, err := st.svc.Init(); err != nil {
		return err
	}
	if !quiet {
		fmt.Fprintln(out, "\033[1mDibs demo\033[0m: three weeks of reps, then the week shown in the video. Real rules, in-memory GitHub.")
	}
	st.quiet = true
	st.history(start)
	st.quiet = quiet
	if st.err != nil {
		return fmt.Errorf("history: %w", st.err)
	}
	st.live(start.AddDate(0, 0, 21))
	if st.err != nil {
		return fmt.Errorf("story: %w", st.err)
	}
	in, err := st.svc.ReportInput()
	if err != nil {
		return err
	}
	html, err := report.HTML(in)
	if err != nil {
		return err
	}
	rep, err := st.svc.Report(junior)
	if err != nil {
		return err
	}
	var md strings.Builder
	for _, m := range rep.Messages {
		if m.Room == cfg.Team.TeamRoom {
			md.WriteString(m.Text + "\n")
		}
	}
	files := map[string][]byte{
		"dashboard.html": html,
		"index.html":     html,
		"report.md":      []byte(md.String()),
		"transcript.md":  []byte("# Dibs demo transcript\n\nEvery message below was produced by the Dibs ledger running its real rules.\n" + st.log.String()),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(outDir, name), b, 0o644); err != nil {
			return err
		}
	}
	if !quiet {
		abs, _ := filepath.Abs(outDir)
		fmt.Fprintf(out, "\n\033[1;32m✔ Done.\033[0m Dashboard: %s\n  Transcript: %s\n", filepath.Join(abs, "dashboard.html"), filepath.Join(abs, "transcript.md"))
	}
	return nil
}

// Main is `dibs demo`.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	outDir := fs.String("out", "dibs-demo", "folder for dashboard.html, report.md and transcript.md")
	quiet := fs.Bool("quiet", false, "only write files")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := Run(*outDir, stdout, *quiet); err != nil {
		fmt.Fprintln(stderr, "demo failed:", err)
		return 1
	}
	return 0
}
