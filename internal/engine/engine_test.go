package engine

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

func lv(m map[string]Level) map[string]SkillState {
	out := map[string]SkillState{}
	for k, v := range m {
		out[k] = SkillState{Level: v}
	}
	return out
}

func TestTransitions(t *testing.T) {
	for from, tos := range transitions {
		for _, to := range tos {
			if !CanTransition(from, to) {
				t.Fatalf("%s→%s should be allowed", from, to)
			}
		}
	}
	forbidden := [][2]State{{New, Reserved}, {Recorded, New}, {Closed, Triaged}, {Merged, Released},
		{ToBuilder, Reserved}, {Proposed, InProgress}, {Released, Reserved}}
	for _, f := range forbidden {
		if CanTransition(f[0], f[1]) {
			t.Fatalf("%s→%s must be forbidden", f[0], f[1])
		}
	}
	for _, s := range AllStates {
		if !s.Valid() {
			t.Fatalf("%s not valid", s)
		}
	}
	if State("BOGUS").Valid() {
		t.Fatal("bogus state valid")
	}
}

func TestSkillLadder(t *testing.T) {
	levels := lv(map[string]Level{"idem": Unseen})
	reqs := []SkillReq{{"idem", Primary}, {"http", Secondary}}
	if ch := OnStart(levels, reqs); len(ch) != 1 || ch[0].To != Exposed {
		t.Fatalf("start: %+v", ch)
	}
	if ch := OnStart(levels, reqs); len(ch) != 0 {
		t.Fatal("start twice must not move again")
	}
	// Any merge from Exposed → Guided, even with deep hints.
	if ch := OnMerge(levels, reqs, 4, 3); len(ch) != 1 || ch[0].To != Guided {
		t.Fatalf("merge: %+v", ch)
	}
	// A non-clean merge at Guided changes nothing.
	if ch := OnMerge(levels, reqs, 2, 0); len(ch) != 0 || levels["idem"].CleanSinceLevel != 0 {
		t.Fatal("non-clean merge must not count")
	}
	OnMerge(levels, reqs, 1, 1)
	if levels["idem"].Level != Guided || levels["idem"].CleanSinceLevel != 1 {
		t.Fatalf("one clean: %+v", levels["idem"])
	}
	if ch := OnMerge(levels, reqs, 0, 0); len(ch) != 1 || ch[0].To != Independent || levels["idem"].CleanSinceLevel != 0 {
		t.Fatalf("two clean: %+v %+v", ch, levels["idem"])
	}
	OnMerge(levels, reqs, 0, 0)
	OnMerge(levels, reqs, 1, 0)
	if levels["idem"].Level != Fluent {
		t.Fatalf("fluent: %+v", levels["idem"])
	}
	OnMerge(levels, reqs, 0, 0)
	if levels["idem"].Level != Fluent {
		t.Fatal("ceiling")
	}
	if _, ok := levels["http"]; ok {
		t.Fatal("secondary skills never move")
	}
	if ch := OnRegression(levels, reqs); len(ch) != 1 || ch[0].To != Independent {
		t.Fatalf("regression %+v", ch)
	}
	low := lv(map[string]Level{"idem": Exposed})
	if ch := OnRegression(low, reqs); len(ch) != 0 || low["idem"].Level != Exposed {
		t.Fatal("floor is Exposed")
	}
	if Level(9).String() != "Level(9)" || Guided.String() != "Guided" {
		t.Fatal("names")
	}
}

func TestEstJuniorHours(t *testing.T) {
	r := []SkillReq{{"a", Primary}}
	if got := EstJuniorHours(1, r, lv(map[string]Level{"a": Independent})); got != 1 {
		t.Fatal(got)
	}
	if got := EstJuniorHours(1, r, lv(map[string]Level{"a": Unseen})); got != 3.25 {
		t.Fatal(got)
	}
	if got := EstJuniorHours(2, r, lv(map[string]Level{"a": Fluent})); got != 2 {
		t.Fatal(got)
	}
	if EstJuniorHours(-1, r, nil) != 0 || EstJuniorHours(2, nil, nil) != 2 {
		t.Fatal("edge")
	}
}

// demoMap is the starting skill map of the demo junior (fixtures/demo).
var demoMap = map[string]Level{"http-handlers": 3, "sql-queries": 3, "testing": 3, "idempotency": 1,
	"error-handling": 1, "migrations": 0, "concurrency": 0, "observability": 0}

