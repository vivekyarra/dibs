package report

import (
	"fmt"
	"html"
	"math"
	"strings"

	"github.com/vivekyarra/dibs/internal/engine"
)

// Charts are drawn server-side as inline SVG. Every piece of user text goes
// through esc; colors come from CSS classes so both themes apply.

func esc(s string) string { return html.EscapeString(s) }

// shortWeek turns "2026-W45" into "W45".
func shortWeek(label string) string {
	if i := strings.LastIndex(label, "-"); i >= 0 && i < len(label)-1 {
		return label[i+1:]
	}
	return label
}

const (
	chW, chH                          = 560, 270
	chL, chR, chT, chB                = 92, 18, 30, 46
	plotW, plotH                      = chW - chL - chR, chH - chT - chB
	hmLabelW, hmCellW, hmRowH         = 150, 58, 34
	hmHeadH                           = 30
	hmCellPad                         = 3
	maxHintLevel              float64 = 4
)

func bandX(i, n int) float64 {
	return chL + (float64(i)+0.5)*float64(plotW)/float64(n)
}

func hintLabel(l int) string {
	if l == 0 {
		return "L0 · none"
	}
	return fmt.Sprintf("L%d · %s", l, engine.HintNames[l])
}

// hintChart: average max hint depth per week, L0 at the bottom, L4 at the top.
func hintChart(id string, weeks []engine.Week) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-labelledby="%s-t %s-d" xmlns="http://www.w3.org/2000/svg">`, chW, chH, id, id)
	fmt.Fprintf(&b, `<title id="%s-t">Average hint depth by week</title>`, id)
	b.WriteString(fmt.Sprintf(`<desc id="%s-d">`, id) + esc(hintDesc(weeks)) + `</desc>`)
	y := func(v float64) float64 { return chT + float64(plotH)*(1-v/maxHintLevel) }
	for l := 0; l <= 4; l++ {
		yy := y(float64(l))
		fmt.Fprintf(&b, `<line class="grid" x1="%d" x2="%d" y1="%.1f" y2="%.1f"/>`, chL, chW-chR, yy, yy)
		fmt.Fprintf(&b, `<text class="axis" x="%d" y="%.1f" text-anchor="end" dominant-baseline="middle">%s</text>`, chL-10, yy, esc(hintLabel(l)))
	}
	fmt.Fprintf(&b, `<text class="note" x="%d" y="16" text-anchor="end">lower is better ↓</text>`, chW-chR)
	n := len(weeks)
	// Line segments across consecutive measured weeks; gaps break the line.
	var seg []string
	flush := func() {
		if len(seg) > 1 {
			fmt.Fprintf(&b, `<polyline class="line" points="%s"/>`, strings.Join(seg, " "))
		}
		seg = seg[:0]
	}
	for i, w := range weeks {
		if w.Merged == 0 {
			flush()
			continue
		}
		seg = append(seg, fmt.Sprintf("%.1f,%.1f", bandX(i, n), y(w.AvgHint)))
	}
	flush()
	for i, w := range weeks {
		x := bandX(i, n)
		fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%d" text-anchor="middle">%s</text>`, x, chH-chB+20, esc(shortWeek(w.Label)))
		if w.Sample {
			fmt.Fprintf(&b, `<text class="axis sim-t" x="%.1f" y="%d" text-anchor="middle">sample</text>`, x, chH-chB+34)
		}
		if w.Merged == 0 {
			fmt.Fprintf(&b, `<text class="note" x="%.1f" y="%.1f" text-anchor="middle">no reps</text>`, x, y(0)-8)
			continue
		}
		cls := "pt"
		if w.Sample {
			cls = "pt sim"
		}
		yy := y(w.AvgHint)
		fmt.Fprintf(&b, `<g><title>%s: avg L%.1f over %d merged %s%s</title>`, esc(w.Label), w.AvgHint, w.Merged, plural(w.Merged, "rep", "reps"), simSuffix(w.Sample))
		fmt.Fprintf(&b, `<circle class="%s" cx="%.1f" cy="%.1f" r="6"/>`, cls, x, yy)
		ly := yy - 13
		if ly < chT-4 {
			ly = yy + 22
		}
		fmt.Fprintf(&b, `<text class="val" x="%.1f" y="%.1f" text-anchor="middle">L%.1f</text></g>`, x, ly, w.AvgHint)
	}
	b.WriteString(`</svg>`)
	return b.String()
}

func simSuffix(sim bool) string {
	if sim {
		return " (sample)"
	}
	return ""
}

func hintDesc(weeks []engine.Week) string {
	var parts []string
	for _, w := range weeks {
		if w.Merged > 0 {
			parts = append(parts, fmt.Sprintf("%s L%.1f", w.Label, w.AvgHint))
		} else {
			parts = append(parts, w.Label+" no merged reps")
		}
	}
	return "Average maximum hint level per merged rep, by week: " + strings.Join(parts, ", ") + "."
}

// niceCeil rounds up to a round number that splits evenly into 4 ticks.
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 10
	}
	p := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10} {
		if m*p >= v {
			return m * p
		}
	}
	return 10 * p
}

