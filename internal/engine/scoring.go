package engine

import (
	"math"
	"sort"
	"strings"
	"time"
)

// Policy is the team's tunable learning budget and safety rules.
type Policy struct {
	RiskPaths              []string `json:"risk_paths" yaml:"risk_paths"`
	MaxActivePerJunior     int      `json:"max_active_per_junior" yaml:"max_active_per_junior"`
	MaxBacklogShare        float64  `json:"max_backlog_share" yaml:"max_backlog_share"`
	ScoreThreshold         float64  `json:"score_threshold" yaml:"score_threshold"`
	ProposalTimeoutHours   float64  `json:"proposal_timeout_hours" yaml:"proposal_timeout_hours"`
	HoldMaxHours           float64  `json:"hold_max_hours" yaml:"hold_max_hours"`
	ReleaseStuckMultiplier float64  `json:"release_stuck_multiplier" yaml:"release_stuck_multiplier"`
	StuckAnswerMinutes     float64  `json:"stuck_answer_minutes" yaml:"stuck_answer_minutes"`
	SlackSafety            float64  `json:"slack_safety" yaml:"slack_safety"`
	DefaultDueDays         float64  `json:"default_due_days" yaml:"default_due_days"`
	BaselineSeniorMinutes  float64  `json:"baseline_senior_minutes" yaml:"baseline_senior_minutes"`
	MaxTeachingPerPR       int      `json:"max_teaching_per_pr" yaml:"max_teaching_per_pr"`
	AutoDelegate           bool     `json:"auto_delegate" yaml:"auto_delegate"`
}

// DefaultPolicy is the policy used when .dibs/dibs.yaml leaves a field out.
func DefaultPolicy() Policy {
	return Policy{
		RiskPaths:              nil,
		MaxActivePerJunior:     1,
		MaxBacklogShare:        0.20,
		ScoreThreshold:         3,
		ProposalTimeoutHours:   24,
		HoldMaxHours:           48,
		ReleaseStuckMultiplier: 2.0,
		StuckAnswerMinutes:     30,
		SlackSafety:            1.5,
		DefaultDueDays:         7,
		BaselineSeniorMinutes:  45,
		MaxTeachingPerPR:       2,
		AutoDelegate:           true,
	}
}

// Reason is why a ticket was or was not proposed as a rep.
type Reason string

const (
	ReasonProposed Reason = "proposed"
	ReasonTooFar   Reason = "too_far"
	ReasonTooEasy  Reason = "too_easy"
	ReasonRisky    Reason = "risky"
	ReasonNoSlack  Reason = "no_slack"
	ReasonNoBudget Reason = "no_budget"
	ReasonLowScore Reason = "below_threshold"
	ReasonNotTop   Reason = "not_top_pick"
	ReasonNoSkills Reason = "untagged"
)

// Transient reasons keep a ticket eligible for a later sweep; the rest send it to the builder.
func (r Reason) Transient() bool {
	return r == ReasonNoBudget || r == ReasonNotTop
}

// Explain is a short human sentence for a reason.
func (r Reason) Explain() string {
	switch r {
	case ReasonTooFar:
		return "too far: more than one unseen skill"
	case ReasonTooEasy:
		return "too easy: every skill already Independent"
	case ReasonRisky:
		return "risky: touches a high blast-radius path"
	case ReasonNoSlack:
		return "no slack: deadline too close for a learning rep"
	case ReasonNoBudget:
		return "held: learning budget is full"
	case ReasonLowScore:
		return "little to learn here"
	case ReasonNotTop:
		return "held: a better rep was picked this sweep"
	case ReasonNoSkills:
		return "not tagged with any skill"
	}
	return string(r)
}

// Candidate is everything scoring needs to know about one ticket.
type Candidate struct {
	Number         int
	Skills         []SkillReq
	TouchedPaths   []string
	EstSeniorHours float64
	DueAt          time.Time
}

// Budget describes the current load on the learning budget.
type Budget struct {
	ActiveForJunior int // reps this junior holds now (proposed, reserved, in progress, in review)
	ReservedOpen    int // reps held across the team now
	OpenTotal       int // all open tickets
}

// Breakdown is the transparent arithmetic behind a decision.
type Breakdown struct {
	Growth          float64            `json:"growth"`
	SlackBonus      float64            `json:"slack_bonus"`
	UnseenPenalty   float64            `json:"unseen_penalty"`
	Score           float64            `json:"score"`
	EstJuniorHours  float64            `json:"est_junior_hours"`
	SlackDays       float64            `json:"slack_days"`
	Levels          map[string]Level   `json:"levels"`
	GrowthBySkill   map[string]float64 `json:"growth_by_skill"`
	RiskyPath       string             `json:"risky_path,omitempty"`
	BacklogShareNew float64            `json:"backlog_share_if_reserved"`
}

// Decision is the outcome of scoring one ticket for one junior.
type Decision struct {
	Number    int       `json:"number"`
	Reason    Reason    `json:"reason"`
	Breakdown Breakdown `json:"breakdown"`
}

