package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vivekyarra/dibs/internal/engine"
	"github.com/vivekyarra/dibs/internal/ledger"
	"github.com/vivekyarra/dibs/internal/report"
)

// ---------- Review ----------

// Review records the reviewer agent's verdict on a PR. For a rep PR, rounds
// are counted, "ready" moves it to the senior, and teaching moments (at most
// a few per PR) are sent to the senior.
func (s *Service) Review(prNumber int, verdict, teaching, agentName string) (*Result, error) {
	if verdict != "changes_requested" && verdict != "ready" {
		return nil, refuse("bad_verdict", "--verdict must be changes_requested or ready")
	}
	r := &Result{OK: true, Action: "review"}
	now := s.now()
	err := s.tx(func(tx *ledger.Tx) error {
		p, err := s.policy(tx)
		if err != nil {
			return err
		}
		all, err := tx.Tickets(engine.JuniorPROpen, engine.SeniorSignoff)
		if err != nil {
			return err
		}
		var t *ledger.Ticket
		for i := range all {
			if all[i].PRNumber == prNumber {
				t = &all[i]
			}
		}
		if t == nil {
			r.Data = map[string]any{"rep": false, "note": "not a rep PR (or not synced yet): review normally"}
			return tx.Event(0, agentName, "review_other", now, map[string]any{"pr": prNumber, "verdict": verdict})
		}
		r.Ticket = t
		senior := s.seniorMentions()
		if verdict == "changes_requested" {
			t.ReviewRounds++
			if t.State == engine.SeniorSignoff {
				if err := tx.Transition(t, engine.JuniorPROpen, agentName, now, map[string]any{"why": "changes requested after ready"}); err != nil {
					return err
				}
			} else if err := tx.SaveTicket(*t, now); err != nil {
				return err
			}
		} else {
			if t.State == engine.JuniorPROpen {
				if err := tx.Transition(t, engine.SeniorSignoff, agentName, now, nil); err != nil {
					return err
				}
			}
			r.say(s.team(), "👀 %s: %s's PR for #%d is ready for your sign-off: %s\n%d review round(s) · deepest hint L%d · %d attempt(s). Merging records the evidence.",
				senior, s.mention(t.ReservedFor), t.Number, t.PRURL, t.ReviewRounds, t.MaxHintLevel, t.Attempts)
		}
		if strings.TrimSpace(teaching) != "" {
			if t.Teaching >= p.MaxTeachingPerPR {
				r.Data = map[string]any{"teaching_dropped": true, "why": fmt.Sprintf("already %d teaching moment(s) on this PR (cap %d)", t.Teaching, p.MaxTeachingPerPR)}
			} else {
				t.Teaching++
				if err := tx.SaveTicket(*t, now); err != nil {
					return err
				}
				r.say(s.team(), "🎓 Teaching moment for %s on #%d (%s): %s (≈5 min)", senior, t.Number, t.PRURL, trunc(strings.TrimSpace(teaching), 400))
				if err := tx.Event(t.Number, agentName, "teaching_moment", now, map[string]any{"text": trunc(teaching, 400)}); err != nil {
					return err
				}
			}
		}
		return tx.Event(t.Number, agentName, "review", now, map[string]any{"pr": prNumber, "verdict": verdict, "round": t.ReviewRounds})
	})
	return r, err
}

// ---------- Release ----------

// Release hands a rep back to the builder. The junior or a senior may release.
func (s *Service) Release(n int, by, reason string) (*Result, error) {
	who, err := s.person(by)
	if err != nil {
		return nil, err
	}
	var out *Result
	err = s.tx(func(tx *ledger.Tx) error {
		t, err := tx.Ticket(n)
		if err != nil {
			return err
		}
		if !t.State.Reserved() {
			return refuse("not_reserved", "#%d is %s; only a rep can be released", n, t.State)
		}
		if who.Role == "junior" && who.Handle != t.ReservedFor {
			return refuse("not_yours", "#%d is reserved for %s", n, t.ReservedFor)
		}
		if reason == "" {
			reason = "asked by " + who.Handle
		}
		out, err = s.release(tx, &t, who.Handle, reason, s.now())
		return err
	})
	return out, err
}