// minutesChart: senior minutes per rep, by week, against the baseline.
func minutesChart(id string, weeks []engine.Week, baseline float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-labelledby="%s-t %s-d" xmlns="http://www.w3.org/2000/svg">`, chW, chH, id, id)
	fmt.Fprintf(&b, `<title id="%s-t">Senior minutes per rep by week, against a %s-minute baseline (estimate)</title>`, id, fnum(baseline))
	var parts []string
	maxV := baseline
	for _, w := range weeks {
		parts = append(parts, fmt.Sprintf("%s %s min", w.Label, fnum(w.SeniorPerRep)))
		maxV = math.Max(maxV, w.SeniorPerRep)
	}
	fmt.Fprintf(&b, `<desc id="%s-d">%s</desc>`, id, esc("Estimated senior minutes per rep: "+strings.Join(parts, ", ")+"."))
	top := niceCeil(maxV * 1.12)
	y := func(v float64) float64 { return chT + float64(plotH)*(1-v/top) }
	for i := 0; i <= 4; i++ {
		v := top * float64(i) / 4
		yy := y(v)
		fmt.Fprintf(&b, `<line class="grid" x1="%d" x2="%d" y1="%.1f" y2="%.1f"/>`, chL, chW-chR, yy, yy)
		fmt.Fprintf(&b, `<text class="axis" x="%d" y="%.1f" text-anchor="end" dominant-baseline="middle">%s min</text>`, chL-10, yy, fnum(v))
	}
	n := len(weeks)
	bw := math.Min(56, float64(plotW)/float64(max(n, 1))*0.56)
	for i, w := range weeks {
		x := bandX(i, n)
		fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%d" text-anchor="middle">%s</text>`, x, chH-chB+20, esc(shortWeek(w.Label)))
		if w.Sample {
			fmt.Fprintf(&b, `<text class="axis sim-t" x="%.1f" y="%d" text-anchor="middle">sample</text>`, x, chH-chB+34)
		}
		if len(w.Reps) == 0 {
			continue
		}
		cls := "bar"
		if w.Sample {
			cls = "bar sim"
		}
		yy := y(w.SeniorPerRep)
		h := y(0) - yy
		fmt.Fprintf(&b, `<g><title>%s: %s senior min per rep, %s min total over %d %s (estimate)%s</title>`,
			esc(w.Label), fnum(w.SeniorPerRep), fnum(w.SeniorMinutes), len(w.Reps), plural(len(w.Reps), "rep", "reps"), simSuffix(w.Sample))
		fmt.Fprintf(&b, `<rect class="%s" x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="5"/>`, cls, x-bw/2, yy, bw, h)
		fmt.Fprintf(&b, `<text class="val" x="%.1f" y="%.1f" text-anchor="middle">%s</text></g>`, x, yy-7, fnum(w.SeniorPerRep))
	}
	by := y(baseline)
	fmt.Fprintf(&b, `<line class="base" x1="%d" x2="%d" y1="%.1f" y2="%.1f"/>`, chL, chW-chR, by, by)
	fmt.Fprintf(&b, `<text class="base-t" x="%d" y="%.1f" text-anchor="end">baseline %s min/PR (team estimate)</text>`, chW-chR, by-7, fnum(baseline))
	b.WriteString(`</svg>`)
	return b.String()
}

var levelAbbr = [...]string{"U", "E", "G", "I", "F"}

func lvl(l engine.Level) int {
	if l < engine.Unseen {
		return 0
	}
	if l > engine.Fluent {
		return 4
	}
	return int(l)
}

// heatmap: skills × weeks, colored by level at the end of each week.
// cols are week labels; at(skill, col) gives the level.
func heatmap(id string, skills, cols []string, sim []bool, at func(skill string, col int) engine.Level) string {
	w := hmLabelW + len(cols)*hmCellW
	h := hmHeadH + len(skills)*hmRowH
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="heat" viewBox="0 0 %d %d" style="max-width:%dpx" role="img" aria-labelledby="%s-t" xmlns="http://www.w3.org/2000/svg">`, w, h, w+80, id)
	fmt.Fprintf(&b, `<title id="%s-t">Skill levels by week: %d skills across %d %s</title>`, id, len(skills), len(cols), plural(len(cols), "column", "columns"))
	for c, col := range cols {
		x := hmLabelW + c*hmCellW + hmCellW/2
		cls := "axis"
		if c < len(sim) && sim[c] {
			cls = "axis sim-t"
		}
		fmt.Fprintf(&b, `<text class="%s" x="%d" y="%d" text-anchor="middle"><title>%s%s</title>%s</text>`,
			cls, x, hmHeadH-11, esc(col), simSuffix(c < len(sim) && sim[c]), esc(shortWeek(col)))
	}
	for r, s := range skills {
		y := hmHeadH + r*hmRowH
		fmt.Fprintf(&b, `<text class="skill" x="%d" y="%d" text-anchor="end" dominant-baseline="middle"><title>%s</title>%s</text>`,
			hmLabelW-12, y+hmRowH/2, esc(s), esc(clip(s, 18)))
		for c, col := range cols {
			l := lvl(at(s, c))
			x := hmLabelW + c*hmCellW
			fmt.Fprintf(&b, `<g><title>%s · %s: %s</title>`, esc(s), esc(col), engine.Level(l))
			fmt.Fprintf(&b, `<rect class="h%d" x="%d" y="%d" width="%d" height="%d" rx="6"/>`,
				l, x+hmCellPad, y+hmCellPad, hmCellW-2*hmCellPad, hmRowH-2*hmCellPad)
			fmt.Fprintf(&b, `<text class="t%d" x="%d" y="%d" text-anchor="middle" dominant-baseline="central">%s</text></g>`,
				l, x+hmCellW/2, y+hmRowH/2+1, levelAbbr[l])
		}
	}
	b.WriteString(`</svg>`)
	return b.String()
}
