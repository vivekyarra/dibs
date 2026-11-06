package tracker

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Run: go test ./internal/tracker -run '^$' -fuzz FuzzCloses -fuzztime 20s
func FuzzCloses(f *testing.F) {
	for _, s := range []string{"Closes #3", "fixes: #3", "Resolves owner/repo#12", "closes #3, closes #4", "#3",
		"enclosed #3", "closes#3", "Fix #0", "closes #99999999999999999999", "reverts #4", "Closes  : o/r#1 fixes #1",
		"ﬁxes #3", "CLOSES #3", "closes #3\x00"} {
		f.Add("", s)
	}
	f.Fuzz(func(t *testing.T, title, body string) {
		pr := PR{Title: title, Body: body}
		got := Closes(pr)
		if !reflect.DeepEqual(got, Closes(pr)) {
			t.Fatal("not deterministic")
		}
		if !sort.IntsAreSorted(got) {
			t.Fatalf("not sorted: %v", got)
		}
		for i, n := range got {
			if n < 0 {
				t.Fatalf("negative issue %d", n)
			}
			if i > 0 && got[i-1] == n {
				t.Fatalf("duplicate %d", n)
			}
		}
		if len(got) > 0 && !strings.Contains(title+"\n"+body, "#") {
			t.Fatalf("found %v without any '#'", got)
		}
		r := Reverts(pr)
		if r < 0 || r != Reverts(pr) {
			t.Fatalf("Reverts %d", r)
		}
	})
}

// Run: go test ./internal/tracker -run '^$' -fuzz FuzzDueFrom -fuzztime 20s
func FuzzDueFrom(f *testing.F) {
	for _, s := range []string{"due:2026-11-10", "due:2026-13-01", "due:", "Due:2026-11-10", "due:0000-01-01",
		"due:9999-12-31", "due:2026-02-29", "due:2028-02-29", "due:+2026-11-10", "x"} {
		f.Add(s)
	}
	ms := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, label string) {
		got := DueFrom(Issue{Labels: []string{label}, DueOn: ms})
		if got.Equal(ms) {
			return // fell back to the milestone (or the label parsed to exactly that instant, impossible: 23:59)
		}
		if !strings.HasPrefix(label, "due:") {
			t.Fatalf("label %q without due: prefix produced %v", label, got)
		}
		if got.Location() != time.UTC || got.Hour() != 23 || got.Minute() != 59 || got.Second() != 0 {
			t.Fatalf("label %q → %v, want end of day UTC", label, got)
		}
		if got.Format("2006-01-02") != strings.TrimPrefix(label, "due:") {
			t.Fatalf("label %q → date %s (not canonical round trip)", label, got.Format("2006-01-02"))
		}
	})
}
