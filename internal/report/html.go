package report

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/vivekyarra/dibs/internal/engine"
)

//go:embed dashboard.html.tmpl
var dashboardSrc string

var dashboardTmpl = template.Must(template.New("dashboard").Parse(dashboardSrc))

type kpi struct {
	Label, Value, Unit, Sub string
	Delta, DeltaClass       string
	Help                    string
}

type change struct{ Skill, From, To string }

type repRow struct {
	Week, WeekShort string
	Number          int
	URL             string
	Title           string
	Hint            int
	HintName        string
	Rounds          int
	Released        bool
	Reason          string
	Sample          bool
	Changes         []change
}

type juniorData struct {
	Handle, ID     string
	Summary        string
	Empty          bool
	KPIs           []kpi
	HintChart      template.HTML
	MinutesChart   template.HTML
	Heatmap        template.HTML
	HeatmapCaption string
	Rows           []repRow
	AnySample      bool
}

type pageData struct {
	Team, Repo, Generated, GeneratedISO string
	Baseline                            string
	AnySample                           bool
	Juniors                             []juniorData
	Levels                              []levelInfo
	Hints                               []string
	Logo                                template.HTML
}

type levelInfo struct {
	Class, Name, Abbr, Meaning string
}

const logoSVG = `<svg class="mark" viewBox="0 0 32 32" aria-hidden="true" focusable="false" xmlns="http://www.w3.org/2000/svg"><rect width="32" height="32" rx="9" class="mark-bg"/><path class="mark-fg" d="M10 8h6.5a8 8 0 0 1 0 16H10z" fill="none" stroke-width="3.2" stroke-linejoin="round"/><circle class="mark-dot" cx="16.5" cy="16" r="2.6"/></svg>`

// HTML renders the self-contained dashboard page.
func HTML(in Input) ([]byte, error) {
	base := in.BaselineMinutes
	if base <= 0 {
		base = 45
	}
	gen := in.Generated
	if gen.IsZero() {
		gen = time.Now()
	}
	p := pageData{
		Team:         in.Team,
		Repo:         in.Repo,
		Generated:    gen.UTC().Format("Mon 2 Jan 2006, 15:04 UTC"),
		GeneratedISO: gen.UTC().Format(time.RFC3339),
		Baseline:     fnum(base),
		Logo:         template.HTML(logoSVG), // #nosec G203 -- constant SVG from this package
		Levels: []levelInfo{
			{"h0", "Unseen", "U", "never attempted"},
			{"h1", "Exposed", "E", "started a rep"},
			{"h2", "Guided", "G", "merged with help"},
			{"h3", "Independent", "I", fmt.Sprintf("%d clean reps", engine.CleanThreshold)},
			{"h4", "Fluent", "F", fmt.Sprintf("%d more clean reps", engine.CleanThreshold)},
		},
	}
	for l := 0; l <= engine.MaxHint; l++ {
		p.Hints = append(p.Hints, hintLabel(l))
	}
	ids := map[string]int{}
	for _, j := range in.Juniors {
		jd := buildJunior(j, base, ids)
		p.AnySample = p.AnySample || jd.AnySample
		p.Juniors = append(p.Juniors, jd)
	}
	var buf bytes.Buffer
	if err := dashboardTmpl.Execute(&buf, p); err != nil {
		return nil, fmt.Errorf("render dashboard: %w", err)
	}
	return buf.Bytes(), nil
}