func demoCandidates() []Candidate {
	due := t0.Add(7 * 24 * time.Hour)
	p := func(n string) SkillReq { return SkillReq{n, Primary} }
	s := func(n string) SkillReq { return SkillReq{n, Secondary} }
	return []Candidate{
		{1, []SkillReq{p("http-handlers")}, []string{"app/api/errors.py"}, 0.1, due},
		{2, []SkillReq{p("http-handlers"), s("sql-queries")}, []string{"app/api/orders.py"}, 1, due},
		{3, []SkillReq{p("idempotency"), s("error-handling"), s("http-handlers")}, []string{"app/orders/service.py"}, 0.5, due},
		{4, []SkillReq{p("migrations")}, []string{"migrations/0002_partition.sql"}, 3, due},
		{5, []SkillReq{p("concurrency"), p("observability")}, []string{"app/api/ratelimit.py"}, 2, due},
		{6, []SkillReq{p("error-handling"), s("concurrency")}, []string{"app/webhooks/sender.py"}, 1, due},
		{7, []SkillReq{p("testing")}, []string{"tests/test_orders.py"}, 0.5, due},
		{8, []SkillReq{p("sql-queries")}, []string{"app/orders/repo.py"}, 0.5, due},
		{9, []SkillReq{p("observability")}, []string{"app/logging.py"}, 1, due},
		{10, []SkillReq{p("testing"), s("error-handling")}, []string{"app/reports/daily.py"}, 1, due},
	}
}

func demoPolicy() Policy {
	p := DefaultPolicy()
	p.RiskPaths = []string{"migrations/", "app/auth/", "app/payments/ledger.py"}
	return p
}

// The invariant the demo depends on: ticket #3 is the rep.
func TestDemoInvariant(t *testing.T) {
	var ds []Decision
	for _, c := range demoCandidates() {
		ds = append(ds, Score(c, lv(demoMap), Budget{0, 0, 10}, demoPolicy(), t0))
	}
	ds = PickTop(ds)
	want := map[int]Reason{1: ReasonTooEasy, 2: ReasonTooEasy, 3: ReasonProposed, 4: ReasonRisky,
		5: ReasonTooFar, 6: ReasonNotTop, 7: ReasonTooEasy, 8: ReasonTooEasy, 9: ReasonLowScore, 10: ReasonLowScore}
	for _, d := range ds {
		if want[d.Number] != d.Reason {
			t.Errorf("#%d: got %s want %s (%+v)", d.Number, d.Reason, want[d.Number], d.Breakdown)
		}
	}
	// After #3 merges, idempotency is Guided and #6 becomes the next rep.
	m := lv(demoMap)
	m["idempotency"] = SkillState{Level: Guided}
	d6 := Score(demoCandidates()[5], m, Budget{0, 0, 9}, demoPolicy(), t0)
	if d6.Reason != ReasonProposed {
		t.Fatalf("#6 next sweep: %s %+v", d6.Reason, d6.Breakdown)
	}
}

func TestScoreFilters(t *testing.T) {
	p := demoPolicy()
	c := demoCandidates()[2]
	if d := Score(c, lv(demoMap), Budget{1, 1, 10}, p, t0); d.Reason != ReasonNoBudget {
		t.Fatal("active cap", d.Reason)
	}
	if d := Score(c, lv(demoMap), Budget{0, 2, 10}, p, t0); d.Reason != ReasonNoBudget {
		t.Fatal("share cap", d.Reason)
	}
	if d := Score(c, lv(demoMap), Budget{0, 1, 10}, p, t0); d.Reason != ReasonProposed {
		t.Fatal("share exactly at cap allowed", d.Reason, d.Breakdown.BacklogShareNew)
	}
	c.DueAt = t0.Add(1 * time.Hour) // junior est 1h × 1.5 safety > 1h left
	if d := Score(c, lv(demoMap), Budget{0, 0, 10}, p, t0); d.Reason != ReasonNoSlack {
		t.Fatal("slack", d.Reason)
	}
	c.Skills = nil
	if d := Score(c, lv(demoMap), Budget{}, p, t0); d.Reason != ReasonNoSkills {
		t.Fatal("untagged")
	}
	c = demoCandidates()[0]
	c.TouchedPaths = []string{".\\migrations\\x.sql"}
	if d := Score(Candidate{Number: 1, Skills: []SkillReq{{"idempotency", Primary}}, TouchedPaths: c.TouchedPaths, EstSeniorHours: 1, DueAt: t0.Add(200 * time.Hour)}, lv(demoMap), Budget{}, p, t0); d.Reason != ReasonRisky {
		t.Fatal("windows path normalised", d.Reason)
	}
	// Too far by count of all unseen (3 secondaries).
	tf := Candidate{Number: 1, Skills: []SkillReq{{"idempotency", Primary}, {"migrations", Secondary}, {"concurrency", Secondary}, {"observability", Secondary}}, EstSeniorHours: 1, DueAt: t0.Add(300 * time.Hour)}
	if d := Score(tf, lv(demoMap), Budget{}, p, t0); d.Reason != ReasonTooFar {
		t.Fatal("too far all", d.Reason)
	}
	for _, r := range []Reason{ReasonTooFar, ReasonTooEasy, ReasonRisky, ReasonNoSlack, ReasonNoBudget, ReasonLowScore, ReasonNotTop, ReasonNoSkills, "x"} {
		if r.Explain() == "" {
			t.Fatal("explain")
		}
	}
	if !ReasonNoBudget.Transient() || ReasonRisky.Transient() {
		t.Fatal("transient")
	}
}