// EstJuniorHours scales an experienced engineer's estimate by how new the skills are.
// All Independent+ → ×1.0; all Unseen → ×3.25.
func EstJuniorHours(estSenior float64, reqs []SkillReq, levels map[string]SkillState) float64 {
	if len(reqs) == 0 || estSenior <= 0 {
		return math.Max(estSenior, 0)
	}
	var gap float64
	for _, r := range reqs {
		l := levels[r.Name].Level
		if l > Independent {
			l = Independent
		}
		gap += float64(Independent - l)
	}
	return estSenior * (1 + 0.75*gap/float64(len(reqs)))
}

func growth(l Level) float64 {
	switch l {
	case Exposed, Guided:
		return 1
	case Unseen:
		return 0.5
	}
	return 0
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// Score evaluates one ticket for one junior. Hard filters run first, in a fixed
// order, so the reason a human sees is always the same for the same facts.
func Score(c Candidate, levels map[string]SkillState, b Budget, p Policy, now time.Time) Decision {
	d := Decision{Number: c.Number}
	if len(c.Skills) > 0 && (math.IsNaN(c.EstSeniorHours) || math.IsInf(c.EstSeniorHours, 0)) {
		d.Breakdown = Breakdown{Levels: map[string]Level{}, GrowthBySkill: map[string]float64{}}
		d.Reason = ReasonNoSlack
		return d
	}
	bd := Breakdown{Levels: map[string]Level{}, GrowthBySkill: map[string]float64{}}
	for _, r := range c.Skills {
		bd.Levels[r.Name] = levels[r.Name].Level
	}
	bd.EstJuniorHours = round2(EstJuniorHours(c.EstSeniorHours, c.Skills, levels))
	hoursLeft := c.DueAt.Sub(now).Hours()
	bd.SlackDays = round2((hoursLeft - bd.EstJuniorHours*p.SlackSafety) / 24)
	if b.OpenTotal > 0 {
		bd.BacklogShareNew = round2(float64(b.ReservedOpen+1) / float64(b.OpenTotal))
	}
	d.Breakdown = bd

	if len(c.Skills) == 0 {
		d.Reason = ReasonNoSkills
		return d
	}
	var unseenPrimary, unseenAll, independentAll int
	for _, r := range c.Skills {
		l := levels[r.Name].Level
		if l == Unseen {
			unseenAll++
			if r.Weight == Primary {
				unseenPrimary++
			}
		}
		if l >= Independent {
			independentAll++
		}
	}
	switch {
	case unseenPrimary > 1 || unseenAll > 2:
		d.Reason = ReasonTooFar
		return d
	case independentAll == len(c.Skills):
		d.Reason = ReasonTooEasy
		return d
	}
	for _, path := range c.TouchedPaths {
		for _, risk := range p.RiskPaths {
			if risk != "" && strings.HasPrefix(normPath(path), normPath(risk)) {
				d.Breakdown.RiskyPath = path
				d.Reason = ReasonRisky
				return d
			}
		}
	}
	if hoursLeft < bd.EstJuniorHours*p.SlackSafety {
		d.Reason = ReasonNoSlack
		return d
	}
	if b.ActiveForJunior >= p.MaxActivePerJunior ||
		(b.OpenTotal > 0 && float64(b.ReservedOpen+1)/float64(b.OpenTotal) > p.MaxBacklogShare+1e-9) {
		d.Reason = ReasonNoBudget
		return d
	}

	for _, r := range c.Skills {
		g := growth(levels[r.Name].Level)
		w := 1.0
		if r.Weight == Primary {
			w = 3
		}
		d.Breakdown.GrowthBySkill[r.Name] = w * g
		d.Breakdown.Growth += w * g
	}
	d.Breakdown.SlackBonus = round2(math.Min(1, math.Max(0, d.Breakdown.SlackDays/3)))
	d.Breakdown.UnseenPenalty = 0.5 * float64(unseenAll)
	d.Breakdown.Score = round2(d.Breakdown.Growth + d.Breakdown.SlackBonus - d.Breakdown.UnseenPenalty)
	if d.Breakdown.Score < p.ScoreThreshold {
		d.Reason = ReasonLowScore
		return d
	}
	d.Reason = ReasonProposed
	return d
}

func normPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	return strings.TrimPrefix(p, "./")
}

// PickTop keeps the single best proposal and marks the other eligible tickets
// as not the top pick. Ties go to the lower issue number (older work first).
func PickTop(ds []Decision) []Decision {
	best := -1
	var top Decision
	for i, d := range ds {
		if d.Reason != ReasonProposed {
			continue
		}
		if best < 0 || d.Breakdown.Score > top.Breakdown.Score ||
			(d.Breakdown.Score == top.Breakdown.Score && d.Number < top.Number) {
			best, top = i, d
		}
	}
	out := make([]Decision, len(ds))
	copy(out, ds)
	for i := range out {
		if out[i].Reason == ReasonProposed && i != best {
			out[i].Reason = ReasonNotTop
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}
