package report

import (
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vivekyarra/dibs/internal/engine"
)

const baseline = 45

var t0 = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC) // Monday of 2026-W43

func rep(n int, title string, day, hint, rounds int, sim bool, ch ...engine.SkillChange) engine.Rep {
	start := t0.AddDate(0, 0, day)
	return engine.Rep{
		Number: n, Title: title, Junior: "asha", PRURL: "https://github.com/acme/shop/pull/" + strconv.Itoa(n+100),
		MaxHint: hint, ReviewRounds: rounds, Attempts: 2,
		StartedAt: start, FinishedAt: start.Add(30 * time.Hour),
		Approvals: 2, SeniorComments: 3 - min(hint, 3)/2, TeachingMoments: boolInt(hint >= 3),
		SkillChanges: ch, Sample: sim,
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func sc(skill string, from, to engine.Level) engine.SkillChange {
	return engine.SkillChange{Skill: skill, From: from, To: to, Reason: "merged a rep"}
}

func fixture() JuniorView {
	w43 := []engine.Rep{
		rep(1, "Add request ID to logs", 0, 3, 2, true, sc("logging", engine.Exposed, engine.Guided)),
		rep(2, "Validate cart quantities", 2, 3, 2, true, sc("validation", engine.Exposed, engine.Guided)),
	}
	w44 := []engine.Rep{
		rep(4, "Retry failed emails", 7, 3, 1, true, sc("error-handling", engine.Unseen, engine.Exposed)),
		rep(5, "Paginate orders API", 9, 3, 2, true),
	}
	rel := rep(7, "Fix flaky checkout test", 15, 2, 0, false)
	rel.Released, rel.ReleasedReason = true, "deadline moved up"
	w45 := []engine.Rep{
		rep(3, "Idempotency keys for payments", 14, 2, 1, false, sc("idempotency", engine.Exposed, engine.Guided)),
		rep(6, "Webhook retries <script>alert(1)</script>", 15, 3, 2, false, sc("error-handling", engine.Exposed, engine.Guided)),
		rep(10, "Timezone bug in reports", 16, 0, 1, false),
		rel,
	}
	weeks := []engine.Week{
		engine.Summarize("asha", "2026-W43", w43, baseline),
		engine.Summarize("asha", "2026-W44", w44, baseline),
		engine.Summarize("asha", "2026-W45", w45, baseline),
	}
	order := []string{"logging", "validation", "error-handling", "idempotency", "concurrency", "timezones"}
	levels := map[string]engine.SkillState{
		"logging":        {Level: engine.Independent},
		"validation":     {Level: engine.Guided, CleanSinceLevel: 1},
		"error-handling": {Level: engine.Guided},
		"idempotency":    {Level: engine.Guided},
		"concurrency":    {Level: engine.Unseen},
		"timezones":      {Level: engine.Fluent},
	}
	var hist []LevelPoint
	add := func(w string, m map[string]engine.Level) {
		for s, l := range m {
			hist = append(hist, LevelPoint{w, s, l})
		}
	}
	add("2026-W43", map[string]engine.Level{"logging": engine.Guided, "validation": engine.Guided, "error-handling": engine.Unseen, "idempotency": engine.Exposed, "timezones": engine.Independent})
	add("2026-W44", map[string]engine.Level{"logging": engine.Independent, "error-handling": engine.Exposed})
	add("2026-W45", map[string]engine.Level{"error-handling": engine.Guided, "idempotency": engine.Guided, "timezones": engine.Fluent})
	fu, _ := engine.FirstUnassisted(append(append(append([]engine.Rep{}, w43...), w44...), w45...))
	return JuniorView{Handle: "asha", Weeks: weeks, Levels: levels, SkillOrder: order, History: hist, FirstUnassisted: &fu}
}

func fixtureInput() Input {
	return Input{
		Team: "Acme <Shop>", Repo: "acme/demo-shop", Generated: time.Date(2026, 11, 6, 17, 30, 0, 0, time.UTC),
		BaselineMinutes: baseline,
		Juniors:         []JuniorView{fixture(), {Handle: "ben", SkillOrder: []string{"logging"}, Levels: map[string]engine.SkillState{"logging": {Level: engine.Exposed}}}},
	}
}

// --- markdown ----------------------------------------------------------------

func TestWeekMarkdown(t *testing.T) {
	j := fixture()
	md := WeekMarkdown(j, j.Weeks[2], baseline)
	t.Log("\n" + md)
	for _, want := range []string{
		"📈 **Dibs ramp report** · asha · week 2026-W45\n",
		"**Reps:** 3 merged (#3 Idempotency keys for payments, #6 Webhook retries <script",
		"#10 Timezone bug in reports)",
		"**Avg hint depth:** L1.7 (last week L3.0) ↓",
		"**Independence ratio:** 33% (1 of 3 with ≤ L1)",
		"idempotency Exposed→Guided (#3) · error-handling Exposed→Guided (#6)",
		"min saved vs 45-min/PR baseline, estimate)",
		"**Delivery:** 1 release (#7 deadline moved up)",
		"**Next edge:** validation (Guided, 1/2 clean reps)",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in\n%s", want, md)
		}
	}
	if strings.Contains(md, "(sample)") {
		t.Error("real week must not be tagged sample")
	}
	if n := utf8.RuneCountInString(md); n >= MaxChatChars {
		t.Errorf("too long: %d", n)
	}
	// Senior minutes: approvals 2 + comments + teaching per rep; check totals match the week.
	w := j.Weeks[2]
	if !strings.Contains(md, "**Senior time:** "+fnum(w.SeniorMinutes)+" min total (≈ "+fnum(w.SavedMinutes)+" min saved") {
		t.Errorf("senior time line wrong: %v / %v\n%s", w.SeniorMinutes, w.SavedMinutes, md)
	}
}

func TestWeekMarkdownSampleAndTrend(t *testing.T) {
	j := fixture()
	first := WeekMarkdown(j, j.Weeks[0], baseline)
	if !strings.Contains(first, "week 2026-W43 (sample)") || !strings.Contains(first, "(first measured week)") {
		t.Errorf("first week:\n%s", first)
	}
	second := WeekMarkdown(j, j.Weeks[1], baseline)
	if !strings.Contains(second, "L3.0 (last week L3.0) →") {
		t.Errorf("flat arrow missing:\n%s", second)
	}
	if !strings.Contains(second, "error-handling Unseen→Exposed (#4)") {
		t.Errorf("skill move missing:\n%s", second)
	}
	up := j.Weeks[1]
	up.AvgHint = 3.5
	j.Weeks[1] = up
	if s := WeekMarkdown(j, up, baseline); !strings.Contains(s, "↑") {
		t.Errorf("up arrow missing:\n%s", s)
	}
}

func TestWeekMarkdownEmptyAndLong(t *testing.T) {
	j := JuniorView{Handle: "zed_*x"}
	md := WeekMarkdown(j, engine.Week{Label: "2026-W45"}, baseline)
	for _, want := range []string{"zed\\_\\*x", "none merged this week", "n/a", "**Skills moved:** none", "0 releases", "no skills tracked"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in\n%s", want, md)
		}
	}
	// A huge week stays under the chat limit.
	var reps []engine.Rep
	for i := 0; i < 40; i++ {
		r := rep(i, strings.Repeat("very long title ", 20), 0, 1, 1, false,
			sc(strings.Repeat("skill", 10), engine.Exposed, engine.Guided), sc("x", engine.Guided, engine.Guided))
		if i%3 == 0 {
			r.Released, r.ReleasedReason = true, strings.Repeat("why ", 30)
		}
		reps = append(reps, r)
	}
	w := engine.Summarize("asha", "2026-W46", reps, baseline)
	j = fixture()
	md = WeekMarkdown(j, w, baseline)
	if n := utf8.RuneCountInString(md); n >= MaxChatChars {
		t.Errorf("too long: %d", n)
	}
	for _, want := range []string{"+20 more", "+34 more", "+11 more", "…"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in\n%s", want, md)
		}
	}
}

