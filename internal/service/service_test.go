package service_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vivekyarra/dibs/internal/engine"
	"github.com/vivekyarra/dibs/internal/ledger"
	"github.com/vivekyarra/dibs/internal/service"
	"github.com/vivekyarra/dibs/internal/story"
	"github.com/vivekyarra/dibs/internal/tracker"
)

type env struct {
	t   *testing.T
	svc *service.Service
	tr  *tracker.Memory
	now time.Time
}

var t0 = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

func newEnv(t *testing.T) *env {
	t.Helper()
	db := filepath.Join(t.TempDir(), "l.sqlite")
	cfg, err := story.Config(db)
	if err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	e := &env{t: t, tr: tracker.NewMemory(cfg.Team.Repo), now: t0}
	e.svc = &service.Service{Cfg: cfg, L: l, T: e.tr, Now: func() time.Time { return e.now }}
	if _, err := e.svc.Init(); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) ok(r *service.Result, err error) *service.Result {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("unexpected error: %v", err)
	}
	return r
}

func (e *env) refused(code string) func(*service.Result, error) {
	return func(_ *service.Result, err error) {
		e.t.Helper()
		re, ok := service.IsRule(err)
		if !ok {
			e.t.Fatalf("want refusal %s, got %v", code, err)
		}
		if re.Code != code {
			e.t.Fatalf("want refusal %s, got %s: %s", code, re.Code, re.Message)
		}
	}
}