var nonID = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string, used map[string]int) string {
	id := strings.Trim(nonID.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if id == "" {
		id = "junior"
	}
	id = "j-" + id
	used[id]++
	if n := used[id]; n > 1 {
		id = fmt.Sprintf("%s-%d", id, n)
	}
	return id
}

func buildJunior(j JuniorView, base float64, ids map[string]int) juniorData {
	jd := juniorData{Handle: j.Handle, ID: slug(j.Handle, ids)}
	var reps []engine.Rep
	merged, released := 0, 0
	var saved, seniorTotal float64
	for _, w := range j.Weeks {
		if w.Sample {
			jd.AnySample = true
		}
		reps = append(reps, w.Reps...)
		merged += w.Merged
		released += w.Releases
		saved += w.SavedMinutes
		seniorTotal += w.SeniorMinutes
	}
	jd.Empty = len(reps) == 0
	jd.Summary = fmt.Sprintf("%d %s · %d %s · %d %s",
		len(j.Weeks), plural(len(j.Weeks), "week", "weeks"),
		merged, plural(merged, "rep merged", "reps merged"),
		released, plural(released, "release", "releases"))

	// Heatmap: every skill × every week, carrying the last known level forward.
	skills := allSkills(j.Levels, j.SkillOrder)
	for _, h := range j.History {
		if !contains(skills, h.Skill) {
			skills = append(skills, h.Skill)
		}
	}
	if len(skills) > 0 {
		var cols []string
		var sim []bool
		hist := map[string]engine.Level{}
		for _, h := range j.History {
			hist[h.Week+"\x00"+h.Skill] = h.Level
		}
		for _, w := range j.Weeks {
			cols = append(cols, w.Label)
			sim = append(sim, w.Sample)
		}
		var at func(string, int) engine.Level
		if len(cols) == 0 {
			cols = []string{"Now"}
			sim = []bool{false}
			at = func(s string, _ int) engine.Level { return j.Levels[s].Level }
			jd.HeatmapCaption = "Current level of each skill. Weekly history appears once reps are merged."
		} else {
			grid := map[string][]engine.Level{}
			for _, s := range skills {
				row := make([]engine.Level, len(cols))
				last := engine.Unseen
				for c, col := range cols {
					if l, ok := hist[col+"\x00"+s]; ok {
						last = l
					}
					row[c] = last
				}
				grid[s] = row
			}
			at = func(s string, c int) engine.Level { return grid[s][c] }
			jd.HeatmapCaption = "Level of each skill at the end of each week. Hover a cell for its level."
		}
		jd.Heatmap = template.HTML(heatmap(jd.ID+"-heat", skills, cols, sim, at)) // #nosec G203 -- SVG built here; every text node is escaped
	}
	if jd.Empty {
		return jd
	}

	jd.HintChart = template.HTML(hintChart(jd.ID+"-hint", j.Weeks))            // #nosec G203 -- SVG built here; every text node is escaped
	jd.MinutesChart = template.HTML(minutesChart(jd.ID+"-min", j.Weeks, base)) // #nosec G203 -- SVG built here; every text node is escaped

	// KPI tiles.
	jd.KPIs = append(jd.KPIs, kpi{
		Label: "Reps merged", Value: fmt.Sprint(merged),
		Sub:  fmt.Sprintf("%d %s to builder", released, plural(released, "release", "releases")),
		Help: "Reserved tickets the junior merged themselves.",
	})
	cur, first, hasCur, hasFirst := rollingWeeks(j.Weeks)
	hk := kpi{Label: "Avg hint depth", Value: "—", Help: "Mean of the deepest hint used per merged rep over the last two weeks with reps. Lower means more independent."}
	ik := kpi{Label: "Independence", Value: "—", Help: "Share of merged reps over the last two weeks that needed at most an L1 nudge."}
	if hasCur {
		hk.Value = fmt.Sprintf("L%.1f", cur.AvgHint)
		hk.Sub = cur.Label + simSuffix(cur.Sample)
		ik.Value = fmt.Sprint(pct(cur.IndependenceRatio))
		ik.Unit = "%"
		ik.Sub = fmt.Sprintf("of %d merged · %s", cur.Merged, cur.Label)
		if hasFirst {
			hk.Delta, hk.DeltaClass = deltaText(cur.AvgHint-first.AvgHint, false, "L%.1f", shortWeek(first.Label)+" (start)")
			ik.Delta, ik.DeltaClass = deltaText(float64(pct(cur.IndependenceRatio)-pct(first.IndependenceRatio)), true, "%.0f pts", shortWeek(first.Label)+" (start)")
		}
	}
	jd.KPIs = append(jd.KPIs, hk, ik)
	jd.KPIs = append(jd.KPIs, kpi{
		Label: "Senior time saved", Value: fnum(math.Round(saved)), Unit: "min",
		Sub:  fmt.Sprintf("estimate · %s min spent vs %s-min/PR baseline", fnum(math.Round(seniorTotal)), fnum(base)),
		Help: "Estimate: baseline minus (1 per approval + 2 per senior comment + 5 per teaching moment), summed over merged reps.",
	})
	fu := kpi{Label: "First unassisted merge", Value: "Not yet", Sub: "no merged rep without hints so far",
		Help: "Time from the junior's first reserved rep to their first merge with no hints at all."}
	if j.FirstUnassisted != nil {
		start := j.FirstUnassisted.StartedAt
		for _, r := range reps {
			if !r.StartedAt.IsZero() && (start.IsZero() || r.StartedAt.Before(start)) {
				start = r.StartedAt
			}
		}
		fu.Sub = fmt.Sprintf("#%d · zero hints", j.FirstUnassisted.Number)
		if !start.IsZero() && !j.FirstUnassisted.FinishedAt.IsZero() {
			d := j.FirstUnassisted.FinishedAt.Sub(start)
			if d < 24*time.Hour {
				fu.Value, fu.Unit = fmt.Sprintf("%.0f", math.Max(1, d.Hours())), "hours"
			} else {
				days := d.Hours() / 24
				fu.Value, fu.Unit = fnum(math.Round(days*10)/10), plural(int(math.Round(days)), "day", "days")
			}
		} else {
			fu.Value = "Done"
		}
	}
	jd.KPIs = append(jd.KPIs, fu)

	// Reps table, newest first.
	for i := len(j.Weeks) - 1; i >= 0; i-- {
		w := j.Weeks[i]
		for k := len(w.Reps) - 1; k >= 0; k-- {
			r := w.Reps[k]
			row := repRow{
				Week: w.Label, WeekShort: shortWeek(w.Label), Number: r.Number, URL: r.PRURL,
				Title: r.Title, Hint: r.MaxHint, HintName: hintName(r.MaxHint), Rounds: r.ReviewRounds,
				Released: r.Released, Reason: r.ReleasedReason, Sample: r.Sample || w.Sample,
			}
			for _, c := range r.SkillChanges {
				if c.From != c.To {
					row.Changes = append(row.Changes, change{c.Skill, c.From.String(), c.To.String()})
				}
			}
			jd.Rows = append(jd.Rows, row)
		}
	}
	return jd
}

func hintName(l int) string {
	if l <= 0 {
		return "none"
	}
	if n, ok := engine.HintNames[l]; ok {
		return n
	}
	return "?"
}

// measuredWeeks returns the latest week with merged reps and the one before it.
func measuredWeeks(weeks []engine.Week) (latest, prev engine.Week, hasLatest, hasPrev bool) {
	for i := len(weeks) - 1; i >= 0; i-- {
		if weeks[i].Merged == 0 {
			continue
		}
		if !hasLatest {
			latest, hasLatest = weeks[i], true
		} else {
			return latest, weeks[i], true, true
		}
	}
	return
}

// deltaText formats a change against last week; upGood says which way is good.
func deltaText(d float64, upGood bool, format, vs string) (string, string) {
	if math.Abs(d) < 0.05 {
		return "no change vs " + vs, "flat"
	}
	sign, cls := "▲ ", "bad"
	if d < 0 {
		sign = "▼ "
	}
	if (d > 0) == upGood {
		cls = "good"
	}
	return sign + fmt.Sprintf(format, math.Abs(d)) + " vs " + vs, cls
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// rollingWeeks merges the last two weeks that have merged reps into one
// "current" window, and returns the first week with merged reps as the start
// of the ramp. Two weeks smooth out a week where the junior opened a new skill.
func rollingWeeks(weeks []engine.Week) (cur, first engine.Week, hasCur, hasFirst bool) {
	var measured []engine.Week
	for _, w := range weeks {
		if w.Merged > 0 {
			measured = append(measured, w)
		}
	}
	if len(measured) == 0 {
		return
	}
	window := measured
	if len(window) > 2 {
		window = window[len(window)-2:]
	}
	var hints, clean float64
	cur.Sample = true
	for _, w := range window {
		cur.Merged += w.Merged
		hints += w.AvgHint * float64(w.Merged)
		clean += w.IndependenceRatio * float64(w.Merged)
		cur.Sample = cur.Sample && w.Sample
	}
	cur.AvgHint = math.Round(hints/float64(cur.Merged)*10) / 10
	cur.IndependenceRatio = clean / float64(cur.Merged)
	if len(window) == 1 {
		cur.Label = shortWeek(window[0].Label)
	} else {
		cur.Label = "last 2 weeks"
	}
	hasCur = true
	if len(measured) > len(window) || len(window) == 2 {
		first, hasFirst = measured[0], true
	}
	return
}
