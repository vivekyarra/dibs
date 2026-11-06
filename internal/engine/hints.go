package engine

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Hint levels. Each unlocks only after the junior has genuinely tried.
const (
	HintNudge   = 1 // a question that points at the next thing to think about
	HintPointer = 2 // names the file/function/concept and one doc link, no code
	HintOutline = 3 // numbered steps or pseudocode, max 8 lines, no repo identifiers
	HintAnalogy = 4 // a worked example on different code, max 20 lines
	MaxHint     = HintAnalogy
)

// HintNames are the labels used in chat and reports.
var HintNames = map[int]string{1: "Nudge", 2: "Pointer", 3: "Outline", 4: "Analogy"}

// HintRules are what each level allows, in the words agents receive.
var HintRules = map[int]string{
	1: "L1 Nudge: ask ONE question that points at the next thing to think about. No file names, no code.",
	2: "L2 Pointer: name the file/function/concept and give at most one doc link. No code at all.",
	3: "L3 Outline: numbered steps or pseudocode, at most 8 lines, no real identifiers from this repo.",
	4: "L4 Analogy: a worked example on DIFFERENT code (a toy problem), at most 20 lines of code.",
}

// unlock thresholds: level → (attempts, minutes)
var unlockAt = map[int]struct {
	attempts int
	minutes  float64
}{2: {1, 20}, 3: {2, 45}, 4: {3, 90}}

// HintGate is the facts that decide how much help is allowed right now.
type HintGate struct {
	Attempts       int
	StartedAt      time.Time // zero if not started (reserved but not begun)
	SeniorUnlocked int       // highest level a senior unlocked, 0 if none
}

// AllowedLevel returns the deepest hint level allowed now.
func AllowedLevel(g HintGate, now time.Time) int {
	mins := 0.0
	if !g.StartedAt.IsZero() {
		mins = now.Sub(g.StartedAt).Minutes()
	}
	allowed := HintNudge
	for lvl := HintPointer; lvl <= MaxHint; lvl++ {
		t := unlockAt[lvl]
		if g.Attempts >= t.attempts || mins >= t.minutes {
			allowed = lvl
		}
	}
	if g.SeniorUnlocked > allowed {
		allowed = min(g.SeniorUnlocked, MaxHint)
	}
	return allowed
}

// NextUnlock describes, in plain words, what unlocks the next level.
func NextUnlock(g HintGate, now time.Time) string {
	cur := AllowedLevel(g, now)
	if cur >= MaxHint {
		return "all hint levels are open"
	}
	next := cur + 1
	t := unlockAt[next]
	needA := t.attempts - g.Attempts
	if g.StartedAt.IsZero() {
		return fmt.Sprintf("L%d after %d more attempt(s), or %0.f min after starting", next, needA, t.minutes)
	}
	left := t.minutes - now.Sub(g.StartedAt).Minutes()
	return fmt.Sprintf("L%d after %d more attempt(s) or in %d min", next, needA, int(math.Ceil(left)))
}

// GrantHint clamps a requested level to what is allowed now.
func GrantHint(requested int, g HintGate, now time.Time) (granted int, clamped bool) {
	if requested < HintNudge {
		requested = HintNudge
	}
	if requested > MaxHint {
		requested = MaxHint
	}
	allowed := AllowedLevel(g, now)
	if requested > allowed {
		return allowed, true
	}
	return requested, false
}

// Violation is a concrete way a hint's text breaks its level's rules.
type Violation struct {
	Rule   string `json:"rule"`
	Detail string `json:"detail"`
}

var (
	fenceRe   = regexp.MustCompile("(?s)```[^\\n]*\\n(.*?)```")
	pathRe    = regexp.MustCompile(`(?i)\b[\w./-]+\.(py|go|ts|tsx|js|jsx|rs|java|kt|swift|rb|php|cs|cpp|c|h|sql|yaml|yml|toml)\b`)
	codeishRe = regexp.MustCompile(`(?m)^\s*(def |func |class |return |import |from \S+ import|if .*:$|for .*:$|while .*:$|[A-Za-z_][\w.]*\s*=\s*\S|[A-Za-z_][\w.]*\(.*\)\s*$)`)
	identRe   = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{3,}`)
)

func codeLines(text string) (fenced int, inline int) {
	for _, m := range fenceRe.FindAllStringSubmatch(text, -1) {
		for _, l := range strings.Split(strings.TrimRight(m[1], "\n"), "\n") {
			if strings.TrimSpace(l) != "" {
				fenced++
			}
		}
	}
	stripped := fenceRe.ReplaceAllString(text, "")
	inline = len(codeishRe.FindAllString(stripped, -1))
	return
}

// CheckHint enforces the hint ladder on the actual words an agent is about to
// post. repoIdents are identifiers that appear in the files this ticket
// touches; quoting more than three of them in an L3/L4 hint is treated as
// handing over the solution. It returns no violations when the text is fine.
func CheckHint(level int, text string, repoIdents map[string]bool) []Violation {
	var v []Violation
	fenced, inline := codeLines(text)
	code := fenced + inline
	switch level {
	case HintNudge:
		if code > 0 {
			v = append(v, Violation{"no_code", fmt.Sprintf("L1 allows no code; found %d code line(s)", code)})
		}
		if p := pathRe.FindString(text); p != "" {
			v = append(v, Violation{"no_files", "L1 allows no file names; found " + p})
		}
		if strings.Count(text, "?") == 0 {
			v = append(v, Violation{"ask_question", "L1 must ask a question"})
		}
	case HintPointer:
		if code > 0 {
			v = append(v, Violation{"no_code", fmt.Sprintf("L2 allows no code; found %d code line(s)", code)})
		}
	case HintOutline:
		if code > 8 {
			v = append(v, Violation{"max_lines", fmt.Sprintf("L3 allows at most 8 lines of pseudocode; found %d", code)})
		}
	case HintAnalogy:
		if code > 20 {
			v = append(v, Violation{"max_lines", fmt.Sprintf("L4 allows at most 20 lines; found %d", code)})
		}
	}
	if level >= HintOutline && len(repoIdents) > 0 {
		seen := map[string]bool{}
		for _, id := range identRe.FindAllString(text, -1) {
			if repoIdents[id] && !commonWord[strings.ToLower(id)] {
				seen[id] = true
			}
		}
		if len(seen) > 3 {
			names := make([]string, 0, len(seen))
			for k := range seen {
				names = append(names, k)
			}
			v = append(v, Violation{"repo_identifiers", fmt.Sprintf("uses %d identifiers from the files this ticket touches (%s); that is the solution, not a hint", len(seen), strings.Join(sortStrings(names), ", "))})
		}
	}
	return v
}

var commonWord = map[string]bool{"self": true, "none": true, "true": true, "false": true, "return": true, "with": true,
	"from": true, "import": true, "this": true, "that": true, "data": true, "value": true, "error": true, "string": true,
	"list": true, "dict": true, "func": true, "def": true, "class": true, "test": true, "request": true, "response": true}

func sortStrings(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}