func TestNextEdge(t *testing.T) {
	cases := []struct {
		levels map[string]engine.SkillState
		want   string
	}{
		{map[string]engine.SkillState{"a": {Level: engine.Guided}, "b": {Level: engine.Exposed}}, "b (Exposed)"},
		{map[string]engine.SkillState{"a": {Level: engine.Independent}, "b": {Level: engine.Unseen}}, "b (Unseen)"},
		{map[string]engine.SkillState{"a": {Level: engine.Fluent}}, "Independent or better"},
	}
	for _, c := range cases {
		if got := nextEdge(c.levels, []string{"a"}); !strings.Contains(got, c.want) {
			t.Errorf("nextEdge = %q, want %q", got, c.want)
		}
	}
}

func TestClipLines(t *testing.T) {
	lines := []string{"head", strings.Repeat("a", 50), strings.Repeat("b", 50), "tail"}
	got := clipLines(lines, 70)
	if got != "head\n"+strings.Repeat("a", 50)+"\ntail" {
		t.Errorf("got %q", got)
	}
	got = clipLines([]string{strings.Repeat("z", 100)}, 10)
	if utf8.RuneCountInString(got) != 10 || !strings.HasSuffix(got, "…") {
		t.Errorf("hard clip: %q", got)
	}
}

func TestSkillMapMarkdown(t *testing.T) {
	j := fixture()
	levels := j.Levels
	levels["zz|extra"] = engine.SkillState{Level: engine.Level(9)}
	recent := make([]engine.SkillChange, 0, 12)
	for i := 0; i < 12; i++ {
		recent = append(recent, sc("s"+fnum(float64(i)), engine.Exposed, engine.Guided))
	}
	md := SkillMapMarkdown("asha", levels, j.SkillOrder, recent)
	t.Log("\n" + md)
	for _, want := range []string{
		"🗺️ **Skill map · asha**",
		"| Skill | Level | Progress | Toward next |",
		"| logging | Independent | ▰▰▰▱ | 0/2 clean reps → Fluent |",
		"| validation | Guided | ▰▰▱▱ | 1/2 clean reps → Independent |",
		"| concurrency | Unseen | ▱▱▱▱ | start a rep → Exposed |",
		"| timezones | Fluent | ▰▰▰▰ | top level |",
		"| zz\\|extra | Level(9) | ▰▰▰▰ | — |",
		"**Recent moves**", "- s11 Exposed→Guided — merged a rep", "**Legend:**",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in\n%s", want, md)
		}
	}
	if strings.Contains(md, "- s1 Exposed") {
		t.Error("recent moves should keep only the last 10")
	}
	// Order: catalog first.
	if strings.Index(md, "| logging") > strings.Index(md, "| validation") {
		t.Error("catalog order not kept")
	}
	md = SkillMapMarkdown("ben", map[string]engine.SkillState{"x": {Level: engine.Exposed}}, nil, nil)
	if !strings.Contains(md, "| x | Exposed | ▰▱▱▱ | merge a rep → Guided |") || strings.Contains(md, "Recent moves") {
		t.Errorf("small map:\n%s", md)
	}
	if md := SkillMapMarkdown("nobody", nil, nil, nil); !strings.Contains(md, "No skills tracked yet") {
		t.Errorf("empty map:\n%s", md)
	}
	if bar(engine.Level(-1)) != "▱▱▱▱" {
		t.Error("negative level bar")
	}
}