// release does the work inside an open transaction; tracker calls are best-effort
// because a deadline release must never be blocked by a GitHub hiccup.
func (s *Service) release(tx *ledger.Tx, t *ledger.Ticket, actor, reason string, now time.Time) (*Result, error) {
	r := &Result{OK: true, Action: "release"}
	junior := t.ReservedFor
	levels, err := tx.Levels(junior)
	if err != nil {
		return nil, err
	}
	ch := engine.OnRelease(levels, t.Skills)
	if err := tx.SetLevels(junior, levels, now); err != nil {
		return nil, err
	}
	if len(ch) > 0 {
		if err := tx.Event(t.Number, actor, "skill_change", now, ch); err != nil {
			return nil, err
		}
	}
	t.WasReleased, t.ReleasedReason, t.ReleasedFor = true, reason, junior
	if err := tx.Transition(t, engine.Released, actor, now, map[string]any{"reason": reason}); err != nil {
		return nil, err
	}
	if err := tx.Transition(t, engine.ToBuilder, actor, now, nil); err != nil {
		return nil, err
	}
	var warn []string
	if err := s.T.RemoveLabel(t.Number, "dibs:reserved:"+junior); err != nil {
		warn = append(warn, err.Error())
	}
	if err := s.T.AddLabels(t.Number, "dibs:released"); err != nil {
		warn = append(warn, err.Error())
	}
	if err := s.T.Comment(t.Number, fmt.Sprintf("↪️ Released from %s's reps (%s). The builder takes it from here%s, and %s gets a walkthrough of the final fix.",
		junior, reason, branchNote(t.Branch), junior)); err != nil {
		warn = append(warn, err.Error())
	}
	if len(warn) > 0 {
		r.Data = map[string]any{"tracker_warnings": warn}
	}
	r.Ticket = t
	moved := "Your time on it still counts"
	if len(ch) > 0 {
		moved = "You still moved: " + describeChanges(ch)
	}
	r.say(s.practice(junior), "↪️ Releasing #%d (%s). Not a failure. %s. The builder will finish it%s, and I'll post a walkthrough here once it merges.",
		t.Number, reason, moved, branchNote(t.Branch))
	r.say(s.team(), "%s take #%d (released rep, %s). Start from %s's branch%s and credit them in the PR: \"Builds on @%s's work\".",
		s.agent("builder"), t.Number, reason, junior, branchNote(t.Branch), s.ghLogin(junior))
	return r, nil
}

func branchNote(b string) string {
	if b == "" {
		return ""
	}
	return " from branch `" + b + "`"
}

func (s *Service) ghLogin(handle string) string {
	if p, ok := s.Cfg.Resolve(handle); ok {
		return p.GitHub
	}
	return handle
}

// ---------- Record ----------

// Record applies a merged rep's evidence to the skill map and announces it.
// Merged released reps get a walkthrough task instead.
func (s *Service) Record(n int, actor string) (*Result, error) {
	r := &Result{OK: true, Action: "record"}
	now := s.now()
	var junior string
	err := s.tx(func(tx *ledger.Tx) error {
		t, err := tx.Ticket(n)
		if err != nil {
			return err
		}
		if t.State == engine.Recorded {
			r.Ticket = &t
			r.Data = map[string]any{"already_recorded": true}
			return nil
		}
		if t.State != engine.Merged {
			return refuse("not_merged", "#%d is %s; evidence is recorded after merge", n, t.State)
		}
		r.Ticket = &t
		junior = t.ReservedFor
		if t.WasReleased {
			junior = t.ReleasedFor
		}
		if junior == "" {
			r.Data = map[string]any{"rep": false}
			return nil // builder ticket: nothing to record
		}
		if t.WasReleased {
			if err := tx.Transition(&t, engine.Recorded, actor, now, map[string]any{"released": true}); err != nil {
				return err
			}
			r.AgentTasks = append(r.AgentTasks, fmt.Sprintf("Write a walkthrough for %s of %s (released rep #%d): 5 bullets — what the fix does, why it works, the ONE concept to learn, what %s's branch got right, and the diff link. Post it in room %q.",
				junior, t.PRURL, t.Number, junior, s.practice(junior)))
			return nil
		}
		levels, err := tx.Levels(junior)
		if err != nil {
			return err
		}
		ch := engine.OnMerge(levels, t.Skills, t.MaxHintLevel, t.ReviewRounds)
		if err := tx.SetLevels(junior, levels, now); err != nil {
			return err
		}
		if len(ch) > 0 {
			if err := tx.Event(n, actor, "skill_change", now, ch); err != nil {
				return err
			}
		}
		if err := tx.Transition(&t, engine.Recorded, actor, now, map[string]any{"max_hint": t.MaxHintLevel, "rounds": t.ReviewRounds}); err != nil {
			return err
		}
		dur := ""
		if !t.StartedAt.IsZero() && !t.MergedAt.IsZero() {
			dur = " · " + humanDur(t.MergedAt.Sub(t.StartedAt))
		}
		clean := ""
		if engine.IsClean(t.MaxHintLevel, t.ReviewRounds) {
			clean = " · ✨ clean rep"
		}
		msg := fmt.Sprintf("🏁 #%d merged · deepest hint L%d · %d review round(s)%s%s\n📈 %s", n, t.MaxHintLevel, t.ReviewRounds, dur, clean, describeChanges(ch))
		r.say(s.practice(junior), "%s", msg)
		r.say(s.team(), "%s's rep: %s", s.mention(junior), msg)
		return nil
	})
	if err != nil || junior == "" {
		return r, err
	}
	docs, err := s.mirrorDocs()
	if err != nil {
		return nil, err
	}
	r.Docs = docs
	return r, nil
}

