// Package report turns engine metrics into the things people read: the weekly
// ramp report posted in chat, the skill-map document kept in the Switch room,
// and the static HTML dashboard.
package report

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vivekyarra/dibs/internal/engine"
)

// LevelPoint is the level of one skill at the end of one week.
type LevelPoint struct {
	Week  string
	Skill string
	Level engine.Level
}

// JuniorView is everything the reports know about one junior.
type JuniorView struct {
	Handle          string
	Weeks           []engine.Week                // oldest first
	Levels          map[string]engine.SkillState // current
	SkillOrder      []string                     // catalog order
	History         []LevelPoint                 // level of each skill at end of each week (for heatmap)
	FirstUnassisted *engine.Rep
}

// Input is the whole dashboard.
type Input struct {
	Team, Repo      string
	Generated       time.Time
	BaselineMinutes float64
	Juniors         []JuniorView
}

// MaxChatChars is the hard ceiling for a chat report (Discord allows 2000).
const MaxChatChars = 1800

const (
	maxListed   = 6  // reps / skill moves listed by name in the chat report
	maxTitle    = 40 // runes of an issue title shown in chat
	maxRecent   = 10 // recent moves shown under the skill map
	cleanNeeded = engine.CleanThreshold
)

// WeekMarkdown renders the weekly ramp report for one junior (spec §18.1).
func WeekMarkdown(j JuniorView, w engine.Week, baseline float64) string {
	var lines []string
	sim := ""
	if w.Sample {
		sim = " (sample)"
	}
	lines = append(lines, fmt.Sprintf("📈 **Dibs ramp report** · %s · week %s%s",
		mdEscape(clip(j.Handle, 40)), mdEscape(w.Label), sim))

	// Reps.
	var merged, released []engine.Rep
	for _, r := range w.Reps {
		if r.Released {
			released = append(released, r)
		} else {
			merged = append(merged, r)
		}
	}
	if len(merged) == 0 {
		lines = append(lines, "**Reps:** none merged this week")
	} else {
		var names []string
		for i, r := range merged {
			if i == maxListed {
				names = append(names, fmt.Sprintf("+%d more", len(merged)-maxListed))
				break
			}
			names = append(names, fmt.Sprintf("#%d %s", r.Number, mdEscape(clip(r.Title, maxTitle))))
		}
		lines = append(lines, fmt.Sprintf("**Reps:** %d merged (%s)", len(merged), strings.Join(names, ", ")))
	}

	// Hint depth with trend.
	if len(merged) == 0 {
		lines = append(lines, "**Avg hint depth:** n/a (no merged reps)")
	} else {
		line := fmt.Sprintf("**Avg hint depth:** L%.1f", w.AvgHint)
		if prev, ok := previousWeek(j.Weeks, w.Label); ok && prev.Merged > 0 {
			line += fmt.Sprintf(" (last week L%.1f) %s", prev.AvgHint, arrow(w.AvgHint, prev.AvgHint))
		} else {
			line += " (first measured week)"
		}
		lines = append(lines, line)
	}

	// Independence.
	if len(merged) > 0 {
		clean := 0
		for _, r := range merged {
			if r.MaxHint <= 1 {
				clean++
			}
		}
		lines = append(lines, fmt.Sprintf("**Independence ratio:** %d%% (%d of %d with ≤ L1)",
			pct(float64(clean)/float64(len(merged))), clean, len(merged)))
	}

	// Skills moved, each with the PR that moved it.
	var moves []string
	total := 0
	for _, r := range w.Reps {
		for _, c := range r.SkillChanges {
			if c.From == c.To {
				continue
			}
			total++
			if len(moves) < maxListed {
				moves = append(moves, fmt.Sprintf("%s %s→%s (#%d)", mdEscape(clip(c.Skill, 30)), c.From, c.To, r.Number))
			}
		}
	}
	if total == 0 {
		lines = append(lines, "**Skills moved:** none this week")
	} else {
		if total > len(moves) {
			moves = append(moves, fmt.Sprintf("+%d more", total-len(moves)))
		}
		lines = append(lines, "**Skills moved:** "+strings.Join(moves, " · "))
	}

	// Senior time.
	lines = append(lines, fmt.Sprintf("**Senior time:** %s min total (≈ %s min saved vs %s-min/PR baseline, estimate)",
		fnum(w.SeniorMinutes), fnum(w.SavedMinutes), fnum(baseline)))

	// Releases / delivery.
	del := fmt.Sprintf("**Delivery:** %d %s", len(released), plural(len(released), "release", "releases"))
	if len(released) > 0 {
		var rs []string
		for i, r := range released {
			if i == 3 {
				rs = append(rs, fmt.Sprintf("+%d more", len(released)-3))
				break
			}
			s := fmt.Sprintf("#%d", r.Number)
			if r.ReleasedReason != "" {
				s += " " + mdEscape(clip(r.ReleasedReason, 40))
			}
			rs = append(rs, s)
		}
		del += " (" + strings.Join(rs, ", ") + ")"
	}
	if w.MedianCycleHours > 0 {
		del += fmt.Sprintf(" · median cycle time %.1f d", w.MedianCycleHours/24)
	}
	lines = append(lines, del)

	lines = append(lines, "**Next edge:** "+nextEdge(j.Levels, j.SkillOrder))

	return clipLines(lines, MaxChatChars)
}