// --- HTML --------------------------------------------------------------------

func render(t *testing.T, in Input) string {
	t.Helper()
	out, err := HTML(in)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestHTMLContainsKeyElements(t *testing.T) {
	s := render(t, fixtureInput())
	for _, want := range []string{
		"<!doctype html>", `<p class="wordmark">Dibs</p>`,
		"The AI does the work. <em>Your juniors still get the reps.</em>",
		"Acme &lt;Shop&gt;", "acme/demo-shop", "Fri 6 Nov 2026, 17:30 UTC",
		`class="badge"`, "Sample team",
		"prefers-color-scheme:dark",
		"Reps merged", "Avg hint depth", "Independence", "Senior time saved", "First unassisted merge",
		"(start)", "delta good",
		"Average hint depth by week", "Senior minutes per rep by week", "baseline 45 min/PR",
		"Skill levels by week", "<title>idempotency · 2026-W45: Guided</title>",
		`href="https://github.com/acme/shop/pull/103"`, ">#3</a>",
		"Released", "deadline moved up", "idempotency Exposed",
		`id="j-asha"`, `href="#j-ben"`,
		"No reps yet for @ben", "Current level of each skill",
		`<th scope="col">Max hint</th>`, "L0 · none", "L4 · Analogy",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{"http://cdn", "https://cdn", "<script", "<link"} {
		if strings.Contains(s, bad) {
			t.Errorf("must be self-contained, found %q", bad)
		}
	}
	// First unassisted: #10 finished day 16+30h, first start day 0 → 17.3 days.
	if !strings.Contains(s, "17.3<small>days</small>") || !strings.Contains(s, "#10 · zero hints") {
		t.Error("first-unassisted tile wrong")
	}
}

func TestHTMLEscapesScript(t *testing.T) {
	in := fixtureInput()
	in.Juniors[0].Handle = `<script>alert("h")</script>`
	in.Juniors[0].SkillOrder = append(in.Juniors[0].SkillOrder, `<script>x</script>`)
	in.Juniors[0].Weeks[2].Reps[0].PRURL = "javascript:alert(1)"
	s := render(t, in)
	if strings.Contains(strings.ToLower(s), "<script") {
		t.Fatal("unescaped <script> in output")
	}
	if !strings.Contains(s, "Webhook retries &lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("title not escaped as text")
	}
	if strings.Contains(s, `href="javascript:`) {
		t.Error("javascript: URL not neutralised")
	}
	checkSVGs(t, s)
}

func TestHTMLZeroWeeksAndNoJuniors(t *testing.T) {
	s := render(t, Input{})
	if !strings.Contains(s, "No juniors on the ledger yet") || strings.Contains(s, `class="badge"`) {
		t.Error("no-junior empty state wrong")
	}
	s = render(t, Input{Juniors: []JuniorView{{Handle: "kim"}, {Handle: "kim"}, {Handle: "!!!"}}})
	for _, want := range []string{"No reps yet for @kim", `id="j-kim-2"`, `id="j-junior"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
	checkSVGs(t, s)
}

func TestHTMLWeeksWithGapsAndNoUnassisted(t *testing.T) {
	j := fixture()
	j.FirstUnassisted = nil
	// A week with only a release (no merges) breaks the line and shows "no reps".
	rel := rep(20, "Released only", 21, 4, 0, false)
	rel.Released = true
	j.Weeks = append(j.Weeks, engine.Summarize("asha", "2026-W46", []engine.Rep{rel}, baseline), engine.Week{Label: "2026-W47"})
	s := render(t, Input{BaselineMinutes: 0, Juniors: []JuniorView{j}})
	for _, want := range []string{"Not yet", "no reps", "baseline 45 min/PR", "(start)"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
	checkSVGs(t, s)

	// A single fast unassisted week: hours, no delta.
	r := rep(30, "Quick fix", 0, 0, 0, false)
	r.FinishedAt = r.StartedAt.Add(5 * time.Hour)
	w := engine.Summarize("ben", "2026-W45", []engine.Rep{r}, baseline)
	s = render(t, Input{Juniors: []JuniorView{{Handle: "ben", Weeks: []engine.Week{w}, FirstUnassisted: &r}}})
	if !strings.Contains(s, "5<small>hours</small>") || strings.Contains(s, "delta good") {
		t.Error("single-week tiles wrong")
	}
	r.FinishedAt = time.Time{}
	s = render(t, Input{Juniors: []JuniorView{{Handle: "ben", Weeks: []engine.Week{w}, FirstUnassisted: &r}}})
	if !strings.Contains(s, ">Done<") {
		t.Error("unknown-duration first unassisted should say Done")
	}
}

func TestDeltaAndHelpers(t *testing.T) {
	if d, c := deltaText(0.01, true, "%.0f", "W1"); c != "flat" || !strings.Contains(d, "no change") {
		t.Error(d, c)
	}
	if _, c := deltaText(5, true, "%.0f", "W1"); c != "good" {
		t.Error(c)
	}
	if _, c := deltaText(1, false, "%.0f", "W1"); c != "bad" {
		t.Error(c)
	}
	if hintName(0) != "none" || hintName(9) != "?" || hintName(2) != "Pointer" {
		t.Error("hintName")
	}
	if shortWeek("2026-W45") != "W45" || shortWeek("Now") != "Now" {
		t.Error("shortWeek")
	}
	if niceCeil(0) != 10 || niceCeil(47) != 50 || niceCeil(9.9) != 10 || niceCeil(50.4) != 60 || niceCeil(101) != 120 {
		t.Error("niceCeil", niceCeil(47), niceCeil(101))
	}
	if lvl(-3) != 0 || lvl(12) != 4 {
		t.Error("lvl clamp")
	}
}

var svgRe = regexp.MustCompile(`(?s)<svg\b.*?</svg>`)

// checkSVGs parses every inline <svg> as XML.
func checkSVGs(t *testing.T, page string) {
	t.Helper()
	svgs := svgRe.FindAllString(page, -1)
	if len(svgs) == 0 {
		t.Fatal("no <svg> found")
	}
	for i, s := range svgs {
		d := xml.NewDecoder(strings.NewReader(s))
		d.Strict = true
		for {
			_, err := d.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("svg %d not well-formed: %v\n%s", i, err, s)
			}
		}
	}
}

func TestSVGWellFormed(t *testing.T) {
	s := render(t, fixtureInput())
	if n := len(svgRe.FindAllString(s, -1)); n < 5 {
		t.Errorf("expected logo + 3 charts + 1 heatmap, got %d svgs", n)
	}
	checkSVGs(t, s)
	for _, svg := range svgRe.FindAllString(s, -1)[1:] {
		if !strings.Contains(svg, `role="img"`) {
			t.Error("chart svg missing role=img")
		}
	}
}

// TestRenderFixtureDashboard writes the fixture dashboard so it can be opened
// in a browser. Set DIBS_DASHBOARD_OUT to choose the path.
func TestRenderFixtureDashboard(t *testing.T) {
	path := RenderFixtureToTemp(t)
	t.Logf("fixture dashboard written to %s", path)
}

// RenderFixtureToTemp renders the fixture dashboard to os.TempDir (or
// $DIBS_DASHBOARD_OUT) and returns the path.
func RenderFixtureToTemp(t *testing.T) string {
	t.Helper()
	out, err := HTML(fixtureInput())
	if err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("DIBS_DASHBOARD_OUT")
	if path == "" {
		path = filepath.Join(os.TempDir(), "dibs-dashboard-fixture.html")
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("Dibs")) {
		t.Fatal("empty render")
	}
	return path
}