func humanDur(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 48*time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%.1f days", d.Hours()/24)
}

// ---------- Maps, mirror, budget, listings ----------

func (s *Service) skillOrder() []string {
	out := make([]string, 0, len(s.Cfg.Skills))
	for _, sk := range s.Cfg.Skills {
		out = append(out, sk.Name)
	}
	return out
}

func (s *Service) recentChanges(tx *ledger.Tx, handle string, limit int) ([]engine.SkillChange, error) {
	evs, err := tx.Events(0, "skill_change")
	if err != nil {
		return nil, err
	}
	var out []engine.SkillChange
	for i := len(evs) - 1; i >= 0 && len(out) < limit; i-- {
		t, err := tx.Ticket(evs[i].Ticket)
		if err != nil {
			continue
		}
		owner := t.ReservedFor
		if t.WasReleased {
			owner = t.ReleasedFor
		}
		if owner != handle {
			continue
		}
		var ch []engine.SkillChange
		if json.Unmarshal(evs[i].Data, &ch) == nil {
			out = append(out, ch...)
		}
	}
	return out, nil
}

// Map shows a junior's skill map.
func (s *Service) Map(handle string) (*Result, error) {
	r := &Result{OK: true, Action: "map"}
	juniors := s.Cfg.Team.Juniors
	if handle != "" {
		p, err := s.person(handle)
		if err != nil {
			return nil, err
		}
		j, ok := s.junior(p.Handle)
		if !ok {
			return nil, refuse("not_junior", "%s is not a junior", p.Handle)
		}
		juniors = append(juniors[:0:0], j)
	}
	maps := map[string]map[string]engine.SkillState{}
	err := s.tx(func(tx *ledger.Tx) error {
		for _, j := range juniors {
			levels, err := tx.Levels(j.Handle)
			if err != nil {
				return err
			}
			recent, err := s.recentChanges(tx, j.Handle, 5)
			if err != nil {
				return err
			}
			maps[j.Handle] = levels
			r.say(j.PracticeRoom, "%s", report.SkillMapMarkdown(j.Handle, levels, s.skillOrder(), recent))
		}
		return nil
	})
	r.Data = maps
	return r, err
}

func (s *Service) mirrorDocs() ([]Doc, error) {
	var docs []Doc
	err := s.tx(func(tx *ledger.Tx) error {
		var ledgerLines []string
		for _, j := range s.Cfg.Team.Juniors {
			levels, err := tx.Levels(j.Handle)
			if err != nil {
				return err
			}
			recent, err := s.recentChanges(tx, j.Handle, 8)
			if err != nil {
				return err
			}
			md := report.SkillMapMarkdown(j.Handle, levels, s.skillOrder(), recent)
			md += "\n\n_Written by the dibs agent from the ledger. Only dibs can edit this document (Switch enforces authorship)._"
			for _, room := range []string{s.team(), j.PracticeRoom} {
				docs = append(docs, Doc{Room: room, Name: "skill-map-" + j.Handle, Description: "Evidence-based skill map for " + j.Handle, Content: md})
			}
		}
		open, err := tx.Tickets(engine.Proposed, engine.Reserved, engine.InProgress, engine.JuniorPROpen, engine.SeniorSignoff)
		if err != nil {
			return err
		}
		ledgerLines = append(ledgerLines, "# Dibs ledger", "", "## Open reps", "", "| # | Title | Junior | State | Deepest hint | Attempts | Due |", "|---|---|---|---|---|---|---|")
		for _, t := range open {
			ledgerLines = append(ledgerLines, fmt.Sprintf("| #%d | %s | %s | %s | L%d | %d | %s |", t.Number, mdCell(t.Title), t.ReservedFor, t.State, t.MaxHintLevel, t.Attempts, t.DueAt.Format("2006-01-02")))
		}
		if len(open) == 0 {
			ledgerLines = append(ledgerLines, "| — | no open reps | | | | | |")
		}
		evs, err := tx.Events(0)
		if err != nil {
			return err
		}
		ledgerLines = append(ledgerLines, "", "## Last 20 events", "")
		start := len(evs) - 20
		if start < 0 {
			start = 0
		}
		for _, e := range evs[start:] {
			ledgerLines = append(ledgerLines, fmt.Sprintf("- %s · #%d · %s · %s", e.At.Format("Jan 2 15:04"), e.Ticket, e.Actor, e.Kind))
		}
		docs = append(docs, Doc{Room: s.team(), Name: "dibs-ledger", Description: "Open reps and recent Dibs decisions", Content: strings.Join(ledgerLines, "\n")})
		return nil
	})
	return docs, err
}