func TestPickTopTieBreak(t *testing.T) {
	ds := PickTop([]Decision{{Number: 9, Reason: ReasonProposed, Breakdown: Breakdown{Score: 4}},
		{Number: 4, Reason: ReasonProposed, Breakdown: Breakdown{Score: 4}},
		{Number: 2, Reason: ReasonTooEasy}})
	if ds[0].Number != 2 || ds[1].Number != 4 || ds[1].Reason != ReasonProposed || ds[2].Reason != ReasonNotTop {
		t.Fatalf("%+v", ds)
	}
}

func TestHintGate(t *testing.T) {
	g := HintGate{StartedAt: t0}
	cases := []struct {
		attempts int
		mins     int
		want     int
	}{{0, 0, 1}, {1, 0, 2}, {0, 20, 2}, {2, 0, 3}, {0, 45, 3}, {3, 0, 4}, {0, 90, 4}, {1, 44, 2}}
	for _, c := range cases {
		g.Attempts = c.attempts
		if got := AllowedLevel(g, t0.Add(time.Duration(c.mins)*time.Minute)); got != c.want {
			t.Errorf("attempts=%d mins=%d got L%d want L%d", c.attempts, c.mins, got, c.want)
		}
	}
	g = HintGate{} // reserved but not started: time never unlocks
	if AllowedLevel(g, t0.Add(10*time.Hour)) != 1 {
		t.Fatal("not started")
	}
	g.SeniorUnlocked = 4
	if AllowedLevel(g, t0) != 4 {
		t.Fatal("senior unlock")
	}
	if lvl, clamped := GrantHint(3, HintGate{StartedAt: t0}, t0); lvl != 1 || !clamped {
		t.Fatal("clamp")
	}
	if lvl, clamped := GrantHint(0, HintGate{StartedAt: t0}, t0); lvl != 1 || clamped {
		t.Fatal("floor")
	}
	if lvl, _ := GrantHint(9, HintGate{Attempts: 5, StartedAt: t0}, t0); lvl != 4 {
		t.Fatal("ceiling")
	}
	if s := NextUnlock(HintGate{StartedAt: t0}, t0.Add(10*time.Minute)); !strings.Contains(s, "L2 after 1 more attempt(s) or in 10 min") {
		t.Fatal(s)
	}
	if s := NextUnlock(HintGate{}, t0); !strings.Contains(s, "after starting") {
		t.Fatal(s)
	}
	if s := NextUnlock(HintGate{Attempts: 3, StartedAt: t0}, t0); s != "all hint levels are open" {
		t.Fatal(s)
	}
}

func TestCheckHint(t *testing.T) {
	ok1 := "What should the server remember if the same request arrives twice?"
	if v := CheckHint(1, ok1, nil); len(v) != 0 {
		t.Fatalf("clean L1 flagged: %+v", v)
	}
	if v := CheckHint(1, "Look at service.py and think.", nil); len(v) != 2 {
		t.Fatalf("L1 file + no question: %+v", v)
	}
	code := "Try this?\n```python\nkey = request.headers['Idempotency-Key']\nif key in seen:\n    return seen[key]\n```"
	if v := CheckHint(2, code, nil); len(v) != 1 || v[0].Rule != "no_code" {
		t.Fatalf("L2 code: %+v", v)
	}
	if v := CheckHint(3, code, nil); len(v) != 0 {
		t.Fatalf("L3 short pseudocode ok: %+v", v)
	}
	long := "```\n" + strings.Repeat("step()\n", 9) + "```"
	if v := CheckHint(3, long, nil); len(v) != 1 {
		t.Fatalf("L3 too long: %+v", v)
	}
	if v := CheckHint(4, long, nil); len(v) != 0 {
		t.Fatalf("L4 allows 20: %+v", v)
	}
	ids := map[string]bool{"create_order": true, "OrderRepo": true, "idem_store": true, "commit_order": true, "self": true}
	leak := "1. call create_order\n2. look up idem_store\n3. then OrderRepo\n4. finally commit_order"
	if v := CheckHint(3, leak, ids); len(v) != 1 || v[0].Rule != "repo_identifiers" {
		t.Fatalf("identifier leak: %+v", v)
	}
	if v := CheckHint(2, "Read about the Idempotency-Key header (RFC draft). Where would you check for a previous result?", nil); len(v) != 0 {
		t.Fatalf("L2 prose flagged: %+v", v)
	}
}

