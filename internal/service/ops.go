package service

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/vivekyarra/dibs/internal/config"
	"github.com/vivekyarra/dibs/internal/engine"
	"github.com/vivekyarra/dibs/internal/ledger"
)

func describeChanges(ch []engine.SkillChange) string {
	if len(ch) == 0 {
		return "no skill changes"
	}
	parts := make([]string, 0, len(ch))
	for _, c := range ch {
		parts = append(parts, fmt.Sprintf("%s %s→%s", c.Skill, c.From, c.To))
	}
	return strings.Join(parts, " · ")
}

// ---------- Suggest & triage ----------

// Suggestion is Dibs' deterministic first guess at a ticket's skills, which the
// dibs agent confirms or corrects after reading the code.
type Suggestion struct {
	Number         int               `json:"number"`
	Skills         []engine.SkillReq `json:"skills"`
	Paths          []string          `json:"paths"`
	EstSeniorHours float64           `json:"est_senior_hours"`
	Evidence       []string          `json:"evidence"`
	Command        string            `json:"command"`
}

var (
	mentionedPathRe = regexp.MustCompile("`?([A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+)`?")
)

// globMatch matches a path against a glob that may contain ** (any depth).
func globMatch(glob, path string) bool {
	glob, path = normSlash(glob), normSlash(path)
	re := regexp.QuoteMeta(glob)
	re = strings.ReplaceAll(re, `\*\*/`, `(?:.*/)?`)
	re = strings.ReplaceAll(re, `\*\*`, `.*`)
	re = strings.ReplaceAll(re, `\*`, `[^/]*`)
	re = strings.ReplaceAll(re, `\?`, `[^/]`)
	ok, _ := regexp.MatchString("^"+re+"$", path)
	return ok || strings.HasPrefix(path, strings.TrimSuffix(glob, "**")) && strings.HasSuffix(glob, "/**")
}

func normSlash(p string) string { return strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./") }

func suggest(cfg *config.Config, t ledger.Ticket) Suggestion {
	sg := Suggestion{Number: t.Number, EstSeniorHours: 1}
	text := t.Title + "\n" + t.Body
	seenPath := map[string]bool{}
	for _, m := range mentionedPathRe.FindAllStringSubmatch(text, -1) {
		p := normSlash(m[1])
		if strings.Contains(p, "://") || strings.HasPrefix(p, "http") || !strings.Contains(filepath.Base(p), ".") {
			continue
		}
		if !seenPath[p] {
			seenPath[p] = true
			sg.Paths = append(sg.Paths, p)
		}
	}
	type hit struct {
		name  string
		score int
		order int
	}
	var hits []hit
	lower := strings.ToLower(text)
	titleLower := strings.ToLower(t.Title)
	for i, sk := range cfg.Skills {
		score := 0
		for _, p := range sg.Paths {
			for _, g := range sk.Paths {
				if globMatch(g, p) {
					score += 3
					sg.Evidence = append(sg.Evidence, fmt.Sprintf("%s: path %s matches %s", sk.Name, p, g))
				}
			}
		}
		for _, kw := range sk.Keywords {
			k := strings.ToLower(kw)
			if strings.Contains(titleLower, k) {
				score += 2
				sg.Evidence = append(sg.Evidence, fmt.Sprintf("%s: title mentions %q", sk.Name, kw))
			} else if strings.Contains(lower, k) {
				score++
				sg.Evidence = append(sg.Evidence, fmt.Sprintf("%s: body mentions %q", sk.Name, kw))
			}
		}
		if score > 0 {
			hits = append(hits, hit{sk.Name, score, i})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].order < hits[j].order
	})
	for i, h := range hits {
		if i >= 3 {
			break
		}
		w := engine.Secondary
		if i == 0 || (i == 1 && h.score >= 3 && h.score*2 >= hits[0].score) {
			w = engine.Primary
		}
		sg.Skills = append(sg.Skills, engine.SkillReq{Name: h.name, Weight: w})
	}
	sg.Command = fmt.Sprintf("dibs triage %d --skills %s --est %g --paths %s", t.Number, skillsFlag(sg.Skills), sg.EstSeniorHours, strings.Join(sg.Paths, ","))
	return sg
}

func skillsFlag(reqs []engine.SkillReq) string {
	parts := make([]string, 0, len(reqs))
	for _, r := range reqs {
		w := "s"
		if r.Weight == engine.Primary {
			w = "p"
		}
		parts = append(parts, r.Name+":"+w)
	}
	return strings.Join(parts, ",")
}

