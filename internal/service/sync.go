package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vivekyarra/dibs/internal/engine"
	"github.com/vivekyarra/dibs/internal/ledger"
	"github.com/vivekyarra/dibs/internal/tracker"
)

// Sync pulls issues and PRs from the tracker and applies every automatic
// rule: new tickets, PR links, merges (with evidence), reverts, deadline
// releases, stuck checks, proposal and hold timeouts.
func (s *Service) Sync(actor string) (*Result, error) {
	r := &Result{OK: true, Action: "sync"}
	issues, err := s.T.Issues()
	if err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}
	prs, err := s.T.PRs()
	if err != nil {
		return nil, fmt.Errorf("list PRs: %w", err)
	}
	now := s.now()
	open := map[int]tracker.Issue{}
	for _, is := range issues {
		open[is.Number] = is
	}
	changes := []string{}
	var record []int

	err = s.tx(func(tx *ledger.Tx) error {
		p, err := s.policy(tx)
		if err != nil {
			return err
		}
		// 1. New and changed issues.
		for _, is := range issues {
			t, err := tx.Ticket(is.Number)
			due := tracker.DueFrom(is)
			if err != nil {
				if !isNoTicket(err) {
					return err
				}
				if due.IsZero() {
					due = now.Add(time.Duration(p.DefaultDueDays*24) * time.Hour)
				}
				t = ledger.Ticket{Number: is.Number, Title: is.Title, URL: is.URL, Body: is.Body, State: engine.New, DueAt: due, Sample: s.Sample}
				if err := tx.SaveTicket(t, now); err != nil {
					return err
				}
				if err := tx.Event(t.Number, actor, "new", now, map[string]any{"title": is.Title}); err != nil {
					return err
				}
				changes = append(changes, fmt.Sprintf("#%d new", t.Number))
				continue
			}
			changed := t.Title != is.Title || t.Body != is.Body || t.URL != is.URL
			t.Title, t.Body, t.URL = is.Title, is.Body, is.URL
			if !due.IsZero() && !due.Equal(t.DueAt) {
				if err := tx.Event(t.Number, actor, "due_changed", now, map[string]any{"from": t.DueAt, "to": due}); err != nil {
					return err
				}
				t.DueAt = due
				changed = true
				changes = append(changes, fmt.Sprintf("#%d due %s", t.Number, due.Format("2006-01-02")))
			}
			if changed {
				if err := tx.SaveTicket(t, now); err != nil {
					return err
				}
			}
		}

		// 2. Pull requests.
		sort.Slice(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
		for _, pr := range prs {
			if rev := tracker.Reverts(pr); rev != 0 && pr.Merged {
				if err := s.applyRevert(tx, rev, pr, actor, now, r, &changes); err != nil {
					return err
				}
			}
			for _, n := range tracker.Closes(pr) {
				t, err := tx.Ticket(n)
				if isNoTicket(err) {
					continue
				} else if err != nil {
					return err
				}
				rec, err := s.applyPR(tx, &t, pr, actor, now, r, &changes)
				if err != nil {
					return err
				}
				if rec {
					record = append(record, t.Number)
				}
			}
		}

		// 3. Tickets whose issue closed without a merged PR.
		all, err := tx.Tickets()
		if err != nil {
			return err
		}
		for _, t := range all {
			if _, isOpen := open[t.Number]; isOpen || !t.State.Open() {
				continue
			}
			is, err := s.T.Issue(t.Number)
			if err != nil || is.State != "closed" {
				continue
			}
			if engine.CanTransition(t.State, engine.Closed) {
				if err := tx.Transition(&t, engine.Closed, actor, now, map[string]any{"why": "issue closed without a merged PR"}); err != nil {
					return err
				}
				changes = append(changes, fmt.Sprintf("#%d closed", t.Number))
			}
		}

		// 4. Senior review effort on reps in review.
		inReview, err := tx.Tickets(engine.JuniorPROpen, engine.SeniorSignoff)
		if err != nil {
			return err
		}
		for _, t := range inReview {
			if t.PRNumber == 0 {
				continue
			}
			if err := s.countSeniorEffort(tx, &t, now); err != nil {
				return err
			}
		}

		// 5. Time-based rules.
		return s.applyTimers(tx, p, actor, now, r, &changes)
	})
	if err != nil {
		return nil, err
	}
	for _, n := range record {
		rr, err := s.Record(n, actor)
		if err != nil {
			return nil, err
		}
		r.merge(rr)
		changes = append(changes, fmt.Sprintf("#%d evidence recorded", n))
	}
	r.Data = map[string]any{"changes": changes, "open_issues": len(issues), "prs_seen": len(prs)}
	return r, nil
}

func isNoTicket(err error) bool {
	return err != nil && strings.Contains(err.Error(), ledger.ErrNoTicket.Error())
}