func mdCell(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "|", "\\|"), "\n", " ") }

// Mirror returns the room documents to push into Switch.
func (s *Service) Mirror() (*Result, error) {
	docs, err := s.mirrorDocs()
	if err != nil {
		return nil, err
	}
	return &Result{OK: true, Action: "mirror", Docs: docs}, nil
}

// Budget lets a senior change the learning budget.
func (s *Service) Budget(slots int, share float64, by string) (*Result, error) {
	sen, err := s.senior(by)
	if err != nil {
		return nil, err
	}
	if slots < 1 || slots > 10 {
		return nil, refuse("bad_budget", "--slots must be 1-10")
	}
	if share <= 0 || share > 1 {
		return nil, refuse("bad_budget", "--share must be in (0, 1], e.g. 0.2")
	}
	r := &Result{OK: true, Action: "budget"}
	now := s.now()
	err = s.tx(func(tx *ledger.Tx) error {
		if err := tx.SetMeta("policy.max_active_per_junior", strconv.Itoa(slots)); err != nil {
			return err
		}
		if err := tx.SetMeta("policy.max_backlog_share", strconv.FormatFloat(share, 'f', -1, 64)); err != nil {
			return err
		}
		return tx.Event(0, sen.Handle, "budget", now, map[string]any{"slots": slots, "share": share})
	})
	if err != nil {
		return nil, err
	}
	r.say(s.team(), "🎚️ %s set the learning budget: %d rep(s) per junior at a time, at most %.0f%% of the open backlog.", sen.Handle, slots, share*100)
	return r, nil
}

// Skills lists the skill catalog.
func (s *Service) Skills() *Result {
	return &Result{OK: true, Action: "skills", Data: s.Cfg.Skills}
}

// Team lists the people.
func (s *Service) Team() *Result {
	return &Result{OK: true, Action: "team", Data: s.Cfg.People()}
}

// ---------- Reports ----------

// Reps returns every finished rep (merged or released) as metrics input.
func (s *Service) Reps() ([]engine.Rep, error) {
	var reps []engine.Rep
	err := s.tx(func(tx *ledger.Tx) error {
		ts, err := tx.Tickets(engine.Recorded)
		if err != nil {
			return err
		}
		for _, t := range ts {
			junior := t.ReservedFor
			if t.WasReleased {
				junior = t.ReleasedFor
			}
			if junior == "" {
				continue
			}
			evs, err := tx.Events(t.Number, "skill_change")
			if err != nil {
				return err
			}
			var changes []engine.SkillChange
			for _, e := range evs {
				var ch []engine.SkillChange
				if json.Unmarshal(e.Data, &ch) == nil {
					changes = append(changes, ch...)
				}
			}
			finished := t.MergedAt
			if finished.IsZero() {
				finished = t.StartedAt
			}
			reps = append(reps, engine.Rep{Number: t.Number, Title: t.Title, Junior: junior, PRURL: t.PRURL, MaxHint: t.MaxHintLevel,
				ReviewRounds: t.ReviewRounds, Attempts: t.Attempts, StartedAt: t.StartedAt, FinishedAt: finished, Released: t.WasReleased,
				ReleasedReason: t.ReleasedReason, Approvals: t.Approvals, SeniorComments: t.SeniorComments, TeachingMoments: t.Teaching,
				SkillChanges: changes, Sample: t.Sample})
		}
		return nil
	})
	sort.Slice(reps, func(i, j int) bool { return reps[i].FinishedAt.Before(reps[j].FinishedAt) })
	return reps, err
}

