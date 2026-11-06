package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/vivekyarra/dibs/internal/engine"
	"github.com/vivekyarra/dibs/internal/ledger"
)

// Poster delivers a message into a Switch room by name.
type Poster interface {
	Post(ctx context.Context, room, text string) error
}

// AutopilotOptions configures the unattended loop.
type AutopilotOptions struct {
	Interval time.Duration // poll interval (default 5m)
	Once     bool          // run a single cycle
	Log      io.Writer
	Poster   Poster // nil → messages are printed instead of posted
	// ReportWeekday/ReportHour: when the weekly ramp report is posted (default Monday 09:00 UTC).
	ReportWeekday time.Weekday
	ReportHour    int
}

// Autopilot runs Dibs without anyone typing: every interval it syncs GitHub,
// triages new issues with its deterministic suggestions, scores, applies
// deadlines and stuck checks, and posts the results into the Switch rooms.
// Once a week it posts the ramp report. Judgment work (walkthroughs, room
// documents) is handed to the dibs agent with an @mention.
func (s *Service) Autopilot(o AutopilotOptions) (*Result, error) {
	if o.Interval <= 0 {
		o.Interval = 5 * time.Minute
	}
	if o.Interval < 30*time.Second {
		return nil, refuse("bad_interval", "--interval must be at least 30s (GitHub rate limits)")
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.ReportWeekday == 0 && o.ReportHour == 0 {
		o.ReportWeekday, o.ReportHour = time.Monday, 9
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	total := &Result{OK: true, Action: "autopilot"}
	cycles := 0
	for {
		res, err := s.cycle(ctx, o)
		cycles++
		if err != nil {
			fmt.Fprintf(o.Log, "%s autopilot: cycle failed: %v\n", s.now().Format(time.RFC3339), err)
			if o.Once {
				return nil, err
			}
		} else {
			fmt.Fprintf(o.Log, "%s autopilot: cycle %d · %d message(s)\n", s.now().Format(time.RFC3339), cycles, len(res.Messages))
			if o.Once || o.Poster == nil {
				total.merge(res)
			}
		}
		if o.Once {
			total.Data = map[string]any{"cycles": cycles, "posted": o.Poster != nil}
			return total, nil
		}
		select {
		case <-ctx.Done():
			total.Data = map[string]any{"cycles": cycles}
			return total, nil
		case <-time.After(o.Interval):
		}
	}
}

func (s *Service) cycle(ctx context.Context, o AutopilotOptions) (*Result, error) {
	res, err := s.Sweep("autopilot", true)
	if err != nil {
		return nil, err
	}
	if rep, err := s.weeklyReportDue(o); err != nil {
		return nil, err
	} else if rep != nil {
		res.merge(rep)
	}
	dibs := "@" + s.Cfg.Agent("dibs")
	if len(res.Docs) > 0 {
		res.say(s.team(), "%s run `dibs mirror` and update the room documents.", dibs)
	}
	for _, task := range res.AgentTasks {
		if strings.HasPrefix(task, "Ask the senior") {
			continue // the card already asks the senior
		}
		res.say(s.team(), "%s %s", dibs, task)
	}
	if o.Poster != nil {
		for _, m := range res.Messages {
			if err := o.Poster.Post(ctx, m.Room, m.Text); err != nil {
				return nil, fmt.Errorf("post to %s: %w", m.Room, err)
			}
		}
	}
	return res, nil
}

func (s *Service) weeklyReportDue(o AutopilotOptions) (*Result, error) {
	now := s.now()
	if now.Weekday() != o.ReportWeekday || now.Hour() < o.ReportHour {
		return nil, nil
	}
	week := engine.ISOWeek(now)
	var last string
	if err := s.tx(func(tx *ledger.Tx) error {
		var err error
		last, err = tx.Meta("autopilot.last_report_week")
		return err
	}); err != nil {
		return nil, err
	}
	if last == week {
		return nil, nil
	}
	rep, err := s.Report("")
	if err != nil {
		return nil, err
	}
	rep.Data = nil
	return rep, s.tx(func(tx *ledger.Tx) error { return tx.SetMeta("autopilot.last_report_week", week) })
}

// Check is one doctor finding.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Doctor verifies the setup end to end and says how to fix each problem.
func (s *Service) Doctor() (*Result, error) {
	var checks []Check
	add := func(name string, ok bool, detail string) { checks = append(checks, Check{name, ok, detail}) }
	add("config", true, fmt.Sprintf("%d senior(s), %d junior(s), %d skill(s) in %s", len(s.Cfg.Team.Seniors), len(s.Cfg.Team.Juniors), len(s.Cfg.Skills), s.Cfg.Dir))
	err := s.tx(func(tx *ledger.Tx) error {
		v, err := tx.Meta("initialized_at")
		if err != nil {
			return err
		}
		if v == "" {
			add("ledger", false, "not initialised: run `dibs init`")
		} else {
			add("ledger", true, s.Cfg.DB+" (initialised "+v+")")
		}
		for _, j := range s.Cfg.Team.Juniors {
			has, err := tx.HasLevels(j.Handle)
			if err != nil {
				return err
			}
			add("skill map: "+j.Handle, has, map[bool]string{true: "initialised", false: "missing: run `dibs init`"}[has])
		}
		return nil
	})
	if err != nil {
		add("ledger", false, err.Error())
	}
	if issues, err := s.T.Issues(); err != nil {
		add("github", false, fmt.Sprintf("cannot read %s: %v (check `gh auth status` or GH_TOKEN)", s.Cfg.Team.Repo, err))
	} else {
		add("github", true, fmt.Sprintf("%s reachable, %d open issue(s)", s.Cfg.Team.Repo, len(issues)))
	}
	ok := true
	for _, c := range checks {
		ok = ok && c.OK
	}
	r := &Result{OK: ok, Action: "doctor", Data: checks}
	if !ok {
		return r, &RuleError{Code: "doctor", Message: "setup is incomplete (see data)", Detail: checks}
	}
	return r, nil
}