func TestRelease(t *testing.T) {
	p := DefaultPolicy()
	f := RepFacts{State: InProgress, DueAt: t0.Add(100 * time.Hour), EstJuniorHours: 3, StartedAt: t0}
	if r := CheckRelease(f, p, t0.Add(time.Hour)); r.Release || r.AskFirst {
		t.Fatal("healthy", r)
	}
	if r := CheckRelease(f, p, t0.Add(7*time.Hour)); !r.AskFirst {
		t.Fatal("stuck → ask", r)
	}
	f.StuckAskedAt = t0.Add(7 * time.Hour)
	if r := CheckRelease(f, p, t0.Add(7*time.Hour+10*time.Minute)); r.Release || r.AskFirst {
		t.Fatal("waiting for answer", r)
	}
	if r := CheckRelease(f, p, t0.Add(7*time.Hour+31*time.Minute)); !r.Release {
		t.Fatal("no answer → release", r)
	}
	f.KeepGoingUntil = t0.Add(8 * time.Hour)
	if r := CheckRelease(f, p, t0.Add(7*time.Hour+40*time.Minute)); r.Release || r.AskFirst {
		t.Fatal("keep going window", r)
	}
	if r := CheckRelease(f, p, t0.Add(8*time.Hour+1*time.Minute)); !r.AskFirst {
		t.Fatal("ask again after window", r)
	}
	f.StuckAskedAt = t0.Add(8*time.Hour + 1*time.Minute)
	if r := CheckRelease(f, p, t0.Add(8*time.Hour+40*time.Minute)); !r.Release {
		t.Fatal("second ask unanswered → release", r)
	}
	d := RepFacts{State: Reserved, DueAt: t0.Add(2 * time.Hour), EstJuniorHours: 3}
	if r := CheckRelease(d, p, t0); !r.Release || r.Reason != "deadline at risk" {
		t.Fatal("deadline", r)
	}
	d.State = SeniorSignoff
	if r := CheckRelease(d, p, t0); r.Release {
		t.Fatal("already approved by reviewer, never auto-release")
	}
	if r := CheckRelease(RepFacts{State: ToBuilder}, p, t0); r.Release {
		t.Fatal("not reserved")
	}
	if !ProposalExpired(t0, p, t0.Add(25*time.Hour)) || ProposalExpired(time.Time{}, p, t0) {
		t.Fatal("proposal timeout")
	}
	if !HoldExpired(t0, p, t0.Add(49*time.Hour)) || HoldExpired(t0, p, t0.Add(time.Hour)) {
		t.Fatal("hold timeout")
	}
}

func TestMetrics(t *testing.T) {
	mk := func(n, hint, days int, released bool, sample bool) Rep {
		start := t0.Add(time.Duration(days) * 24 * time.Hour)
		return Rep{Number: n, Junior: "asha", MaxHint: hint, StartedAt: start, FinishedAt: start.Add(3 * time.Hour),
			Released: released, Approvals: 2, TeachingMoments: 1, SeniorComments: 1, Sample: sample}
	}
	reps := []Rep{mk(1, 3, 0, false, true), mk(2, 1, 1, false, true), mk(3, 0, 8, false, false), mk(4, 2, 9, true, false)}
	ws := Weekly(reps, 45)
	if len(ws) != 2 || ws[0].Label != "2026-W45" || ws[1].Label != "2026-W46" {
		t.Fatalf("%+v", ws)
	}
	w := ws[0]
	if w.Merged != 2 || w.AvgHint != 2 || w.IndependenceRatio != 0.5 || w.SeniorMinutes != 18 || w.SavedMinutes != 72 || !w.Sample {
		t.Fatalf("week1 %+v", w)
	}
	if ws[1].Releases != 1 || ws[1].Merged != 1 || ws[1].Sample {
		t.Fatalf("week2 %+v", ws[1])
	}
	if r, ok := FirstUnassisted(reps); !ok || r.Number != 3 {
		t.Fatal("first unassisted")
	}
	if median([]float64{4, 1, 3, 2}) != 2.5 || median(nil) != 0 {
		t.Fatal("median")
	}
	if (Rep{}).CycleHours() != 0 {
		t.Fatal("cycle")
	}
}