// applyPR links a PR to its ticket. It returns true when the ticket was merged
// and needs its evidence recorded.
func (s *Service) applyPR(tx *ledger.Tx, t *ledger.Ticket, pr tracker.PR, actor string, now time.Time, r *Result, changes *[]string) (bool, error) {
	junior, isRep := s.junior(t.ReservedFor)
	byJunior := isRep && strings.EqualFold(pr.Author, junior.GitHub)

	setPR := func() {
		t.PRNumber, t.PRURL, t.PRAuthor, t.Branch = pr.Number, pr.URL, pr.Author, pr.HeadRef
	}

	if pr.Merged {
		switch {
		case t.State.Reserved() && byJunior:
			setPR()
			t.MergedAt = pr.MergedAt
			if t.StartedAt.IsZero() {
				t.StartedAt = pr.MergedAt
			}
			var path []engine.State
			switch t.State {
			case engine.Reserved, engine.InProgress:
				path = []engine.State{engine.JuniorPROpen, engine.Merged}
			default:
				path = []engine.State{engine.Merged}
			}
			for _, st := range path {
				if err := tx.Transition(t, st, actor, now, map[string]any{"pr": pr.Number}); err != nil {
					return false, err
				}
			}
			*changes = append(*changes, fmt.Sprintf("#%d merged (rep)", t.Number))
			return true, nil
		case t.State == engine.ToBuilder || t.State == engine.BuilderPR:
			setPR()
			t.MergedAt = pr.MergedAt
			if err := tx.Transition(t, engine.Merged, actor, now, map[string]any{"pr": pr.Number}); err != nil {
				return false, err
			}
			*changes = append(*changes, fmt.Sprintf("#%d merged (builder)", t.Number))
			return t.WasReleased, nil
		case t.State.Reserved() && !byJunior:
			// Someone else merged a reserved rep around the guard: close it, record nothing.
			if err := tx.Event(t.Number, actor, "bypass", now, map[string]any{"pr": pr.Number, "author": pr.Author}); err != nil {
				return false, err
			}
			if err := tx.Transition(t, engine.Closed, actor, now, map[string]any{"why": "merged by someone other than the reserved junior"}); err != nil {
				return false, err
			}
			r.say(s.team(), "⚠️ #%d was a rep for %s but %s merged %s. Closed without evidence; check that `dibs-guard` is a required check on the default branch.",
				t.Number, s.mention(t.ReservedFor), pr.Author, pr.URL)
		case t.State == engine.New || t.State == engine.Triaged || t.State == engine.Proposed:
			if err := tx.Transition(t, engine.Closed, actor, now, map[string]any{"why": "merged before Dibs routed it", "pr": pr.Number}); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	if pr.State != "open" {
		return false, nil
	}
	switch {
	case (t.State == engine.Reserved || t.State == engine.InProgress) && byJunior:
		setPR()
		if t.StartedAt.IsZero() {
			t.StartedAt = now
		}
		if err := tx.Transition(t, engine.JuniorPROpen, actor, now, map[string]any{"pr": pr.Number}); err != nil {
			return false, err
		}
		r.say(s.practice(t.ReservedFor), "🚀 PR for #%d is up: %s. %s please do a Socratic review: questions, not fixes.", t.Number, pr.URL, s.agent("reviewer"))
		*changes = append(*changes, fmt.Sprintf("#%d junior PR #%d", t.Number, pr.Number))
	case t.State.Reserved() && !byJunior && t.PRNumber != pr.Number:
		seen, err := tx.Events(t.Number, "foreign_pr")
		if err != nil {
			return false, err
		}
		for _, e := range seen {
			if strings.Contains(string(e.Data), fmt.Sprintf(`"pr":%d`, pr.Number)) {
				return false, nil
			}
		}
		if err := tx.Event(t.Number, actor, "foreign_pr", now, map[string]any{"pr": pr.Number, "author": pr.Author}); err != nil {
			return false, err
		}
		r.say(s.team(), "🛑 %s opened %s, which closes #%d, a rep reserved for %s. `dibs-guard` will block it. If the rep should go, ask `@%s release %d`.",
			pr.Author, pr.URL, t.Number, s.mention(t.ReservedFor), s.Cfg.Agent("dibs"), t.Number)
	case t.State == engine.ToBuilder:
		setPR()
		if err := tx.Transition(t, engine.BuilderPR, actor, now, map[string]any{"pr": pr.Number}); err != nil {
			return false, err
		}
		*changes = append(*changes, fmt.Sprintf("#%d builder PR #%d", t.Number, pr.Number))
	}
	return false, nil
}

func (s *Service) applyRevert(tx *ledger.Tx, revertedPR int, pr tracker.PR, actor string, now time.Time, r *Result, changes *[]string) error {
	recs, err := tx.Tickets(engine.Recorded)
	if err != nil {
		return err
	}
	for _, t := range recs {
		if t.PRNumber != revertedPR || t.ReservedFor == "" || t.WasReleased {
			continue
		}
		evs, err := tx.Events(t.Number, "regression")
		if err != nil {
			return err
		}
		if len(evs) > 0 {
			return nil // already applied
		}
		levels, err := tx.Levels(t.ReservedFor)
		if err != nil {
			return err
		}
		ch := engine.OnRegression(levels, t.Skills)
		if err := tx.SetLevels(t.ReservedFor, levels, now); err != nil {
			return err
		}
		if err := tx.Event(t.Number, actor, "regression", now, map[string]any{"revert_pr": pr.Number, "changes": ch}); err != nil {
			return err
		}
		*changes = append(*changes, fmt.Sprintf("#%d reverted", t.Number))
		r.say(s.practice(t.ReservedFor), "↩️ The fix for #%d was reverted (%s). %s. That's normal; reverts are how teams learn. The skill map shows it honestly.",
			t.Number, pr.URL, describeChanges(ch))
	}
	return nil
}

func (s *Service) countSeniorEffort(tx *ledger.Tx, t *ledger.Ticket, now time.Time) error {
	reviews, commenters, err := s.T.Reviews(t.PRNumber)
	if err != nil {
		return nil // effort accounting is best-effort; never block sync on it
	}
	comments, approved := 0, 0
	for _, a := range commenters {
		if s.Cfg.IsSenior(a) {
			comments++
		}
	}
	for _, rv := range reviews {
		if !s.Cfg.IsSenior(rv.Author) {
			continue
		}
		switch rv.State {
		case "APPROVED":
			approved = 1
		case "CHANGES_REQUESTED", "COMMENTED":
			comments++
		}
	}
	// Approvals = the reservation (1) + a senior approval on the PR.
	approvals := 1 + approved
	if comments == t.SeniorComments && approvals == t.Approvals {
		return nil
	}
	t.SeniorComments, t.Approvals = comments, approvals
	return tx.SaveTicket(*t, now)
}

func (s *Service) applyTimers(tx *ledger.Tx, p engine.Policy, actor string, now time.Time, r *Result, changes *[]string) error {
	reps, err := tx.Tickets(engine.Reserved, engine.InProgress, engine.JuniorPROpen, engine.SeniorSignoff)
	if err != nil {
		return err
	}
	for _, t := range reps {
		chk := engine.CheckRelease(engine.RepFacts{State: t.State, DueAt: t.DueAt, EstJuniorHours: t.EstJuniorHours,
			StartedAt: t.StartedAt, StuckAskedAt: t.StuckAskedAt, KeepGoingUntil: t.KeepGoingUntil}, p, now)
		switch {
		case chk.Release:
			rr, err := s.release(tx, &t, "dibs", chk.Reason, now)
			if err != nil {
				return err
			}
			r.merge(rr)
			*changes = append(*changes, fmt.Sprintf("#%d released (%s)", t.Number, chk.Reason))
		case chk.AskFirst:
			t.StuckAskedAt = now
			if err := tx.SaveTicket(t, now); err != nil {
				return err
			}
			if err := tx.Event(t.Number, actor, "stuck_ask", now, nil); err != nil {
				return err
			}
			r.say(s.practice(t.ReservedFor), "⏱️ %s, #%d is taking longer than twice the estimate. That's fine, and it's your call: reply `@%s keep-going %d` for another hour, or `@%s release %d` and the builder will finish it from your branch (you'll get a walkthrough). No answer in %0.f min → I'll release it.",
				s.mention(t.ReservedFor), t.Number, s.Cfg.Agent("dibs"), t.Number, s.Cfg.Agent("dibs"), t.Number, p.StuckAnswerMinutes)
		}
	}
	proposed, err := tx.Tickets(engine.Proposed)
	if err != nil {
		return err
	}
	for _, t := range proposed {
		if engine.ProposalExpired(t.ProposedAt, p, now) {
			t.Reason = "proposal timed out"
			t.ReservedFor = ""
			if err := tx.Transition(&t, engine.ToBuilder, actor, now, map[string]any{"why": "no senior answer"}); err != nil {
				return err
			}
			r.say(s.team(), "⌛ Nobody approved #%d as a rep within %0.fh, so it goes to the builder. %s take #%d", t.Number, p.ProposalTimeoutHours, s.agent("builder"), t.Number)
			*changes = append(*changes, fmt.Sprintf("#%d proposal expired", t.Number))
		}
	}
	held, err := tx.Tickets(engine.Triaged)
	if err != nil {
		return err
	}
	var expired []string
	for _, t := range held {
		if t.Reason != "" && engine.HoldExpired(t.TriagedAt, p, now) {
			t.Reason = "held too long"
			if err := tx.Transition(&t, engine.ToBuilder, actor, now, map[string]any{"why": "hold expired"}); err != nil {
				return err
			}
			expired = append(expired, fmt.Sprintf("#%d", t.Number))
		}
	}
	if len(expired) > 0 {
		r.say(s.team(), "⌛ Held longer than %0.fh with no rep slot: %s go to the builder. %s take %s", p.HoldMaxHours, strings.Join(expired, " "), s.agent("builder"), strings.Join(expired, " "))
		*changes = append(*changes, "hold expired: "+strings.Join(expired, " "))
	}
	return nil
}