// SkillMapMarkdown renders the skill-map document kept in the Switch room.
func SkillMapMarkdown(handle string, levels map[string]engine.SkillState, order []string, recent []engine.SkillChange) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🗺️ **Skill map · %s**\n\n", mdEscape(handle))
	skills := allSkills(levels, order)
	if len(skills) == 0 {
		b.WriteString("_No skills tracked yet._\n")
	} else {
		b.WriteString("| Skill | Level | Progress | Toward next |\n|---|---|---|---|\n")
		for _, s := range skills {
			st := levels[s]
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", cellEscape(s), st.Level, bar(st.Level), towardNext(st))
		}
	}
	if len(recent) > 0 {
		b.WriteString("\n**Recent moves**\n")
		start := 0
		if len(recent) > maxRecent {
			start = len(recent) - maxRecent
		}
		for _, c := range recent[start:] {
			fmt.Fprintf(&b, "- %s %s→%s", mdEscape(c.Skill), c.From, c.To)
			if c.Reason != "" {
				fmt.Fprintf(&b, " — %s", mdEscape(c.Reason))
			}
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "\n**Legend:** ▰ = one level earned, 4 cells = Fluent. "+
		"Unseen → Exposed (started a rep) → Guided (merged a rep) → Independent (%d clean reps) → Fluent (%d more). "+
		"Clean = max hint ≤ L1 and ≤ 1 review round. Only @dibs writes this map.\n", cleanNeeded, cleanNeeded)
	return b.String()
}

// --- helpers ---------------------------------------------------------------

func previousWeek(weeks []engine.Week, label string) (engine.Week, bool) {
	var best engine.Week
	found := false
	for _, w := range weeks {
		if w.Label < label && (!found || w.Label > best.Label) {
			best, found = w, true
		}
	}
	return best, found
}

func arrow(cur, prev float64) string {
	switch {
	case cur < prev:
		return "↓"
	case cur > prev:
		return "↑"
	}
	return "→"
}

// nextEdge picks the lowest skill that is in progress (Exposed or Guided),
// else the first Unseen skill in catalog order.
func nextEdge(levels map[string]engine.SkillState, order []string) string {
	skills := allSkills(levels, order)
	best := ""
	for _, s := range skills {
		l := levels[s].Level
		if (l == engine.Exposed || l == engine.Guided) && (best == "" || l < levels[best].Level) {
			best = s
		}
	}
	if best != "" {
		st := levels[best]
		if st.Level == engine.Guided {
			return fmt.Sprintf("%s (Guided, %d/%d clean reps) — the next clean rep moves it.", mdEscape(best), st.CleanSinceLevel, cleanNeeded)
		}
		return fmt.Sprintf("%s (Exposed) — one merged rep makes it Guided.", mdEscape(best))
	}
	for _, s := range skills {
		if levels[s].Level == engine.Unseen {
			return fmt.Sprintf("%s (Unseen) — will look for a low-risk ticket.", mdEscape(s))
		}
	}
	if len(skills) == 0 {
		return "no skills tracked yet."
	}
	return "every tracked skill is Independent or better — time for a stretch ticket."
}

// allSkills is catalog order, then any extra skills alphabetically.
func allSkills(levels map[string]engine.SkillState, order []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range order {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	var extra []string
	for s := range levels {
		if !seen[s] {
			extra = append(extra, s)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

func bar(l engine.Level) string {
	n := int(l)
	if n < 0 {
		n = 0
	}
	if n > 4 {
		n = 4
	}
	return strings.Repeat("▰", n) + strings.Repeat("▱", 4-n)
}

func towardNext(st engine.SkillState) string {
	switch st.Level {
	case engine.Unseen:
		return "start a rep → Exposed"
	case engine.Exposed:
		return "merge a rep → Guided"
	case engine.Guided, engine.Independent:
		return fmt.Sprintf("%d/%d clean reps → %s", st.CleanSinceLevel, cleanNeeded, st.Level+1)
	case engine.Fluent:
		return "top level"
	}
	return "—"
}

func pct(x float64) int { return int(x*100 + 0.5) }

func fnum(x float64) string {
	if x == float64(int64(x)) {
		return fmt.Sprintf("%d", int64(x))
	}
	return fmt.Sprintf("%.1f", x)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

var mdReplacer = strings.NewReplacer(
	`\`, `\\`, `*`, `\*`, `_`, `\_`, `~`, `\~`, "`", "\\`", `|`, `\|`, `>`, `\>`, `@`, "@200b",
)

// mdEscape neutralises Discord markdown and mentions in user text.
func mdEscape(s string) string { return mdReplacer.Replace(s) }

func cellEscape(s string) string {
	return mdEscape(strings.Join(strings.Fields(s), " "))
}

// clipLines joins lines and drops trailing ones (keeping the last, the next
// edge) until the text fits in max runes.
func clipLines(lines []string, limit int) string {
	out := strings.Join(lines, "\n")
	for utf8.RuneCountInString(out) > limit && len(lines) > 2 {
		lines = append(lines[:len(lines)-2], lines[len(lines)-1])
		out = strings.Join(lines, "\n")
	}
	if utf8.RuneCountInString(out) > limit {
		r := []rune(out)
		out = string(r[:limit-1]) + "…"
	}
	return out
}