// ParseSkills parses "idempotency:p,error-handling:s".
func ParseSkills(s string) ([]engine.SkillReq, error) {
	var out []engine.SkillReq
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, w, _ := strings.Cut(part, ":")
		weight := engine.Primary
		switch strings.ToLower(w) {
		case "", "p", "primary":
		case "s", "secondary":
			weight = engine.Secondary
		default:
			return nil, refuse("bad_skills", "skill %q: weight must be p or s", part)
		}
		if seen[name] {
			return nil, refuse("bad_skills", "skill %q listed twice", name)
		}
		seen[name] = true
		out = append(out, engine.SkillReq{Name: name, Weight: weight})
	}
	return out, nil
}

// Suggest returns the deterministic skill suggestion for a ticket.
func (s *Service) Suggest(n int) (*Result, error) {
	r := &Result{OK: true, Action: "suggest"}
	err := s.tx(func(tx *ledger.Tx) error {
		t, err := tx.Ticket(n)
		if err != nil {
			return err
		}
		r.Data = suggest(s.Cfg, t)
		r.Ticket = &t
		return nil
	})
	return r, err
}

// Sweep syncs, then lists the tickets that still need triage with suggestions.
// If everything is triaged it goes straight on to scoring.
func (s *Service) Sweep(actor string, accept bool) (*Result, error) {
	r, err := s.Sync(actor)
	if err != nil {
		return nil, err
	}
	r.Action = "sweep"
	var pending []Suggestion
	err = s.tx(func(tx *ledger.Tx) error {
		news, err := tx.Tickets(engine.New)
		if err != nil {
			return err
		}
		for _, t := range news {
			pending = append(pending, suggest(s.Cfg, t))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if accept {
		for _, sg := range pending {
			if len(sg.Skills) == 0 {
				continue
			}
			if _, err := s.Triage(sg.Number, sg.Skills, sg.EstSeniorHours, sg.Paths, actor); err != nil {
				return nil, err
			}
		}
		pending = nil
	}
	if len(pending) > 0 {
		for _, sg := range pending {
			r.AgentTasks = append(r.AgentTasks, fmt.Sprintf("Triage #%d: read the issue and the code it touches, then run `%s` (adjust skills/estimate/paths if the suggestion is wrong; evidence: %s).",
				sg.Number, sg.Command, strings.Join(sg.Evidence, "; ")))
		}
		r.AgentTasks = append(r.AgentTasks, "After triaging every ticket above, run `dibs score` and post its messages.")
		r.Data = map[string]any{"needs_triage": pending}
		return r, nil
	}
	sr, err := s.Score(actor)
	if err != nil {
		return nil, err
	}
	r.merge(sr)
	r.Data = sr.Data
	return r, nil
}

// Triage stores the skills, estimate and touched paths for a ticket.
func (s *Service) Triage(n int, skills []engine.SkillReq, est float64, paths []string, actor string) (*Result, error) {
	if len(skills) == 0 {
		return nil, refuse("bad_skills", "give at least one skill, e.g. --skills idempotency:p")
	}
	primary := 0
	for _, sk := range skills {
		if _, ok := s.Cfg.Skill(sk.Name); !ok {
			return nil, refuse("unknown_skill", "unknown skill %q (see `dibs skills`)", sk.Name)
		}
		if sk.Weight == engine.Primary {
			primary++
		}
	}
	if primary == 0 || primary > 2 {
		return nil, refuse("bad_skills", "mark one or two skills as primary (:p); got %d", primary)
	}
	if est <= 0 || est > 80 {
		return nil, refuse("bad_estimate", "--est must be hours for an experienced engineer, between 0 and 80")
	}
	clean := []string{}
	for _, p := range paths {
		if p = normSlash(strings.TrimSpace(p)); p != "" {
			clean = append(clean, p)
		}
	}
	r := &Result{OK: true, Action: "triage"}
	now := s.now()
	err := s.tx(func(tx *ledger.Tx) error {
		t, err := tx.Ticket(n)
		if err != nil {
			return err
		}
		if t.State != engine.New && t.State != engine.Triaged {
			return refuse("not_triageable", "#%d is %s; only NEW or TRIAGED tickets can be (re)triaged", n, t.State)
		}
		t.Skills, t.EstSeniorHours, t.TouchedPaths, t.TriagedAt, t.Reason = skills, est, clean, now, ""
		if err := tx.Transition(&t, engine.Triaged, actor, now, map[string]any{"skills": skills, "est": est, "paths": clean}); err != nil {
			return err
		}
		r.Ticket = &t
		return nil
	})
	return r, err
}

// ---------- Score & proposals ----------

func activeRep(st engine.State) bool { return st == engine.Proposed || st.Reserved() }

// Score evaluates every triaged ticket for every junior, proposes the best rep
// per junior, routes the rest, and returns the sweep card.
func (s *Service) Score(actor string) (*Result, error) {
	r := &Result{OK: true, Action: "score"}
	now := s.now()
	type row struct {
		t        ledger.Ticket
		decision engine.Decision
		junior   string
	}
	var proposals, toBuilder, held []row
	err := s.tx(func(tx *ledger.Tx) error {
		p, err := s.policy(tx)
		if err != nil {
			return err
		}
		all, err := tx.Tickets()
		if err != nil {
			return err
		}
		var cands []ledger.Ticket
		openTotal, reservedOpen := 0, 0
		active := map[string]int{}
		for _, t := range all {
			if t.State.Open() {
				openTotal++
			}
			if activeRep(t.State) {
				reservedOpen++
				active[t.ReservedFor]++
			}
			if t.State == engine.Triaged {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return nil
		}
		// For each candidate, the best (most favourable) reason across juniors.
		best := map[int]engine.Decision{}
		bestJunior := map[int]string{}
		taken := map[int]bool{}
		for _, j := range s.Cfg.Team.Juniors {
			levels, err := tx.Levels(j.Handle)
			if err != nil {
				return err
			}
			var ds []engine.Decision
			for _, t := range cands {
				if taken[t.Number] {
					continue
				}
				c := engine.Candidate{Number: t.Number, Skills: t.Skills, TouchedPaths: t.TouchedPaths, EstSeniorHours: t.EstSeniorHours, DueAt: t.DueAt}
				ds = append(ds, engine.Score(c, levels, engine.Budget{ActiveForJunior: active[j.Handle], ReservedOpen: reservedOpen, OpenTotal: openTotal}, p, now))
			}
			ds = engine.PickTop(ds)
			for _, d := range ds {
				if d.Reason == engine.ReasonProposed {
					taken[d.Number] = true
					reservedOpen++
					active[j.Handle]++
				}
				prev, seen := best[d.Number]
				if !seen || rank(d.Reason) < rank(prev.Reason) {
					best[d.Number], bestJunior[d.Number] = d, j.Handle
				}
			}
		}
		for _, t := range cands {
			d := best[t.Number]
			j := bestJunior[t.Number]
			t.Score, t.Breakdown, t.Reason = d.Breakdown.Score, d.Breakdown, string(d.Reason)
			switch {
			case d.Reason == engine.ReasonProposed:
				t.ReservedFor, t.ProposedAt, t.EstJuniorHours = j, now, d.Breakdown.EstJuniorHours
				if err := tx.Transition(&t, engine.Proposed, actor, now, map[string]any{"junior": j, "score": d.Breakdown.Score}); err != nil {
					return err
				}
				proposals = append(proposals, row{t, d, j})
			case d.Reason.Transient():
				if err := tx.SaveTicket(t, now); err != nil {
					return err
				}
				held = append(held, row{t, d, j})
			default:
				if err := tx.Transition(&t, engine.ToBuilder, actor, now, map[string]any{"reason": d.Reason}); err != nil {
					return err
				}
				toBuilder = append(toBuilder, row{t, d, j})
			}
		}
		// The card.
		var b strings.Builder
		fmt.Fprintf(&b, "**🙋 Dibs sweep** · %d ticket(s) scored\n", len(cands))
		for _, pr := range proposals {
			bd := pr.decision.Breakdown
			fmt.Fprintf(&b, "\n🙋 **DIBS for %s → #%d %s**\n", s.mention(pr.junior), pr.t.Number, pr.t.Title)
			fmt.Fprintf(&b, "   growth edge: %s\n", growthEdge(pr.t.Skills, bd))
			fmt.Fprintf(&b, "   builder ≈%s · %s ≈%s · slack %.1f days · score %.2f\n", hrs(pr.t.EstSeniorHours), pr.junior, hrs(bd.EstJuniorHours), bd.SlackDays, bd.Score)
			fmt.Fprintf(&b, "   budget: %d/%d rep slot(s) · backlog share %.0f%% (cap %.0f%%)\n", active[pr.junior], p.MaxActivePerJunior, bd.BacklogShareNew*100, p.MaxBacklogShare*100)
			fmt.Fprintf(&b, "   %s approve with **Allow**, or reply `@%s ok %d` / `@%s no %d`\n", s.seniorMentions(), s.Cfg.Agent("dibs"), pr.t.Number, s.Cfg.Agent("dibs"), pr.t.Number)
		}
		if len(toBuilder) > 0 {
			b.WriteString("\n→ **builder:** ")
			parts := make([]string, 0, len(toBuilder))
			for _, tb := range toBuilder {
				parts = append(parts, fmt.Sprintf("#%d %s", tb.t.Number, shortReason(tb.decision)))
			}
			b.WriteString(strings.Join(parts, " · "))
			b.WriteString("\n")
		}
		if len(held) > 0 {
			parts := make([]string, 0, len(held))
			for _, h := range held {
				parts = append(parts, fmt.Sprintf("#%d", h.t.Number))
			}
			fmt.Fprintf(&b, "⏸ **held for a later rep:** %s (%s)\n", strings.Join(parts, " "), held[0].decision.Reason.Explain())
		}
		if len(proposals) == 0 {
			b.WriteString("\nNo good rep this sweep. Everything goes to the builder.\n")
		}
		fmt.Fprintf(&b, "`@%s why <n>` shows the arithmetic.", s.Cfg.Agent("dibs"))
		r.say(s.team(), "%s", b.String())
		if p.AutoDelegate && len(toBuilder) > 0 {
			nums := make([]string, 0, len(toBuilder))
			for _, tb := range toBuilder {
				nums = append(nums, fmt.Sprintf("#%d", tb.t.Number))
			}
			r.say(s.team(), "%s take %s (not reps; build them normally).", s.agent("builder"), strings.Join(nums, " "))
		}
		for _, pr := range proposals {
			r.AgentTasks = append(r.AgentTasks, fmt.Sprintf("Ask the senior to approve #%d: run `dibs approve %d --by <senior>` when they press Allow, or when a senior says `ok %d`.", pr.t.Number, pr.t.Number, pr.t.Number))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sum := func(rows []row) []int {
		out := make([]int, 0, len(rows))
		for _, x := range rows {
			out = append(out, x.t.Number)
		}
		return out
	}
	r.Data = map[string]any{"proposed": sum(proposals), "to_builder": sum(toBuilder), "held": sum(held)}
	return r, nil
}

func rank(r engine.Reason) int {
	switch r {
	case engine.ReasonProposed:
		return 0
	case engine.ReasonNotTop, engine.ReasonNoBudget:
		return 1
	}
	return 2
}

func hrs(h float64) string {
	if h < 1 {
		return fmt.Sprintf("%.0f min", h*60)
	}
	return fmt.Sprintf("%.1fh", h)
}

func growthEdge(reqs []engine.SkillReq, bd engine.Breakdown) string {
	parts := []string{}
	for _, r := range reqs {
		l := bd.Levels[r.Name]
		if l >= engine.Independent {
			continue
		}
		next := l + 1
		if l == engine.Unseen {
			next = engine.Exposed
		}
		tag := ""
		if r.Weight == engine.Primary {
			tag = fmt.Sprintf(" (%s→%s)", l, next)
		} else {
			tag = fmt.Sprintf(" (%s)", l)
		}
		parts = append(parts, r.Name+tag)
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " · ")
}

func shortReason(d engine.Decision) string {
	switch d.Reason {
	case engine.ReasonTooEasy:
		return "too easy"
	case engine.ReasonTooFar:
		return "too far"
	case engine.ReasonRisky:
		if d.Breakdown.RiskyPath != "" {
			return "risky (" + d.Breakdown.RiskyPath + ")"
		}
		return "risky"
	case engine.ReasonNoSlack:
		return "no slack"
	case engine.ReasonLowScore:
		return "little to learn"
	case engine.ReasonNoSkills:
		return "untagged"
	}
	return string(d.Reason)
}

func (s *Service) seniorMentions() string {
	parts := []string{}
	for _, p := range s.Cfg.Team.Seniors {
		parts = append(parts, s.mention(p.Handle))
	}
	return strings.Join(parts, " / ")
}

// Why explains a ticket's routing.
func (s *Service) Why(n int) (*Result, error) {
	r := &Result{OK: true, Action: "why"}
	err := s.tx(func(tx *ledger.Tx) error {
		t, err := tx.Ticket(n)
		if err != nil {
			return err
		}
		r.Ticket = &t
		bd := t.Breakdown
		var b strings.Builder
		fmt.Fprintf(&b, "**Why #%d went where it went** (%s)\n", t.Number, t.State)
		if t.Reason != "" {
			fmt.Fprintf(&b, "decision: %s\n", engine.Reason(t.Reason).Explain())
		}
		for _, sk := range t.Skills {
			fmt.Fprintf(&b, "• %s (%s): junior at %s → growth %.1f\n", sk.Name, sk.Weight, bd.Levels[sk.Name], bd.GrowthBySkill[sk.Name])
		}
		fmt.Fprintf(&b, "growth %.2f + slack bonus %.2f − unseen penalty %.2f = **%.2f** (threshold %.1f)\n", bd.Growth, bd.SlackBonus, bd.UnseenPenalty, bd.Score, s.Cfg.Policy.ScoreThreshold)
		fmt.Fprintf(&b, "estimate: builder %s, junior %s · slack %.1f days", hrs(t.EstSeniorHours), hrs(bd.EstJuniorHours), bd.SlackDays)
		if bd.RiskyPath != "" {
			fmt.Fprintf(&b, " · risky path %s", bd.RiskyPath)
		}
		r.say(s.team(), "%s", b.String())
		return nil
	})
	return r, err
}

// ---------- Approve / decline / start ----------

// Approve reserves a proposed ticket for its junior. Only a senior may approve.
func (s *Service) Approve(n int, by string) (*Result, error) {
	sen, err := s.senior(by)
	if err != nil {
		return nil, err
	}
	var t ledger.Ticket
	if err := s.tx(func(tx *ledger.Tx) error {
		t, err = tx.Ticket(n)
		return err
	}); err != nil {
		return nil, err
	}
	if t.State != engine.Proposed {
		return nil, refuse("not_proposed", "#%d is %s; only a proposed rep can be approved", n, t.State)
	}
	j, ok := s.junior(t.ReservedFor)
	if !ok {
		return nil, refuse("unknown_person", "#%d was proposed for %q, who is not a junior in team.yaml", n, t.ReservedFor)
	}
	// Tracker first: if GitHub fails, the ledger is unchanged and approve can be retried.
	labels := []string{"dibs:reserved:" + j.Handle}
	for _, sk := range t.Skills {
		labels = append(labels, "skill:"+sk.Name)
	}
	if err := s.T.AddLabels(n, labels...); err != nil {
		return nil, fmt.Errorf("label #%d: %w", n, err)
	}
	if err := s.T.Assign(n, j.GitHub); err != nil {
		return nil, fmt.Errorf("assign #%d: %w", n, err)
	}
	edge := growthEdge(t.Skills, t.Breakdown)
	if err := s.T.Comment(n, fmt.Sprintf("🙋 **Dibs for @%s.** This ticket is a learning rep (growth edge: %s), approved by @%s.\n\nAgents on this team will coach, not solve. `dibs-guard` only lets @%s close it. If the deadline gets tight, Dibs releases it to the builder automatically and @%s gets a walkthrough.",
		j.GitHub, edge, sen.GitHub, j.GitHub, j.GitHub)); err != nil {
		return nil, fmt.Errorf("comment #%d: %w", n, err)
	}
	r := &Result{OK: true, Action: "approve"}
	now := s.now()
	err = s.tx(func(tx *ledger.Tx) error {
		t, err := tx.Ticket(n)
		if err != nil {
			return err
		}
		if t.State != engine.Proposed {
			return refuse("not_proposed", "#%d changed to %s while approving", n, t.State)
		}
		t.Approvals = 1
		if err := tx.Transition(&t, engine.Reserved, sen.Handle, now, map[string]any{"approved_by": sen.Handle}); err != nil {
			return err
		}
		r.Ticket = &t
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.say(s.team(), "✅ #%d is %s's rep, approved by %s. Labelled, assigned and guarded on GitHub.", n, s.mention(j.Handle), sen.Handle)
	r.say(j.PracticeRoom, "🙋 %s, dibs! **#%d %s** is your next rep.\nGrowth edge: %s. Rough size for you: %s. Deadline: %s.\nWhen you start, say `@%s start %d`. Ask %s or %s anything here: you'll get hints, never answers.",
		s.mention(j.Handle), n, t.Title, edge, hrs(t.EstJuniorHours), t.DueAt.Format("Mon 2 Jan"), s.Cfg.Agent("dibs"), n, s.agent("builder"), s.agent("reviewer"))
	return r, nil
}

// Decline sends a proposal to the builder. Only a senior may decline.
func (s *Service) Decline(n int, by string) (*Result, error) {
	sen, err := s.senior(by)
	if err != nil {
		return nil, err
	}
	r := &Result{OK: true, Action: "decline"}
	now := s.now()
	err = s.tx(func(tx *ledger.Tx) error {
		t, err := tx.Ticket(n)
		if err != nil {
			return err
		}
		if t.State != engine.Proposed {
			return refuse("not_proposed", "#%d is %s; only a proposed rep can be declined", n, t.State)
		}
		t.ReservedFor, t.Reason = "", "declined by senior"
		if err := tx.Transition(&t, engine.ToBuilder, sen.Handle, now, map[string]any{"declined_by": sen.Handle}); err != nil {
			return err
		}
		r.Ticket = &t
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.say(s.team(), "👌 %s declined #%d as a rep. %s take #%d", sen.Handle, n, s.agent("builder"), n)
	return r, nil
}

var acceptanceRe = regexp.MustCompile(`(?is)#+\s*acceptance criteria\s*\n(.*?)(?:\n#+\s|\z)`)

// Start begins a rep: the clock starts and the brief is posted (never the solution).
func (s *Service) Start(n int, by string) (*Result, error) {
	who, err := s.person(by)
	if err != nil {
		return nil, err
	}
	r := &Result{OK: true, Action: "start"}
	now := s.now()
	err = s.tx(func(tx *ledger.Tx) error {
		t, err := tx.Ticket(n)
		if err != nil {
			return err
		}
		if t.State == engine.InProgress {
			return refuse("already_started", "#%d is already in progress", n)
		}
		if t.State != engine.Reserved {
			return refuse("not_reserved", "#%d is %s; only a reserved rep can be started", n, t.State)
		}
		if who.Handle != t.ReservedFor {
			return refuse("not_yours", "#%d is reserved for %s, not %s", n, t.ReservedFor, who.Handle)
		}
		t.StartedAt = now
		levels, err := tx.Levels(t.ReservedFor)
		if err != nil {
			return err
		}
		ch := engine.OnStart(levels, t.Skills)
		if err := tx.SetLevels(t.ReservedFor, levels, now); err != nil {
			return err
		}
		if len(ch) > 0 {
			if err := tx.Event(n, who.Handle, "skill_change", now, ch); err != nil {
				return err
			}
		}
		if err := tx.Transition(&t, engine.InProgress, who.Handle, now, nil); err != nil {
			return err
		}
		r.Ticket = &t
		var b strings.Builder
		fmt.Fprintf(&b, "⏱️ **#%d %s** — clock started. Brief:\n", n, t.Title)
		if m := acceptanceRe.FindStringSubmatch(t.Body); m != nil {
			fmt.Fprintf(&b, "**Done when:**\n%s\n", strings.TrimSpace(m[1]))
		}
		fmt.Fprintf(&b, "Issue: %s\n", t.URL)
		fmt.Fprintf(&b, "Skills you're growing: %s\n", growthEdge(t.Skills, t.Breakdown))
		fmt.Fprintf(&b, "Hints: **L1 Nudge** available now. %s.\n", engine.NextUnlock(engine.HintGate{StartedAt: now}, now))
		fmt.Fprintf(&b, "Tell %s or %s what you tried: every honest attempt unlocks deeper hints. Open your PR with `Closes #%d`.", s.agent("builder"), s.agent("reviewer"), n)
		r.say(s.practice(t.ReservedFor), "%s", b.String())
		return nil
	})
	return r, err
}

// ---------- Status, hints, attempts ----------

// StatusView is what agents read before touching a ticket.
type StatusView struct {
	Number       int          `json:"number"`
	State        engine.State `json:"state"`
	Reserved     bool         `json:"reserved"`
	ReservedFor  string       `json:"reserved_for,omitempty"`
	YourRole     string       `json:"your_role"`
	AllowedHint  int          `json:"allowed_hint_level,omitempty"`
	HintRule     string       `json:"hint_rule,omitempty"`
	NextUnlock   string       `json:"next_unlock,omitempty"`
	Attempts     int          `json:"attempts"`
	Branch       string       `json:"junior_branch,omitempty"`
	PRURL        string       `json:"pr_url,omitempty"`
	WasReleased  bool         `json:"was_released"`
	ReleasedFor  string       `json:"released_for,omitempty"`
	Instructions string       `json:"instructions"`
}

// Status tells an agent how it may act on a ticket.
func (s *Service) Status(n int) (*Result, error) {
	r := &Result{OK: true, Action: "status"}
	now := s.now()
	err := s.tx(func(tx *ledger.Tx) error {
		t, err := tx.Ticket(n)
		if err != nil {
			return err
		}
		r.Ticket = &t
		v := StatusView{Number: n, State: t.State, Reserved: t.State.Reserved(), ReservedFor: t.ReservedFor, Attempts: t.Attempts,
			Branch: t.Branch, PRURL: t.PRURL, WasReleased: t.WasReleased, ReleasedFor: t.ReleasedFor}
		switch {
		case t.State.Reserved():
			g := gate(t)
			v.YourRole = "coach"
			v.AllowedHint = engine.AllowedLevel(g, now)
			v.HintRule = engine.HintRules[v.AllowedHint]
			v.NextUnlock = engine.NextUnlock(g, now)
			v.Instructions = fmt.Sprintf("Reserved learning rep for %s. You are a COACH: do not write, paste, commit or push code that solves it, and never open a PR closing it. Give at most an L%d hint via `dibs hint give`.", t.ReservedFor, v.AllowedHint)
		case t.State == engine.Proposed:
			v.YourRole = "wait"
			v.Instructions = "Proposed as a rep, waiting for a senior. Do not start it."
		case t.State == engine.ToBuilder || t.State == engine.BuilderPR:
			v.YourRole = "build"
			v.Instructions = "Not a rep. Build it normally."
			if t.WasReleased {
				v.Instructions = fmt.Sprintf("Released rep. Start from %s's branch %q if it exists, keep their commits, and write \"Builds on @%s's work\" in the PR description.", t.ReleasedFor, t.Branch, t.ReleasedFor)
			}
		default:
			v.YourRole = "none"
			v.Instructions = fmt.Sprintf("Ticket is %s; nothing to do.", t.State)
		}
		r.Data = v
		return nil
	})
	return r, err
}

func gate(t ledger.Ticket) engine.HintGate {
	return engine.HintGate{Attempts: t.Attempts, StartedAt: t.StartedAt, SeniorUnlocked: t.SeniorUnlocked}
}

// HintRequest asks how deep a hint may go right now.
func (s *Service) HintRequest(n, level int, agentName string) (*Result, error) {
	r := &Result{OK: true, Action: "hint_request"}
	now := s.now()
	err := s.tx(func(tx *ledger.Tx) error {
		t, err := tx.Ticket(n)
		if err != nil {
			return err
		}
		r.Ticket = &t
		if !t.State.Reserved() {
			r.Data = map[string]any{"not_reserved": true, "granted_level": engine.MaxHint, "rule": "Not a rep: help normally."}
			return nil
		}
		g := gate(t)
		granted, clamped := engine.GrantHint(level, g, now)
		reason := "granted"
		if clamped {
			reason = fmt.Sprintf("L%d is not unlocked yet; giving L%d", level, granted)
		}
		r.Data = map[string]any{"granted_level": granted, "requested_level": level, "clamped": clamped, "reason": reason,
			"rule": engine.HintRules[granted], "next_unlock": engine.NextUnlock(g, now)}
		return tx.Event(n, agentName, "hint_request", now, map[string]any{"requested": level, "granted": granted})
	})
	return r, err
}

var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{3,}`)

// repoIdents reads identifiers defined or used in the files a ticket touches.
func (s *Service) repoIdents(paths []string) map[string]bool {
	out := map[string]bool{}
	if s.RepoDir == "" {
		return out
	}
	for _, p := range paths {
		full := filepath.Join(s.RepoDir, filepath.FromSlash(p))
		rel, err := filepath.Rel(s.RepoDir, full)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		b, err := os.ReadFile(full)
		if err != nil || len(b) > 2<<20 {
			continue
		}
		for _, id := range identRe.FindAllString(string(b), -1) {
			out[id] = true
		}
	}
	return out
}

// HintGive checks an agent's hint text against its level and logs it. If the
// text breaks the ladder, it is refused (exit 2) and must be rewritten.
func (s *Service) HintGive(n, level int, agentName, text string) (*Result, error) {
	if strings.TrimSpace(text) == "" {
		return nil, refuse("empty_hint", "hint text is empty")
	}
	if level < engine.HintNudge || level > engine.MaxHint {
		return nil, refuse("bad_level", "level must be 1-4")
	}
	r := &Result{OK: true, Action: "hint_give"}
	var violation *RuleError
	now := s.now()
	err := s.tx(func(tx *ledger.Tx) error {
		t, err := tx.Ticket(n)
		if err != nil {
			return err
		}
		r.Ticket = &t
		if !t.State.Reserved() {
			r.Data = map[string]any{"not_reserved": true}
			return nil
		}
		allowed := engine.AllowedLevel(gate(t), now)
		if level > allowed {
			return &RuleError{Code: "level_locked", Message: fmt.Sprintf("L%d is locked; allowed now: L%d (%s)", level, allowed, engine.NextUnlock(gate(t), now))}
		}
		if v := engine.CheckHint(level, text, s.repoIdents(t.TouchedPaths)); len(v) > 0 {
			violation = &RuleError{Code: "hint_violation", Message: fmt.Sprintf("this L%d hint breaks the ladder; rewrite it (%s)", level, v[0].Detail), Detail: v}
			// Commit the rejection to the log; the refusal is returned after the transaction.
			return tx.Event(n, agentName, "hint_rejected", now, map[string]any{"level": level, "violations": v})
		}
		if level > t.MaxHintLevel {
			t.MaxHintLevel = level
		}
		if err := tx.SaveTicket(t, now); err != nil {
			return err
		}
		r.Data = map[string]any{"logged_level": level, "max_hint_level": t.MaxHintLevel}
		return tx.Event(n, agentName, "hint_given", now, map[string]any{"level": level, "chars": len(text)})
	})
	if err != nil {
		return nil, err
	}
	if violation != nil {
		return nil, violation
	}
	return r, nil
}

// Attempt records an honest attempt by the junior; attempts unlock deeper hints.
func (s *Service) Attempt(n int, by, note string) (*Result, error) {
	who, err := s.person(by)
	if err != nil {
		return nil, err
	}
	r := &Result{OK: true, Action: "attempt"}
	now := s.now()
	err = s.tx(func(tx *ledger.Tx) error {
		t, err := tx.Ticket(n)
		if err != nil {
			return err
		}
		if !t.State.Reserved() {
			return refuse("not_reserved", "#%d is %s; attempts only count on a rep", n, t.State)
		}
		if who.Role == "junior" && who.Handle != t.ReservedFor {
			return refuse("not_yours", "#%d is reserved for %s", n, t.ReservedFor)
		}
		before := engine.AllowedLevel(gate(t), now)
		if t.State == engine.Reserved {
			t.StartedAt = now
			if err := tx.Transition(&t, engine.InProgress, who.Handle, now, map[string]any{"why": "first attempt"}); err != nil {
				return err
			}
		}
		t.Attempts++
		if err := tx.SaveTicket(t, now); err != nil {
			return err
		}
		after := engine.AllowedLevel(gate(t), now)
		r.Ticket = &t
		r.Data = map[string]any{"attempts": t.Attempts, "allowed_hint_level": after, "unlocked": after > before, "next_unlock": engine.NextUnlock(gate(t), now)}
		return tx.Event(n, who.Handle, "attempt", now, map[string]any{"note": trunc(note, 280)})
	})
	return r, err
}

// KeepGoing answers the stuck check: one more hour before Dibs asks again.
func (s *Service) KeepGoing(n int, by string) (*Result, error) {
	who, err := s.person(by)
	if err != nil {
		return nil, err
	}
	r := &Result{OK: true, Action: "keep_going"}
	now := s.now()
	err = s.tx(func(tx *ledger.Tx) error {
		t, err := tx.Ticket(n)
		if err != nil {
			return err
		}
		if t.State != engine.InProgress || who.Handle != t.ReservedFor {
			return refuse("not_yours", "#%d is not your rep in progress", n)
		}
		t.KeepGoingUntil = now.Add(time.Hour)
		if err := tx.SaveTicket(t, now); err != nil {
			return err
		}
		r.Ticket = &t
		r.say(s.practice(t.ReservedFor), "💪 Keep going on #%d. I'll check back in an hour.", n)
		return tx.Event(n, who.Handle, "keep_going", now, nil)
	})
	return r, err
}

// Unlock lets a senior open deeper hints early.
func (s *Service) Unlock(n, level int, by string) (*Result, error) {
	sen, err := s.senior(by)
	if err != nil {
		return nil, err
	}
	if level < 2 || level > engine.MaxHint {
		return nil, refuse("bad_level", "unlock level must be 2-4")
	}
	r := &Result{OK: true, Action: "unlock"}
	now := s.now()
	err = s.tx(func(tx *ledger.Tx) error {
		t, err := tx.Ticket(n)
		if err != nil {
			return err
		}
		if !t.State.Reserved() {
			return refuse("not_reserved", "#%d is not a rep", n)
		}
		t.SeniorUnlocked = level
		if err := tx.SaveTicket(t, now); err != nil {
			return err
		}
		r.Ticket = &t
		r.say(s.practice(t.ReservedFor), "🔓 %s unlocked hints up to **L%d %s** on #%d.", sen.Handle, level, engine.HintNames[level], n)
		return tx.Event(n, sen.Handle, "unlock", now, map[string]any{"level": level})
	})
	return r, err
}
