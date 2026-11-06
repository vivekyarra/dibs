package engine

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Rep is one finished (merged or released) learning rep, as metrics see it.
type Rep struct {
	Number          int           `json:"number"`
	Title           string        `json:"title"`
	Junior          string        `json:"junior"`
	PRURL           string        `json:"pr_url,omitempty"`
	MaxHint         int           `json:"max_hint"`
	ReviewRounds    int           `json:"review_rounds"`
	Attempts        int           `json:"attempts"`
	StartedAt       time.Time     `json:"started_at"`
	FinishedAt      time.Time     `json:"finished_at"`
	Released        bool          `json:"released"`
	ReleasedReason  string        `json:"released_reason,omitempty"`
	Approvals       int           `json:"approvals"`
	SeniorComments  int           `json:"senior_comments"`
	TeachingMoments int           `json:"teaching_moments"`
	SkillChanges    []SkillChange `json:"skill_changes"`
	Sample          bool          `json:"sample,omitempty"`
}

// SeniorMinutes is the estimated senior time a rep cost: 1 min per approval,
// 2 per review comment, 5 per teaching moment. It is an estimate and is
// always labelled as one.
func (r Rep) SeniorMinutes() float64 {
	return float64(r.Approvals) + 2*float64(r.SeniorComments) + 5*float64(r.TeachingMoments)
}

// CycleHours is the time from start to finish.
func (r Rep) CycleHours() float64 {
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt).Hours()
}

// Week is a report for one junior over one ISO week.
type Week struct {
	Label             string        `json:"week"`
	Junior            string        `json:"junior"`
	Reps              []Rep         `json:"reps"`
	Merged            int           `json:"merged"`
	Releases          int           `json:"releases"`
	AvgHint           float64       `json:"avg_hint"`
	IndependenceRatio float64       `json:"independence_ratio"`
	SeniorMinutes     float64       `json:"senior_minutes"`
	SeniorPerRep      float64       `json:"senior_minutes_per_rep"`
	SavedMinutes      float64       `json:"saved_minutes_estimate"`
	SkillChanges      []SkillChange `json:"skill_changes"`
	MedianCycleHours  float64       `json:"median_cycle_hours"`
	Sample            bool          `json:"sample"`
}

// ISOWeek returns the "2026-W45" label for t.
func ISOWeek(t time.Time) string {
	y, w := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// Weekly groups reps by junior and ISO week of finishing, oldest first.
func Weekly(reps []Rep, baselineMinutes float64) []Week {
	type key struct{ junior, week string }
	groups := map[key][]Rep{}
	var keys []key
	for _, r := range reps {
		k := key{r.Junior, ISOWeek(r.FinishedAt)}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], r)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].junior != keys[j].junior {
			return keys[i].junior < keys[j].junior
		}
		return keys[i].week < keys[j].week
	})
	out := make([]Week, 0, len(keys))
	for _, k := range keys {
		out = append(out, Summarize(k.junior, k.week, groups[k], baselineMinutes))
	}
	return out
}

// Summarize computes the week's numbers from its reps.
func Summarize(junior, label string, reps []Rep, baselineMinutes float64) Week {
	w := Week{Label: label, Junior: junior, Reps: reps, Sample: len(reps) > 0}
	var hintSum float64
	var clean int
	var cycles []float64
	for _, r := range reps {
		if !r.Sample {
			w.Sample = false
		}
		w.SeniorMinutes += r.SeniorMinutes()
		w.SkillChanges = append(w.SkillChanges, r.SkillChanges...)
		if r.Released {
			w.Releases++
			continue
		}
		w.Merged++
		hintSum += float64(r.MaxHint)
		if r.MaxHint <= 1 {
			clean++
		}
		if c := r.CycleHours(); c > 0 {
			cycles = append(cycles, c)
		}
		w.SavedMinutes += baselineMinutes - r.SeniorMinutes()
	}
	if w.Merged > 0 {
		w.AvgHint = round2(hintSum / float64(w.Merged))
		w.IndependenceRatio = round2(float64(clean) / float64(w.Merged))
		w.SeniorPerRep = round2(w.SeniorMinutes / float64(len(reps)))
	}
	w.SavedMinutes = math.Max(0, round2(w.SavedMinutes))
	w.MedianCycleHours = round2(median(cycles))
	return w
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

// FirstUnassisted returns the first merged rep done with no hints at all.
func FirstUnassisted(reps []Rep) (Rep, bool) {
	var best Rep
	found := false
	for _, r := range reps {
		if !r.Released && r.MaxHint == 0 && (!found || r.FinishedAt.Before(best.FinishedAt)) {
			best, found = r, true
		}
	}
	return best, found
}
