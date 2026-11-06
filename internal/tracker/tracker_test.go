package tracker

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	_ Tracker = (*Memory)(nil)
	_ Tracker = (*GitHub)(nil)
)

func TestClosesKnownForms(t *testing.T) {
	cases := []struct {
		title, body string
		want        []int
	}{
		{"", "Closes #3", []int{3}},
		{"", "fixes: #3", []int{3}},
		{"", "Resolves owner/repo#12", []int{12}},
		{"", "closes #3, closes #4", []int{3, 4}},
		{"Fix #8", "", []int{8}},
		{"", "closed #5\nFIXED #6\nresolved #7\nresolve #9\nclose #10\nfixes #11", []int{5, 6, 7, 9, 10, 11}},
		{"Closes #4", "closes #4 and Fixes #2", []int{2, 4}}, // dedup + sorted
		{"", "Resolves my.org/my-repo.js#21", []int{21}},
		{"", "(closes #13)", []int{13}},
		{"", "un-fixes #14", []int{14}}, // '-' is a word boundary
		{"", "Closes\t#15", []int{15}},
		{"", "Closes  :  #16", []int{16}},
		// Documented non-matches (current behaviour):
		{"", "#3", nil},
		{"", "enclosed #3", nil},                            // no word boundary before "close"
		{"", "prefixes #3", nil},                            // no word boundary before "fix"
		{"", "closes#3", nil},                               // whitespace required before '#'
		{"", "fixes:#3", nil},                               // colon must be followed by whitespace
		{"", "closing #3", nil},                             // only close/closes/closed
		{"", "closes issue #3", nil},                        // nothing may sit between keyword and ref
		{"", "Closes https://github.com/o/r/issues/3", nil}, // URL form not supported
		{"", "Closes GH-3", nil},
		{"", "closes #99999999999999999999999", nil}, // overflow is skipped, not a panic
	}
	for _, c := range cases {
		got := Closes(PR{Title: c.title, Body: c.body})
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Closes(%q, %q) = %v, want %v", c.title, c.body, got, c.want)
		}
	}
	// "closes #3, #4": like GitHub, only the first ref after a keyword counts.
	if got := Closes(PR{Body: "closes #3, #4"}); !reflect.DeepEqual(got, []int{3}) {
		t.Errorf("list form: %v", got)
	}
}

func TestReverts(t *testing.T) {
	cases := map[string]int{
		"Reverts owner/repo#12":            12,
		"This reverts #4":                  4,
		"revert #7":                        7,
		"REVERTS #8 and reverts #9":        8, // first one wins
		`Revert "Make orders idempotent"`:  0,
		"reverted #3":                      0,
		"#3":                               0,
		"reverts #99999999999999999999999": 0,
	}
	for body, want := range cases {
		if got := Reverts(PR{Body: body}); got != want {
			t.Errorf("Reverts(%q) = %d, want %d", body, got, want)
		}
	}
	if Reverts(PR{Title: "Reverts #5"}) != 5 {
		t.Error("title is searched too")
	}
}

func TestDueFrom(t *testing.T) {
	ms := time.Date(2026, 12, 1, 7, 0, 0, 0, time.UTC)
	want := time.Date(2026, 11, 10, 23, 59, 0, 0, time.UTC)
	cases := []struct {
		labels []string
		want   time.Time
	}{
		{[]string{"due:2026-11-10"}, want},
		{[]string{"bug", "due:2026-11-10"}, want},
		{[]string{"due:garbage", "due:2026-11-10"}, want}, // first *valid* label wins
		{[]string{"due:2026-11-10", "due:2026-11-20"}, want},
		{[]string{"due:2026-13-01"}, ms},
		{[]string{"Due:2026-11-10"}, ms},  // prefix is case-sensitive
		{[]string{"due: 2026-11-10"}, ms}, // no spaces allowed
		{[]string{"due:2026-11-10 "}, ms},
		{nil, ms},
	}
	for _, c := range cases {
		if got := DueFrom(Issue{Labels: c.labels, DueOn: ms}); !got.Equal(c.want) {
			t.Errorf("DueFrom(%v) = %v, want %v", c.labels, got, c.want)
		}
	}
	if !DueFrom(Issue{}).IsZero() {
		t.Error("no label, no milestone → zero")
	}
}

func TestLabelHelpers(t *testing.T) {
	is := Issue{Labels: []string{"Skill:Idem", "dibs:reserved:asha", "dibs:reserved:ben"}}
	if !HasLabel(is, "skill:idem") || HasLabel(is, "skill") {
		t.Error("HasLabel")
	}
	if ReservedFor(is) != "asha" || ReservedFor(Issue{}) != "" {
		t.Error("ReservedFor")
	}
}