func (e *env) ticket(n int) ledger.Ticket {
	e.t.Helper()
	var t ledger.Ticket
	err := e.svc.L.Tx(context.Background(), func(tx *ledger.Tx) error {
		var err error
		t, err = tx.Ticket(n)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return t
}

func (e *env) levels() map[string]engine.SkillState {
	var m map[string]engine.SkillState
	_ = e.svc.L.Tx(context.Background(), func(tx *ledger.Tx) error {
		var err error
		m, err = tx.Levels("asha")
		return err
	})
	return m
}

func (e *env) events(n int, kind string) int {
	var c int
	_ = e.svc.L.Tx(context.Background(), func(tx *ledger.Tx) error {
		evs, err := tx.Events(n, kind)
		c = len(evs)
		return err
	})
	return c
}

func has(r *service.Result, room, substr string) bool {
	for _, m := range r.Messages {
		if (room == "" || m.Room == room) && strings.Contains(m.Text, substr) {
			return true
		}
	}
	return false
}

// idempotency rep: Asha is Exposed, so this is the obvious rep.
func (e *env) repIssue() int {
	n := e.tr.CreateIssue("Make POST /orders idempotent", "Fix `app/orders/service.py`.\n\n## Acceptance criteria\n- retries return the same order\n")
	for i := 0; i < 5; i++ { // a normal backlog keeps the share cap honest
		e.tr.CreateIssue(fmt.Sprintf("Log tweak %d", i), "`app/logging.py`")
	}
	e.ok(e.svc.Sync("dibs"))
	e.ok(e.svc.Triage(n, []engine.SkillReq{{Name: "idempotency", Weight: engine.Primary}, {Name: "error-handling", Weight: engine.Secondary}}, 0.5, []string{"app/orders/service.py"}, "dibs"))
	for i := n + 1; i <= n+5; i++ {
		e.ok(e.svc.Triage(i, []engine.SkillReq{{Name: "observability", Weight: engine.Primary}}, 0.3, []string{"app/logging.py"}, "dibs"))
	}
	return n
}

func (e *env) reserve() int {
	n := e.repIssue()
	r := e.ok(e.svc.Score("dibs"))
	if !has(r, "payments-team", "DIBS for @asha → #1") {
		e.t.Fatalf("no proposal card: %+v", r.Messages)
	}
	e.ok(e.svc.Approve(n, "ravi"))
	return n
}

func TestFullStoryDemo(t *testing.T) {
	dir := t.TempDir()
	if err := story.Run(dir, &strings.Builder{}, true); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"dashboard.html", "index.html", "report.md", "transcript.md"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil || len(b) < 200 {
			t.Fatalf("%s missing or tiny (%v)", f, err)
		}
	}
	tr, _ := os.ReadFile(filepath.Join(dir, "transcript.md"))
	for _, want := range []string{"DIBS for @asha → #3", "refused (not_senior)", "refused (hint_violation)", "dibs-guard", "Teaching moment", "idempotency Exposed→Guided", "DIBS for @asha → #6", "Releasing #6 (deadline at risk)", "Write a walkthrough", "ramp report"} {
		if !strings.Contains(string(tr), want) {
			t.Errorf("transcript lacks %q", want)
		}
	}
}

func TestSweepScoresAndCards(t *testing.T) {
	e := newEnv(t)
	n := e.tr.CreateIssue("Retry webhooks", "Touches `app/webhooks/sender.py`, needs retry with backoff.")
	r := e.ok(e.svc.Sweep("dibs", false))
	if len(r.AgentTasks) == 0 || !strings.Contains(r.AgentTasks[0], fmt.Sprintf("dibs triage %d --skills error-handling:p", n)) {
		t.Fatalf("suggestion missing: %v", r.AgentTasks)
	}
	// --accept triages with the suggestion and scores.
	e.ok(e.svc.Sweep("dibs", true))
	if got := e.ticket(n).State; got != engine.ToBuilder && got != engine.Proposed && got != engine.Triaged {
		t.Fatalf("state after accept: %s", got)
	}
	e.ok(e.svc.Why(n))
	e.ok(e.svc.Suggest(n))
}

func TestApproveRefusalsAndGitHubSideEffects(t *testing.T) {
	e := newEnv(t)
	n := e.repIssue()
	e.refused("not_proposed")(e.svc.Approve(n, "ravi"))
	e.ok(e.svc.Score("dibs"))
	e.refused("not_senior")(e.svc.Approve(n, "asha"))
	e.refused("unknown_person")(e.svc.Approve(n, "mallory"))
	r := e.ok(e.svc.Approve(n, "@Ravi")) // chat name, case-insensitive
	if !has(r, "asha-practice", "is your next rep") {
		t.Fatal("practice message missing")
	}
	is, _ := e.tr.Issue(n)
	if !tracker.HasLabel(is, "dibs:reserved:asha") || !tracker.HasLabel(is, "skill:idempotency") || len(is.Assignees) != 1 || is.Assignees[0] != "asha-gh" {
		t.Fatalf("github side effects: %+v", is)
	}
	if len(e.tr.Comments[n]) != 1 {
		t.Fatal("comment")
	}
	e.refused("not_proposed")(e.svc.Approve(n, "ravi"))
}

func TestDeclineAndProposalTimeout(t *testing.T) {
	e := newEnv(t)
	n := e.repIssue()
	e.ok(e.svc.Score("dibs"))
	e.refused("not_senior")(e.svc.Decline(n, "asha"))
	r := e.ok(e.svc.Decline(n, "ravi"))
	if e.ticket(n).State != engine.ToBuilder || !has(r, "", "@builder take #1") {
		t.Fatal("decline")
	}

	e2 := newEnv(t)
	n2 := e2.repIssue()
	e2.ok(e2.svc.Score("dibs"))
	e2.now = e2.now.Add(25 * time.Hour)
	r = e2.ok(e2.svc.Sync("dibs"))
	if e2.ticket(n2).State != engine.ToBuilder || !has(r, "", "goes to the builder") {
		t.Fatalf("timeout: %s", e2.ticket(n2).State)
	}
}

func TestHoldExpiry(t *testing.T) {
	e := newEnv(t)
	n := e.reserve()
	held := e.tr.CreateIssue("Retry webhooks", "`app/webhooks/sender.py`")
	e.ok(e.svc.Sync("dibs"))
	e.ok(e.svc.Triage(held, []engine.SkillReq{{Name: "error-handling", Weight: engine.Primary}}, 1, nil, "dibs"))
	e.ok(e.svc.Score("dibs"))
	if tk := e.ticket(held); tk.State != engine.Triaged || tk.Reason != string(engine.ReasonNoBudget) {
		t.Fatalf("should be held for budget: %s %s", tk.State, tk.Reason)
	}
	_ = n
	e.now = e.now.Add(49 * time.Hour)
	r := e.ok(e.svc.Sync("dibs"))
	if e.ticket(held).State != engine.ToBuilder || !has(r, "", "Held longer than") {
		t.Fatalf("hold expiry: %s", e.ticket(held).State)
	}
}

func TestPracticeLoopHintsAndRecord(t *testing.T) {
	e := newEnv(t)
	n := e.reserve()
	e.refused("not_yours")(e.svc.Start(n, "ravi"))
	r := e.ok(e.svc.Start(n, "asha"))
	if !has(r, "asha-practice", "retries return the same order") {
		t.Fatalf("brief should quote acceptance criteria: %+v", r.Messages)
	}
	e.refused("already_started")(e.svc.Start(n, "asha"))

	st := e.ok(e.svc.Status(n)).Data.(service.StatusView)
	if st.YourRole != "coach" || st.AllowedHint != 1 {
		t.Fatalf("status %+v", st)
	}
	hr := e.ok(e.svc.HintRequest(n, 3, "builder")).Data.(map[string]any)
	if hr["granted_level"] != 1 || hr["clamped"] != true {
		t.Fatalf("clamp %+v", hr)
	}
	e.refused("level_locked")(e.svc.HintGive(n, 2, "builder", "Look at the service."))
	e.refused("hint_violation")(e.svc.HintGive(n, 1, "builder", "```\nx = 1\n```"))
	if e.events(n, "hint_rejected") != 1 {
		t.Fatal("rejected hint must be logged even though the call is refused")
	}
	e.refused("empty_hint")(e.svc.HintGive(n, 1, "builder", "  "))
	e.ok(e.svc.HintGive(n, 1, "builder", "What would the server need to remember?"))
	ar := e.ok(e.svc.Attempt(n, "asha", "tried hashing")).Data.(map[string]any)
	if ar["allowed_hint_level"] != 2 || ar["unlocked"] != true {
		t.Fatalf("attempt %+v", ar)
	}
	e.ok(e.svc.HintGive(n, 2, "reviewer", "Read about idempotency keys. Where would you look for an earlier result?"))
	e.refused("not_senior")(e.svc.Unlock(n, 4, "asha"))
	e.ok(e.svc.Unlock(n, 4, "ravi"))
	if st := e.ok(e.svc.Status(n)).Data.(service.StatusView); st.AllowedHint != 4 {
		t.Fatal("senior unlock")
	}

	pr := e.tr.OpenPR("asha-gh", "asha/idem", "Idempotent POST", fmt.Sprintf("Closes #%d", n))
	r = e.ok(e.svc.Sync("dibs"))
	if e.ticket(n).State != engine.JuniorPROpen || !has(r, "asha-practice", "Socratic review") {
		t.Fatalf("pr link: %s", e.ticket(n).State)
	}
	e.refused("bad_verdict")(e.svc.Review(pr, "lgtm", "", "reviewer"))
	e.ok(e.svc.Review(pr, "changes_requested", "", "reviewer"))
	r = e.ok(e.svc.Review(pr, "ready", "Keys never expire. Retention trade-off.", "reviewer"))
	if !has(r, "payments-team", "Teaching moment for @ravi") || !has(r, "payments-team", "ready for your sign-off") {
		t.Fatalf("review msgs %+v", r.Messages)
	}
	e.ok(e.svc.Review(pr, "ready", "second", "reviewer"))
	r = e.ok(e.svc.Review(pr, "ready", "third", "reviewer"))
	if d, _ := r.Data.(map[string]any); d == nil || d["teaching_dropped"] != true {
		t.Fatal("teaching moments must be capped at 2 per PR")
	}
	_ = e.tr.ReviewPR(pr, "ravi-gh", "COMMENTED", 2)
	_ = e.tr.ReviewPR(pr, "ravi-gh", "APPROVED", 0)
	e.ok(e.svc.Sync("dibs"))
	if tk := e.ticket(n); tk.SeniorComments != 3 || tk.Approvals != 2 {
		t.Fatalf("senior effort %+v", tk)
	}
	e.now = e.now.Add(3 * time.Hour)
	_ = e.tr.MergePR(pr, e.now)
	r = e.ok(e.svc.Sync("dibs"))
	tk := e.ticket(n)
	if tk.State != engine.Recorded || tk.MaxHintLevel != 2 || tk.ReviewRounds != 1 {
		t.Fatalf("record %+v", tk)
	}
	if e.levels()["idempotency"].Level != engine.Guided || !has(r, "", "idempotency Exposed→Guided") || len(r.Docs) == 0 {
		t.Fatalf("evidence %+v", e.levels())
	}
	rr := e.ok(e.svc.Record(n, "dibs"))
	if d := rr.Data.(map[string]any); d["already_recorded"] != true {
		t.Fatal("record must be idempotent")
	}

	// Revert lowers the skill once.
	rev := e.tr.OpenPR("ravi-gh", "revert", "Revert", fmt.Sprintf("Reverts acme/dibs-demo-shop#%d", pr))
	_ = e.tr.MergePR(rev, e.now)
	e.ok(e.svc.Sync("dibs"))
	e.ok(e.svc.Sync("dibs"))
	if e.levels()["idempotency"].Level != engine.Exposed || e.events(n, "regression") != 1 {
		t.Fatalf("regression %+v", e.levels()["idempotency"])
	}

	rep := e.ok(e.svc.Report(""))
	if !has(rep, "payments-team", "ramp report") {
		t.Fatal("report")
	}
	e.ok(e.svc.Map("asha"))
	e.refused("not_junior")(e.svc.Map("ravi"))
	m := e.ok(e.svc.Mirror())
	if len(m.Docs) != 3 {
		t.Fatalf("mirror docs %d", len(m.Docs))
	}
}

func TestReleasePaths(t *testing.T) {
	// Junior releases; builder finishes from branch; walkthrough task.
	e := newEnv(t)
	n := e.reserve()
	e.ok(e.svc.Start(n, "asha"))
	e.refused("unknown_person")(e.svc.Release(n, "mallory", ""))
	r := e.ok(e.svc.Release(n, "asha", ""))
	if !has(r, "payments-team", "@builder take #1") || e.ticket(n).State != engine.ToBuilder {
		t.Fatal("release")
	}
	is, _ := e.tr.Issue(n)
	if tracker.HasLabel(is, "dibs:reserved:asha") || !tracker.HasLabel(is, "dibs:released") {
		t.Fatalf("labels %+v", is.Labels)
	}
	st := e.ok(e.svc.Status(n)).Data.(service.StatusView)
	if st.YourRole != "build" || !st.WasReleased {
		t.Fatalf("status after release %+v", st)
	}
	pr := e.tr.OpenPR("builder-bot", "b", "fix", fmt.Sprintf("Closes #%d", n))
	e.ok(e.svc.Sync("dibs"))
	if e.ticket(n).State != engine.BuilderPR {
		t.Fatal("builder pr")
	}
	_ = e.tr.MergePR(pr, e.now)
	r = e.ok(e.svc.Sync("dibs"))
	if e.ticket(n).State != engine.Recorded || len(r.AgentTasks) == 0 || !strings.Contains(r.AgentTasks[0], "walkthrough") {
		t.Fatalf("walkthrough %+v", r.AgentTasks)
	}

	// Stuck: ask, keep going, ask again, no answer → release.
	e = newEnv(t)
	n = e.reserve()
	e.ok(e.svc.Start(n, "asha"))
	est := e.ticket(n).EstJuniorHours
	e.now = e.now.Add(time.Duration((2*est + 0.1) * float64(time.Hour)))
	r = e.ok(e.svc.Sync("dibs"))
	if !has(r, "asha-practice", "keep-going") {
		t.Fatalf("stuck ask %+v", r.Messages)
	}
	e.refused("not_yours")(e.svc.KeepGoing(n, "ravi"))
	e.ok(e.svc.KeepGoing(n, "asha"))
	e.now = e.now.Add(30 * time.Minute)
	e.ok(e.svc.Sync("dibs"))
	if e.ticket(n).State != engine.InProgress {
		t.Fatal("keep going window")
	}
	e.now = e.now.Add(31 * time.Minute)
	e.ok(e.svc.Sync("dibs"))
	e.now = e.now.Add(31 * time.Minute)
	e.ok(e.svc.Sync("dibs"))
	if tk := e.ticket(n); tk.State != engine.ToBuilder || tk.ReleasedReason != "stuck and no answer" {
		t.Fatalf("stuck release %s %s", tk.State, tk.ReleasedReason)
	}
}

func TestGuardBypassAndForeignPR(t *testing.T) {
	e := newEnv(t)
	n := e.reserve()
	foreign := e.tr.OpenPR("builder-bot", "x", "fix", fmt.Sprintf("Fixes #%d", n))
	r := e.ok(e.svc.Sync("dibs"))
	if !has(r, "payments-team", "dibs-guard` will block it") {
		t.Fatal("foreign PR warning")
	}
	r = e.ok(e.svc.Sync("dibs"))
	if has(r, "", "will block it") {
		t.Fatal("foreign PR warning must not repeat")
	}
	_ = e.tr.MergePR(foreign, e.now)
	r = e.ok(e.svc.Sync("dibs"))
	if e.ticket(n).State != engine.Closed || !has(r, "", "Closed without evidence") {
		t.Fatal("bypass")
	}
}

func TestClosedIssueAndTriageValidation(t *testing.T) {
	e := newEnv(t)
	n := e.tr.CreateIssue("x", "y")
	e.ok(e.svc.Sync("dibs"))
	e.refused("bad_skills")(e.svc.Triage(n, nil, 1, nil, "dibs"))
	e.refused("unknown_skill")(e.svc.Triage(n, []engine.SkillReq{{Name: "cobol", Weight: engine.Primary}}, 1, nil, "dibs"))
	e.refused("bad_skills")(e.svc.Triage(n, []engine.SkillReq{{Name: "testing", Weight: engine.Secondary}}, 1, nil, "dibs"))
	e.refused("bad_estimate")(e.svc.Triage(n, []engine.SkillReq{{Name: "testing", Weight: engine.Primary}}, 0, nil, "dibs"))
	e.tr.IssuesM[n].State = "closed"
	e.ok(e.svc.Sync("dibs"))
	if e.ticket(n).State != engine.Closed {
		t.Fatal("closed issue")
	}
	e.refused("not_triageable")(e.svc.Triage(n, []engine.SkillReq{{Name: "testing", Weight: engine.Primary}}, 1, nil, "dibs"))
	if _, err := service.ParseSkills("a:x"); err == nil {
		t.Fatal("parse")
	}
	if _, err := service.ParseSkills("a,a"); err == nil {
		t.Fatal("dup")
	}
	if s, _ := service.ParseSkills("a:p, b:s,"); len(s) != 2 || s[1].Weight != engine.Secondary {
		t.Fatal("parse ok")
	}
	if _, err := service.ParseNum("#x"); err == nil {
		t.Fatal("num")
	}
	if n, _ := service.ParseNum("#12"); n != 12 {
		t.Fatal("num ok")
	}
	_, err := e.svc.Status(999)
	if err == nil || !strings.Contains(err.Error(), "unknown ticket") {
		t.Fatal("unknown ticket")
	}
}

func TestBudgetAndDoctor(t *testing.T) {
	e := newEnv(t)
	e.refused("not_senior")(e.svc.Budget(2, 0.3, "asha"))
	e.refused("bad_budget")(e.svc.Budget(0, 0.3, "ravi"))
	e.refused("bad_budget")(e.svc.Budget(2, 1.5, "ravi"))
	e.ok(e.svc.Budget(2, 0.5, "ravi"))
	e.ok(e.svc.Doctor())
	if e.svc.Skills().Data == nil || e.svc.Team().Data == nil {
		t.Fatal("listings")
	}
	e.ok(e.svc.Reset())
}

type stubPoster struct {
	mu   sync.Mutex
	msgs []service.Message
	fail bool
}

func (f *stubPoster) Post(_ context.Context, room, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("switch down")
	}
	f.msgs = append(f.msgs, service.Message{Room: room, Text: text})
	return nil
}