// ReportInput builds the dashboard input from the ledger.
func (s *Service) ReportInput() (report.Input, error) {
	in := report.Input{Team: s.Cfg.Team.TeamRoom, Repo: s.Cfg.Team.Repo, Generated: s.now(), BaselineMinutes: s.Cfg.Policy.BaselineSeniorMinutes}
	reps, err := s.Reps()
	if err != nil {
		return in, err
	}
	weeks := engine.Weekly(reps, s.Cfg.Policy.BaselineSeniorMinutes)
	err = s.tx(func(tx *ledger.Tx) error {
		for _, j := range s.Cfg.Team.Juniors {
			jv := report.JuniorView{Handle: j.Handle, SkillOrder: s.skillOrder()}
			for _, w := range weeks {
				if w.Junior == j.Handle {
					jv.Weeks = append(jv.Weeks, w)
				}
			}
			levels, err := tx.Levels(j.Handle)
			if err != nil {
				return err
			}
			jv.Levels = levels
			hist, err := s.levelHistory(tx, j.Handle, jv.Weeks)
			if err != nil {
				return err
			}
			jv.History = hist
			var mine []engine.Rep
			for _, r := range reps {
				if r.Junior == j.Handle {
					mine = append(mine, r)
				}
			}
			if fu, ok := engine.FirstUnassisted(mine); ok {
				jv.FirstUnassisted = &fu
			}
			in.Juniors = append(in.Juniors, jv)
		}
		return nil
	})
	return in, err
}

// levelHistory replays initial + skill_change events to get each skill's level at the end of each week.
func (s *Service) levelHistory(tx *ledger.Tx, handle string, weeks []engine.Week) ([]report.LevelPoint, error) {
	if len(weeks) == 0 {
		return nil, nil
	}
	cur := map[string]engine.Level{}
	evs, err := tx.Events(0, "initial_levels", "skill_change")
	if err != nil {
		return nil, err
	}
	owner := map[int]string{}
	ownerOf := func(n int) string {
		if o, ok := owner[n]; ok {
			return o
		}
		t, err := tx.Ticket(n)
		o := ""
		if err == nil {
			o = t.ReservedFor
			if t.WasReleased {
				o = t.ReleasedFor
			}
		}
		owner[n] = o
		return o
	}
	var out []report.LevelPoint
	ei := 0
	for _, w := range weeks {
		for ei < len(evs) && engine.ISOWeek(evs[ei].At) <= w.Label {
			e := evs[ei]
			ei++
			if e.Kind == "initial_levels" {
				var d struct {
					Junior string                       `json:"junior"`
					Levels map[string]engine.SkillState `json:"levels"`
				}
				if json.Unmarshal(e.Data, &d) == nil && d.Junior == handle {
					for k, v := range d.Levels {
						cur[k] = v.Level
					}
				}
				continue
			}
			if ownerOf(e.Ticket) != handle {
				continue
			}
			var ch []engine.SkillChange
			if json.Unmarshal(e.Data, &ch) == nil {
				for _, c := range ch {
					cur[c.Skill] = c.To
				}
			}
		}
		for _, sk := range s.skillOrder() {
			out = append(out, report.LevelPoint{Week: w.Label, Skill: sk, Level: cur[sk]})
		}
	}
	return out, nil
}

// Report renders the weekly chat report (and optionally the HTML dashboard).
func (s *Service) Report(junior string) (*Result, error) {
	in, err := s.ReportInput()
	if err != nil {
		return nil, err
	}
	r := &Result{OK: true, Action: "report"}
	week := engine.ISOWeek(s.now())
	for _, jv := range in.Juniors {
		if junior != "" && jv.Handle != junior {
			continue
		}
		var w engine.Week
		found := false
		for _, x := range jv.Weeks {
			if x.Label == week {
				w, found = x, true
			}
		}
		if !found && len(jv.Weeks) > 0 {
			w, found = jv.Weeks[len(jv.Weeks)-1], true
		}
		if !found {
			w = engine.Week{Label: week, Junior: jv.Handle}
		}
		md := report.WeekMarkdown(jv, w, in.BaselineMinutes)
		j, _ := s.junior(jv.Handle)
		r.say(s.team(), "%s", md)
		if j.PracticeRoom != "" {
			r.say(j.PracticeRoom, "%s", md)
		}
	}
	r.Data = in
	return r, nil
}