func TestMemoryBehaviour(t *testing.T) {
	m := NewMemory("o/r")
	a := m.CreateIssue("first", "body", "bug")
	b := m.CreateIssue("second", "", "skill:idem")
	p := m.OpenPR("asha", "asha/fix", "Fix things", "Closes #1\nfixes o/r#2\ncloses #999")
	if a != 1 || b != 2 || p != 3 {
		t.Fatalf("numbering %d %d %d", a, b, p)
	}
	is, _ := m.Issues()
	if len(is) != 2 || is[0].Number != 1 || is[1].URL != "https://github.com/o/r/issues/2" {
		t.Fatalf("issues %+v", is)
	}
	// Returned issues are copies.
	is[0].Labels[0] = "mutated"
	if got, _ := m.Issue(1); got.Labels[0] != "bug" {
		t.Fatal("Issues() leaked internal slice")
	}
	if _, err := m.Issue(99); err == nil {
		t.Fatal("unknown issue")
	}

	if err := m.AddLabels(1, "BUG", "dibs:reserved:asha"); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Issue(1); !reflect.DeepEqual(got.Labels, []string{"bug", "dibs:reserved:asha"}) {
		t.Fatalf("AddLabels dedupe: %v", got.Labels)
	}
	if err := m.RemoveLabel(1, "Bug"); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveLabel(1, "absent"); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Issue(1); !reflect.DeepEqual(got.Labels, []string{"dibs:reserved:asha"}) {
		t.Fatalf("RemoveLabel: %v", got.Labels)
	}
	m.SetLabels(2, "due:2026-11-10")
	if got, _ := m.Issue(2); !reflect.DeepEqual(got.Labels, []string{"due:2026-11-10"}) {
		t.Fatalf("SetLabels: %v", got.Labels)
	}
	if err := m.Assign(1, "asha"); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Issue(1); !reflect.DeepEqual(got.Assignees, []string{"asha"}) {
		t.Fatal("Assign")
	}
	if err := m.Comment(1, "hello"); err != nil || m.Comments[1][0] != "hello" {
		t.Fatal("Comment")
	}
	if err := m.EnsureLabels([]Label{{"dibs:proposed", "ffffff", "d"}, {"dibs:proposed", "000000", "d2"}}); err != nil {
		t.Fatal(err)
	}
	if m.LabelsM["dibs:proposed"].Color != "000000" {
		t.Fatal("EnsureLabels should update")
	}
	for _, err := range []error{m.AddLabels(99, "x"), m.RemoveLabel(99, "x"), m.Assign(99, "x"),
		m.ReviewPR(99, "x", "APPROVED", 0), m.MergePR(99, time.Now())} {
		if err == nil {
			t.Fatal("unknown number must error")
		}
	}
	if _, _, err := m.Reviews(99); err == nil {
		t.Fatal("Reviews unknown")
	}

	if err := m.ReviewPR(p, "maya", "CHANGES_REQUESTED", 2); err != nil {
		t.Fatal(err)
	}
	m.ReviewPR(p, "maya", "APPROVED", 0)
	rv, ca, _ := m.Reviews(p)
	if len(rv) != 2 || rv[1].State != "APPROVED" || !reflect.DeepEqual(ca, []string{"maya", "maya"}) {
		t.Fatalf("reviews %+v %v", rv, ca)
	}

	at := time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)
	if err := m.MergePR(p, at); err != nil {
		t.Fatal(err)
	}
	prs, _ := m.PRs()
	if len(prs) != 1 || !prs[0].Merged || prs[0].State != "closed" || !prs[0].MergedAt.Equal(at) {
		t.Fatalf("merged PR %+v", prs)
	}
	if open, _ := m.Issues(); len(open) != 0 {
		t.Fatalf("MergePR should close #1 and #2 (incl. owner/repo#2), open: %+v", open)
	}
	if got, _ := m.Issue(1); got.State != "closed" {
		t.Fatal("closed issue still fetchable by number")
	}
}

func TestMemoryJSONRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tracker.json")
	m, err := LoadMemory(path, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("LoadMemory should create the file:", err)
	}
	m.CreateIssue("one", "b", "skill:idem", "due:2026-11-10")
	m.CreateIssue("two", "")
	p := m.OpenPR("asha", "asha/x", "t", "Closes #1")
	m.ReviewPR(p, "maya", "APPROVED", 1)
	m.Assign(1, "asha")
	m.Comment(2, "hi")
	m.EnsureLabels([]Label{{"dibs:proposed", "abcdef", "proposed"}})
	m.MergePR(p, time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC))

	m2, err := LoadMemory(path, "ignored/when-file-exists")
	if err != nil {
		t.Fatal(err)
	}
	if m2.Repo != "o/r" || m2.NextNum != 4 {
		t.Fatalf("repo/next: %q %d", m2.Repo, m2.NextNum)
	}
	for _, n := range []int{1, 2} {
		a, _ := m.Issue(n)
		b, _ := m2.Issue(n)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("issue %d:\n%+v\n%+v", n, a, b)
		}
	}
	p1, _ := m.PRs()
	p2, _ := m2.PRs()
	if len(p2) != 1 || !p1[0].MergedAt.Equal(p2[0].MergedAt) {
		t.Fatalf("prs %+v", p2)
	}
	p1[0].MergedAt, p2[0].MergedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(p1, p2) {
		t.Fatalf("prs:\n%+v\n%+v", p1, p2)
	}
	if !reflect.DeepEqual(m.LabelsM, m2.LabelsM) || !reflect.DeepEqual(m.Comments, m2.Comments) {
		t.Fatalf("labels/comments: %v %v", m2.LabelsM, m2.Comments)
	}
	// New writes on the reloaded tracker persist to the same file and continue numbering.
	if n := m2.CreateIssue("three", ""); n != 4 {
		t.Fatalf("next number %d", n)
	}
	m3, _ := LoadMemory(path, "o/r")
	if _, err := m3.Issue(4); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
}

func TestLoadMemoryErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{not json"), 0o644)
	if _, err := LoadMemory(bad, "o/r"); err == nil || !strings.Contains(err.Error(), "bad.json") {
		t.Fatalf("corrupt file: %v", err)
	}
	// A minimal file without labels/comments still gives usable maps.
	minimal := filepath.Join(dir, "minimal.json")
	os.WriteFile(minimal, []byte(`{"issues":{},"prs":{},"repo":"o/r","next":1}`), 0o644)
	m, err := LoadMemory(minimal, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureLabels([]Label{{Name: "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Comment(1, "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMemory(filepath.Join(dir, "no", "such", "dir", "x.json"), "o/r"); err == nil {
		t.Fatal("unwritable path should error")
	}
}