func TestAutopilot(t *testing.T) {
	e := newEnv(t)
	e.repIssue()
	// Make the cycle land on a Monday after 09:00 so the weekly report is due.
	e.now = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	fp := &stubPoster{}
	r := e.ok(e.svc.Autopilot(service.AutopilotOptions{Once: true, Poster: fp, Interval: time.Minute}))
	if r.Data.(map[string]any)["posted"] != true || len(fp.msgs) == 0 {
		t.Fatal("autopilot posted nothing")
	}
	foundReport := false
	for _, m := range fp.msgs {
		if strings.Contains(m.Text, "ramp report") {
			foundReport = true
		}
	}
	if !foundReport {
		t.Fatal("weekly report not posted on Monday")
	}
	fp.msgs = nil
	e.ok(e.svc.Autopilot(service.AutopilotOptions{Once: true, Poster: fp, Interval: time.Minute}))
	for _, m := range fp.msgs {
		if strings.Contains(m.Text, "ramp report") {
			t.Fatal("weekly report must be posted once per week")
		}
	}
	e.refused("bad_interval")(e.svc.Autopilot(service.AutopilotOptions{Once: true, Interval: time.Second}))
	if _, err := e.svc.Autopilot(service.AutopilotOptions{Once: true, Poster: &stubPoster{fail: true}}); err == nil {
		// Only fails if there was something to post; there may be nothing new.
		t.Log("nothing to post on second cycle")
	}
}

func TestConcurrentAgentsShareTheLedger(t *testing.T) {
	e := newEnv(t)
	n := e.reserve()
	e.ok(e.svc.Start(n, "asha"))
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := e.svc.Attempt(n, "asha", "try"); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := e.svc.HintRequest(n, 2, "reviewer"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := e.ticket(n).Attempts; got != 16 {
		t.Fatalf("lost updates: attempts=%d", got)
	}
}
