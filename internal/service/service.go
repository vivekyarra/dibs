// Package service runs Dibs' operations: it reads facts from the tracker and
// the ledger, asks the engine for decisions, writes them back, and returns
// the exact chat messages the dibs agent should post. The agent delivers
// words; it never decides.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vivekyarra/dibs/internal/config"
	"github.com/vivekyarra/dibs/internal/engine"
	"github.com/vivekyarra/dibs/internal/ledger"
	"github.com/vivekyarra/dibs/internal/tracker"
)

// Message is one chat message the dibs agent must post, verbatim, in Room.
type Message struct {
	Room string `json:"room"`
	Text string `json:"text"`
}

// Doc is a Switch room document the dibs agent must create or update.
type Doc struct {
	Room        string `json:"room"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

// Result is what every operation returns.
type Result struct {
	OK         bool           `json:"ok"`
	Action     string         `json:"action"`
	Ticket     *ledger.Ticket `json:"ticket,omitempty"`
	Messages   []Message      `json:"messages"`
	Docs       []Doc          `json:"docs,omitempty"`
	AgentTasks []string       `json:"agent_tasks,omitempty"`
	Data       any            `json:"data,omitempty"`
}

func (r *Result) say(room, f string, a ...any) {
	r.Messages = append(r.Messages, Message{Room: room, Text: fmt.Sprintf(f, a...)})
}

func (r *Result) merge(o *Result) {
	if o == nil {
		return
	}
	r.Messages = append(r.Messages, o.Messages...)
	r.Docs = append(r.Docs, o.Docs...)
	r.AgentTasks = append(r.AgentTasks, o.AgentTasks...)
}

// RuleError is a refusal by a Dibs rule (exit code 2), as opposed to a failure.
type RuleError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

func (e *RuleError) Error() string { return e.Message }

func refuse(code, f string, a ...any) error {
	return &RuleError{Code: code, Message: fmt.Sprintf(f, a...)}
}

// Service holds everything an operation needs.
type Service struct {
	Cfg *config.Config
	L   *ledger.Ledger
	T   tracker.Tracker
	Now func() time.Time
	// Sample marks tickets created by `dibs demo` so reports label them as a sample team.
	Sample bool
	// RepoDir is the repository root, used to read identifiers from touched files.
	RepoDir string
}

// Clock returns DIBS_NOW (RFC3339) if set, else the real time. Used for demos and tests.
func Clock() func() time.Time {
	if v := os.Getenv("DIBS_NOW"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return func() time.Time { return t }
		}
	}
	return func() time.Time { return time.Now().UTC() }
}

func (s *Service) now() time.Time { return s.Now().UTC() }

func (s *Service) tx(fn func(*ledger.Tx) error) error {
	return s.L.Tx(context.Background(), fn)
}

func (s *Service) policy(tx *ledger.Tx) (engine.Policy, error) {
	p := s.Cfg.Policy
	if v, err := tx.Meta("policy.max_active_per_junior"); err != nil {
		return p, err
	} else if v != "" {
		p.MaxActivePerJunior, _ = strconv.Atoi(v)
	}
	if v, err := tx.Meta("policy.max_backlog_share"); err != nil {
		return p, err
	} else if v != "" {
		p.MaxBacklogShare, _ = strconv.ParseFloat(v, 64)
	}
	return p, nil
}

// person resolves a --by name to a team member.
func (s *Service) person(name string) (config.Person, error) {
	p, ok := s.Cfg.Resolve(name)
	if !ok {
		return p, refuse("unknown_person", "%q is not on the team (see .dibs/team.yaml)", name)
	}
	return p, nil
}

func (s *Service) senior(name string) (config.Person, error) {
	p, err := s.person(name)
	if err != nil {
		return p, err
	}
	if p.Role != "senior" {
		return p, refuse("not_senior", "only a senior can do that; %s is a %s", p.Handle, p.Role)
	}
	return p, nil
}

func (s *Service) junior(handle string) (config.Person, bool) {
	for _, j := range s.Cfg.Team.Juniors {
		if j.Handle == handle {
			return j, true
		}
	}
	return config.Person{}, false
}

func (s *Service) practice(handle string) string {
	if j, ok := s.junior(handle); ok {
		return j.PracticeRoom
	}
	return s.Cfg.Team.TeamRoom
}

func (s *Service) team() string { return s.Cfg.Team.TeamRoom }

// mention returns how to @-mention a person in chat.
func (s *Service) mention(handle string) string {
	p, ok := s.Cfg.Resolve(handle)
	if ok && len(p.Chat) > 0 {
		return "@" + strings.TrimPrefix(p.Chat[0], "@")
	}
	return "@" + handle
}

func (s *Service) agent(role string) string { return "@" + s.Cfg.Agent(role) }

// Labels Dibs uses on GitHub.
func (s *Service) labels() []tracker.Label {
	ls := []tracker.Label{
		{Name: "dibs:released", Color: "c5def5", Description: "Was a learning rep; released to the builder"},
		{Name: "regression", Color: "b60205", Description: "Broke something that worked"},
	}
	for _, j := range s.Cfg.Team.Juniors {
		ls = append(ls, tracker.Label{Name: "dibs:reserved:" + j.Handle, Color: "fbca04", Description: "Learning rep reserved for " + j.Handle + ". Agents coach, never solve."})
	}
	for _, sk := range s.Cfg.Skills {
		ls = append(ls, tracker.Label{Name: "skill:" + sk.Name, Color: "d4c5f9", Description: trunc(sk.Description, 90)})
	}
	return ls
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// Init creates labels and initialises each junior's skill map from team.yaml (never
// overwriting levels already earned).
func (s *Service) Init() (*Result, error) {
	r := &Result{OK: true, Action: "init"}
	if err := s.T.EnsureLabels(s.labels()); err != nil {
		return nil, fmt.Errorf("create labels: %w", err)
	}
	now := s.now()
	initialised := []string{}
	err := s.tx(func(tx *ledger.Tx) error {
		for _, j := range s.Cfg.Team.Juniors {
			has, err := tx.HasLevels(j.Handle)
			if err != nil {
				return err
			}
			if has {
				continue
			}
			levels := map[string]engine.SkillState{}
			for _, sk := range s.Cfg.Skills {
				levels[sk.Name] = engine.SkillState{Level: j.Skills[sk.Name]}
			}
			if err := tx.SetLevels(j.Handle, levels, now); err != nil {
				return err
			}
			initialised = append(initialised, j.Handle)
			if err := tx.Event(0, "dibs", "initial_levels", now, map[string]any{"junior": j.Handle, "levels": levels}); err != nil {
				return err
			}
		}
		return tx.SetMeta("initialized_at", now.Format(time.RFC3339))
	})
	if err != nil {
		return nil, err
	}
	r.Data = map[string]any{"labels": len(s.labels()), "initialised": initialised, "db": s.Cfg.DB}
	docs, err := s.mirrorDocs()
	if err != nil {
		return nil, err
	}
	r.Docs = docs
	return r, nil
}

// Reset wipes the ledger (for demo takes) and re-initialises.
func (s *Service) Reset() (*Result, error) {
	if err := s.tx(func(tx *ledger.Tx) error { return tx.Reset() }); err != nil {
		return nil, err
	}
	r, err := s.Init()
	if err != nil {
		return nil, err
	}
	r.Action = "reset"
	return r, nil
}

func parseNum(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	if err != nil || n <= 0 {
		return 0, refuse("bad_number", "%q is not an issue number", s)
	}
	return n, nil
}

// ParseNum accepts "3" or "#3".
func ParseNum(s string) (int, error) { return parseNum(s) }

// IsRule reports whether err is a rule refusal.
func IsRule(err error) (*RuleError, bool) {
	var re *RuleError
	ok := errors.As(err, &re)
	return re, ok
}
