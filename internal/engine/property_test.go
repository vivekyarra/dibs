package engine

import (
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

// Property tests: each runs a few thousand initialised random cases so failures
// are reproducible (the initial is fixed; bump propIters locally to dig deeper).

const propIters = 4000

var propSkills = []string{"alpha", "beta", "gamma", "delta", "eps"}
var propPaths = []string{"app/api/x.go", "migrations/001.sql", ".\\migrations\\002.sql", "app/auth/login.go",
	"./app/auth/z.go", "app/orders/s.go", "tests/t.go", "app/payments/ledger.py", "app/payments/ledger.pyc"}

func propRand(t *testing.T) *rand.Rand {
	t.Helper()
	return rand.New(rand.NewSource(20261106))
}

func propLevels(r *rand.Rand) map[string]SkillState {
	m := map[string]SkillState{}
	for _, s := range propSkills {
		if r.Intn(5) == 0 {
			continue // missing → Unseen
		}
		m[s] = SkillState{Level: Level(r.Intn(5)), CleanSinceLevel: r.Intn(CleanThreshold)}
	}
	return m
}

func propReqs(r *rand.Rand) []SkillReq {
	n := r.Intn(len(propSkills) + 1)
	perm := r.Perm(len(propSkills))
	out := make([]SkillReq, 0, n)
	for i := 0; i < n; i++ {
		w := Secondary
		if r.Intn(2) == 0 {
			w = Primary
		}
		out = append(out, SkillReq{propSkills[perm[i]], w})
	}
	return out
}

func propCandidate(r *rand.Rand, num int) Candidate {
	var paths []string
	for i := r.Intn(3); i > 0; i-- {
		paths = append(paths, propPaths[r.Intn(len(propPaths))])
	}
	est := []float64{0, -1, 0.1, 0.5, 1, 2, 4, 8, 16}[r.Intn(9)]
	due := t0.Add(time.Duration(r.Intn(24*40)-48) * time.Hour)
	if r.Intn(20) == 0 {
		due = time.Time{}
	}
	return Candidate{Number: num, Skills: propReqs(r), TouchedPaths: paths, EstSeniorHours: est, DueAt: due}
}

func propPolicy(r *rand.Rand) Policy {
	p := DefaultPolicy()
	p.RiskPaths = []string{"migrations/", "app/auth/", "app/payments/ledger.py", ""}
	p.ScoreThreshold = float64(r.Intn(9))
	p.MaxActivePerJunior = 1 + r.Intn(2)
	p.MaxBacklogShare = []float64{0.1, 0.2, 0.5, 1}[r.Intn(4)]
	p.SlackSafety = 1 + float64(r.Intn(3))*0.5
	return p
}

func propBudget(r *rand.Rand) Budget {
	return Budget{ActiveForJunior: r.Intn(3), ReservedOpen: r.Intn(5), OpenTotal: r.Intn(20)}
}

// oracleReason is an independent re-statement of DIBS_SPEC §8.2 step 1 (in the
// documented order); it returns "" when no hard filter applies.
func oracleReason(c Candidate, levels map[string]SkillState, b Budget, p Policy, now time.Time) Reason {
	if len(c.Skills) == 0 {
		return ReasonNoSkills
	}
	up, ua, ind := 0, 0, 0
	for _, s := range c.Skills {
		l := levels[s.Name].Level
		if l == Unseen {
			ua++
			if s.Weight == Primary {
				up++
			}
		}
		if l >= Independent {
			ind++
		}
	}
	if up > 1 || ua > 2 {
		return ReasonTooFar
	}
	if ind == len(c.Skills) {
		return ReasonTooEasy
	}
	for _, path := range c.TouchedPaths {
		np := strings.TrimPrefix(strings.ReplaceAll(path, "\\", "/"), "./")
		for _, rp := range p.RiskPaths {
			if rp != "" && strings.HasPrefix(np, rp) {
				return ReasonRisky
			}
		}
	}
	est := round2(EstJuniorHours(c.EstSeniorHours, c.Skills, levels))
	if c.DueAt.Sub(now).Hours() < est*p.SlackSafety {
		return ReasonNoSlack
	}
	if b.ActiveForJunior >= p.MaxActivePerJunior {
		return ReasonNoBudget
	}
	if b.OpenTotal > 0 && float64(b.ReservedOpen+1)/float64(b.OpenTotal) > p.MaxBacklogShare+1e-9 {
		return ReasonNoBudget
	}
	return ""
}

func TestPropScoreNeverPanicsAndIsDeterministic(t *testing.T) {
	r := propRand(t)
	for i := 0; i < propIters; i++ {
		c := propCandidate(r, i+1)
		lv := propLevels(r)
		b := propBudget(r)
		p := propPolicy(r)
		d1 := Score(c, lv, b, p, t0)
		d2 := Score(c, lv, b, p, t0)
		if !reflect.DeepEqual(d1, d2) {
			t.Fatalf("case %d: Score not deterministic:\n%+v\n%+v", i, d1, d2)
		}
		if d1.Number != c.Number {
			t.Fatalf("case %d: number %d != %d", i, d1.Number, c.Number)
		}
		// Score must not mutate the caller's skill map.
		before := len(lv)
		Score(c, lv, b, p, t0)
		if len(lv) != before {
			t.Fatalf("case %d: Score mutated levels map", i)
		}
	}
	// Degenerate inputs must not panic.
	Score(Candidate{}, nil, Budget{}, Policy{}, time.Time{})
	Score(Candidate{Skills: []SkillReq{{"", ""}}}, nil, Budget{OpenTotal: -1, ReservedOpen: -1}, Policy{}, t0)
}

func TestPropFilterPrecedenceMatchesSpec(t *testing.T) {
	r := propRand(t)
	counts := map[Reason]int{}
	for i := 0; i < propIters*2; i++ {
		c := propCandidate(r, i+1)
		lv := propLevels(r)
		b := propBudget(r)
		p := propPolicy(r)
		d := Score(c, lv, b, p, t0)
		want := oracleReason(c, lv, b, p, t0)
		counts[d.Reason]++
		if want != "" {
			if d.Reason != want {
				t.Fatalf("case %d: reason %s, oracle %s\ncand %+v\nlevels %+v budget %+v", i, d.Reason, want, c, lv, b)
			}
			continue
		}
		// No hard filter hit: proposed iff score ≥ threshold.
		switch d.Reason {
		case ReasonProposed:
			if !(d.Breakdown.Score >= p.ScoreThreshold) {
				t.Fatalf("case %d: proposed with score %v < threshold %v", i, d.Breakdown.Score, p.ScoreThreshold)
			}
		case ReasonLowScore:
			if d.Breakdown.Score >= p.ScoreThreshold {
				t.Fatalf("case %d: below_threshold with score %v ≥ %v", i, d.Breakdown.Score, p.ScoreThreshold)
			}
		default:
			t.Fatalf("case %d: no filter applies but reason %s", i, d.Reason)
		}
		// Score arithmetic matches its own breakdown.
		bd := d.Breakdown
		if got := round2(bd.Growth + bd.SlackBonus - bd.UnseenPenalty); got != bd.Score {
			t.Fatalf("case %d: breakdown %+v does not add up (%v)", i, bd, got)
		}
		if bd.SlackBonus < 0 || bd.SlackBonus > 1 {
			t.Fatalf("case %d: slack bonus %v outside [0,1]", i, bd.SlackBonus)
		}
	}
	// Make sure the generator actually exercised every reason Score can return.
	for _, want := range []Reason{ReasonNoSkills, ReasonTooFar, ReasonTooEasy, ReasonRisky, ReasonNoSlack, ReasonNoBudget, ReasonLowScore, ReasonProposed} {
		if counts[want] == 0 {
			t.Errorf("generator never produced %s (counts %v)", want, counts)
		}
	}
}

func TestPropEstJuniorHoursMonotonic(t *testing.T) {
	r := propRand(t)
	for i := 0; i < propIters; i++ {
		reqs := propReqs(r)
		lv := propLevels(r)
		est := r.Float64() * 20
		prev := EstJuniorHours(est, reqs, lv)
		if len(reqs) > 0 && (prev < est-1e-9 || prev > est*3.25+1e-9) {
			t.Fatalf("case %d: est %v outside [%v, %v]", i, prev, est, est*3.25)
		}
		// Raise one skill at a time until everything is Fluent; hours never go up.
		for step := 0; step < 30; step++ {
			s := propSkills[r.Intn(len(propSkills))]
			st := lv[s]
			if st.Level >= Fluent {
				continue
			}
			st.Level++
			lv[s] = st
			cur := EstJuniorHours(est, reqs, lv)
			if cur > prev+1e-9 {
				t.Fatalf("case %d: raising %s increased hours %v → %v", i, s, prev, cur)
			}
			prev = cur
		}
	}
}

func TestPropPickTop(t *testing.T) {
	r := propRand(t)
	reasons := []Reason{ReasonProposed, ReasonProposed, ReasonProposed, ReasonTooEasy, ReasonLowScore, ReasonNoBudget, ReasonRisky}
	for i := 0; i < propIters; i++ {
		n := r.Intn(12)
		nums := r.Perm(50)[:n]
		ds := make([]Decision, n)
		for j := range ds {
			ds[j] = Decision{Number: nums[j] + 1, Reason: reasons[r.Intn(len(reasons))],
				Breakdown: Breakdown{Score: float64(r.Intn(6)) / 2}} // coarse scores → lots of ties
		}
		orig := append([]Decision(nil), ds...)
		out := PickTop(ds)
		if len(ds) > 0 && !reflect.DeepEqual(ds, orig) {
			t.Fatalf("case %d: PickTop mutated its input", i)
		}
		if len(out) != n {
			t.Fatalf("case %d: len %d != %d", i, len(out), n)
		}
		proposed := 0
		var best *Decision
		for j := range out {
			if j > 0 && out[j-1].Number >= out[j].Number {
				t.Fatalf("case %d: output not sorted by number", i)
			}
			if out[j].Reason == ReasonProposed {
				proposed++
				best = &out[j]
			}
		}
		hadProposal := false
		for _, d := range orig {
			hadProposal = hadProposal || d.Reason == ReasonProposed
		}
		if proposed > 1 || (hadProposal && proposed != 1) {
			t.Fatalf("case %d: %d proposed (had any: %v)", i, proposed, hadProposal)
		}
		if best != nil {
			for _, d := range orig {
				if d.Reason != ReasonProposed {
					continue
				}
				if d.Breakdown.Score > best.Breakdown.Score || (d.Breakdown.Score == best.Breakdown.Score && d.Number < best.Number) {
					t.Fatalf("case %d: #%d (%v) beats picked #%d (%v)", i, d.Number, d.Breakdown.Score, best.Number, best.Breakdown.Score)
				}
			}
		}
		// Non-proposed reasons pass through untouched; demoted ones become not_top.
		byNum := map[int]Reason{}
		for _, d := range orig {
			byNum[d.Number] = d.Reason
		}
		for _, d := range out {
			was := byNum[d.Number]
			if was != ReasonProposed && d.Reason != was {
				t.Fatalf("case %d: #%d %s changed to %s", i, d.Number, was, d.Reason)
			}
			if was == ReasonProposed && d.Reason != ReasonProposed && d.Reason != ReasonNotTop {
				t.Fatalf("case %d: demoted to %s", i, d.Reason)
			}
		}
		// Order independence.
		for k := 0; k < 3; k++ {
			sh := append([]Decision(nil), orig...)
			r.Shuffle(len(sh), func(a, b int) { sh[a], sh[b] = sh[b], sh[a] })
			if got := PickTop(sh); !reflect.DeepEqual(got, out) {
				t.Fatalf("case %d: PickTop depends on input order:\n%+v\n%+v", i, got, out)
			}
		}
	}
}

func propGate(r *rand.Rand) (HintGate, time.Time) {
	g := HintGate{Attempts: r.Intn(6), SeniorUnlocked: r.Intn(7) - 1}
	if r.Intn(4) != 0 {
		g.StartedAt = t0
	}
	return g, t0.Add(time.Duration(r.Intn(180)) * time.Minute)
}

func TestPropAllowedLevelMonotonic(t *testing.T) {
	r := propRand(t)
	for i := 0; i < propIters; i++ {
		g, now := propGate(r)
		a := AllowedLevel(g, now)
		if a < HintNudge || a > MaxHint {
			t.Fatalf("case %d: allowed %d out of range for %+v", i, a, g)
		}
		if g.SeniorUnlocked >= HintNudge && a < min(g.SeniorUnlocked, MaxHint) {
			t.Fatalf("case %d: senior unlock %d ignored (allowed %d)", i, g.SeniorUnlocked, a)
		}
		g2 := g
		g2.Attempts += r.Intn(3)
		now2 := now.Add(time.Duration(r.Intn(60)) * time.Minute)
		if b := AllowedLevel(g2, now2); b < a {
			t.Fatalf("case %d: more attempts/time lowered allowed %d → %d (%+v → %+v)", i, a, b, g, g2)
		}
		// Starting the clock can only help.
		g3 := g
		g3.StartedAt = t0
		if AllowedLevel(g3, now) < AllowedLevel(HintGate{Attempts: g.Attempts, SeniorUnlocked: g.SeniorUnlocked}, now) {
			t.Fatalf("case %d: started clock lowered allowed level", i)
		}
		if s := NextUnlock(g, now); s == "" {
			t.Fatalf("case %d: empty NextUnlock", i)
		}
	}
}

func TestPropGrantHintNeverExceedsAllowed(t *testing.T) {
	r := propRand(t)
	for i := 0; i < propIters; i++ {
		g, now := propGate(r)
		req := r.Intn(9) - 2
		got, clamped := GrantHint(req, g, now)
		allowed := AllowedLevel(g, now)
		if got > allowed || got < HintNudge || got > MaxHint {
			t.Fatalf("case %d: granted %d (allowed %d, req %d)", i, got, allowed, req)
		}
		want := min(max(req, HintNudge), MaxHint)
		if clamped != (want > allowed) {
			t.Fatalf("case %d: clamped=%v req=%d allowed=%d", i, clamped, req, allowed)
		}
		if !clamped && got != want {
			t.Fatalf("case %d: unclamped grant %d != requested %d", i, got, want)
		}
	}
}

func cloneLevels(m map[string]SkillState) map[string]SkillState {
	out := make(map[string]SkillState, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func isPrimary(reqs []SkillReq, s string) bool {
	for _, r := range reqs {
		if r.Name == s && r.Weight == Primary {
			return true
		}
	}
	return false
}

func TestPropOnMerge(t *testing.T) {
	r := propRand(t)
	for i := 0; i < propIters; i++ {
		lv := propLevels(r)
		reqs := propReqs(r)
		before := cloneLevels(lv)
		ch := OnMerge(lv, reqs, r.Intn(5), r.Intn(4))
		for _, s := range propSkills {
			b, a := before[s], lv[s]
			if a.Level < b.Level {
				t.Fatalf("case %d: %s went down %v → %v", i, s, b.Level, a.Level)
			}
			if a.Level > Fluent {
				t.Fatalf("case %d: %s above Fluent: %v", i, s, a.Level)
			}
			if a.CleanSinceLevel < 0 || a.CleanSinceLevel >= CleanThreshold {
				t.Fatalf("case %d: %s clean count %d", i, s, a.CleanSinceLevel)
			}
			if !isPrimary(reqs, s) && a != b {
				t.Fatalf("case %d: non-primary %s changed %+v → %+v", i, s, b, a)
			}
			if isPrimary(reqs, s) && a.Level < Guided {
				t.Fatalf("case %d: merged primary %s below Guided: %v", i, s, a.Level)
			}
		}
		for _, c := range ch {
			if c.To <= c.From || c.To != lv[c.Skill].Level || c.From != before[c.Skill].Level {
				t.Fatalf("case %d: bad change %+v", i, c)
			}
		}
	}
}

func TestPropOnRegression(t *testing.T) {
	r := propRand(t)
	for i := 0; i < propIters; i++ {
		lv := propLevels(r)
		reqs := propReqs(r)
		before := cloneLevels(lv)
		ch := OnRegression(lv, reqs)
		for _, s := range propSkills {
			b, a := before[s].Level, lv[s].Level
			switch {
			case !isPrimary(reqs, s):
				if lv[s] != before[s] {
					t.Fatalf("case %d: non-primary %s changed", i, s)
				}
			case b >= Exposed && a < Exposed:
				t.Fatalf("case %d: %s fell below Exposed: %v → %v", i, s, b, a)
			case b > Exposed && a != b-1:
				t.Fatalf("case %d: %s should drop by exactly one: %v → %v", i, s, b, a)
			case b <= Exposed && a != b:
				t.Fatalf("case %d: %s at %v must not move (got %v)", i, s, b, a)
			}
		}
		for _, c := range ch {
			if c.To != c.From-1 || c.To < Exposed {
				t.Fatalf("case %d: bad change %+v", i, c)
			}
		}
	}
}

func TestPropOnStartAndRelease(t *testing.T) {
	r := propRand(t)
	for i := 0; i < propIters; i++ {
		lv := propLevels(r)
		reqs := propReqs(r)
		before := cloneLevels(lv)
		f := OnStart
		if i%2 == 1 {
			f = OnRelease
		}
		f(lv, reqs)
		for _, s := range propSkills {
			b, a := before[s].Level, lv[s].Level
			if isPrimary(reqs, s) && b == Unseen {
				if a != Exposed {
					t.Fatalf("case %d: %s Unseen primary → %v", i, s, a)
				}
			} else if lv[s] != before[s] {
				t.Fatalf("case %d: %s changed %+v → %+v", i, s, before[s], lv[s])
			}
		}
	}
}

func TestPropCheckReleaseOnlyReserved(t *testing.T) {
	r := propRand(t)
	p := DefaultPolicy()
	for i := 0; i < propIters; i++ {
		st := AllStates[r.Intn(len(AllStates))]
		if r.Intn(10) == 0 {
			st = State("BOGUS")
		}
		f := RepFacts{State: st, DueAt: t0.Add(time.Duration(r.Intn(200)-50) * time.Hour), EstJuniorHours: float64(r.Intn(10))}
		if r.Intn(2) == 0 {
			f.StartedAt = t0.Add(-time.Duration(r.Intn(48)) * time.Hour)
		}
		if r.Intn(3) == 0 {
			f.StuckAskedAt = t0.Add(-time.Duration(r.Intn(120)) * time.Minute)
		}
		if r.Intn(4) == 0 {
			f.KeepGoingUntil = t0.Add(time.Duration(r.Intn(120)-60) * time.Minute)
		}
		rc := CheckRelease(f, p, t0)
		if !st.Reserved() && rc != (ReleaseCheck{}) {
			t.Fatalf("case %d: non-reserved %s got %+v", i, st, rc)
		}
		if rc.Release && rc.AskFirst {
			t.Fatalf("case %d: release and ask at once %+v", i, rc)
		}
		if (rc.Release || rc.AskFirst) && rc.Reason == "" {
			t.Fatalf("case %d: decision without reason %+v", i, rc)
		}
		if rc.AskFirst && st != InProgress {
			t.Fatalf("case %d: asked in state %s", i, st)
		}
		if st == SeniorSignoff && rc.Release {
			t.Fatalf("case %d: released during senior signoff: %+v", i, rc)
		}
		if rc != CheckRelease(f, p, t0) {
			t.Fatalf("case %d: not deterministic", i)
		}
	}
}

func TestPropWeeklySums(t *testing.T) {
	r := propRand(t)
	juniors := []string{"asha", "ben", "cy"}
	for i := 0; i < propIters/4; i++ {
		var reps []Rep
		for j := r.Intn(25); j > 0; j-- {
			fin := t0.Add(time.Duration(r.Intn(24*60)-24*30) * time.Hour)
			rep := Rep{Number: j, Junior: juniors[r.Intn(3)], MaxHint: r.Intn(5), Approvals: r.Intn(3),
				SeniorComments: r.Intn(5), TeachingMoments: r.Intn(3), Released: r.Intn(4) == 0,
				FinishedAt: fin, StartedAt: fin.Add(-time.Duration(r.Intn(50)) * time.Hour), Sample: r.Intn(2) == 0,
				SkillChanges: make([]SkillChange, r.Intn(3))}
			reps = append(reps, rep)
		}
		weeks := Weekly(reps, 45)
		var n, merged, rel, changes int
		var mins float64
		seen := map[[2]string]bool{}
		for k, w := range weeks {
			key := [2]string{w.Junior, w.Label}
			if seen[key] {
				t.Fatalf("case %d: duplicate week %v", i, key)
			}
			seen[key] = true
			if k > 0 {
				pk := weeks[k-1]
				if pk.Junior > w.Junior || (pk.Junior == w.Junior && pk.Label >= w.Label) {
					t.Fatalf("case %d: weeks out of order", i)
				}
			}
			for _, rp := range w.Reps {
				if rp.Junior != w.Junior || ISOWeek(rp.FinishedAt) != w.Label {
					t.Fatalf("case %d: rep in wrong bucket", i)
				}
			}
			if w.Merged+w.Releases != len(w.Reps) {
				t.Fatalf("case %d: merged+releases %d+%d != %d", i, w.Merged, w.Releases, len(w.Reps))
			}
			if w.IndependenceRatio < 0 || w.IndependenceRatio > 1 || w.SavedMinutes < 0 {
				t.Fatalf("case %d: bad week %+v", i, w)
			}
			n += len(w.Reps)
			merged += w.Merged
			rel += w.Releases
			mins += w.SeniorMinutes
			changes += len(w.SkillChanges)
		}
		var wantMerged, wantRel, wantChanges int
		var wantMins float64
		for _, rp := range reps {
			if rp.Released {
				wantRel++
			} else {
				wantMerged++
			}
			wantMins += rp.SeniorMinutes()
			wantChanges += len(rp.SkillChanges)
		}
		if n != len(reps) || merged != wantMerged || rel != wantRel || changes != wantChanges || math.Abs(mins-wantMins) > 1e-6 {
			t.Fatalf("case %d: sums differ: reps %d/%d merged %d/%d rel %d/%d mins %v/%v changes %d/%d",
				i, n, len(reps), merged, wantMerged, rel, wantRel, mins, wantMins, changes, wantChanges)
		}
	}
}

// testing/quick variant: arbitrary ints for attempts/minutes never escape [1,4].
func TestQuickAllowedLevelRange(t *testing.T) {
	f := func(attempts int16, minutes int32, senior int8) bool {
		g := HintGate{Attempts: int(attempts), StartedAt: t0, SeniorUnlocked: int(senior)}
		a := AllowedLevel(g, t0.Add(time.Duration(minutes)*time.Minute))
		return a >= HintNudge && a <= MaxHint
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 5000, Rand: rand.New(rand.NewSource(1))}); err != nil {
		t.Fatal(err)
	}
}

func TestQuickIsClean(t *testing.T) {
	f := func(h, rr int8) bool { return IsClean(int(h), int(rr)) == (h <= 1 && rr <= 1) }
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

// BUG: a non-finite senior estimate (NaN) slips past the no_slack filter
// (NaN comparisons are false) and is PROPOSED with a NaN score, violating
// "proposed iff score ≥ threshold". The same NaN/Inf later makes
// json.Marshal(Breakdown) fail silently in ledger.SaveTicket.
func TestScoreNonFiniteEstimateIsNotProposed(t *testing.T) {
	c := Candidate{Number: 1, Skills: []SkillReq{{"alpha", Primary}}, EstSeniorHours: math.NaN(), DueAt: t0.Add(200 * time.Hour)}
	lv := map[string]SkillState{"alpha": {Level: Exposed}}
	d := Score(c, lv, Budget{OpenTotal: 10}, DefaultPolicy(), t0)
	if d.Reason == ReasonProposed {
		t.Fatalf("NaN estimate proposed with score %v (breakdown %+v)", d.Breakdown.Score, d.Breakdown)
	}
	for _, f := range []float64{d.Breakdown.Score, d.Breakdown.EstJuniorHours, d.Breakdown.SlackDays, d.Breakdown.SlackBonus} {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			t.Fatalf("non-finite number in breakdown: %+v", d.Breakdown)
		}
	}
	c.EstSeniorHours = math.Inf(1)
	d = Score(c, lv, Budget{OpenTotal: 10}, DefaultPolicy(), t0)
	if d.Reason == ReasonProposed {
		t.Fatalf("+Inf estimate proposed")
	}
	for _, f := range []float64{d.Breakdown.EstJuniorHours, d.Breakdown.SlackDays} {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			t.Fatalf("non-finite number in breakdown with +Inf estimate: %+v", d.Breakdown)
		}
	}
}
